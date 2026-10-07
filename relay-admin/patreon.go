package main

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Patreon -> RevivetendoCoin automation.
//
// A player signs in at /my/ with their PNID (existing web-password session),
// clicks "Link Patreon", and Patreon's OAuth (identity scope only) proves which
// Patreon account they own; the pair is stored in patreon_links. Patreon then
// calls /patreon/webhook for every membership event; a successful charge of an
// active patron credits coins through account-proxy's coin endpoint, keyed by
// "member id + charge date" so Patreon's retries can never double-credit. A
// pledge that arrives before the player has linked waits in patreon_pending and
// is applied the moment they link.
//
// Configuration (all in the shared .env; nothing is enabled until it is set):
//
//	PATREON_CLIENT_ID, PATREON_CLIENT_SECRET, PATREON_REDIRECT_URI  (linking)
//	PATREON_WEBHOOK_SECRET, PATREON_COINS_PER_EUR                   (crediting)

func patreonLinkConfigured() bool {
	return os.Getenv("PATREON_CLIENT_ID") != "" && os.Getenv("PATREON_CLIENT_SECRET") != "" && os.Getenv("PATREON_REDIRECT_URI") != ""
}

// patreonCoinsPerEuro is how many coins one euro of pledge earns (may be fractional,
// e.g. 66.6667 makes a 3.00 pledge exactly 200 coins).
func patreonCoinsPerEuro() float64 {
	n, _ := strconv.ParseFloat(strings.TrimSpace(os.Getenv("PATREON_COINS_PER_EUR")), 64)
	return n
}

// coinsForCents converts a pledge amount in cents to coins at perEuro coins per 1.00,
// rounded to the nearest whole coin.
func coinsForCents(cents int, perEuro float64) int {
	if cents <= 0 || perEuro <= 0 {
		return 0
	}
	return int(math.Round(float64(cents) * perEuro / 100))
}

// validPatreonSignature checks X-Patreon-Signature, which Patreon defines as the
// hex HMAC-MD5 of the raw request body keyed with the webhook secret.
func validPatreonSignature(secret string, body []byte, sig string) bool {
	mac := hmac.New(md5.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(strings.ToLower(strings.TrimSpace(sig))))
}

type patreonMember struct {
	MemberID         string
	UserID           string
	PatronStatus     string
	LastChargeStatus string
	LastChargeDate   string
	Cents            int
}

// parsePatreonMember extracts the fields we need from a v2 "member" webhook payload.
func parsePatreonMember(body []byte) (patreonMember, bool) {
	var p struct {
		Data struct {
			ID         string `json:"id"`
			Type       string `json:"type"`
			Attributes struct {
				PatronStatus                 string `json:"patron_status"`
				LastChargeStatus             string `json:"last_charge_status"`
				LastChargeDate               string `json:"last_charge_date"`
				CurrentlyEntitledAmountCents int    `json:"currently_entitled_amount_cents"`
			} `json:"attributes"`
			Relationships struct {
				User struct {
					Data struct {
						ID string `json:"id"`
					} `json:"data"`
				} `json:"user"`
			} `json:"relationships"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &p) != nil || p.Data.Type != "member" || p.Data.ID == "" || p.Data.Relationships.User.Data.ID == "" {
		return patreonMember{}, false
	}
	a := p.Data.Attributes
	return patreonMember{p.Data.ID, p.Data.Relationships.User.Data.ID, a.PatronStatus, a.LastChargeStatus, a.LastChargeDate, a.CurrentlyEntitledAmountCents}, true
}

// patreonWebhook receives Patreon's membership events. It answers 5xx when a
// credit could not be applied so Patreon retries later.
func patreonWebhook(w http.ResponseWriter, r *http.Request) {
	secret := os.Getenv("PATREON_WEBHOOK_SECRET")
	perEuro := patreonCoinsPerEuro()
	if secret == "" || perEuro <= 0 {
		http.Error(w, "patreon crediting not configured", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if !validPatreonSignature(secret, body, r.Header.Get("X-Patreon-Signature")) {
		http.Error(w, "bad signature", http.StatusForbidden)
		return
	}
	event := r.Header.Get("X-Patreon-Event")
	m, ok := parsePatreonMember(body)
	// Only a successful charge of an active patron earns coins; every other
	// event (pledge created but unpaid, declined, deleted, ...) is acknowledged.
	if !ok || !strings.HasPrefix(event, "members:") || m.PatronStatus != "active_patron" || m.LastChargeStatus != "Paid" || m.LastChargeDate == "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	coins := coinsForCents(m.Cents, perEuro)
	if coins <= 0 {
		w.WriteHeader(http.StatusOK)
		return
	}
	ref := "patreon:" + m.MemberID + ":" + m.LastChargeDate

	var pid int64
	err := db.QueryRow(`SELECT pid FROM patreon_links WHERE patreon_user_id = $1`, m.UserID).Scan(&pid)
	if err != nil {
		if _, e := db.Exec(`INSERT INTO patreon_pending(ref, patreon_user_id, coins) VALUES($1,$2,$3) ON CONFLICT (ref) DO NOTHING`, ref, m.UserID, coins); e != nil {
			log.Printf("patreon webhook: pending insert: %v", e)
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		log.Printf("patreon webhook: %s coins=%d held pending (patreon user %s not linked yet)", event, coins, m.UserID)
		w.WriteHeader(http.StatusOK)
		return
	}
	res, status, text, err := coinsAdd(map[string]interface{}{"pid": pid, "amount": coins, "reason": "patreon " + event, "ref": ref})
	if err != nil || status != http.StatusOK {
		log.Printf("patreon webhook: credit PID %d coins=%d failed: status=%d text=%q err=%v", pid, coins, status, text, err)
		http.Error(w, "credit failed", http.StatusBadGateway)
		return
	}
	log.Printf("patreon webhook: %s PID %d +%d coins applied=%v balance=%d", event, pid, coins, res.Applied, res.Balance)
	w.WriteHeader(http.StatusOK)
}

// applyPendingPatreon credits any pledges that were paid before the player linked.
func applyPendingPatreon(pid int64, patreonUserID string) {
	rows, err := db.Query(`SELECT ref, coins FROM patreon_pending WHERE patreon_user_id = $1`, patreonUserID)
	if err != nil {
		log.Printf("patreon: pending query: %v", err)
		return
	}
	type pend struct {
		ref   string
		coins int
	}
	var list []pend
	for rows.Next() {
		var p pend
		if rows.Scan(&p.ref, &p.coins) == nil {
			list = append(list, p)
		}
	}
	rows.Close()
	for _, p := range list {
		if _, status, _, err := coinsAdd(map[string]interface{}{"pid": pid, "amount": p.coins, "reason": "patreon (paid before linking)", "ref": p.ref}); err == nil && status == http.StatusOK {
			db.Exec(`DELETE FROM patreon_pending WHERE ref = $1`, p.ref)
		} else {
			log.Printf("patreon: pending %s for PID %d not applied yet: status=%d err=%v", p.ref, pid, status, err)
		}
	}
}

type myPatreonData struct {
	PNID        string
	Msg         string
	Configured  bool
	Crediting   bool
	Linked      bool
	PatreonName string
	Balance     int
	HasWallet   bool
	History     []coinLedgerRow
}

func myPatreonHandler(w http.ResponseWriter, r *http.Request) {
	pid, ok := mySessionPID(r)
	if !ok {
		http.Redirect(w, r, "/inkay/my/login", http.StatusFound)
		return
	}
	data := myPatreonData{
		Msg:        r.URL.Query().Get("msg"),
		Configured: patreonLinkConfigured(),
		Crediting:  os.Getenv("PATREON_WEBHOOK_SECRET") != "" && patreonCoinsPerEuro() > 0,
	}
	data.PNID, _ = pnidForPID(pid)
	if err := db.QueryRow(`SELECT full_name FROM patreon_links WHERE pid = $1`, pid).Scan(&data.PatreonName); err == nil {
		data.Linked = true
	}
	if err := db.QueryRow(`SELECT balance FROM coin_wallets WHERE pid = $1`, pid).Scan(&data.Balance); err == nil {
		data.HasWallet = true
	}
	if rows, err := db.Query(`SELECT pid, '', delta, balance_after, reason, created_at FROM coin_ledger WHERE pid = $1 ORDER BY id DESC LIMIT 10`, pid); err == nil {
		defer rows.Close()
		for rows.Next() {
			var c coinLedgerRow
			if rows.Scan(&c.PID, &c.PNID, &c.Delta, &c.BalanceAfter, &c.Reason, &c.CreatedAt) == nil {
				data.History = append(data.History, c)
			}
		}
	}
	w.Header().Set("Content-Type", "text/html")
	myPatreonTmpl.Execute(w, data)
}

func myPatreonStart(w http.ResponseWriter, r *http.Request) {
	pid, ok := mySessionPID(r)
	if !ok {
		http.Redirect(w, r, "/inkay/my/login", http.StatusFound)
		return
	}
	if r.Method != http.MethodPost || !patreonLinkConfigured() {
		http.Redirect(w, r, "/inkay/my/patreon", http.StatusSeeOther)
		return
	}
	b := make([]byte, 16)
	rand.Read(b)
	state := hex.EncodeToString(b)
	runtimeCacheSet(context.Background(), "patreonstate:"+state, strconv.FormatInt(pid, 10), 10*time.Minute)
	q := url.Values{
		"response_type": {"code"},
		"client_id":     {os.Getenv("PATREON_CLIENT_ID")},
		"redirect_uri":  {os.Getenv("PATREON_REDIRECT_URI")},
		"scope":         {"identity"},
		"state":         {state},
	}
	http.Redirect(w, r, "https://www.patreon.com/oauth2/authorize?"+q.Encode(), http.StatusSeeOther)
}

func myPatreonCallback(w http.ResponseWriter, r *http.Request) {
	back := func(msg string) {
		http.Redirect(w, r, "/inkay/my/patreon?msg="+url.QueryEscape(msg), http.StatusSeeOther)
	}
	pid, ok := mySessionPID(r)
	if !ok {
		http.Redirect(w, r, "/inkay/my/login", http.StatusFound)
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		back("Patreon linking was cancelled")
		return
	}
	code, state := r.URL.Query().Get("code"), r.URL.Query().Get("state")
	v, found := runtimeCacheGet(context.Background(), "patreonstate:"+state)
	if code == "" || !found || v != strconv.FormatInt(pid, 10) {
		back("That link request expired or does not belong to this account - please try again")
		return
	}

	client := &http.Client{Timeout: 10 * time.Second}
	tokResp, err := client.PostForm("https://www.patreon.com/api/oauth2/token", url.Values{
		"code":          {code},
		"grant_type":    {"authorization_code"},
		"client_id":     {os.Getenv("PATREON_CLIENT_ID")},
		"client_secret": {os.Getenv("PATREON_CLIENT_SECRET")},
		"redirect_uri":  {os.Getenv("PATREON_REDIRECT_URI")},
	})
	if err != nil {
		log.Printf("patreon: token exchange: %v", err)
		back("Could not reach Patreon")
		return
	}
	defer tokResp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	json.NewDecoder(io.LimitReader(tokResp.Body, 1<<16)).Decode(&tok)
	if tokResp.StatusCode != http.StatusOK || tok.AccessToken == "" {
		log.Printf("patreon: token exchange status %d", tokResp.StatusCode)
		back("Patreon did not accept the login")
		return
	}

	req, _ := http.NewRequest("GET", "https://www.patreon.com/api/oauth2/v2/identity?fields%5Buser%5D=full_name", nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	idResp, err := client.Do(req)
	if err != nil {
		log.Printf("patreon: identity: %v", err)
		back("Could not reach Patreon")
		return
	}
	defer idResp.Body.Close()
	var ident struct {
		Data struct {
			ID         string `json:"id"`
			Attributes struct {
				FullName string `json:"full_name"`
			} `json:"attributes"`
		} `json:"data"`
	}
	json.NewDecoder(io.LimitReader(idResp.Body, 1<<16)).Decode(&ident)
	if idResp.StatusCode != http.StatusOK || ident.Data.ID == "" {
		log.Printf("patreon: identity status %d", idResp.StatusCode)
		back("Could not read your Patreon account")
		return
	}

	if _, err := db.Exec(`INSERT INTO patreon_links(pid, patreon_user_id, full_name) VALUES($1,$2,$3)
		ON CONFLICT (pid) DO UPDATE SET patreon_user_id = EXCLUDED.patreon_user_id, full_name = EXCLUDED.full_name, linked_at = NOW()`,
		pid, ident.Data.ID, ident.Data.Attributes.FullName); err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			back("That Patreon account is already linked to another PNID")
			return
		}
		log.Printf("patreon: link insert: %v", err)
		back("Could not save the link")
		return
	}
	applyPendingPatreon(pid, ident.Data.ID)
	back("Patreon linked")
}

func myPatreonUnlink(w http.ResponseWriter, r *http.Request) {
	pid, ok := mySessionPID(r)
	if !ok {
		http.Redirect(w, r, "/inkay/my/login", http.StatusFound)
		return
	}
	if r.Method == http.MethodPost {
		db.Exec(`DELETE FROM patreon_links WHERE pid = $1`, pid)
	}
	http.Redirect(w, r, "/inkay/my/patreon?msg="+url.QueryEscape("Patreon unlinked"), http.StatusSeeOther)
}

var myPatreonTmpl = template.Must(template.New("my-patreon").Funcs(tmplFuncs).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Coins &amp; Patreon — Pretendo Bridge</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
body{font-family:system-ui,sans-serif;max-width:620px;margin:2rem auto;padding:0 1rem;color:#222}
h1{font-size:1.3rem}h2{font-size:.95rem;color:#666;margin:1.5rem 0 .6rem}
a{color:#2563eb}
.box{background:#f8fafc;border:1px solid #e2e8f0;border-radius:10px;padding:1rem 1.25rem;margin-bottom:1rem}
.msg{background:#dcfce7;border:1px solid #bbf7d0;color:#166534;border-radius:6px;padding:.5rem 1rem;margin-bottom:1rem;font-size:.9rem}
.big{font-size:1.6rem;font-weight:700}
button{font:inherit;cursor:pointer;border:none;border-radius:6px;padding:.5rem 1rem;background:#f96854;color:#fff}
button.ghost{background:none;color:#dc2626;text-decoration:underline;padding:0}
table{width:100%;border-collapse:collapse;font-size:.85rem}
th{text-align:left;color:#888;font-weight:600;border-bottom:1px solid #e4e4e7;padding:.3rem .5rem}
td{padding:.35rem .5rem;border-bottom:1px solid #f4f4f5}
.pos{color:#166534}.neg{color:#b91c1c}.muted{color:#888;font-size:.85rem}
</style>
</head>
<body>
<p><a href="/inkay/my/">← Back</a></p>
<h1>Coins &amp; Patreon</h1>
{{if .Msg}}<div class="msg">{{.Msg}}</div>{{end}}
<div class="box">
  <div class="muted">@{{.PNID}}</div>
  <div class="big">{{if .HasWallet}}{{.Balance}}{{else}}—{{end}} <span style="font-size:1rem;font-weight:400">RevivetendoCoin</span></div>
  <div class="muted">One "5 Runden" pack in Badge Arcade costs 1 coin. Everyone gets 1 free coin per day while under 500.</div>
</div>

<h2>Patreon</h2>
<div class="box">
{{if not .Configured}}
  <p style="margin:0">Patreon linking isn't set up yet.</p>
{{else if .Linked}}
  <p style="margin:0 0 .6rem">Linked to Patreon account <strong>{{if .PatreonName}}{{.PatreonName}}{{else}}(name hidden){{end}}</strong>.</p>
  <p class="muted" style="margin:0 0 .8rem">{{if .Crediting}}Each successful monthly payment adds coins to your wallet automatically.{{else}}Automatic crediting isn't switched on yet.{{end}}</p>
  <form method="post" action="/inkay/my/patreon/unlink" onsubmit="return confirm('Unlink this Patreon account?')"><button class="ghost" type="submit">Unlink</button></form>
{{else}}
  <p style="margin:0 0 .8rem">Link your Patreon account to receive coins automatically when you support us. Use the Patreon account you pay with.</p>
  <form method="post" action="/inkay/my/patreon/start"><button type="submit">Link Patreon</button></form>
{{end}}
</div>

{{if .History}}
<h2>Recent coin activity</h2>
<table>
<tr><th>When</th><th>Change</th><th>Balance</th><th>Reason</th></tr>
{{range .History}}
<tr><td class="muted">{{localTime .CreatedAt "datetime"}}</td>
<td>{{if gt .Delta 0}}<span class="pos">+{{.Delta}}</span>{{else}}<span class="neg">{{.Delta}}</span>{{end}}</td>
<td>{{.BalanceAfter}}</td><td>{{.Reason}}</td></tr>
{{end}}
</table>
{{end}}
` + localTimeScript + `
</body>
</html>`))
