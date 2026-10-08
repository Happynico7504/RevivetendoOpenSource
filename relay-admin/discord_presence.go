package main

// Discord Rich Presence linking (/my/discord/presence/...). Only offered to a
// player whose PNID is already linked to Discord through the bot's /link_pnid,
// and the Discord account that authorizes must be that linked one. The stored
// tokens are used by account-proxy's discord_presence.go, which publishes the
// presence while the console is online.
//
// Configuration (shared .env): DISCORD_PRESENCE_CLIENT_ID,
// DISCORD_PRESENCE_CLIENT_SECRET, DISCORD_PRESENCE_REDIRECT_URI.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
)

const discordPresenceScopes = "identify openid sdk.social_layer_presence"

func discordPresenceConfigured() bool {
	return os.Getenv("DISCORD_PRESENCE_CLIENT_ID") != "" && os.Getenv("DISCORD_PRESENCE_CLIENT_SECRET") != "" &&
		os.Getenv("DISCORD_PRESENCE_REDIRECT_URI") != ""
}

// Same table as account-proxy's ensureDiscordPresenceTable.
func ensureDiscordPresenceTable() {
	db.Exec(`CREATE TABLE IF NOT EXISTS discord_presence_links (
		pid             BIGINT PRIMARY KEY,
		discord_user_id TEXT NOT NULL,
		access_token    TEXT NOT NULL,
		refresh_token   TEXT NOT NULL,
		expires_at      TIMESTAMPTZ NOT NULL,
		headless_token  TEXT NOT NULL DEFAULT '',
		last_key        TEXT NOT NULL DEFAULT '',
		last_sent_at    TIMESTAMPTZ,
		started_at      TIMESTAMPTZ,
		linked_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`)
}

func discordPresenceEnabled(pid int64) bool {
	var on bool
	db.QueryRow(`SELECT EXISTS(SELECT 1 FROM discord_presence_links WHERE pid = $1)`, pid).Scan(&on)
	return on
}

// myLinkedDiscord is the session's PID and the Discord account its PNID is linked to.
func myLinkedDiscord(r *http.Request) (pid int64, discordID string, ok bool) {
	pid, ok = mySessionPID(r)
	if !ok {
		return 0, "", false
	}
	var pnid string
	db.QueryRow(`SELECT COALESCE(NULLIF(pnid,''), '') FROM pnid_cache WHERE pid = $1`, pid).Scan(&pnid)
	if pnid != "" {
		discordID = discordIDForPNID(pnid)
	}
	return pid, discordID, true
}

func myDiscordPresenceStart(w http.ResponseWriter, r *http.Request) {
	pid, discordID, ok := myLinkedDiscord(r)
	if !ok {
		http.Redirect(w, r, "/inkay/my/login", http.StatusFound)
		return
	}
	if r.Method != http.MethodPost || !discordPresenceConfigured() || discordID == "" {
		http.Redirect(w, r, "/inkay/my/discord", http.StatusSeeOther)
		return
	}
	b := make([]byte, 16)
	rand.Read(b)
	state := hex.EncodeToString(b)
	runtimeCacheSet(context.Background(), "discordpresencestate:"+state, strconv.FormatInt(pid, 10), 10*time.Minute)
	q := url.Values{
		"response_type": {"code"},
		"client_id":     {os.Getenv("DISCORD_PRESENCE_CLIENT_ID")},
		"redirect_uri":  {os.Getenv("DISCORD_PRESENCE_REDIRECT_URI")},
		"scope":         {discordPresenceScopes},
		"state":         {state},
		"prompt":        {"consent"},
	}
	http.Redirect(w, r, "https://discord.com/oauth2/authorize?"+q.Encode(), http.StatusSeeOther)
}

func myDiscordPresenceCallback(w http.ResponseWriter, r *http.Request) {
	back := func(msg string) {
		http.Redirect(w, r, "/inkay/my/discord?msg="+url.QueryEscape(msg), http.StatusSeeOther)
	}
	pid, discordID, ok := myLinkedDiscord(r)
	if !ok {
		http.Redirect(w, r, "/inkay/my/login", http.StatusFound)
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		log.Printf("discord presence: pid=%d authorize error %q: %s", pid, e, r.URL.Query().Get("error_description"))
		switch e {
		case "access_denied":
			back("Discord Rich Presence was cancelled")
		case "invalid_scope":
			back("Discord refused the presence permission (invalid_scope) - the Revivetendo app is not set up for it yet")
		default:
			back("Discord returned an error: " + e)
		}
		return
	}
	code, state := r.URL.Query().Get("code"), r.URL.Query().Get("state")
	v, found := runtimeCacheGet(context.Background(), "discordpresencestate:"+state)
	if code == "" || !found || v != strconv.FormatInt(pid, 10) {
		back("That request expired or does not belong to this account - please try again")
		return
	}
	if discordID == "" {
		back("Link your PNID to Discord with /link_pnid first")
		return
	}

	client := &http.Client{Timeout: 10 * time.Second}
	tokResp, err := client.PostForm("https://discord.com/api/v10/oauth2/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {os.Getenv("DISCORD_PRESENCE_REDIRECT_URI")},
		"client_id":     {os.Getenv("DISCORD_PRESENCE_CLIENT_ID")},
		"client_secret": {os.Getenv("DISCORD_PRESENCE_CLIENT_SECRET")},
	})
	if err != nil {
		log.Printf("discord presence: token exchange: %v", err)
		back("Could not reach Discord")
		return
	}
	defer tokResp.Body.Close()
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	json.NewDecoder(io.LimitReader(tokResp.Body, 1<<16)).Decode(&tok)
	if tokResp.StatusCode != http.StatusOK || tok.AccessToken == "" {
		log.Printf("discord presence: token exchange status %d", tokResp.StatusCode)
		back("Discord did not accept the login")
		return
	}

	req, _ := http.NewRequest("GET", "https://discord.com/api/v10/users/@me", nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	meResp, err := client.Do(req)
	if err != nil {
		back("Could not reach Discord")
		return
	}
	defer meResp.Body.Close()
	var me struct {
		ID string `json:"id"`
	}
	json.NewDecoder(io.LimitReader(meResp.Body, 1<<16)).Decode(&me)
	if meResp.StatusCode != http.StatusOK || me.ID == "" {
		log.Printf("discord presence: /users/@me status %d", meResp.StatusCode)
		back("Could not read your Discord account")
		return
	}
	if me.ID != discordID {
		log.Printf("discord presence: pid=%d authorized as %s but linked to %s", pid, me.ID, discordID)
		back("That Discord account is not the one linked to your PNID - sign in to Discord with the linked account")
		return
	}

	ensureDiscordPresenceTable()
	if _, err := db.Exec(`INSERT INTO discord_presence_links (pid, discord_user_id, access_token, refresh_token, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (pid) DO UPDATE SET discord_user_id = EXCLUDED.discord_user_id, access_token = EXCLUDED.access_token,
			refresh_token = EXCLUDED.refresh_token, expires_at = EXCLUDED.expires_at, linked_at = NOW()`,
		pid, me.ID, tok.AccessToken, tok.RefreshToken, time.Now().Add(time.Duration(tok.ExpiresIn)*time.Second)); err != nil {
		log.Printf("discord presence: save: %v", err)
		back("Could not save the link")
		return
	}
	log.Printf("discord presence: pid=%d enabled (scopes %q)", pid, tok.Scope)
	back("Discord Rich Presence enabled - it shows while your console is online")
}

func myDiscordPresenceUnlink(w http.ResponseWriter, r *http.Request) {
	pid, ok := mySessionPID(r)
	if !ok {
		http.Redirect(w, r, "/inkay/my/login", http.StatusFound)
		return
	}
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/inkay/my/discord", http.StatusSeeOther)
		return
	}
	var access, headless string
	if db.QueryRow(`SELECT access_token, headless_token FROM discord_presence_links WHERE pid = $1`, pid).Scan(&access, &headless) == nil && headless != "" {
		// Clear what is showing now; an expired access token just leaves it to
		// Discord's own 20-minute expiry.
		b, _ := json.Marshal(map[string]string{"token": headless})
		req, _ := http.NewRequest("POST", "https://discord.com/api/v10/users/@me/headless-sessions/delete", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+access)
		req.Header.Set("Content-Type", "application/json")
		if resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req); err == nil {
			resp.Body.Close()
		}
	}
	db.Exec(`DELETE FROM discord_presence_links WHERE pid = $1`, pid)
	http.Redirect(w, r, "/inkay/my/discord?msg="+url.QueryEscape("Discord Rich Presence turned off"), http.StatusSeeOther)
}
