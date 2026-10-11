package main

// Admin view of RvtA, the 3DS analytics sysmodule (battery, temperature,
// Wi-Fi, lid/sleep, running title, error codes, plus crash and memory dumps).
// account-proxy owns the tables (rvta_consoles, rvta_samples, rvta_dumps) and
// stores dumps under /nico-pretendo-bridge/rvta-dumps/<console id>/; this page
// only reads them. Staff search by friend code.

import (
	"encoding/csv"
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

const rvtaAdminDumpDir = "/nico-pretendo-bridge/rvta-dumps"

var (
	rvtaAdminConsolePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	rvtaAdminFilePattern    = regexp.MustCompile(`^(crash|memory)-[A-Za-z0-9._-]{1,80}$`)
	rvtaAdminEvents         = []string{"sample", "boot", "sleep", "wake", "shell_open", "shell_close", "title", "error"}
)

// rvtaParseFriendCode accepts "3919-5651-5704", "3919 5651 5704" or "391956515704".
func rvtaParseFriendCode(s string) (int64, bool) {
	s = strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(s))
	if s == "0" {
		return 0, true
	}
	if len(s) != 12 {
		return 0, false
	}
	fc, err := strconv.ParseInt(s, 10, 64)
	return fc, err == nil && fc >= 0 // 0 = rows written without a friend account
}

func rvtaFormatFriendCode(fc int64) string {
	s := fmt.Sprintf("%012d", fc)
	return s[0:4] + "-" + s[4:8] + "-" + s[8:12]
}

type rvtaConsoleRow struct {
	ConsoleID   string
	FriendCode  int64
	Version     string
	FirstSeen   time.Time
	LastSeen    time.Time
	Samples     int
	LastBattery int
	LastTemp    int
}

type rvtaFCRow struct {
	FriendCode int64
	Consoles   int
	Rows       int
	Dumps      int
	FirstSeen  time.Time
	LastSeen   time.Time
}

type rvtaSampleRow struct {
	ConsoleID     string
	ConsoleTime   time.Time
	ReceivedAt    time.Time
	Event         string
	BatteryPct    int
	BatteryMV     int
	Charging      bool
	Adapter       bool
	TempC         int
	NetState      int
	WifiConnected bool
	WifiStrength  int
	WirelessMode  int
	ShellOpen     bool
	TitleID       int64
	ACError       int64
	FRDResult     int64
	ACTResult     int64
}

type rvtaDumpRow struct {
	ConsoleID string
	Name      string
	Kind      string
	Total     int64
	Received  int64
	Started   time.Time
	Completed *time.Time
}

func (d rvtaDumpRow) Done() bool { return d.Received >= d.Total }

func adminRvta(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("fc"))
	event := r.URL.Query().Get("event")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 5000 {
		limit = 300
	}
	data := struct {
		Q, Event, Error, FCFormatted string
		FC                           int64
		Searched                     bool
		Limit                        int
		Events                       []string
		FriendCodes                  []rvtaFCRow
		Consoles                     []rvtaConsoleRow
		Samples                      []rvtaSampleRow
		Dumps                        []rvtaDumpRow
		TotalConsoles, TotalSamples  int
	}{Q: q, Event: event, Limit: limit, Events: rvtaAdminEvents}

	db.QueryRow(`SELECT (SELECT COUNT(*) FROM rvta_consoles), (SELECT COUNT(*) FROM rvta_samples)`).
		Scan(&data.TotalConsoles, &data.TotalSamples)

	if q != "" {
		fc, ok := rvtaParseFriendCode(q)
		if !ok {
			data.Error = "That is not a 12-digit friend code."
		} else {
			data.FC = fc
			data.Searched = true
			data.FCFormatted = rvtaFormatFriendCode(fc)
		}
	}

	// Without a search: every friend code that has data, most recent first.
	if !data.Searched {
		if rows, err := db.Query(`SELECT s.friend_code, COUNT(DISTINCT s.console_id), COUNT(*),
				(SELECT COUNT(*) FROM rvta_dumps d WHERE d.friend_code = s.friend_code), MIN(s.received_at), MAX(s.received_at)
			FROM rvta_samples s
			GROUP BY s.friend_code ORDER BY MAX(s.received_at) DESC LIMIT 1000`); err != nil {
			log.Printf("rvta admin: friend codes: %v", err)
		} else {
			for rows.Next() {
				var f rvtaFCRow
				if rows.Scan(&f.FriendCode, &f.Consoles, &f.Rows, &f.Dumps, &f.FirstSeen, &f.LastSeen) == nil {
					data.FriendCodes = append(data.FriendCodes, f)
				}
			}
			rows.Close()
		}
	}

	// Consoles that uploaded under the searched friend code.
	consoleQuery := `SELECT c.console_id, s.friend_code, c.version, c.first_seen, c.last_seen,
			s.n, s.battery, s.temp
		FROM rvta_consoles c
		JOIN LATERAL (
			SELECT friend_code, COUNT(*) OVER () AS n, battery_pct AS battery, temp_c AS temp
			FROM rvta_samples x WHERE x.console_id = c.console_id AND x.friend_code = $1
			ORDER BY x.seq DESC LIMIT 1
		) s ON true
		ORDER BY c.last_seen DESC LIMIT 100`
	if !data.Searched {
		// no search: the friend code list above is the overview
	} else if rows, err := db.Query(consoleQuery, data.FC); err != nil {
		log.Printf("rvta admin: consoles: %v", err)
	} else {
		for rows.Next() {
			var c rvtaConsoleRow
			if rows.Scan(&c.ConsoleID, &c.FriendCode, &c.Version, &c.FirstSeen, &c.LastSeen, &c.Samples,
				&c.LastBattery, &c.LastTemp) == nil {
				data.Consoles = append(data.Consoles, c)
			}
		}
		rows.Close()
	}

	if data.Searched {
		if rows, err := db.Query(`SELECT console_id, console_time, received_at, event, battery_pct, battery_mv, charging,
				adapter, temp_c, net_state, wifi_connected, wifi_strength, wireless_mode, shell_open, title_id, ac_error,
				frd_result, act_result
			FROM rvta_samples WHERE friend_code = $1 AND ($2 = '' OR event = $2)
			ORDER BY received_at DESC, seq DESC LIMIT $3`, data.FC, event, limit); err != nil {
			log.Printf("rvta admin: samples: %v", err)
		} else {
			for rows.Next() {
				var s rvtaSampleRow
				if rows.Scan(&s.ConsoleID, &s.ConsoleTime, &s.ReceivedAt, &s.Event, &s.BatteryPct, &s.BatteryMV,
					&s.Charging, &s.Adapter, &s.TempC, &s.NetState, &s.WifiConnected, &s.WifiStrength,
					&s.WirelessMode, &s.ShellOpen, &s.TitleID, &s.ACError, &s.FRDResult, &s.ACTResult) == nil {
					data.Samples = append(data.Samples, s)
				}
			}
			rows.Close()
		}
		if rows, err := db.Query(`SELECT console_id, name, kind, total, received, started_at, completed_at
			FROM rvta_dumps WHERE friend_code = $1 ORDER BY started_at DESC LIMIT 100`, data.FC); err != nil {
			log.Printf("rvta admin: dumps: %v", err)
		} else {
			for rows.Next() {
				var d rvtaDumpRow
				if rows.Scan(&d.ConsoleID, &d.Name, &d.Kind, &d.Total, &d.Received, &d.Started, &d.Completed) == nil {
					data.Dumps = append(data.Dumps, d)
				}
			}
			rows.Close()
		}
	}

	w.Header().Set("Content-Type", "text/html")
	if err := rvtaAdminTmpl.Execute(w, data); err != nil {
		log.Printf("rvta admin: template: %v", err)
	}
}

// adminRvtaExport streams every row of one friend code as CSV.
func adminRvtaExport(w http.ResponseWriter, r *http.Request) {
	fc, ok := rvtaParseFriendCode(r.URL.Query().Get("fc"))
	if !ok {
		http.Error(w, "bad friend code", http.StatusBadRequest)
		return
	}
	rows, err := db.Query(`SELECT console_id, seq, console_time, received_at, event, battery_pct, battery_mv, charging,
			adapter, temp_c, net_state, wifi_connected, wifi_strength, wireless_mode, shell_open, title_id, ac_error,
			frd_result, act_result
		FROM rvta_samples WHERE friend_code = $1 ORDER BY console_id, seq`, fc)
	if err != nil {
		log.Printf("rvta admin: export: %v", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="rvta-%012d.csv"`, fc))
	cw := csv.NewWriter(w)
	cw.Write([]string{"console_id", "seq", "console_clock", "received_at_utc", "event", "battery_pct", "battery_mv",
		"charging", "adapter", "temp_c", "net_state", "wifi_connected", "wifi_strength", "wireless_mode", "shell_open",
		"title_id", "ac_error", "frd_result", "act_result"})
	for rows.Next() {
		var (
			s   rvtaSampleRow
			seq int64
		)
		if rows.Scan(&s.ConsoleID, &seq, &s.ConsoleTime, &s.ReceivedAt, &s.Event, &s.BatteryPct, &s.BatteryMV,
			&s.Charging, &s.Adapter, &s.TempC, &s.NetState, &s.WifiConnected, &s.WifiStrength, &s.WirelessMode,
			&s.ShellOpen, &s.TitleID, &s.ACError, &s.FRDResult, &s.ACTResult) != nil {
			continue
		}
		cw.Write([]string{s.ConsoleID, strconv.FormatInt(seq, 10), rvtaConsoleClock(s.ConsoleTime),
			s.ReceivedAt.UTC().Format("2006-01-02 15:04:05"), s.Event, strconv.Itoa(s.BatteryPct),
			strconv.Itoa(s.BatteryMV), strconv.FormatBool(s.Charging), strconv.FormatBool(s.Adapter),
			strconv.Itoa(s.TempC), strconv.Itoa(s.NetState), strconv.FormatBool(s.WifiConnected),
			strconv.Itoa(s.WifiStrength), strconv.Itoa(s.WirelessMode), strconv.FormatBool(s.ShellOpen),
			fmt.Sprintf("%016X", s.TitleID), fmt.Sprintf("%08X", s.ACError), fmt.Sprintf("%08X", s.FRDResult),
			fmt.Sprintf("%08X", s.ACTResult)})
	}
	cw.Flush()
}

// adminRvtaDump downloads one stored dump file.
func adminRvtaDump(w http.ResponseWriter, r *http.Request) {
	console := r.URL.Query().Get("console")
	name := r.URL.Query().Get("name")
	if !rvtaAdminConsolePattern.MatchString(console) || !rvtaAdminFilePattern.MatchString(name) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	path := filepath.Join(rvtaAdminDumpDir, console, name)
	if _, err := os.Stat(path); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s"`, console[:8], name))
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, path)
}

// The 3DS clock has no time zone: RvtA stores local console time as if it were UTC.
func rvtaConsoleClock(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05") }

var rvtaWirelessModes = map[int]string{0: "—", 1: "internet", 2: "local", 3: "StreetPass", 4: "StreetPass data"}

var rvtaAdminTmpl = template.Must(template.New("rvta").Funcs(tmplFuncs).Funcs(template.FuncMap{
	"fc":       rvtaFormatFriendCode,
	"clock":    rvtaConsoleClock,
	"hex8":     func(v int64) string { return fmt.Sprintf("%08X", v) },
	"tid":      func(v int64) string { return fmt.Sprintf("%016X", v) },
	"short":    func(s string) string { return s[:8] },
	"wireless": func(m int) string { return rvtaWirelessModes[m] },
	"mib":      func(n int64) string { return fmt.Sprintf("%.1f MB", float64(n)/(1<<20)) },
	"pct":      func(a, b int64) int64 { return a * 100 / max(b, 1) },
}).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>RvtA diagnostics</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
body{font-family:system-ui,sans-serif;max-width:1200px;margin:2rem auto;padding:0 1rem;color:#222}
h1{font-size:1.4rem}
h2{font-size:1.1rem;margin-top:2rem}
a{color:#2563eb}
table{width:100%;border-collapse:collapse;font-size:.85rem;margin-bottom:1.5rem}
th{text-align:left;border-bottom:2px solid #e4e4e7;padding:.4rem .6rem;color:#666;font-weight:600;white-space:nowrap}
td{padding:.35rem .6rem;border-bottom:1px solid #f0f0f0;vertical-align:middle}
tr:last-child td{border-bottom:none}
button,input,select{font:inherit}
button{cursor:pointer;border:none;border-radius:4px;padding:.35rem .8rem;font-size:.85rem;background:#2563eb;color:#fff}
.row{display:flex;gap:.75rem;flex-wrap:wrap;align-items:flex-end;margin-bottom:1rem}
.field{display:flex;flex-direction:column;gap:.3rem}
label{font-size:.8rem;color:#666;font-weight:600}
input[type=text],input[type=number],select{border:1px solid #d1d5db;border-radius:4px;padding:.4rem .6rem}
.note{background:#f8fafc;border:1px solid #e2e8f0;border-radius:6px;padding:.75rem 1rem;margin-bottom:1.5rem;font-size:.9rem}
.err{background:#fee2e2;border:1px solid #fecaca;color:#991b1b;padding:.5rem 1rem;border-radius:6px;margin-bottom:1rem}
.mono{font-family:monospace;font-size:.8rem}
.dim{color:#999}
.warn{color:#b45309;font-weight:600}
.bad{color:#b91c1c;font-weight:600}
.ev{display:inline-block;padding:0 .4rem;border-radius:4px;background:#f1f5f9;font-size:.75rem}
.ev-error{background:#fee2e2;color:#991b1b}.ev-boot{background:#dbeafe}.ev-sleep,.ev-wake{background:#ede9fe}.ev-title{background:#dcfce7}
</style>
</head>
<body>
<p><a href="/inkay/admin/">← Back to admin</a></p>
<h1>RvtA diagnostics</h1>
<div class="note">Battery, temperature, Wi-Fi, lid/sleep, running title and error codes from 3DS consoles with the Revivetendo
patches, uploaded every 15 minutes, plus crash dumps and memory dumps (L + START + SELECT). <strong>{{.TotalConsoles}}</strong>
consoles, <strong>{{.TotalSamples}}</strong> rows. "Console clock" is the console's own local time (the 3DS has no time zone);
"Received" is the server's time.</div>

<form method="get" action="/inkay/admin/rvta/" class="row">
  <div class="field"><label>Friend code</label><input type="text" name="fc" value="{{.Q}}" placeholder="1234-5678-9012" size="16"></div>
  <div class="field"><label>Event</label><select name="event"><option value="">all</option>
    {{range .Events}}<option value="{{.}}"{{if eq . $.Event}} selected{{end}}>{{.}}</option>{{end}}</select></div>
  <div class="field"><label>Rows</label><input type="number" name="limit" value="{{.Limit}}" min="1" max="5000" style="width:6rem"></div>
  <button type="submit">Search</button>
  {{if .Searched}}<a href="/inkay/admin/rvta/export.csv?fc={{.FC}}">⬇ All rows as CSV</a>{{end}}
</form>
{{if .Error}}<div class="err">{{.Error}}</div>{{end}}

{{if .Searched}}<h2>{{if .FC}}{{.FCFormatted}}{{else}}No friend account{{end}}</h2>{{end}}

{{if not .Searched}}
<h2>Friend codes with data ({{len .FriendCodes}})</h2>
<table>
<tr><th>Friend code</th><th>Consoles</th><th>Rows</th><th>Dumps</th><th>First upload</th><th>Last upload</th></tr>
{{range .FriendCodes}}
<tr>
  <td class="mono"><a href="/inkay/admin/rvta/?fc={{.FriendCode}}">{{if .FriendCode}}{{fc .FriendCode}}{{else}}no friend account{{end}}</a></td>
  <td>{{.Consoles}}</td>
  <td>{{.Rows}}</td>
  <td>{{if .Dumps}}{{.Dumps}}{{else}}<span class="dim">0</span>{{end}}</td>
  <td>{{localTime .FirstSeen "datetime-sec"}}</td>
  <td>{{localTime .LastSeen "datetime-sec"}}</td>
</tr>
{{else}}<tr><td colspan="6" class="dim">No data yet</td></tr>
{{end}}
</table>
{{else}}
<h2>Consoles with this friend code</h2>
<table>
<tr><th>Console</th><th>Friend code</th><th>RvtA</th><th>First seen</th><th>Last upload</th><th>Rows</th><th>Last battery</th><th>Last temp</th></tr>
{{range .Consoles}}
<tr>
  <td class="mono">{{short .ConsoleID}}</td>
  <td class="mono"><a href="/inkay/admin/rvta/?fc={{.FriendCode}}">{{fc .FriendCode}}</a></td>
  <td>{{.Version}}</td>
  <td>{{localTime .FirstSeen "datetime-sec"}}</td>
  <td>{{localTime .LastSeen "datetime-sec"}}</td>
  <td>{{.Samples}}</td>
  <td{{if lt .LastBattery 15}} class="warn"{{end}}>{{.LastBattery}} %</td>
  <td{{if ge .LastTemp 45}} class="bad"{{end}}>{{if eq .LastTemp -128}}<span class="dim">?</span>{{else}}{{.LastTemp}} °C{{end}}</td>
</tr>
{{else}}<tr><td colspan="8" class="dim">No consoles with this friend code</td></tr>
{{end}}
</table>

<h2>Dumps</h2>
<table>
<tr><th>Kind</th><th>File</th><th>Console</th><th>Size</th><th>Received</th><th>Started</th><th></th></tr>
{{range .Dumps}}
<tr>
  <td>{{.Kind}}</td>
  <td class="mono">{{.Name}}</td>
  <td class="mono">{{short .ConsoleID}}</td>
  <td>{{mib .Total}}</td>
  <td>{{if .Done}}complete{{else}}<span class="warn">{{pct .Received .Total}} %</span>{{end}}</td>
  <td>{{localTime .Started "datetime-sec"}}</td>
  <td>{{if .Done}}<a href="/inkay/admin/rvta/dump?console={{.ConsoleID}}&name={{.Name}}">⬇ download</a>{{end}}</td>
</tr>
{{else}}<tr><td colspan="7" class="dim">No dumps</td></tr>
{{end}}
</table>

<h2>Rows (newest first{{if .Event}}, only "{{.Event}}"{{end}})</h2>
<table>
<tr><th>Console clock</th><th>Received</th><th>Event</th><th>Battery</th><th>Temp</th><th>Power</th><th>Wi-Fi</th><th>Lid</th><th>Title</th><th>AC</th><th>FRD</th><th>ACT</th></tr>
{{range .Samples}}
<tr>
  <td class="mono">{{clock .ConsoleTime}}</td>
  <td class="mono dim">{{localTime .ReceivedAt "datetime-sec"}}</td>
  <td><span class="ev ev-{{.Event}}">{{.Event}}</span></td>
  <td{{if lt .BatteryPct 15}} class="warn"{{end}}>{{.BatteryPct}} %{{if .BatteryMV}} <span class="dim">{{.BatteryMV}} mV</span>{{end}}</td>
  <td{{if ge .TempC 45}} class="bad"{{end}}>{{if eq .TempC -128}}<span class="dim">?</span>{{else}}{{.TempC}} °C{{end}}</td>
  <td>{{if .Charging}}charging{{else if .Adapter}}adapter{{else}}<span class="dim">battery</span>{{end}}</td>
  <td>{{if .WifiConnected}}online {{.WifiStrength}}/3{{else}}<span class="dim">offline</span>{{end}} <span class="dim">{{wireless .WirelessMode}}</span></td>
  <td>{{if .ShellOpen}}open{{else}}<span class="dim">closed</span>{{end}}</td>
  <td class="mono">{{if .TitleID}}{{tid .TitleID}}{{else}}<span class="dim">HOME</span>{{end}}</td>
  <td class="mono{{if .ACError}} bad{{end}}">{{if .ACError}}{{hex8 .ACError}}{{else}}<span class="dim">0</span>{{end}}</td>
  <td class="mono{{if .FRDResult}} bad{{end}}">{{if .FRDResult}}{{hex8 .FRDResult}}{{else}}<span class="dim">0</span>{{end}}</td>
  <td class="mono{{if .ACTResult}} bad{{end}}">{{if .ACTResult}}{{hex8 .ACTResult}}{{else}}<span class="dim">0</span>{{end}}</td>
</tr>
{{else}}<tr><td colspan="12" class="dim">No rows</td></tr>
{{end}}
</table>
{{end}}
</body>
</html>
`))
