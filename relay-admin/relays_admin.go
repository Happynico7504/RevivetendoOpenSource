package main

// Relay registry page (/admin/relays/): register regional relays, hand out
// their secret bundle ONCE, enable/disable and delete them. The registry is the
// `relays` table that relayhub reads (its key cache makes a disable take effect
// within ~10s). The main's private key is never read here: bundles only need
// its PUBLIC half (~/.relayhub/main-public.pem, written by `relayhub keygen`).

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const relaysSchema = `
CREATE TABLE IF NOT EXISTS relays (
	id          TEXT        PRIMARY KEY,
	name        TEXT        NOT NULL DEFAULT '',
	region      TEXT        NOT NULL,
	host        TEXT        NOT NULL,
	health_port INTEGER     NOT NULL DEFAULT 0,
	public_key  BYTEA       NOT NULL,
	enabled     BOOLEAN     NOT NULL DEFAULT TRUE,
	created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	last_seen   TIMESTAMPTZ
)`

// relayRegions must match relayhub's RegionCatalog.
var relayRegions = []struct{ Key, Label string }{
	{"na", "North America (US, CA, MX + South America)"},
	{"jp", "Japan region (JP, KR, TW)"},
	{"asia", "Asia / Oceania (rest)"},
	{"eu", "Europe / Africa"},
}

var (
	relayIDRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)
	relayHostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.:-]{0,251}[A-Za-z0-9])?$`)
)

type relayRow struct {
	ID, Name, Region, Host string
	HealthPort             int
	Enabled                bool
	CreatedAt              time.Time
	LastSeen               *time.Time
}

func allRelays() []relayRow {
	rows, err := db.Query(`SELECT id, name, region, host, health_port, enabled, created_at, last_seen FROM relays ORDER BY id`)
	if err != nil {
		log.Printf("relays query: %v", err)
		return nil
	}
	defer rows.Close()
	var out []relayRow
	for rows.Next() {
		var r relayRow
		var seen *time.Time
		if err := rows.Scan(&r.ID, &r.Name, &r.Region, &r.Host, &r.HealthPort, &r.Enabled, &r.CreatedAt, &seen); err != nil {
			continue
		}
		r.LastSeen = seen
		out = append(out, r)
	}
	return out
}

func relayHubPublicKeyPath() string {
	if p := os.Getenv("RELAYHUB_PUBLIC_KEY_FILE"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".relayhub", "main-public.pem")
}

// relayHubReleasePublicKey returns the pinned OTA release key (base64) if one
// has been created with `relayhub release keygen`; relays registered without
// one never auto-update.
func relayHubReleasePublicKey() string {
	p := os.Getenv("RELAYHUB_RELEASE_PUBLIC_KEY_FILE")
	if p == "" {
		home, _ := os.UserHomeDir()
		p = filepath.Join(home, ".relayhub", "release-key.pub")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func relayHubPublicURL() string {
	if u := os.Getenv("RELAYHUB_PUBLIC_URL"); u != "" {
		return u
	}
	return "http://netcup-server.nicochristmann.net:7777"
}

type relaysPage struct {
	Relays    []relayRow
	Regions   []struct{ Key, Label string }
	Msg, Err  string
	MainURL   string
	NewID     string
	Bundle    string       // the secret bundle, shown once, only in the response to "add"
	BundleURL template.URL // data: URL for the download button
}

func renderRelays(w http.ResponseWriter, p relaysPage) {
	p.Relays, p.Regions, p.MainURL = allRelays(), relayRegions, relayHubPublicURL()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store") // a page that can contain a secret must not be cached
	relaysTmpl.Execute(w, p)
}

func adminRelays(w http.ResponseWriter, r *http.Request) {
	renderRelays(w, relaysPage{Msg: r.URL.Query().Get("msg")})
}

func adminRelaysAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/inkay/admin/relays/", http.StatusSeeOther)
		return
	}
	id := strings.TrimSpace(r.FormValue("id"))
	region := r.FormValue("region")
	host := strings.TrimSpace(r.FormValue("host"))
	name := strings.TrimSpace(r.FormValue("name"))
	port, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("health_port")))

	fail := func(msg string) { renderRelays(w, relaysPage{Err: msg}) }
	validRegion := false
	for _, rg := range relayRegions {
		if rg.Key == region {
			validRegion = true
		}
	}
	switch {
	case !relayIDRe.MatchString(id):
		fail("ID must be 1-31 characters of a-z, 0-9 and dashes, e.g. us-1.")
		return
	case !validRegion:
		fail("Unknown region.")
		return
	case !relayHostRe.MatchString(host):
		fail("Host must be a hostname or IP address.")
		return
	case port < 0 || port > 65535:
		fail("Health port must be 0-65535.")
		return
	}
	pubPEM, err := os.ReadFile(relayHubPublicKeyPath())
	if err != nil {
		fail("The main's public key is missing (" + relayHubPublicKeyPath() + "). Run `relayhub pubkey` once.")
		return
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fail("Could not generate a key.")
		return
	}
	if _, err := db.Exec(`INSERT INTO relays (id, name, region, host, health_port, public_key, enabled) VALUES ($1,$2,$3,$4,$5,$6,TRUE)`,
		id, name, region, host, port, []byte(pub)); err != nil {
		log.Printf("relay insert: %v", err)
		fail("Could not add the relay (is the ID already taken?).")
		return
	}
	// Same JSON as relaylink.RelayBundle: what the relay's relayd reads.
	fields := map[string]string{
		"relay_id":        id,
		"signing_key":     base64.StdEncoding.EncodeToString(priv),
		"main_public_key": string(pubPEM),
		"main_url":        relayHubPublicURL(),
	}
	if rk := relayHubReleasePublicKey(); rk != "" {
		fields["release_public_key"] = rk // enables over-the-air updates on this relay
	}
	bundle, _ := json.MarshalIndent(fields, "", "  ")
	log.Printf("relay %q added (region %s, host %s)", id, region, host)
	renderRelays(w, relaysPage{
		Msg: fmt.Sprintf("Relay %s added.", id), NewID: id, Bundle: string(bundle),
		BundleURL: template.URL("data:application/json;base64," + base64.StdEncoding.EncodeToString(bundle)),
	})
}

func adminRelaysToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/inkay/admin/relays/", http.StatusSeeOther)
		return
	}
	id := r.FormValue("id")
	if !relayIDRe.MatchString(id) {
		http.Redirect(w, r, "/inkay/admin/relays/?msg=Invalid+ID", http.StatusSeeOther)
		return
	}
	db.Exec(`UPDATE relays SET enabled = NOT enabled WHERE id = $1`, id)
	http.Redirect(w, r, "/inkay/admin/relays/?msg=Updated+"+id+"+(takes+effect+within+about+10+seconds)", http.StatusSeeOther)
}

func adminRelaysDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/inkay/admin/relays/", http.StatusSeeOther)
		return
	}
	id := r.FormValue("id")
	if !relayIDRe.MatchString(id) {
		http.Redirect(w, r, "/inkay/admin/relays/?msg=Invalid+ID", http.StatusSeeOther)
		return
	}
	db.Exec(`DELETE FROM relays WHERE id = $1`, id)
	http.Redirect(w, r, "/inkay/admin/relays/?msg=Deleted+"+id, http.StatusSeeOther)
}

var relaysTmpl = template.Must(template.New("relays").Funcs(tmplFuncs).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Relays</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
body{font-family:system-ui,sans-serif;max-width:860px;margin:2rem auto;padding:0 1rem;color:#222}
h1{font-size:1.4rem}
a{color:#2563eb}
table{width:100%;border-collapse:collapse;font-size:.9rem;margin-bottom:2rem}
th{text-align:left;border-bottom:2px solid #e4e4e7;padding:.5rem .75rem;color:#666;font-weight:600}
td{padding:.5rem .75rem;border-bottom:1px solid #f0f0f0;vertical-align:middle}
tr:last-child td{border-bottom:none}
button,input,select,textarea{font:inherit}
button{cursor:pointer;border:none;border-radius:4px;padding:.3rem .7rem;font-size:.85rem}
.btn-on{background:#dcfce7;color:#166534}
.btn-off{background:#fef9c3;color:#854d0e}
.btn-del{background:#fee2e2;color:#991b1b}
fieldset{border:1px solid #e4e4e7;border-radius:8px;padding:1rem 1.25rem;margin-bottom:2rem}
legend{font-weight:600;padding:0 .4rem}
.row{display:flex;gap:.75rem;flex-wrap:wrap;align-items:flex-end}
.field{display:flex;flex-direction:column;gap:.3rem;flex:1;min-width:150px}
label{font-size:.8rem;color:#666;font-weight:600}
input[type=text],input[type=number],select{border:1px solid #d1d5db;border-radius:4px;padding:.4rem .6rem;width:100%;box-sizing:border-box}
.submit{background:#2563eb;color:#fff;padding:.4rem 1rem}
.msg{background:#dcfce7;border:1px solid #bbf7d0;color:#166534;padding:.5rem 1rem;border-radius:6px;margin-bottom:1rem;font-size:.9rem}
.err{background:#fee2e2;border:1px solid #fecaca;color:#991b1b;padding:.5rem 1rem;border-radius:6px;margin-bottom:1rem;font-size:.9rem}
.note{background:#fff7ed;border:1px solid #fed7aa;border-radius:6px;padding:.75rem 1rem;margin-bottom:1.5rem;font-size:.9rem}
.secret{background:#fefce8;border:2px solid #facc15;border-radius:8px;padding:1rem 1.25rem;margin-bottom:2rem}
.secret textarea{width:100%;box-sizing:border-box;height:14rem;font-family:monospace;font-size:.75rem}
.dl{display:inline-block;background:#f4f4f5;border:1px solid #d1d5db;border-radius:4px;padding:.3rem .8rem;font-size:.85rem;color:#222;text-decoration:none}
code{background:#f4f4f5;padding:.1rem .3rem;border-radius:3px;font-size:.85em}
</style>
</head>
<body>
<p><a href="/inkay/admin/">← Back to admin</a></p>
<h1>Relays</h1>
{{if .Msg}}<div class="msg">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="err">{{.Err}}</div>{{end}}

{{if .Bundle}}
<div class="secret">
  <strong>Secret bundle for relay {{.NewID}} - shown only once.</strong>
  <p style="margin:.4rem 0 .8rem">It contains the relay's private signing key. Copy it to the relay over a secure channel (for example <code>scp</code>) and do not store it anywhere else. If it is lost, delete this relay and add it again.</p>
  <textarea readonly onclick="this.select()">{{.Bundle}}</textarea>
  <p style="margin-top:.6rem"><a class="dl" href="{{.BundleURL}}" download="relay-{{.NewID}}-bundle.json">⬇ Download bundle</a></p>
</div>
{{end}}

<div class="note">
  Relays reach the main at <code>{{.MainURL}}</code> (set <code>RELAYHUB_PUBLIC_URL</code> to change it). Enabling, disabling or deleting a relay takes effect on the relay API within about 10 seconds.
  After adding or changing a relay, update the DNS regions on the server with
  <code>relayhub dns-config -o /etc/revivetendo-dns/regiondns.json</code> (the DNS server reloads it by itself).
</div>

<table>
<tr><th>ID</th><th>Region</th><th>Host</th><th>Health</th><th>Last seen</th><th>Status</th><th></th></tr>
{{range .Relays}}
<tr>
  <td><strong>{{.ID}}</strong>{{if .Name}}<br><span style="color:#999;font-size:.8rem">{{.Name}}</span>{{end}}</td>
  <td>{{.Region}}</td>
  <td style="font-family:monospace;font-size:.85rem">{{.Host}}</td>
  <td>{{if .HealthPort}}tcp:{{.HealthPort}}{{else}}<span style="color:#aaa">none</span>{{end}}</td>
  <td style="font-size:.8rem;color:#666">{{if .LastSeen}}{{localTime .LastSeen "datetime"}}{{else}}<span style="color:#aaa">never</span>{{end}}</td>
  <td>{{if .Enabled}}enabled{{else}}<span style="color:#991b1b">disabled</span>{{end}}</td>
  <td style="white-space:nowrap">
    <form method="post" action="/inkay/admin/relays/toggle" style="display:inline">
      <input type="hidden" name="id" value="{{.ID}}">
      {{if .Enabled}}<button class="btn-off" type="submit">Disable</button>{{else}}<button class="btn-on" type="submit">Enable</button>{{end}}
    </form>
    <form method="post" action="/inkay/admin/relays/delete" style="display:inline" onsubmit="return confirm('Delete relay {{.ID}}? Its bundle stops working immediately.')">
      <input type="hidden" name="id" value="{{.ID}}">
      <button class="btn-del" type="submit">Delete</button>
    </form>
  </td>
</tr>
{{else}}<tr><td colspan="7" style="color:#aaa">No relays registered yet</td></tr>
{{end}}
</table>

<fieldset>
<legend>Add relay</legend>
<form method="post" action="/inkay/admin/relays/add">
  <div class="row">
    <div class="field" style="max-width:130px"><label>ID</label><input type="text" name="id" placeholder="us-1" required pattern="[a-z0-9][a-z0-9-]{0,30}"></div>
    <div class="field"><label>Region</label>
      <select name="region">{{range .Regions}}<option value="{{.Key}}">{{.Label}}</option>{{end}}</select></div>
    <div class="field"><label>Host (name or IP handed out by DNS)</label><input type="text" name="host" placeholder="relay-us.nicochristmann.net" required></div>
  </div>
  <div class="row" style="margin-top:.75rem">
    <div class="field"><label>Display name (optional)</label><input type="text" name="name" placeholder="US East"></div>
    <div class="field" style="max-width:190px"><label>Health check TCP port (0 = none)</label><input type="number" name="health_port" value="443" min="0" max="65535"></div>
    <div class="field" style="max-width:100px"><label>&nbsp;</label><button class="submit" type="submit">Add</button></div>
  </div>
</form>
</fieldset>
` + localTimeScript + `
</body>
</html>`))
