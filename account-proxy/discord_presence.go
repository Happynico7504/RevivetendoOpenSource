package main

// Discord Rich Presence: shows "Playing <game>" on a player's Discord profile
// while their console is online on Revivetendo.
//
// relay-admin's /my/discord page links it (OAuth with the Revivetendo Discord
// app, scopes "openid sdk.social_layer_presence"), and only for a player whose
// PNID is already linked to that same Discord account through the bot's
// /link_pnid. The tokens are stored in discord_presence_links; this loop
// publishes the presence as a Discord "headless session"
// (POST /users/@me/headless-sessions), which needs no Discord client running.
// A headless session expires after 20 minutes, so it is re-sent every 14
// minutes and deleted when the console goes offline.
//
// Headless sessions are not a documented public Discord API (see
// docs.discord.food/resources/presence); they may change without notice.
//
// Configuration: DISCORD_PRESENCE_CLIENT_ID, DISCORD_PRESENCE_CLIENT_SECRET
// (the user-installable app); nothing runs until both are set.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	discordAPI                 = "https://discord.com/api/v10"
	discordPresenceInterval    = 20 * time.Second
	discordPresenceRefresh     = 14 * time.Minute
	discordPresenceIconBaseURL = "https://olv-data.sos-de-fra-1.exo.io"
)

// discordPresenceServers names games by NEX game server ID for titles that are
// not in the curated list (discord_titles.go).
var discordPresenceServers = map[uint32]string{
	0x1005A000: "Wii U Chat",
	0x1010EB00: "Mario Kart 8",
	0x1012F100: "Wii Sports Club",
	0x10145E00: "Angry Birds Star Wars",
	0x10162B00: "Splatoon",
	0x101D9D00: "Minecraft: Wii U Edition",
	0x10110E00: "Super Smash Bros. for Wii U",
	0x100E4B00: "Super Smash Bros. for Wii U",
	0x1014B700: "Minecraft: Wii U Edition",
	0x10138B00: "Pokémon Art Academy",
	0x10104E00: "Animal Crossing: amiibo Festival",
	0x1019EC00: "Yo-kai Watch Blasters",
	0x10189B00: "Pokémon Rumble World",
	0x00134600: "Nintendo Badge Arcade",
}

func discordPresenceConfigured() bool {
	return os.Getenv("DISCORD_PRESENCE_CLIENT_ID") != "" && os.Getenv("DISCORD_PRESENCE_CLIENT_SECRET") != ""
}

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

type discordTitleInfo struct {
	name, icon string
	expires    time.Time
}

var (
	discordTitleCacheMu sync.Mutex
	discordTitleCache   = map[uint64]discordTitleInfo{}
)

// discordTitleFor is the game's name and icon URL from its Juxt community
// ("" when there is none), cached for an hour.
func discordTitleFor(titleID uint64) (string, string) {
	if mongoDB == nil {
		return "", ""
	}
	discordTitleCacheMu.Lock()
	e, ok := discordTitleCache[titleID]
	discordTitleCacheMu.Unlock()
	if ok && time.Now().Before(e.expires) {
		return e.name, e.icon
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var c struct {
		Name string `bson:"name"`
		ID   string `bson:"olive_community_id"`
	}
	opts := options.FindOne().SetSort(bson.D{{Key: "parent", Value: 1}})
	err := mongoDB.Client().Database("juxt").Collection("communities").
		FindOne(ctx, bson.M{"title_id": strconv.FormatUint(titleID, 10), "type": bson.M{"$ne": 3}}, opts).Decode(&c)
	var info discordTitleInfo
	if err == nil {
		info.name = c.Name
		info.icon = discordPresenceIconBaseURL + "/icons/" + c.ID + "/128.png"
	}
	info.expires = time.Now().Add(time.Hour)
	discordTitleCacheMu.Lock()
	discordTitleCache[titleID] = info
	discordTitleCacheMu.Unlock()
	return info.name, info.icon
}

type discordPresenceLink struct {
	pid                                 int64
	discordUserID, linkedDiscordID      string
	accessToken, refreshToken, headless string
	lastKey                             string
	expiresAt                           time.Time
	lastSentAt, startedAt               sql.NullTime
	online                              bool
	titleID                             uint64
	gameServerID                        uint32
	pnid                                string
}

func discordPresenceLoop() {
	if !discordPresenceConfigured() {
		log.Printf("discord presence: DISCORD_PRESENCE_CLIENT_ID/SECRET not set, disabled")
		return
	}
	ensureDiscordPresenceTable()
	for {
		runDiscordPresence()
		time.Sleep(discordPresenceInterval)
	}
}

func runDiscordPresence() {
	rows, err := db.Query(`
		SELECT l.pid, l.discord_user_id, l.access_token, l.refresh_token, l.expires_at,
		       l.headless_token, l.last_key, l.last_sent_at, l.started_at,
		       COALESCE(s.is_online, false), COALESCE(s.presence_title_id, 0), COALESCE(s.presence_game_server_id, 0),
		       COALESCE(p.pnid, ''),
		       COALESCE((SELECT COALESCE(NULLIF(w.discord_id, ''), n.discord_id, '')
		                 FROM (SELECT p.pnid AS u) x
		                 LEFT JOIN wii_devices w ON w.username = x.u
		                 LEFT JOIN n3ds_devices n ON n.username = x.u LIMIT 1), '')
		FROM discord_presence_links l
		LEFT JOIN user_settings s ON s.pid = l.pid
		LEFT JOIN pnid_cache p ON p.pid = l.pid`)
	if err != nil {
		log.Printf("discord presence: query: %v", err)
		return
	}
	var links []discordPresenceLink
	for rows.Next() {
		var l discordPresenceLink
		var title, gsid int64
		if err := rows.Scan(&l.pid, &l.discordUserID, &l.accessToken, &l.refreshToken, &l.expiresAt,
			&l.headless, &l.lastKey, &l.lastSentAt, &l.startedAt, &l.online, &title, &gsid, &l.pnid, &l.linkedDiscordID); err != nil {
			log.Printf("discord presence: scan: %v", err)
			continue
		}
		l.titleID, l.gameServerID = uint64(title), uint32(gsid)
		links = append(links, l)
	}
	rows.Close()
	for i := range links {
		updateDiscordPresence(&links[i])
	}
}

func updateDiscordPresence(l *discordPresenceLink) {
	// Gate: the PNID must still be linked (bot /link_pnid) to the same Discord
	// account that authorized the presence.
	if l.linkedDiscordID == "" || l.linkedDiscordID != l.discordUserID {
		log.Printf("discord presence: pid=%d no longer linked to discord %s, removing", l.pid, l.discordUserID)
		if l.headless != "" && discordPresenceToken(l) == nil {
			deleteDiscordHeadless(l.accessToken, l.headless)
		}
		db.Exec(`DELETE FROM discord_presence_links WHERE pid = $1`, l.pid)
		return
	}

	activity, key := discordActivityFor(l)
	if key == "" {
		if l.headless != "" {
			if discordPresenceToken(l) == nil {
				deleteDiscordHeadless(l.accessToken, l.headless)
			}
			db.Exec(`UPDATE discord_presence_links SET headless_token = '', last_key = '', started_at = NULL WHERE pid = $1`, l.pid)
		}
		return
	}
	if key == l.lastKey && l.headless != "" && l.lastSentAt.Valid && time.Since(l.lastSentAt.Time) < discordPresenceRefresh {
		return
	}
	if err := discordPresenceToken(l); err != nil {
		return
	}

	started := time.Now()
	if key == l.lastKey && l.startedAt.Valid {
		started = l.startedAt.Time
	}
	activity["timestamps"] = map[string]any{"start": strconv.FormatInt(started.UnixMilli(), 10)}
	body := map[string]any{"activities": []any{activity}}
	if l.headless != "" {
		body["token"] = l.headless
	}
	var resp struct {
		Token string `json:"token"`
	}
	status, err := discordJSON("POST", "/users/@me/headless-sessions", l.accessToken, body, &resp)
	if err == nil && (status == 400 || status == 404) && l.headless != "" {
		// The old session expired; start a new one.
		delete(body, "token")
		status, err = discordJSON("POST", "/users/@me/headless-sessions", l.accessToken, body, &resp)
	}
	if err != nil || status/100 != 2 {
		log.Printf("discord presence: pid=%d publish: status=%d err=%v", l.pid, status, err)
		return
	}
	db.Exec(`UPDATE discord_presence_links SET headless_token = $2, last_key = $3, last_sent_at = NOW(), started_at = $4 WHERE pid = $1`,
		l.pid, resp.Token, key, started)
	if key != l.lastKey {
		log.Printf("discord presence: pid=%d now %q", l.pid, key)
	}
}

// discordActivityFor builds the activity for the player's current state; key is
// "" when nothing should be shown (offline).
func discordActivityFor(l *discordPresenceLink) (map[string]any, string) {
	if !l.online {
		return nil, ""
	}
	// No title while online means the console is on the HOME Menu.
	name, icon := "Wii U Menu", ""
	if l.titleID != 0 {
		var juxtName string
		juxtName, icon = discordTitleFor(l.titleID)
		if name = curatedTitleName(l.titleID); name == "" {
			name = strings.TrimSuffix(juxtName, " Community")
		}
	}
	if name == "" {
		name = discordPresenceServers[l.gameServerID]
	}
	if name == "" {
		name = "Unknown title"
	}
	if icon == "" {
		icon = discordPresenceIconBaseURL + "/mii/" + strconv.FormatInt(l.pid, 10) + "/normal_face.png"
	}
	assets := map[string]any{
		"large_image": icon,
		"large_text":  name,
		"small_image": discordPresenceIconBaseURL + "/mii/" + strconv.FormatInt(l.pid, 10) + "/normal_face.png",
		"small_text":  l.pnid,
	}
	activity := map[string]any{
		"application_id":      os.Getenv("DISCORD_PRESENCE_CLIENT_ID"),
		"platform":            "desktop",
		"supported_platforms": []string{"desktop"},
		"type":                0,
		"name":                name,
		"details":             "on Revivetendo",
		"assets":              assets,
	}
	return activity, fmt.Sprintf("%016x/%s", l.titleID, name)
}

// discordPresenceToken refreshes l's access token when it is about to expire. A
// refused refresh (the player revoked the app) removes the link.
func discordPresenceToken(l *discordPresenceLink) error {
	if time.Until(l.expiresAt) > time.Minute {
		return nil
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.PostForm(discordAPI+"/oauth2/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {l.refreshToken},
		"client_id":     {os.Getenv("DISCORD_PRESENCE_CLIENT_ID")},
		"client_secret": {os.Getenv("DISCORD_PRESENCE_CLIENT_SECRET")},
	})
	if err != nil {
		log.Printf("discord presence: pid=%d refresh: %v", l.pid, err)
		return err
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&tok)
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
		log.Printf("discord presence: pid=%d refresh refused (%d), removing link", l.pid, resp.StatusCode)
		db.Exec(`DELETE FROM discord_presence_links WHERE pid = $1`, l.pid)
		return fmt.Errorf("refresh refused")
	}
	if resp.StatusCode != http.StatusOK || tok.AccessToken == "" {
		log.Printf("discord presence: pid=%d refresh status %d", l.pid, resp.StatusCode)
		return fmt.Errorf("refresh status %d", resp.StatusCode)
	}
	l.accessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		l.refreshToken = tok.RefreshToken
	}
	l.expiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	db.Exec(`UPDATE discord_presence_links SET access_token = $2, refresh_token = $3, expires_at = $4 WHERE pid = $1`,
		l.pid, l.accessToken, l.refreshToken, l.expiresAt)
	return nil
}

func deleteDiscordHeadless(accessToken, headless string) {
	if status, err := discordJSON("POST", "/users/@me/headless-sessions/delete", accessToken, map[string]any{"token": headless}, nil); err != nil || status/100 != 2 {
		log.Printf("discord presence: delete session: status=%d err=%v", status, err)
	}
}

func discordJSON(method, path, accessToken string, body any, out any) (int, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(method, discordAPI+path, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		log.Printf("discord presence: %s %s -> %d %s", method, path, resp.StatusCode, bytes.TrimSpace(data))
	} else if out != nil {
		json.Unmarshal(data, out)
	}
	return resp.StatusCode, nil
}
