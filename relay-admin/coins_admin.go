package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// RevivetendoCoin wallets (Badge Arcade buy-plays). Reads go straight to the
// shared Postgres tables that account-proxy owns (coin_wallets, coin_ledger);
// every change is sent to account-proxy's /internal/coins/add so the daily
// grant, floor and ledger logic live in exactly one place.

type coinWalletRow struct {
	PID       int64
	PNID      string
	Balance   int
	LastGrant time.Time
}

type coinLedgerRow struct {
	PID          int64
	PNID         string
	Delta        int
	BalanceAfter int
	Reason       string
	CreatedAt    time.Time
}

func adminCoins(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	data := struct {
		Wallets      []coinWalletRow
		Ledger       []coinLedgerRow
		Msg, Q       string
		Nonce        string
		Total, Coins int
	}{Msg: r.URL.Query().Get("msg"), Q: q}

	nonce := make([]byte, 8)
	rand.Read(nonce)
	data.Nonce = hex.EncodeToString(nonce)

	db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(balance),0) FROM coin_wallets`).Scan(&data.Total, &data.Coins)

	rows, err := db.Query(`SELECT w.pid, COALESCE(p.pnid,''), w.balance, w.last_grant
		FROM coin_wallets w LEFT JOIN pnid_cache p ON p.pid = w.pid
		WHERE $1 = '' OR p.pnid ILIKE '%' || $1 || '%' OR w.pid::text = $1
		ORDER BY w.balance DESC, w.pid LIMIT 200`, q)
	if err != nil {
		log.Printf("coins admin: wallets query: %v", err)
	} else {
		defer rows.Close()
		for rows.Next() {
			var c coinWalletRow
			if rows.Scan(&c.PID, &c.PNID, &c.Balance, &c.LastGrant) == nil {
				data.Wallets = append(data.Wallets, c)
			}
		}
	}

	lrows, err := db.Query(`SELECT l.pid, COALESCE(p.pnid,''), l.delta, l.balance_after, l.reason, l.created_at
		FROM coin_ledger l LEFT JOIN pnid_cache p ON p.pid = l.pid
		ORDER BY l.id DESC LIMIT 50`)
	if err != nil {
		log.Printf("coins admin: ledger query: %v", err)
	} else {
		defer lrows.Close()
		for lrows.Next() {
			var c coinLedgerRow
			if lrows.Scan(&c.PID, &c.PNID, &c.Delta, &c.BalanceAfter, &c.Reason, &c.CreatedAt) == nil {
				data.Ledger = append(data.Ledger, c)
			}
		}
	}

	w.Header().Set("Content-Type", "text/html")
	coinsTmpl.Execute(w, data)
}

func adminCoinsAdjust(w http.ResponseWriter, r *http.Request) {
	back := func(msg string) {
		http.Redirect(w, r, "/inkay/admin/coins/?msg="+url.QueryEscape(msg), http.StatusSeeOther)
	}
	if r.Method != http.MethodPost {
		back("")
		return
	}
	token := os.Getenv("PN_COIN_ADMIN_TOKEN")
	if token == "" {
		back("PN_COIN_ADMIN_TOKEN is not configured")
		return
	}
	who := strings.TrimSpace(r.FormValue("who"))
	mode := r.FormValue("mode")
	amount, err := strconv.Atoi(strings.TrimSpace(r.FormValue("amount")))
	if who == "" || err != nil {
		back("Need a PNID/PID and a whole-number amount")
		return
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	if reason == "" {
		reason = "admin"
	}
	if len(reason) > 180 {
		reason = reason[:180]
	}

	req := map[string]interface{}{"reason": "admin: " + reason}
	if pid, err := strconv.ParseUint(who, 10, 32); err == nil {
		req["pid"] = pid
	} else {
		req["pnid"] = who
	}
	if mode == "set" {
		if amount < 0 {
			back("Balance cannot be set below 0")
			return
		}
		req["set"] = amount
	} else {
		req["amount"] = amount
	}
	if nonce := r.FormValue("nonce"); nonce != "" && len(nonce) <= 40 {
		req["ref"] = "admin-" + nonce
	}

	res, status, text, err := coinsAdd(req)
	if err != nil {
		log.Printf("coins admin: adjust: %v", err)
		back("Could not reach account-proxy")
		return
	}
	if status != http.StatusOK {
		back(fmt.Sprintf("Failed (%d): %s", status, text))
		return
	}
	name := who
	if pnid, err := pnidForPID(res.PID); err == nil && pnid != "" {
		name = pnid
	}
	if !res.Applied {
		back(fmt.Sprintf("%s: already applied (duplicate submit ignored), balance %d", name, res.Balance))
		return
	}
	log.Printf("coins admin: %s (PID %d) mode=%s amount=%d -> balance %d", who, res.PID, mode, amount, res.Balance)
	back(fmt.Sprintf("%s: balance is now %d", name, res.Balance))
}

// coinsAddURL is account-proxy's localhost-only wallet endpoint (a var so tests can point it elsewhere).
var coinsAddURL = "http://127.0.0.1:9191/internal/coins/add"

type coinsAddResult struct {
	PID     int64 `json:"pid"`
	Balance int   `json:"balance"`
	Applied bool  `json:"applied"`
}

// coinsAdd is the single way relay-admin changes a wallet: it calls
// account-proxy so the daily-grant, floor and ledger logic stay in one place.
// It returns the parsed result, the HTTP status and the raw response text.
func coinsAdd(req map[string]interface{}) (*coinsAddResult, int, string, error) {
	token := os.Getenv("PN_COIN_ADMIN_TOKEN")
	if token == "" {
		return nil, 0, "", fmt.Errorf("PN_COIN_ADMIN_TOKEN is not configured")
	}
	body, _ := json.Marshal(req)
	hreq, _ := http.NewRequest("POST", coinsAddURL, bytes.NewReader(body))
	hreq.Header.Set("X-Coin-Token", token)
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(hreq)
	if err != nil {
		return nil, 0, "", err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	text := strings.TrimSpace(string(out))
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, text, nil
	}
	var res coinsAddResult
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, resp.StatusCode, text, err
	}
	return &res, resp.StatusCode, text, nil
}

var coinsTmpl = template.Must(template.New("coins").Funcs(tmplFuncs).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>RevivetendoCoin wallets</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
body{font-family:system-ui,sans-serif;max-width:900px;margin:2rem auto;padding:0 1rem;color:#222}
h1{font-size:1.4rem}
a{color:#2563eb}
table{width:100%;border-collapse:collapse;font-size:.9rem;margin-bottom:2rem}
th{text-align:left;border-bottom:2px solid #e4e4e7;padding:.5rem .75rem;color:#666;font-weight:600}
td{padding:.5rem .75rem;border-bottom:1px solid #f0f0f0;vertical-align:middle}
tr:last-child td{border-bottom:none}
button,input,select{font:inherit}
button{cursor:pointer;border:none;border-radius:4px;padding:.3rem .7rem;font-size:.85rem;background:#2563eb;color:#fff}
fieldset{border:1px solid #e4e4e7;border-radius:8px;padding:1rem 1.25rem;margin-bottom:2rem}
legend{font-weight:600;padding:0 .4rem}
.row{display:flex;gap:.75rem;flex-wrap:wrap;align-items:flex-end}
.field{display:flex;flex-direction:column;gap:.3rem;flex:1;min-width:140px}
label{font-size:.8rem;color:#666;font-weight:600}
input[type=text],input[type=number],select{border:1px solid #d1d5db;border-radius:4px;padding:.4rem .6rem;width:100%;box-sizing:border-box}
.msg{background:#dcfce7;border:1px solid #bbf7d0;color:#166534;padding:.5rem 1rem;border-radius:6px;margin-bottom:1rem;font-size:.9rem}
.note{background:#f8fafc;border:1px solid #e2e8f0;border-radius:6px;padding:.75rem 1rem;margin-bottom:1.5rem;font-size:.9rem}
.pos{color:#166534}.neg{color:#b91c1c}
.mono{font-family:monospace;color:#999;font-size:.75rem}
td form{display:flex;gap:.4rem;margin:0;align-items:center}
td form input[type=number]{width:5.5rem}
td form select{width:auto}
</style>
</head>
<body>
<p><a href="/inkay/admin/">← Back to admin</a></p>
<h1>RevivetendoCoin wallets</h1>
{{if .Msg}}<div class="msg">{{.Msg}}</div>{{end}}
<div class="note"><strong>{{.Total}}</strong> wallets holding <strong>{{.Coins}}</strong> coins in total. One "5 Runden" pack costs 1 coin; every account gets 1 free coin per UTC day while under 500. The daily grant is applied when a player opens the shop, so stored balances may not include today's grant yet. Coins added here can go above 500.</div>

<fieldset>
<legend>Add or set funds</legend>
<form method="post" action="/inkay/admin/coins/adjust">
  <input type="hidden" name="nonce" value="{{.Nonce}}">
  <div class="row">
    <div class="field"><label>PNID or PID</label><input type="text" name="who" required></div>
    <div class="field" style="max-width:160px"><label>Action</label>
      <select name="mode"><option value="add">Add (use − to remove)</option><option value="set">Set balance to</option></select></div>
    <div class="field" style="max-width:120px"><label>Coins</label><input type="number" name="amount" required></div>
    <div class="field"><label>Reason (ledger)</label><input type="text" name="reason" placeholder="e.g. patreon tier 2"></div>
    <button type="submit">Apply</button>
  </div>
</form>
</fieldset>

<form method="get" action="/inkay/admin/coins/" class="row" style="margin-bottom:1rem">
  <div class="field" style="max-width:300px"><label>Search wallets (PNID or PID)</label><input type="text" name="q" value="{{.Q}}"></div>
  <button type="submit">Search</button>
</form>

<table>
<tr><th>Player</th><th>Balance</th><th>Last grant</th><th>Quick edit</th></tr>
{{range .Wallets}}
<tr>
  <td>{{if .PNID}}<strong>{{.PNID}}</strong><br><span class="mono">{{.PID}}</span>{{else}}<span class="mono">{{.PID}}</span>{{end}}</td>
  <td><strong>{{.Balance}}</strong></td>
  <td style="font-size:.8rem;color:#666">{{localTime .LastGrant "date"}}</td>
  <td>
    <form method="post" action="/inkay/admin/coins/adjust">
      <input type="hidden" name="nonce" value="{{$.Nonce}}">
      <input type="hidden" name="who" value="{{.PID}}">
      <select name="mode"><option value="add">Add</option><option value="set">Set</option></select>
      <input type="number" name="amount" required>
      <input type="text" name="reason" placeholder="reason" style="width:9rem">
      <button type="submit">Apply</button>
    </form>
  </td>
</tr>
{{else}}<tr><td colspan="4" style="color:#aaa">No wallets{{if .Q}} match "{{.Q}}"{{end}}</td></tr>
{{end}}
</table>

<h2 style="font-size:1.1rem">Recent activity</h2>
<table>
<tr><th>When</th><th>Player</th><th>Change</th><th>Balance after</th><th>Reason</th></tr>
{{range .Ledger}}
<tr>
  <td style="font-size:.8rem;color:#666">{{localTime .CreatedAt "datetime"}}</td>
  <td>{{if .PNID}}{{.PNID}}{{else}}<span class="mono">{{.PID}}</span>{{end}}</td>
  <td>{{if gt .Delta 0}}<span class="pos">+{{.Delta}}</span>{{else}}<span class="neg">{{.Delta}}</span>{{end}}</td>
  <td>{{.BalanceAfter}}</td>
  <td style="font-size:.85rem">{{.Reason}}</td>
</tr>
{{else}}<tr><td colspan="5" style="color:#aaa">Nothing yet</td></tr>
{{end}}
</table>
</body>
</html>
`))
