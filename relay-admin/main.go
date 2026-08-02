package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
)

var gameServerTitles = map[string]string{
	"00003200": "Friends / Presence",
	"1005A000": "WiiU Chat",
	"1010EB00": "Mario Kart 8",
	"1012F100": "Wii Sports Club",
	"10145E00": "Angry Birds Star Wars",
	"10176A00": "Super Mario Maker",
	"100E4B00": "Super Smash Bros.",
	"1014B700": "Minecraft: WiiU Edition",
	"10138B00": "Pokemon Art Academy",
	"10104E00": "Animal Crossing: amiibo Festival",
	"1019EC00": "Yo-Kai Watch Blasters",
	"10189B00": "Pokémon Rumble World",
}

var tmplFuncs = template.FuncMap{
	"gameTitle": func(id string) string {
		if id == "" {
			return "—"
		}
		upper := strings.ToUpper(id)
		if title, ok := gameServerTitles[upper]; ok {
			return title
		}
		return upper
	},
	"gameTitleFull": func(id string) string {
		if id == "" {
			return "—"
		}
		upper := strings.ToUpper(id)
		if title, ok := gameServerTitles[upper]; ok {
			return title + " (" + upper + ")"
		}
		return upper
	},
	"gameName": func(id int64) string {
		if id == 0 {
			return ""
		}
		key := fmt.Sprintf("%08X", uint64(id)&0xFFFFFFFF)
		if name, ok := gameServerTitles[key]; ok {
			return name
		}
		return fmt.Sprintf("%016X", uint64(id))
	},
	// resolveGameName tries game server hex first (current connection), then TitleID as fallback.
	// Returns "" when neither resolves so templates can show a fallback.
	"resolveGameName": func(titleID int64, serverHex string) string {
		if serverHex != "" {
			if name, ok := gameServerTitles[strings.ToUpper(serverHex)]; ok {
				return name
			}
		}
		if titleID != 0 {
			key := fmt.Sprintf("%08X", uint64(titleID)&0xFFFFFFFF)
			if name, ok := gameServerTitles[key]; ok {
				return name
			}
		}
		return ""
	},
	"matchTime": func(t time.Time) string {
		now := time.Now()
		if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
			return t.Format("15:04")
		}
		return t.Format("Jan 2, 15:04")
	},
}

const dbSchema = `
CREATE TABLE IF NOT EXISTS redirects (
	id             SERIAL      PRIMARY KEY,
	type           TEXT        NOT NULL CHECK (type IN ('iosu', 'dns')),
	address        TEXT,
	from_host      TEXT        NOT NULL,
	to_host        TEXT        NOT NULL,
	game_server_id TEXT,
	port           INTEGER,
	enabled        BOOLEAN     NOT NULL DEFAULT true,
	created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS relay_requests (
	id             BIGSERIAL   PRIMARY KEY,
	pid            BIGINT      NOT NULL,
	game_server_id TEXT        NOT NULL,
	requested_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS user_access (
	pid            BIGINT      NOT NULL,
	game_server_id TEXT        NOT NULL,
	note           TEXT,
	created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	PRIMARY KEY (pid, game_server_id)
);

CREATE TABLE IF NOT EXISTS banned_users (
	pid            BIGINT      PRIMARY KEY,
	reason         TEXT,
	created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS review_queue (
	pid            BIGINT      NOT NULL,
	game_server_id TEXT        NOT NULL,
	first_seen     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	last_seen      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	attempt_count  INTEGER     NOT NULL DEFAULT 1,
	PRIMARY KEY (pid, game_server_id)
);

CREATE TABLE IF NOT EXISTS admin_certs (
	id         SERIAL      PRIMARY KEY,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	cert_pem   TEXT        NOT NULL,
	key_pem    TEXT        NOT NULL
);

CREATE TABLE IF NOT EXISTS pnid_cache (
	pid        BIGINT      PRIMARY KEY,
	pnid       TEXT        NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`

const seedRedirects = `
INSERT INTO redirects (type, from_host, to_host, game_server_id, port, access_mode)
SELECT 'dns', 'account.pretendo.cc', '45.157.178.35', '1005A000', 60004, 'whitelist'
WHERE NOT EXISTS (SELECT 1 FROM redirects WHERE game_server_id = '1005A000');

UPDATE redirects SET port = 60004 WHERE game_server_id = '1005A000' AND port IS NULL;

INSERT INTO redirects (type, from_host, to_host, game_server_id, port, access_mode)
SELECT 'dns', 'account.pretendo.cc', '45.157.178.35', '1010EB00', 60002, 'whitelist'
WHERE NOT EXISTS (SELECT 1 FROM redirects WHERE game_server_id = '1010EB00');
`


type Redirect struct {
	ID           int       `json:"id"`
	Type         string    `json:"type"`
	Address      string    `json:"address,omitempty"`
	FromHost     string    `json:"from_host"`
	ToHost       string    `json:"to_host"`
	GameServerID string    `json:"game_server_id,omitempty"`
	Port         int       `json:"port,omitempty"`
	AccessMode   string    `json:"access_mode"`
	Enabled      bool      `json:"enabled"`
	CreatedAt    time.Time `json:"created_at"`
}

type UserAccess struct {
	PID          int64
	PNID         string
	GameServerID string
	Note         string
	CreatedAt    time.Time
}

type BannedUser struct {
	PID       int64
	PNID      string
	Reason    string
	CreatedAt time.Time
}

type ReviewEntry struct {
	PID          int64
	PNID         string
	GameServerID string
	FirstSeen    time.Time
	LastSeen     time.Time
	Attempts     int
}

type RecentRequest struct {
	PID          int64     `json:"pid"`
	PNID         string    `json:"pnid,omitempty"`
	GameServerID string    `json:"game_server_id"`
	RequestedAt  time.Time `json:"requested_at"`
}

type OnlineUser struct {
	PID          int64
	PNID         string
	TitleID      int64
	GameServerHex string
	GameName      string
}

type Stats struct {
	TotalPIDs       int `json:"total_pids"`
	TotalRequests   int `json:"total_requests"`
	Requests24h     int `json:"requests_last_24h"`
	ActiveRedirects int `json:"active_redirects"`
}


var db *sql.DB

func fetchWiiUTitleDB() {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get("https://dantheman827.github.io/nus-info/complete-wup-regionprimaries.json")
	if err != nil {
		log.Printf("[titledb] fetch failed: %v", err)
		return
	}
	// Format: { "US": { "titleID": {name, ...}, ... }, "GB": {...}, ... }
	var db map[string]map[string]struct {
		Name string `json:"name"`
	}
	err = json.NewDecoder(resp.Body).Decode(&db)
	resp.Body.Close()
	if err != nil {
		log.Printf("[titledb] parse failed: %v", err)
		return
	}
	added := 0
	for _, titles := range db {
		for titleID, entry := range titles {
			if len(titleID) != 16 || entry.Name == "" {
				continue
			}
			key := strings.ToUpper(titleID[8:])
			gameServerTitles[key] = entry.Name
			added++
		}
	}
	log.Printf("[titledb] loaded %d additional titles", added)
}

func main() {
	godotenv.Load("../wiiu-chat-secure/.env")
	fetchWiiUTitleDB()

	var err error
	db, err = sql.Open("postgres", os.Getenv("PN_WUC_POSTGRES_URI"))
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer db.Close()

	if _, err = db.Exec(dbSchema); err != nil {
		log.Fatalf("schema: %v", err)
	}
	db.Exec(`ALTER TABLE redirects ADD COLUMN IF NOT EXISTS game_server_id TEXT`)
	db.Exec(`ALTER TABLE redirects ADD COLUMN IF NOT EXISTS port INTEGER`)
	db.Exec(`ALTER TABLE redirects ADD COLUMN IF NOT EXISTS access_mode TEXT NOT NULL DEFAULT 'whitelist'`)
	// Default existing open game-server redirects to whitelist so unknown users fall through to Pretendo.
	db.Exec(`UPDATE redirects SET access_mode = 'whitelist' WHERE access_mode = 'open' AND game_server_id IS NOT NULL`)

	if _, err = db.Exec(seedRedirects); err != nil {
		log.Fatalf("seed: %v", err)
	}

	// Ensure at least one admin client cert exists; rotate on schedule.
	if initCert, err := latestAdminCert(); err != nil {
		log.Fatalf("cert check: %v", err)
	} else if initCert == nil {
		log.Printf("no admin cert found, generating initial cert")
		if err := rotateCert(); err != nil {
			log.Printf("initial cert generation failed: %v", err)
		}
	}
	go certRotationLoop()

	http.HandleFunc("/", landingUI)
	http.HandleFunc("/api/redirects", apiRedirects)
	http.HandleFunc("/api/stats", apiStats)
	http.HandleFunc("/stats/", statsUI)
	http.HandleFunc("/stats/card.svg", statsCard)
	http.HandleFunc("/my/login", myLoginHandler)
	http.HandleFunc("/my/logout", myLogoutHandler)
	http.HandleFunc("/my/discord", myDiscordHandler)
	http.HandleFunc("/my/", myHandler)
	http.HandleFunc("/admin/", adminUI)
	http.HandleFunc("/admin/add", adminAdd)
	http.HandleFunc("/admin/delete", adminDelete)
	http.HandleFunc("/admin/toggle", adminToggle)

	http.HandleFunc("/admin/users/", adminUsers)
	http.HandleFunc("/admin/users/add", adminUserAdd)
	http.HandleFunc("/admin/users/delete", adminUserDelete)
	http.HandleFunc("/admin/bans/", adminBans)
	http.HandleFunc("/admin/bans/add", adminBanAdd)
	http.HandleFunc("/admin/bans/remove", adminBanRemove)
	http.HandleFunc("/admin/review/", adminReview)
	http.HandleFunc("/admin/review/approve", adminReviewApprove)
	http.HandleFunc("/admin/review/dismiss", adminReviewDismiss)
	http.HandleFunc("/admin/certs/rotate", adminCertsRotate)
	http.HandleFunc("/admin/client-cert.p12", adminClientCert)
	http.HandleFunc("/wsc-public/", adminWSC)
	http.HandleFunc("/wsc-public/nat/", wscNATInfo)
	http.HandleFunc("/wsc-public/api/players", apiWSCPlayers)
	http.HandleFunc("/wsc-public/overlay", apiWSCOverlay)

	addr := "127.0.0.1:9004"
	log.Printf("relay-admin listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

// --- API ---

func apiRedirects(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(`
		SELECT id, type, COALESCE(address, ''), from_host, to_host, COALESCE(game_server_id, ''), COALESCE(port, 0), COALESCE(access_mode, 'whitelist'), enabled, created_at
		FROM redirects WHERE enabled = true ORDER BY id`)
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	defer rows.Close()

	var list []Redirect
	for rows.Next() {
		var rd Redirect
		if err := rows.Scan(&rd.ID, &rd.Type, &rd.Address, &rd.FromHost, &rd.ToHost, &rd.GameServerID, &rd.Port, &rd.AccessMode, &rd.Enabled, &rd.CreatedAt); err != nil {
			continue
		}
		list = append(list, rd)
	}
	if list == nil {
		list = []Redirect{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

func apiStats(w http.ResponseWriter, r *http.Request) {
	s := collectStats()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s)
}


func collectStats() Stats {
	var s Stats
	db.QueryRow(`SELECT COUNT(*) FROM nex_accounts`).Scan(&s.TotalPIDs)
	db.QueryRow(`SELECT COUNT(*) FROM relay_requests`).Scan(&s.TotalRequests)
	db.QueryRow(`SELECT COUNT(*) FROM relay_requests WHERE requested_at > NOW() - INTERVAL '24 hours'`).Scan(&s.Requests24h)
	db.QueryRow(`SELECT COUNT(*) FROM redirects WHERE enabled = true`).Scan(&s.ActiveRedirects)
	return s
}

func recentRequests(limit int) []RecentRequest {
	rows, err := db.Query(`
		SELECT r.pid, COALESCE(p.pnid, ''), r.game_server_id, r.requested_at
		FROM relay_requests r
		LEFT JOIN pnid_cache p ON p.pid = r.pid
		ORDER BY r.requested_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []RecentRequest
	for rows.Next() {
		var rr RecentRequest
		rows.Scan(&rr.PID, &rr.PNID, &rr.GameServerID, &rr.RequestedAt)
		out = append(out, rr)
	}
	return out
}

func allRedirects() []Redirect {
	rows, err := db.Query(`
		SELECT id, type, COALESCE(address, ''), from_host, to_host, COALESCE(game_server_id, ''), COALESCE(port, 0), COALESCE(access_mode, 'whitelist'), enabled, created_at
		FROM redirects ORDER BY id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var list []Redirect
	for rows.Next() {
		var rd Redirect
		rows.Scan(&rd.ID, &rd.Type, &rd.Address, &rd.FromHost, &rd.ToHost, &rd.GameServerID, &rd.Port, &rd.AccessMode, &rd.Enabled, &rd.CreatedAt)
		list = append(list, rd)
	}
	return list
}

func usersForGame(gameServerID string) []UserAccess {
	rows, err := db.Query(`
		SELECT u.pid, COALESCE(p.pnid, ''), u.game_server_id, COALESCE(u.note, ''), u.created_at
		FROM user_access u
		LEFT JOIN pnid_cache p ON p.pid = u.pid
		WHERE UPPER(u.game_server_id) = UPPER($1) ORDER BY u.created_at DESC`, gameServerID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var list []UserAccess
	for rows.Next() {
		var u UserAccess
		rows.Scan(&u.PID, &u.PNID, &u.GameServerID, &u.Note, &u.CreatedAt)
		list = append(list, u)
	}
	return list
}

func allBans() []BannedUser {
	rows, err := db.Query(`
		SELECT b.pid, COALESCE(p.pnid, ''), COALESCE(b.reason, ''), b.created_at
		FROM banned_users b
		LEFT JOIN pnid_cache p ON p.pid = b.pid
		ORDER BY b.created_at DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var list []BannedUser
	for rows.Next() {
		var b BannedUser
		rows.Scan(&b.PID, &b.PNID, &b.Reason, &b.CreatedAt)
		list = append(list, b)
	}
	return list
}

func onlineUsers() []OnlineUser {
	rows, err := db.Query(`
		SELECT s.pid, COALESCE(NULLIF(p.pnid,''), ''),
		       COALESCE(s.presence_title_id, 0),
		       COALESCE(s.presence_game_server_id, 0)
		FROM user_settings s
		LEFT JOIN pnid_cache p ON p.pid = s.pid
		WHERE s.is_online IS TRUE
		ORDER BY p.pnid`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var list []OnlineUser
	for rows.Next() {
		var u OnlineUser
		var gsid int64
		rows.Scan(&u.PID, &u.PNID, &u.TitleID, &gsid)
		if gsid != 0 {
			u.GameServerHex = fmt.Sprintf("%08X", uint64(gsid))
			u.GameName = gameServerTitles[u.GameServerHex]
		}
		list = append(list, u)
	}
	return list
}

func pendingReviews() []ReviewEntry {
	rows, err := db.Query(`
		SELECT r.pid, COALESCE(p.pnid, ''), r.game_server_id, r.first_seen, r.last_seen, r.attempt_count
		FROM review_queue r
		LEFT JOIN pnid_cache p ON p.pid = r.pid
		ORDER BY r.last_seen DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var list []ReviewEntry
	for rows.Next() {
		var e ReviewEntry
		rows.Scan(&e.PID, &e.PNID, &e.GameServerID, &e.FirstSeen, &e.LastSeen, &e.Attempts)
		list = append(list, e)
	}
	return list
}

func reviewCount() int {
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM review_queue`).Scan(&n)
	return n
}

// --- WSC dashboard ---

const wscStatusURL = "http://127.0.0.1:9015/status"

var wscSportNames = map[int64]string{
	0: "Baseball",
	1: "Tennis",
	2: "Golf",
	3: "Bowling",
	4: "Boxing",
}

type WSCPlayerRow struct {
	PID  int64
	PNID string
	NATm int64
	IP   string
	Port string
}

type WSCGatheringRow struct {
	GID         int64
	SportName   string
	Host        int64
	HostPNID    string
	PlayerCount int64
	MaxPlayers  int64
	Players     []WSCPlayerRow
	Open        bool
}

type WSCMatchRow struct {
	GID         int64
	SportName   string
	HostPNID    string
	Players     []string // PNIDs (or "PID:xxx" fallback)
	PlayerCount int64
	StartedAt   time.Time
}

type WSCDashData struct {
	ServerUp   bool
	Players    []WSCPlayerRow
	Gatherings []WSCGatheringRow
	Matches    []WSCMatchRow
}

func lookupPNIDs(pids []int64) map[int64]string {
	if len(pids) == 0 {
		return map[int64]string{}
	}
	placeholders := make([]string, len(pids))
	args := make([]interface{}, len(pids))
	for i, pid := range pids {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = pid
	}
	query := fmt.Sprintf(`SELECT pid, pnid FROM pnid_cache WHERE pid IN (%s)`,
		strings.Join(placeholders, ","))
	rows, err := db.Query(query, args...)
	result := map[int64]string{}
	if err != nil {
		return result
	}
	defer rows.Close()
	for rows.Next() {
		var pid int64
		var pnid string
		rows.Scan(&pid, &pnid)
		result[pid] = pnid
	}
	return result
}

func fetchWSCStatus() WSCDashData {
	var data WSCDashData
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(wscStatusURL)
	if err != nil {
		return data
	}
	defer resp.Body.Close()
	data.ServerUp = true

	var raw struct {
		Sessions []struct {
			PID  int64  `json:"pid"`
			NATm int64  `json:"natm"`
			IP   string `json:"ip"`
			Port string `json:"port"`
		} `json:"sessions"`
		Gatherings []struct {
			GID         int64   `json:"gid"`
			Host        int64   `json:"host"`
			SportType   int64   `json:"sport_type"`
			MaxPlayers  int64   `json:"max_players"`
			PlayerCount int64   `json:"player_count"`
			Players     []int64 `json:"players"`
			Open        bool    `json:"open"`
		} `json:"gatherings"`
		Matches []struct {
			GID         int64   `json:"gid"`
			Host        int64   `json:"host"`
			SportType   int64   `json:"sport_type"`
			Players     []int64 `json:"players"`
			PlayerCount int64   `json:"player_count"`
			StartedAt   int64   `json:"started_at"`
		} `json:"matches"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return data
	}

	// Collect all PIDs for batch PNID lookup.
	pidSet := map[int64]struct{}{}
	for _, s := range raw.Sessions {
		pidSet[s.PID] = struct{}{}
	}
	for _, g := range raw.Gatherings {
		pidSet[g.Host] = struct{}{}
		for _, p := range g.Players {
			pidSet[p] = struct{}{}
		}
	}
	for _, m := range raw.Matches {
		pidSet[m.Host] = struct{}{}
		for _, p := range m.Players {
			pidSet[p] = struct{}{}
		}
	}
	pids := make([]int64, 0, len(pidSet))
	for pid := range pidSet {
		pids = append(pids, pid)
	}
	pnids := lookupPNIDs(pids)

	for _, s := range raw.Sessions {
		data.Players = append(data.Players, WSCPlayerRow{
			PID:  s.PID,
			PNID: pnids[s.PID],
			NATm: s.NATm,
			IP:   s.IP,
			Port: s.Port,
		})
	}

	for _, g := range raw.Gatherings {
		sport := wscSportNames[g.SportType]
		if sport == "" {
			sport = fmt.Sprintf("Sport %d", g.SportType)
		}
		row := WSCGatheringRow{
			GID:         g.GID,
			SportName:   sport,
			Host:        g.Host,
			HostPNID:    pnids[g.Host],
			PlayerCount: g.PlayerCount,
			MaxPlayers:  g.MaxPlayers,
			Open:        g.Open,
		}
		for _, pid := range g.Players {
			row.Players = append(row.Players, WSCPlayerRow{
				PID:  pid,
				PNID: pnids[pid],
			})
		}
		data.Gatherings = append(data.Gatherings, row)
	}

	for _, m := range raw.Matches {
		sport := wscSportNames[m.SportType]
		if sport == "" {
			sport = fmt.Sprintf("Sport %d", m.SportType)
		}
		row := WSCMatchRow{
			GID:         m.GID,
			SportName:   sport,
			HostPNID:    pnids[m.Host],
			PlayerCount: m.PlayerCount,
			StartedAt:   time.Unix(m.StartedAt, 0),
		}
		for _, pid := range m.Players {
			if pnid := pnids[pid]; pnid != "" {
				row.Players = append(row.Players, "@"+pnid)
			} else {
				row.Players = append(row.Players, fmt.Sprintf("PID:%d", pid))
			}
		}
		data.Matches = append(data.Matches, row)
	}
	return data
}

var wscTmpl = template.Must(template.New("wsc").Funcs(tmplFuncs).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>WSC Dashboard — Inkay Relay Admin</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
body{font-family:system-ui,sans-serif;max-width:1100px;margin:2rem auto;padding:0 1rem;color:#222}
h1{font-size:1.4rem}
h2{font-size:1.1rem;margin-top:2rem}
a{color:#2563eb}
table{width:100%;border-collapse:collapse;font-size:.9rem;margin-bottom:2rem}
th{text-align:left;border-bottom:2px solid #e4e4e7;padding:.5rem .75rem;color:#666;font-weight:600}
td{padding:.5rem .75rem;border-bottom:1px solid #f0f0f0;vertical-align:middle}
tr:last-child td{border-bottom:none}
.badge{display:inline-block;padding:.2rem .5rem;border-radius:4px;font-size:.75rem;font-weight:600}
.on{background:#dcfce7;color:#166534}.off{background:#fee2e2;color:#991b1b}
.tag{display:inline-block;padding:.15rem .4rem;border-radius:4px;font-size:.75rem;background:#e0e7ff;color:#3730a3}
.mono{font-family:monospace;font-size:.85rem}
</style>
</head>
<body>
<h1>Wii Sports Club Dashboard</h1>
<p style="margin-bottom:1.5rem">
  <a href="/inkay/stats/" target="_blank">← Public stats</a> &nbsp;|&nbsp;
  <a href="/wsc-public/nat/">NAT type guide</a> &nbsp;|&nbsp;
  <a href="/wsc-public/overlay" target="_blank">Stream overlay</a>
</p>

<div style="display:flex;align-items:center;gap:.6rem;margin-bottom:1.5rem;font-size:.9rem">
  <span class="badge {{if .ServerUp}}on{{else}}off{{end}}">{{if .ServerUp}}Server online{{else}}Server unreachable{{end}}</span>
  {{if .ServerUp}}<span style="color:#666">{{len .Players}} connected{{if .Gatherings}}, {{len .Gatherings}} gathering{{if gt (len .Gatherings) 1}}s{{end}}{{end}}</span>{{end}}
  <span style="margin-left:auto;font-size:.8rem;color:#aaa" id="refresh-label">refreshes in 15s</span>
</div>

<h2>Connected Players{{if .Players}} <span style="background:#dcfce7;color:#166534;border-radius:999px;padding:.1rem .5rem;font-size:.75rem;font-weight:700;vertical-align:middle">{{len .Players}}</span>{{end}}</h2>
{{if .Players}}
<table>
<tr><th>PNID</th><th>PID</th><th>NAT</th></tr>
{{range .Players}}
<tr>
  <td>{{if .PNID}}<strong>@{{.PNID}}</strong>{{else}}<span style="color:#aaa">—</span>{{end}}</td>
  <td class="mono">{{.PID}}</td>
  <td>{{if eq .NATm 1}}<span class="badge on">Open</span>{{else if eq .NATm 2}}<span class="badge" style="background:#fef9c3;color:#854d0e">Moderate</span>{{else if eq .NATm 3}}<span class="badge off">Strict</span>{{else}}<span style="color:#aaa">—</span>{{end}}</td>
</tr>
{{end}}
</table>
{{else}}
<p style="color:#aaa;font-size:.9rem;margin-top:0">No players connected.</p>
{{end}}

<h2>Active Gatherings{{if .Gatherings}} <span style="background:#dcfce7;color:#166534;border-radius:999px;padding:.1rem .5rem;font-size:.75rem;font-weight:700;vertical-align:middle">{{len .Gatherings}}</span>{{end}}</h2>
{{if .Gatherings}}
<table>
<tr><th>GID</th><th>Sport</th><th>Host</th><th>Players</th><th>Capacity</th><th>Open</th></tr>
{{range .Gatherings}}
<tr>
  <td class="mono">{{.GID}}</td>
  <td><span class="tag">{{.SportName}}</span></td>
  <td>{{if .HostPNID}}<strong>@{{.HostPNID}}</strong>{{else}}<span class="mono">{{.Host}}</span>{{end}}</td>
  <td>
    {{range .Players}}{{if .PNID}}@{{.PNID}}{{else}}<span class="mono">{{.PID}}</span>{{end}} {{end}}
  </td>
  <td class="mono">{{.PlayerCount}}/{{.MaxPlayers}}</td>
  <td><span class="badge {{if .Open}}on{{else}}off{{end}}">{{if .Open}}open{{else}}full{{end}}</span></td>
</tr>
{{end}}
</table>
{{else}}
<p style="color:#aaa;font-size:.9rem;margin-top:0">No active gatherings.</p>
{{end}}

<h2>Matches (last 24 h){{if .Matches}} <span style="background:#dcfce7;color:#166534;border-radius:999px;padding:.1rem .5rem;font-size:.75rem;font-weight:700;vertical-align:middle">{{len .Matches}}</span>{{end}}</h2>
{{if .Matches}}
<table>
<tr><th>Time</th><th>Sport</th><th>Host</th><th>Players</th></tr>
{{range .Matches}}
<tr>
  <td class="mono" style="white-space:nowrap">{{matchTime .StartedAt}}</td>
  <td><span class="tag">{{.SportName}}</span></td>
  <td>{{if .HostPNID}}<strong>@{{.HostPNID}}</strong>{{else}}<span style="color:#aaa">—</span>{{end}}</td>
  <td>{{range .Players}}{{.}} {{end}}</td>
</tr>
{{end}}
</table>
{{else}}
<p style="color:#aaa;font-size:.9rem;margin-top:0">No matches recorded yet.</p>
{{end}}

<script>
var countdown = 15;
function tick() {
  countdown--;
  if (countdown <= 0) { location.reload(); return; }
  document.getElementById('refresh-label').textContent = 'refreshes in ' + countdown + 's';
}
setInterval(tick, 1000);
</script>
</body>
</html>`))

var wscNATTmpl = template.Must(template.New("wsc-nat").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>NAT Types — Wii Sports Club</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
body{font-family:system-ui,sans-serif;max-width:780px;margin:2rem auto;padding:0 1rem;color:#222;line-height:1.6}
h1{font-size:1.4rem;margin-bottom:.25rem}
h2{font-size:1.05rem;margin-top:2rem;margin-bottom:.5rem}
a{color:#2563eb}
p{margin:.5rem 0 1rem}
.cards{display:flex;gap:1rem;flex-wrap:wrap;margin-bottom:2rem}
.card{flex:1;min-width:180px;border:1px solid #e4e4e7;border-radius:8px;padding:1rem 1.25rem}
.card h3{font-size:.95rem;margin:0 0 .4rem}
.card p{font-size:.875rem;color:#555;margin:0}
.badge{display:inline-block;padding:.2rem .55rem;border-radius:4px;font-size:.8rem;font-weight:600;margin-bottom:.5rem}
.open{background:#dcfce7;color:#166534}
.mod{background:#fef9c3;color:#854d0e}
.strict{background:#fee2e2;color:#991b1b}
table{width:100%;border-collapse:collapse;font-size:.9rem;margin-bottom:2rem}
th{text-align:center;padding:.5rem .75rem;border:1px solid #e4e4e7;background:#f8fafc;font-weight:600;color:#444}
td{text-align:center;padding:.5rem .75rem;border:1px solid #e4e4e7}
td:first-child{text-align:left;font-weight:600}
.yes{color:#166534;font-weight:700}
.no{color:#991b1b;font-weight:700}
.maybe{color:#854d0e;font-weight:700}
.tip{background:#eff6ff;border:1px solid #bfdbfe;border-radius:6px;padding:.75rem 1rem;font-size:.875rem;color:#1e40af;margin-bottom:1rem}
</style>
</head>
<body>
<p><a href="/wsc-public/">← WSC Dashboard</a></p>
<h1>NAT Types — Wii Sports Club</h1>
<p>NAT (Network Address Translation) determines whether two players can establish a direct peer-to-peer connection for online play. The server matches players based on their NAT type to avoid failed connections.</p>

<h2>Your NAT Type</h2>
<div class="cards">
  <div class="card">
    <span class="badge open">Open</span>
    <h3>NAT Type 1 — Open</h3>
    <p>Your router forwards traffic freely. You can connect with anyone. Best experience for online play.</p>
  </div>
  <div class="card">
    <span class="badge mod">Moderate</span>
    <h3>NAT Type 2 — Moderate</h3>
    <p>Your router filters by IP address. You can connect with Open and Moderate players. Works well for most matches.</p>
  </div>
  <div class="card">
    <span class="badge strict">Strict</span>
    <h3>NAT Type 3 — Strict</h3>
    <p>Your router uses symmetric NAT. Port numbers change with each connection, making hole-punching unreliable. You can only be matched with other Strict players, and even then the connection may fail.</p>
  </div>
</div>

<h2>Compatibility Table</h2>
<table>
<tr>
  <th></th>
  <th><span class="badge open">Open</span></th>
  <th><span class="badge mod">Moderate</span></th>
  <th><span class="badge strict">Strict</span></th>
</tr>
<tr>
  <td><span class="badge open">Open</span></td>
  <td class="yes">✓ Works</td>
  <td class="yes">✓ Works</td>
  <td class="no">✗ Blocked</td>
</tr>
<tr>
  <td><span class="badge mod">Moderate</span></td>
  <td class="yes">✓ Works</td>
  <td class="yes">✓ Works</td>
  <td class="no">✗ Blocked</td>
</tr>
<tr>
  <td><span class="badge strict">Strict</span></td>
  <td class="no">✗ Blocked</td>
  <td class="no">✗ Blocked</td>
  <td class="maybe">~ May fail</td>
</tr>
</table>
<p style="font-size:.85rem;color:#666;margin-top:-.5rem">Strict↔Strict matches are allowed by the server but peer-to-peer connection may still fail depending on each player's router configuration.</p>

<h2>How to Improve Your NAT Type</h2>
<div class="tip">Enable UPnP on your router, or set up a port forward for UDP port 60015 pointed at your Wii U's local IP address. This typically changes a Strict NAT to Moderate or Open.</div>
<p>Steps vary by router model — search for "<em>your router model</em> UPnP" or "<em>your router model</em> port forwarding" for instructions.</p>
</body>
</html>`))

func lookupMiiNames(pids []int64) map[int64]string {
	if len(pids) == 0 {
		return map[int64]string{}
	}
	placeholders := make([]string, len(pids))
	args := make([]interface{}, len(pids))
	for i, pid := range pids {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = pid
	}
	query := fmt.Sprintf(`SELECT pid, mii_name FROM mii_names WHERE pid IN (%s)`,
		strings.Join(placeholders, ","))
	rows, err := db.Query(query, args...)
	result := map[int64]string{}
	if err != nil {
		return result
	}
	defer rows.Close()
	for rows.Next() {
		var pid int64
		var name string
		rows.Scan(&pid, &name)
		result[pid] = name
	}
	return result
}

func apiWSCPlayers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	data := fetchWSCStatus()

	// Collect all PIDs for Mii name lookup
	pidSet := map[int64]struct{}{}
	for _, p := range data.Players {
		pidSet[p.PID] = struct{}{}
	}
	for _, g := range data.Gatherings {
		for _, p := range g.Players {
			pidSet[p.PID] = struct{}{}
		}
	}
	pids := make([]int64, 0, len(pidSet))
	for pid := range pidSet {
		pids = append(pids, pid)
	}
	miiNames := lookupMiiNames(pids)

	type PlayerJSON struct {
		PID     int64  `json:"pid"`
		PNID    string `json:"pnid"`
		MiiName string `json:"mii_name,omitempty"`
	}
	type GatheringJSON struct {
		GID         int64        `json:"gid"`
		SportName   string       `json:"sport_name"`
		HostPNID    string       `json:"host_pnid"`
		PlayerCount int64        `json:"player_count"`
		MaxPlayers  int64        `json:"max_players"`
		Open        bool         `json:"open"`
		Players     []PlayerJSON `json:"players"`
	}
	type ResponseJSON struct {
		ServerUp   bool            `json:"server_up"`
		Players    []PlayerJSON    `json:"players"`
		Gatherings []GatheringJSON `json:"gatherings"`
	}

	resp := ResponseJSON{ServerUp: data.ServerUp}
	for _, p := range data.Players {
		resp.Players = append(resp.Players, PlayerJSON{
			PID:     p.PID,
			PNID:    p.PNID,
			MiiName: miiNames[p.PID],
		})
	}
	for _, g := range data.Gatherings {
		gj := GatheringJSON{
			GID:         g.GID,
			SportName:   g.SportName,
			HostPNID:    g.HostPNID,
			PlayerCount: g.PlayerCount,
			MaxPlayers:  g.MaxPlayers,
			Open:        g.Open,
		}
		for _, p := range g.Players {
			gj.Players = append(gj.Players, PlayerJSON{
				PID:     p.PID,
				PNID:    p.PNID,
				MiiName: miiNames[p.PID],
			})
		}
		resp.Gatherings = append(resp.Gatherings, gj)
	}
	json.NewEncoder(w).Encode(resp)
}

const wscOverlayHTML = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>WSC Overlay</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{background:transparent;font-family:'Segoe UI',Arial,sans-serif;padding:14px}
#overlay{display:inline-block;min-width:200px}
.card{background:rgba(0,0,0,0.72);border-radius:10px;padding:10px 14px;color:#fff;backdrop-filter:blur(4px)}
.sport{font-size:12px;font-weight:bold;letter-spacing:.5px;color:#7ecfff;margin-bottom:8px;display:flex;align-items:center;gap:6px}
.badge{font-size:10px;border-radius:3px;padding:1px 6px;font-weight:normal}
.open{background:#4caf50}.match{background:#e53935}
.player{display:flex;align-items:center;gap:8px;padding:4px 0;border-bottom:1px solid rgba(255,255,255,.08)}
.player:last-child{border-bottom:none}
.mii{width:36px;height:36px;border-radius:50%;background:rgba(255,255,255,.12);flex-shrink:0;overflow:hidden}
.mii img{width:100%;height:100%}
.name{font-size:14px;font-weight:bold;line-height:1.2}
.you{color:#7ecfff}
.pnid{font-size:11px;color:rgba(255,255,255,.55)}
.offline{color:rgba(255,255,255,.5);font-size:13px;padding:4px 0}
.setup{background:rgba(0,0,0,.8);border-radius:10px;padding:16px;color:#fff;max-width:340px;font-size:13px;line-height:1.6}
.setup h2{font-size:15px;margin-bottom:8px;color:#7ecfff}
.setup code{background:rgba(255,255,255,.15);border-radius:4px;padding:2px 6px;font-family:monospace;font-size:12px;word-break:break-all}
.setup p{margin-top:8px}
</style>
</head>
<body>
<div id="overlay"></div>
<script>
var params=new URLSearchParams(location.search);
var PNID=(params.get('pnid')||'').trim();

if(!PNID){
  document.getElementById('overlay').innerHTML='<div class="setup"><h2>WSC Stream Overlay</h2><p>Add your PNID to the URL to use this overlay:</p><p><code>'+location.href+(location.search?'&':'?')+'pnid=YourPNID</code></p><p>Then add this URL as a Browser Source in OBS with <strong>transparent background</strong>.</p></div>';
}else{
  function render(data){
    var el=document.getElementById('overlay');
    if(!data||!data.server_up){el.innerHTML='<div class="card"><span class="offline">WSC server offline</span></div>';return;}
    var g=null;
    (data.gatherings||[]).forEach(function(gg){
      (gg.players||[]).forEach(function(p){if((p.pnid||'').toLowerCase()===PNID.toLowerCase())g=gg;});
    });
    if(!g){el.innerHTML='<div class="card"><span class="offline">Not in a session</span></div>';return;}
    var badge=g.open?'<span class="badge open">Open</span>':'<span class="badge match">In Match</span>';
    var rows=(g.players||[]).map(function(p){
      var you=(p.pnid||'').toLowerCase()===PNID.toLowerCase();
      var nm=p.mii_name||p.pnid||('PID:'+p.pid);
      var mii='<div class="mii"><img src="https://sos-de-fra-1.exo.io/olv-data/mii/'+p.pid+'/normal_face.png" onerror="this.style.display=\'none\'"></div>';
      return '<div class="player">'+mii+'<div><div class="name'+(you?' you':'')+'">'+(you?'&#9654; ':'')+nm+'</div>'+(p.pnid?'<div class="pnid">@'+p.pnid+'</div>':'')+'</div></div>';
    }).join('');
    el.innerHTML='<div class="card"><div class="sport">'+g.sport_name+badge+'</div>'+rows+'</div>';
  }
  function refresh(){fetch('/wsc-public/api/players').then(function(r){return r.json();}).then(render).catch(function(){});}
  refresh();
  setInterval(refresh,5000);
}
</script>
</body>
</html>`

func apiWSCOverlay(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, wscOverlayHTML)
}

func wscNATInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	wscNATTmpl.Execute(w, nil)
}

func adminWSC(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/wsc-public/" {
		http.NotFound(w, r)
		return
	}
	data := fetchWSCStatus()
	w.Header().Set("Content-Type", "text/html")
	wscTmpl.Execute(w, data)
}

// --- Admin client cert rotation ---

const (
	certRotationDays = 14
	certValidDays    = 28 // keeps newest 2 valid at all times
	caCertPath       = "/var/ca/netcup-server/client-ca.pem"
	caKeyPath        = "/var/ca/netcup-server/client-ca.key"
)

type AdminCert struct {
	ID        int
	CreatedAt time.Time
	CertPEM   string
	KeyPEM    string
}

func latestAdminCert() (*AdminCert, error) {
	var c AdminCert
	err := db.QueryRow(`SELECT id, created_at, cert_pem, key_pem FROM admin_certs ORDER BY created_at DESC LIMIT 1`).
		Scan(&c.ID, &c.CreatedAt, &c.CertPEM, &c.KeyPEM)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &c, err
}

// daysUntilRotation returns how many days remain before the next scheduled rotation.
// Negative means overdue.
func daysUntilRotation(cert *AdminCert) int {
	if cert == nil {
		return 0
	}
	next := cert.CreatedAt.Add(certRotationDays * 24 * time.Hour)
	return int(math.Ceil(time.Until(next).Hours() / 24))
}

func generateClientCert() (certPEM, keyPEM []byte, err error) {
	keyF, err := os.CreateTemp("", "inkay-key-*.pem")
	if err != nil {
		return nil, nil, err
	}
	defer os.Remove(keyF.Name())
	keyF.Close()

	csrF, err := os.CreateTemp("", "inkay-csr-*.pem")
	if err != nil {
		return nil, nil, err
	}
	defer os.Remove(csrF.Name())
	csrF.Close()

	certF, err := os.CreateTemp("", "inkay-cert-*.pem")
	if err != nil {
		return nil, nil, err
	}
	defer os.Remove(certF.Name())
	certF.Close()

	// Random serial avoids needing write access to the CA directory for a serial file.
	serialOut, err := exec.Command("openssl", "rand", "-hex", "16").Output()
	if err != nil {
		return nil, nil, fmt.Errorf("rand serial: %w", err)
	}
	serial := "0x" + strings.TrimSpace(string(serialOut))

	// Verify CA key is readable before proceeding.
	if _, err = os.Open(caKeyPath); err != nil {
		return nil, nil, fmt.Errorf("CA key not readable (%s) — run: sudo chown root:nico %s && sudo chmod 640 %s", caKeyPath, caKeyPath, caKeyPath)
	}

	steps := [][]string{
		{"openssl", "genrsa", "-out", keyF.Name(), "2048"},
		{"openssl", "req", "-new", "-key", keyF.Name(), "-out", csrF.Name(),
			"-subj", "/CN=Inkay Admin/O=Revivetendo"},
		{"openssl", "x509", "-req",
			"-days", strconv.Itoa(certValidDays),
			"-in", csrF.Name(),
			"-CA", caCertPath, "-CAkey", caKeyPath,
			"-set_serial", serial,
			"-out", certF.Name()},
	}
	for _, args := range steps {
		if out, err2 := exec.Command(args[0], args[1:]...).CombinedOutput(); err2 != nil {
			return nil, nil, fmt.Errorf("openssl %s: %w\n%s", args[1], err2, out)
		}
	}

	certPEM, err = os.ReadFile(certF.Name())
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err = os.ReadFile(keyF.Name())
	return certPEM, keyPEM, err
}

func certToP12(certPEM, keyPEM []byte) ([]byte, error) {
	certF, err := os.CreateTemp("", "inkay-cert-*.pem")
	if err != nil {
		return nil, err
	}
	defer os.Remove(certF.Name())
	certF.Write(certPEM)
	certF.Close()

	keyF, err := os.CreateTemp("", "inkay-key-*.pem")
	if err != nil {
		return nil, err
	}
	defer os.Remove(keyF.Name())
	keyF.Write(keyPEM)
	keyF.Close()

	args := []string{"pkcs12", "-export",
		"-in", certF.Name(), "-inkey", keyF.Name(),
		"-certfile", caCertPath,
		"-passout", "pass:",
		"-legacy",
	}
	out, err := exec.Command("openssl", args...).Output()
	if err != nil {
		// OpenSSL < 3.0 doesn't have -legacy
		out, err = exec.Command("openssl", append(args[:len(args)-1])...).Output()
	}
	return out, err
}

func rotateCert() error {
	log.Printf("rotating admin client cert")
	certPEM, keyPEM, err := generateClientCert()
	if err != nil {
		return fmt.Errorf("generate: %w", err)
	}
	if _, err = db.Exec(`INSERT INTO admin_certs (cert_pem, key_pem) VALUES ($1, $2)`, string(certPEM), string(keyPEM)); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	// Keep only the newest certRotationDays/certValidDays window worth of certs.
	db.Exec(`DELETE FROM admin_certs WHERE id NOT IN (SELECT id FROM admin_certs ORDER BY created_at DESC LIMIT $1)`,
		certValidDays/certRotationDays+1)
	log.Printf("admin client cert rotated")
	return nil
}

func certRotationLoop() {
	for {
		time.Sleep(time.Hour)
		cert, err := latestAdminCert()
		if err != nil {
			log.Printf("cert check: %v", err)
			continue
		}
		if cert != nil && time.Since(cert.CreatedAt) < certRotationDays*24*time.Hour {
			continue
		}
		if err := rotateCert(); err != nil {
			log.Printf("cert rotation failed: %v", err)
		}
	}
}

// --- Landing page ---

var landingTmpl = template.Must(template.New("landing").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Pretendo Bridge</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta property="og:type" content="website">
<meta property="og:site_name" content="Inkay Relay">
<meta property="og:title" content="Pretendo Bridge">
<meta property="og:description" content="Private Wii U relay — play Mario Kart 8 and WiiU Chat on a custom Pretendo server.">
<meta property="og:image" content="` + siteHost + `/inkay/stats/card.svg">
<meta property="og:image:width" content="1200">
<meta property="og:image:height" content="630">
<meta property="og:url" content="` + siteHost + `/">
<meta name="twitter:card" content="summary_large_image">
<meta name="twitter:title" content="Pretendo Bridge">
<meta name="twitter:description" content="Private Wii U relay — play Mario Kart 8 and WiiU Chat on a custom Pretendo server.">
<meta name="twitter:image" content="` + siteHost + `/inkay/stats/card.svg">
<style>
body{font-family:system-ui,sans-serif;max-width:640px;margin:4rem auto;padding:0 1rem;color:#222}
h1{font-size:1.6rem;margin-bottom:.25rem}
p.sub{color:#666;margin-top:0;margin-bottom:2.5rem}
.cards{display:flex;flex-direction:column;gap:1rem}
.card{border:1px solid #e4e4e7;border-radius:8px;padding:1.25rem 1.5rem;text-decoration:none;color:inherit;display:block}
.card:hover{background:#f9f9fb;border-color:#c4c4c7}
.card h2{font-size:1rem;margin:0 0 .3rem}
.card p{font-size:.875rem;color:#555;margin:0}
.api-list{margin-top:1.5rem;border-top:1px solid #e4e4e7;padding-top:1.5rem}
.api-list h2{font-size:1rem;margin-bottom:.75rem}
table{width:100%;border-collapse:collapse;font-size:.85rem}
th{text-align:left;color:#666;font-weight:600;padding:.4rem .5rem;border-bottom:1px solid #e4e4e7}
td{padding:.4rem .5rem;border-bottom:1px solid #f4f4f5;font-family:monospace}
td:last-child{font-family:system-ui,sans-serif;color:#555}
tr:last-child td{border-bottom:none}
</style>
</head>
<body>
<h1>Pretendo Bridge</h1>
<p class="sub">netcup-server.nicochristmann.net</p>
<div class="cards">
  <a class="card" href="/inkay/stats/">
    <h2>Stats</h2>
    <p>Connected PIDs, request history, active redirects</p>
  </a>
  <a class="card" href="/inkay/my/">
    <h2>My Status</h2>
    <p>Your online state and friends list — sign in with your web password</p>
  </a>
  <a class="card" href="/inkay/my/discord">
    <h2>Discord Link</h2>
    <p>Link your PNID to Discord for WiiU Chat call notifications</p>
  </a>
  <a class="card" href="/wsc-public/">
    <h2>WSC Status and Players/Sessions</h2>
    <p>Live Wii Sports Club players and active matchmaking sessions</p>
  </a>
  <a class="card" href="/inkay/admin/">
    <h2>Admin</h2>
    <p>Manage redirects — requires client certificate</p>
  </a>
</div>
<div class="api-list">
  <h2>API</h2>
  <table>
    <tr><th>Endpoint</th><th>Description</th></tr>
    <tr><td>GET /inkay/api/stats</td><td>Stats as JSON</td></tr>
    <tr><td>GET /inkay/api/redirects</td><td>Active redirects as JSON</td></tr>
  </table>
</div>
</body>
</html>`))

func landingUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	landingTmpl.Execute(w, nil)
}

// --- Stats UI ---

const siteHost = "https://netcup-server.nicochristmann.net"

var statsTmpl = template.Must(template.New("stats").Funcs(tmplFuncs).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Inkay Relay — Stats</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta property="og:type" content="website">
<meta property="og:site_name" content="Inkay Relay">
<meta property="og:title" content="Pretendo Bridge — Live Stats">
<meta property="og:description" content="{{.Stats.TotalPIDs}} players · {{.Stats.Requests24h}} requests today · {{.Stats.ActiveRedirects}} active servers">
<meta property="og:image" content="{{.Host}}/inkay/stats/card.svg">
<meta property="og:image:width" content="1200">
<meta property="og:image:height" content="630">
<meta property="og:url" content="{{.Host}}/inkay/stats/">
<meta name="twitter:card" content="summary_large_image">
<meta name="twitter:title" content="Pretendo Bridge — Live Stats">
<meta name="twitter:description" content="{{.Stats.TotalPIDs}} players · {{.Stats.Requests24h}} requests today">
<meta name="twitter:image" content="{{.Host}}/inkay/stats/card.svg">
<style>
body{font-family:system-ui,sans-serif;max-width:900px;margin:2rem auto;padding:0 1rem;color:#222}
h1{font-size:1.4rem;margin-bottom:1.5rem}
.cards{display:flex;gap:1rem;flex-wrap:wrap;margin-bottom:2rem}
.card{background:#f4f4f5;border-radius:8px;padding:1rem 1.5rem;flex:1;min-width:140px}
.card .num{font-size:2rem;font-weight:700}
.card .label{font-size:.8rem;color:#666;margin-top:.2rem}
table{width:100%;border-collapse:collapse;font-size:.9rem}
th{text-align:left;border-bottom:2px solid #e4e4e7;padding:.5rem .75rem;color:#666;font-weight:600}
td{padding:.5rem .75rem;border-bottom:1px solid #f0f0f0}
tr:last-child td{border-bottom:none}
h2{font-size:1rem;margin-bottom:.75rem;margin-top:2rem}
.tag{display:inline-block;padding:.15rem .5rem;border-radius:4px;font-size:.75rem;background:#e0e7ff;color:#3730a3;font-family:monospace}
</style>
</head>
<body>
<h1>Inkay Relay</h1>
<div class="cards">
  <div class="card"><div class="num">{{.Stats.TotalPIDs}}</div><div class="label">Unique players</div></div>
  <div class="card"><div class="num">{{.Stats.TotalRequests}}</div><div class="label">Total requests</div></div>
  <div class="card"><div class="num">{{.Stats.Requests24h}}</div><div class="label">Last 24h</div></div>
  <div class="card"><div class="num">{{.Stats.ActiveRedirects}}</div><div class="label">Active servers</div></div>
</div>

<h2>Approved players</h2>
<table>
<tr><th>PNID</th><th>Name</th><th>Game</th><th>Since</th></tr>
{{range .Users}}
<tr>
  <td>{{if .PNID}}<strong>{{.PNID}}</strong>{{else}}<span style="font-family:monospace;color:#999;font-size:.85rem">{{.PID}}</span>{{end}}</td>
  <td>{{if .Note}}{{.Note}}{{else}}<span style="color:#aaa">—</span>{{end}}</td>
  <td><span class="tag">{{gameTitleFull .GameServerID}}</span></td>
  <td style="font-size:.85rem;color:#666">{{.CreatedAt.Format "2006-01-02"}}</td>
</tr>
{{else}}<tr><td colspan="4" style="color:#aaa">No approved players yet</td></tr>
{{end}}
</table>

<h2>Banned players</h2>
<table>
<tr><th>PNID</th><th>Reason</th><th>Banned</th></tr>
{{range .Bans}}
<tr>
  <td>{{if .PNID}}<strong>{{.PNID}}</strong>{{else}}<span style="font-family:monospace;color:#999;font-size:.85rem">{{.PID}}</span>{{end}}</td>
  <td>{{if .Reason}}{{.Reason}}{{else}}<span style="color:#aaa">—</span>{{end}}</td>
  <td style="font-size:.85rem;color:#666">{{.CreatedAt.Format "2006-01-02"}}</td>
</tr>
{{else}}<tr><td colspan="3" style="color:#aaa">No banned players</td></tr>
{{end}}
</table>

<h2>Pending access requests</h2>
<table>
<tr><th>PNID</th><th>Game</th><th>Attempts</th><th>Last seen</th></tr>
{{range .Pending}}
<tr>
  <td>{{if .PNID}}<strong>{{.PNID}}</strong>{{else}}<span style="font-family:monospace;color:#999;font-size:.85rem">{{.PID}}</span>{{end}}</td>
  <td><span class="tag">{{gameTitleFull .GameServerID}}</span></td>
  <td style="color:#666">{{.Attempts}}</td>
  <td style="font-size:.85rem;color:#666">{{.LastSeen.Format "2006-01-02 15:04 UTC"}}</td>
</tr>
{{else}}<tr><td colspan="4" style="color:#aaa">No pending requests</td></tr>
{{end}}
</table>

<h2>Recent requests</h2>
<table>
<tr><th>PNID</th><th>Game</th><th>Time</th></tr>
{{range .Recent}}
<tr>
  <td>{{if .PNID}}<strong>{{.PNID}}</strong>{{else}}<span style="font-family:monospace;color:#999;font-size:.85rem">{{.PID}}</span>{{end}}</td>
  <td><span class="tag">{{gameTitleFull .GameServerID}}</span></td>
  <td style="font-size:.85rem;color:#666">{{.RequestedAt.Format "2006-01-02 15:04:05 UTC"}}</td>
</tr>
{{else}}<tr><td colspan="3" style="color:#aaa">No requests yet</td></tr>
{{end}}
</table>
</body>
</html>`))

func publicUsers() []UserAccess {
	rows, err := db.Query(`
		SELECT u.pid, COALESCE(p.pnid, ''), u.game_server_id, COALESCE(u.note, ''), u.created_at
		FROM user_access u
		LEFT JOIN pnid_cache p ON p.pid = u.pid
		ORDER BY u.game_server_id, u.created_at`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var list []UserAccess
	for rows.Next() {
		var u UserAccess
		rows.Scan(&u.PID, &u.PNID, &u.GameServerID, &u.Note, &u.CreatedAt)
		list = append(list, u)
	}
	return list
}

func statsUI(w http.ResponseWriter, r *http.Request) {
	data := struct {
		Stats   Stats
		Users   []UserAccess
		Bans    []BannedUser
		Pending []ReviewEntry
		Recent  []RecentRequest
		Host    string
	}{collectStats(), publicUsers(), allBans(), pendingReviews(), recentRequests(20), siteHost}
	w.Header().Set("Content-Type", "text/html")
	statsTmpl.Execute(w, data)
}

var cardTmpl = template.Must(template.New("card").Parse(`<svg xmlns="http://www.w3.org/2000/svg" width="1200" height="630" viewBox="0 0 1200 630">
  <defs>
    <linearGradient id="bg" x1="0" y1="0" x2="1" y2="1">
      <stop offset="0%" stop-color="#0f172a"/>
      <stop offset="100%" stop-color="#1e1b4b"/>
    </linearGradient>
    <linearGradient id="accent" x1="0" y1="0" x2="1" y2="0">
      <stop offset="0%" stop-color="#6366f1"/>
      <stop offset="100%" stop-color="#8b5cf6"/>
    </linearGradient>
  </defs>

  <!-- background -->
  <rect width="1200" height="630" fill="url(#bg)"/>

  <!-- top accent bar -->
  <rect x="0" y="0" width="1200" height="6" fill="url(#accent)"/>

  <!-- logo / title -->
  <text x="80" y="120" font-family="system-ui,sans-serif" font-size="52" font-weight="700" fill="#f8fafc">Pretendo Bridge</text>
  <text x="80" y="168" font-family="system-ui,sans-serif" font-size="24" fill="#94a3b8">Private Wii U relay — live stats</text>

  <!-- divider -->
  <rect x="80" y="200" width="1040" height="1" fill="#334155"/>

  <!-- stat cards -->
  <!-- Players -->
  <rect x="80" y="240" width="230" height="160" rx="12" fill="#1e293b"/>
  <text x="195" y="322" font-family="system-ui,sans-serif" font-size="58" font-weight="700" fill="#f8fafc" text-anchor="middle">{{.TotalPIDs}}</text>
  <text x="195" y="368" font-family="system-ui,sans-serif" font-size="20" fill="#94a3b8" text-anchor="middle">players</text>

  <!-- Requests today -->
  <rect x="330" y="240" width="230" height="160" rx="12" fill="#1e293b"/>
  <text x="445" y="322" font-family="system-ui,sans-serif" font-size="58" font-weight="700" fill="#f8fafc" text-anchor="middle">{{.Requests24h}}</text>
  <text x="445" y="368" font-family="system-ui,sans-serif" font-size="20" fill="#94a3b8" text-anchor="middle">requests today</text>

  <!-- Total requests -->
  <rect x="580" y="240" width="230" height="160" rx="12" fill="#1e293b"/>
  <text x="695" y="322" font-family="system-ui,sans-serif" font-size="58" font-weight="700" fill="#f8fafc" text-anchor="middle">{{.TotalRequests}}</text>
  <text x="695" y="368" font-family="system-ui,sans-serif" font-size="20" fill="#94a3b8" text-anchor="middle">total</text>

  <!-- Active servers -->
  <rect x="830" y="240" width="230" height="160" rx="12" fill="#1e293b"/>
  <text x="945" y="322" font-family="system-ui,sans-serif" font-size="58" font-weight="700" fill="#a78bfa" text-anchor="middle">{{.ActiveRedirects}}</text>
  <text x="945" y="368" font-family="system-ui,sans-serif" font-size="20" fill="#94a3b8" text-anchor="middle">active servers</text>

  <!-- footer -->
  <text x="80" y="560" font-family="system-ui,sans-serif" font-size="18" fill="#475569">netcup-server.nicochristmann.net</text>
  <text x="1120" y="560" font-family="system-ui,sans-serif" font-size="18" fill="#475569" text-anchor="end">{{.UpdatedAt}}</text>
</svg>`))

func statsCard(w http.ResponseWriter, r *http.Request) {
	s := collectStats()
	data := struct {
		TotalPIDs      int
		Requests24h    int
		TotalRequests  int
		ActiveRedirects int
		UpdatedAt      string
	}{
		s.TotalPIDs,
		s.Requests24h,
		s.TotalRequests,
		s.ActiveRedirects,
		time.Now().UTC().Format("2006-01-02 15:04 UTC"),
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "no-cache, max-age=60")
	cardTmpl.Execute(w, data)
}

// --- Admin UI ---

var adminTmpl = template.Must(template.New("admin").Funcs(tmplFuncs).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Inkay Relay — Admin</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
body{font-family:system-ui,sans-serif;max-width:1100px;margin:2rem auto;padding:0 1rem;color:#222}
h1{font-size:1.4rem}
h2{font-size:1.1rem;margin-top:2rem}
a{color:#2563eb}
table{width:100%;border-collapse:collapse;font-size:.9rem;margin-bottom:2rem}
th{text-align:left;border-bottom:2px solid #e4e4e7;padding:.5rem .75rem;color:#666;font-weight:600}
td{padding:.5rem .75rem;border-bottom:1px solid #f0f0f0;vertical-align:middle}
tr:last-child td{border-bottom:none}
.badge{display:inline-block;padding:.2rem .5rem;border-radius:4px;font-size:.75rem;font-weight:600}
.on{background:#dcfce7;color:#166534}.off{background:#fee2e2;color:#991b1b}
.tag{display:inline-block;padding:.15rem .4rem;border-radius:4px;font-size:.75rem;background:#e0e7ff;color:#3730a3}
button,input,select{font:inherit}
button{cursor:pointer;border:none;border-radius:4px;padding:.3rem .7rem;font-size:.85rem}
.btn-del{background:#fee2e2;color:#991b1b}
.btn-tog{background:#f4f4f5;color:#444}
.btn-link{background:#eff6ff;color:#1d4ed8;text-decoration:none;display:inline-block;padding:.3rem .7rem;border-radius:4px;font-size:.85rem}
.btn-ban{background:#fff7ed;color:#9a3412;text-decoration:none;display:inline-block;padding:.3rem .7rem;border-radius:4px;font-size:.85rem}
fieldset{border:1px solid #e4e4e7;border-radius:8px;padding:1rem 1.25rem;margin-bottom:2rem}
legend{font-weight:600;padding:0 .4rem}
.row{display:flex;gap:.75rem;flex-wrap:wrap;align-items:flex-end;margin-bottom:.75rem}
.field{display:flex;flex-direction:column;gap:.3rem;flex:1;min-width:160px}
label{font-size:.8rem;color:#666;font-weight:600}
input[type=text],select{border:1px solid #d1d5db;border-radius:4px;padding:.4rem .6rem;width:100%;box-sizing:border-box}
.submit{background:#2563eb;color:#fff;padding:.4rem 1rem}
.dl{display:inline-block;background:#f4f4f5;border:1px solid #d1d5db;border-radius:4px;padding:.3rem .8rem;font-size:.85rem;color:#222;text-decoration:none}
.msg{background:#dcfce7;border:1px solid #bbf7d0;color:#166534;padding:.5rem 1rem;border-radius:6px;margin-bottom:1rem;font-size:.9rem}
</style>
</head>
<body>
<h1>Inkay Relay Admin</h1>
{{if .Msg}}<div class="msg">{{.Msg}}</div>{{end}}
<p style="margin-bottom:1.5rem">
  <a href="/inkay/stats/" target="_blank">← Public stats</a> &nbsp;|&nbsp;
  <a class="dl" href="/inkay/admin/client-cert.p12" download="inkay-admin.p12">⬇ Download client cert</a> &nbsp;|&nbsp;
  <a href="/inkay/admin/review/">🕐 Review queue{{if .ReviewCount}} <span style="background:#ef4444;color:#fff;border-radius:999px;padding:.1rem .45rem;font-size:.75rem;font-weight:700">{{.ReviewCount}}</span>{{end}}</a> &nbsp;|&nbsp;
  <a href="/inkay/admin/bans/">🚫 Banned users</a> &nbsp;|&nbsp;
  <a href="/wsc-public/">🎳 WSC</a>
</p>
<div style="display:flex;align-items:center;gap:1rem;background:#f8fafc;border:1px solid #e2e8f0;border-radius:6px;padding:.6rem 1rem;margin-bottom:1.5rem;font-size:.875rem">
  <span>🔑 Client cert: <strong>{{.CertAge}}</strong>
  {{- if gt .DaysUntilRotation 0}} · rotates in <strong>{{.DaysUntilRotation}} day{{if ne .DaysUntilRotation 1}}s{{end}}</strong>
  {{- else}} · <span style="color:#dc2626">rotation overdue</span>{{end}}</span>
  <form method="post" action="/inkay/admin/certs/rotate" style="margin:0">
    <button class="btn-tog" type="submit" style="font-size:.8rem">Rotate now</button>
  </form>
</div>

<h2>Currently Online{{if .OnlineUsers}} <span style="background:#dcfce7;color:#166534;border-radius:999px;padding:.1rem .5rem;font-size:.75rem;font-weight:700;vertical-align:middle">{{len .OnlineUsers}}</span>{{end}}</h2>
{{if .OnlineUsers}}
<table>
<tr><th>PNID</th><th>Game</th><th>Game Server</th></tr>
{{range .OnlineUsers}}
<tr>
  <td>{{if .PNID}}<strong>@{{.PNID}}</strong>{{else}}<span style="font-family:monospace;font-size:.85rem">{{.PID}}</span>{{end}}</td>
  <td>{{with resolveGameName .TitleID .GameServerHex}}{{.}}{{else}}<span style="color:#aaa">in menu</span>{{end}}</td>
  <td>{{if .GameServerHex}}{{if .GameName}}{{.GameName}} <span style="font-family:monospace;font-size:.75rem;color:#666">({{.GameServerHex}})</span>{{else}}<span style="font-family:monospace;font-size:.85rem">{{.GameServerHex}}</span>{{end}}{{else}}<span style="color:#aaa">—</span>{{end}}</td>
</tr>
{{end}}
</table>
{{else}}
<p style="color:#aaa;font-size:.9rem;margin-top:0">No users currently online.</p>
{{end}}

<h2 style="margin-top:0">Redirects</h2>
<table>
<tr><th>ID</th><th>Type</th><th>From</th><th>To</th><th>Port</th><th>Game ID</th><th>Address</th><th>Status</th><th></th></tr>
{{range .Redirects}}
<tr>
  <td>{{.ID}}</td>
  <td><span class="tag">{{.Type}}</span></td>
  <td>{{.FromHost}}</td>
  <td>{{.ToHost}}</td>
  <td style="font-family:monospace;font-size:.8rem">{{if .Port}}{{.Port}}{{else}}—{{end}}</td>
  <td style="font-size:.8rem">{{if .GameServerID}}<span title="{{.GameServerID}}">{{gameTitle .GameServerID}}</span>{{else}}—{{end}}</td>
  <td style="font-family:monospace;font-size:.8rem">{{if .Address}}{{.Address}}{{else}}—{{end}}</td>
  <td><span class="badge {{if .Enabled}}on{{else}}off{{end}}">{{if .Enabled}}enabled{{else}}disabled{{end}}</span></td>
  <td style="white-space:nowrap">
    <form method="post" action="/inkay/admin/toggle" style="display:inline">
      <input type="hidden" name="id" value="{{.ID}}">
      <button class="btn-tog" type="submit">{{if .Enabled}}Disable{{else}}Enable{{end}}</button>
    </form>
    {{if .GameServerID}}
    <a class="btn-link" href="/inkay/admin/users/?game={{.GameServerID}}">Users</a>
    {{end}}
    <form method="post" action="/inkay/admin/delete" style="display:inline" onsubmit="return confirm('Delete redirect {{.ID}}?')">
      <input type="hidden" name="id" value="{{.ID}}">
      <button class="btn-del" type="submit">Delete</button>
    </form>
  </td>
</tr>
{{else}}<tr><td colspan="10" style="color:#aaa">No redirects yet</td></tr>
{{end}}
</table>

<fieldset>
<legend>Add redirect</legend>
<form method="post" action="/inkay/admin/add">
  <div class="row">
    <div class="field" style="max-width:120px">
      <label>Type</label>
      <select name="type">
        <option value="iosu">iosu</option>
        <option value="dns">dns</option>
      </select>
    </div>
    <div class="field" style="max-width:160px">
      <label>IOSU address (hex)</label>
      <input type="text" name="address" placeholder="E31930D4">
    </div>
    <div class="field" style="max-width:140px">
      <label>Game server ID</label>
      <input type="text" name="game_server_id" placeholder="1005A000">
    </div>
    <div class="field" style="max-width:100px">
      <label>Port</label>
      <input type="text" name="port" placeholder="60004">
    </div>
    <div class="field">
      <label>From host</label>
      <input type="text" name="from_host" placeholder="account.pretendo.cc" required>
    </div>
    <div class="field">
      <label>To host</label>
      <input type="text" name="to_host" placeholder="45.157.178.35" required>
    </div>
    <div class="field" style="max-width:100px">
      <label>&nbsp;</label>
      <button class="submit" type="submit">Add</button>
    </div>
  </div>
</form>
</fieldset>
</body>
</html>`))

func adminUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/" {
		http.NotFound(w, r)
		return
	}
	msg := r.URL.Query().Get("msg")
	cert, _ := latestAdminCert()
	data := struct {
		Redirects         []Redirect
		OnlineUsers       []OnlineUser
		Msg               string
		ReviewCount       int
		CertAge           string
		DaysUntilRotation int
	}{
		Redirects:         allRedirects(),
		OnlineUsers:       onlineUsers(),
		Msg:               msg,
		ReviewCount:       reviewCount(),
		CertAge:           certAgeLabel(cert),
		DaysUntilRotation: daysUntilRotation(cert),
	}
	w.Header().Set("Content-Type", "text/html")
	adminTmpl.Execute(w, data)
}

func certAgeLabel(cert *AdminCert) string {
	if cert == nil {
		return "no cert"
	}
	d := int(time.Since(cert.CreatedAt).Hours() / 24)
	switch {
	case d == 0:
		return "generated today"
	case d == 1:
		return "1 day old"
	default:
		return fmt.Sprintf("%d days old", d)
	}
}

func adminAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/inkay/admin/", http.StatusSeeOther)
		return
	}
	rType := r.FormValue("type")
	address := r.FormValue("address")
	gameServerID := r.FormValue("game_server_id")
	portStr := r.FormValue("port")
	fromHost := r.FormValue("from_host")
	toHost := r.FormValue("to_host")

	if fromHost == "" || toHost == "" || (rType != "iosu" && rType != "dns") {
		http.Redirect(w, r, "/inkay/admin/?msg=Invalid+input", http.StatusSeeOther)
		return
	}
	var addrVal, gameVal, portVal interface{}
	if address != "" {
		addrVal = address
	}
	if gameServerID != "" {
		gameVal = gameServerID
	}
	if p, err := strconv.Atoi(portStr); err == nil && p > 0 {
		portVal = p
	}
	_, err := db.Exec(`INSERT INTO redirects (type, address, game_server_id, port, from_host, to_host) VALUES ($1, $2, $3, $4, $5, $6)`,
		rType, addrVal, gameVal, portVal, fromHost, toHost)
	if err != nil {
		log.Printf("add redirect: %v", err)
		http.Redirect(w, r, "/inkay/admin/?msg=DB+error", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/inkay/admin/?msg=Redirect+added", http.StatusSeeOther)
}

func adminDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/inkay/admin/", http.StatusSeeOther)
		return
	}
	id, err := strconv.Atoi(r.FormValue("id"))
	if err != nil {
		http.Redirect(w, r, "/inkay/admin/?msg=Invalid+ID", http.StatusSeeOther)
		return
	}
	db.Exec(`DELETE FROM redirects WHERE id = $1`, id)
	http.Redirect(w, r, fmt.Sprintf("/inkay/admin/?msg=Deleted+%d", id), http.StatusSeeOther)
}

func adminToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/inkay/admin/", http.StatusSeeOther)
		return
	}
	id, err := strconv.Atoi(r.FormValue("id"))
	if err != nil {
		http.Redirect(w, r, "/inkay/admin/?msg=Invalid+ID", http.StatusSeeOther)
		return
	}
	db.Exec(`UPDATE redirects SET enabled = NOT enabled WHERE id = $1`, id)
	http.Redirect(w, r, "/inkay/admin/", http.StatusSeeOther)
}


// --- User access management ---

var usersTmpl = template.Must(template.New("users").Funcs(tmplFuncs).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>User Access — {{.Game}}</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
body{font-family:system-ui,sans-serif;max-width:700px;margin:2rem auto;padding:0 1rem;color:#222}
h1{font-size:1.4rem}
a{color:#2563eb}
table{width:100%;border-collapse:collapse;font-size:.9rem;margin-bottom:2rem}
th{text-align:left;border-bottom:2px solid #e4e4e7;padding:.5rem .75rem;color:#666;font-weight:600}
td{padding:.5rem .75rem;border-bottom:1px solid #f0f0f0;vertical-align:middle}
tr:last-child td{border-bottom:none}
button,input{font:inherit}
button{cursor:pointer;border:none;border-radius:4px;padding:.3rem .7rem;font-size:.85rem}
.btn-del{background:#fee2e2;color:#991b1b}
fieldset{border:1px solid #e4e4e7;border-radius:8px;padding:1rem 1.25rem;margin-bottom:2rem}
legend{font-weight:600;padding:0 .4rem}
.row{display:flex;gap:.75rem;flex-wrap:wrap;align-items:flex-end}
.field{display:flex;flex-direction:column;gap:.3rem;flex:1;min-width:160px}
label{font-size:.8rem;color:#666;font-weight:600}
input[type=text]{border:1px solid #d1d5db;border-radius:4px;padding:.4rem .6rem;width:100%;box-sizing:border-box}
.submit{background:#2563eb;color:#fff;padding:.4rem 1rem;cursor:pointer;border:none;border-radius:4px}
.msg{background:#dcfce7;border:1px solid #bbf7d0;color:#166534;padding:.5rem 1rem;border-radius:6px;margin-bottom:1rem;font-size:.9rem}
.mode-note{background:#f8fafc;border:1px solid #e2e8f0;border-radius:6px;padding:.75rem 1rem;margin-bottom:1.5rem;font-size:.9rem}
</style>
</head>
<body>
<p><a href="/inkay/admin/">← Back to admin</a></p>
<h1>User Access — {{gameTitle .Game}} <span style="font-size:.9rem;color:#666;font-weight:400">({{.Game}})</span></h1>
{{if .Msg}}<div class="msg">{{.Msg}}</div>{{end}}

<div class="mode-note">
  Only users listed below can connect. Others fall through to Pretendo.
</div>

<table>
<tr><th>PNID</th><th>Label</th><th>Added</th><th></th></tr>
{{range .Users}}
<tr>
  <td>{{if .PNID}}<strong>{{.PNID}}</strong><br><span style="font-family:monospace;color:#999;font-size:.75rem">{{.PID}}</span>{{else}}<span style="font-family:monospace;font-size:.85rem">{{.PID}}</span>{{end}}</td>
  <td>{{if .Note}}{{.Note}}{{else}}<span style="color:#aaa">—</span>{{end}}</td>
  <td style="font-size:.8rem;color:#666">{{.CreatedAt.Format "2006-01-02 15:04"}}</td>
  <td>
    <form method="post" action="/inkay/admin/users/delete" onsubmit="return confirm('Remove PID {{.PID}}?')">
      <input type="hidden" name="pid" value="{{.PID}}">
      <input type="hidden" name="game" value="{{.GameServerID}}">
      <button class="btn-del" type="submit">Remove</button>
    </form>
  </td>
</tr>
{{else}}<tr><td colspan="4" style="color:#aaa">No users in list</td></tr>
{{end}}
</table>

<fieldset>
<legend>Add user</legend>
<form method="post" action="/inkay/admin/users/add">
  <input type="hidden" name="game" value="{{.Game}}">
  <div class="row">
    <div class="field" style="max-width:200px">
      <label>PID</label>
      <input type="text" name="pid" placeholder="1435853600" required>
    </div>
    <div class="field">
      <label>Label (optional)</label>
      <input type="text" name="note" placeholder="Nico">
    </div>
    <div class="field" style="max-width:100px">
      <label>&nbsp;</label>
      <button class="submit" type="submit">Add</button>
    </div>
  </div>
</form>
</fieldset>
</body>
</html>`))

func adminUsers(w http.ResponseWriter, r *http.Request) {
	game := r.URL.Query().Get("game")
	if game == "" {
		http.Redirect(w, r, "/inkay/admin/", http.StatusSeeOther)
		return
	}
	msg := r.URL.Query().Get("msg")

	data := struct {
		Game  string
		Users []UserAccess
		Msg   string
	}{game, usersForGame(game), msg}
	w.Header().Set("Content-Type", "text/html")
	usersTmpl.Execute(w, data)
}

func adminUserAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/inkay/admin/", http.StatusSeeOther)
		return
	}
	game := r.FormValue("game")
	pidStr := r.FormValue("pid")
	note := r.FormValue("note")

	pid, err := strconv.ParseInt(pidStr, 10, 64)
	if err != nil || pid <= 0 || game == "" {
		http.Redirect(w, r, "/inkay/admin/users/?game="+game+"&msg=Invalid+PID", http.StatusSeeOther)
		return
	}
	var noteVal interface{}
	if note != "" {
		noteVal = note
	}
	_, err = db.Exec(`INSERT INTO user_access (pid, game_server_id, note) VALUES ($1, UPPER($2), $3) ON CONFLICT (pid, game_server_id) DO UPDATE SET note = EXCLUDED.note`,
		pid, game, noteVal)
	if err != nil {
		log.Printf("user_access insert: %v", err)
		http.Redirect(w, r, "/inkay/admin/users/?game="+game+"&msg=DB+error", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/inkay/admin/users/?game="+game+"&msg=User+added", http.StatusSeeOther)
}

func adminUserDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/inkay/admin/", http.StatusSeeOther)
		return
	}
	game := r.FormValue("game")
	pidStr := r.FormValue("pid")
	pid, err := strconv.ParseInt(pidStr, 10, 64)
	if err != nil || game == "" {
		http.Redirect(w, r, "/inkay/admin/users/?game="+game+"&msg=Invalid+PID", http.StatusSeeOther)
		return
	}
	db.Exec(`DELETE FROM user_access WHERE pid = $1 AND UPPER(game_server_id) = UPPER($2)`, pid, game)
	http.Redirect(w, r, "/inkay/admin/users/?game="+game+"&msg=User+removed", http.StatusSeeOther)
}

// --- Ban management ---

var bansTmpl = template.Must(template.New("bans").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Banned Users</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
body{font-family:system-ui,sans-serif;max-width:700px;margin:2rem auto;padding:0 1rem;color:#222}
h1{font-size:1.4rem}
a{color:#2563eb}
table{width:100%;border-collapse:collapse;font-size:.9rem;margin-bottom:2rem}
th{text-align:left;border-bottom:2px solid #e4e4e7;padding:.5rem .75rem;color:#666;font-weight:600}
td{padding:.5rem .75rem;border-bottom:1px solid #f0f0f0;vertical-align:middle}
tr:last-child td{border-bottom:none}
button,input{font:inherit}
button{cursor:pointer;border:none;border-radius:4px;padding:.3rem .7rem;font-size:.85rem}
.btn-del{background:#dcfce7;color:#166534}
fieldset{border:1px solid #e4e4e7;border-radius:8px;padding:1rem 1.25rem;margin-bottom:2rem}
legend{font-weight:600;padding:0 .4rem}
.row{display:flex;gap:.75rem;flex-wrap:wrap;align-items:flex-end}
.field{display:flex;flex-direction:column;gap:.3rem;flex:1;min-width:160px}
label{font-size:.8rem;color:#666;font-weight:600}
input[type=text]{border:1px solid #d1d5db;border-radius:4px;padding:.4rem .6rem;width:100%;box-sizing:border-box}
.submit{background:#dc2626;color:#fff;padding:.4rem 1rem;cursor:pointer;border:none;border-radius:4px}
.msg{background:#dcfce7;border:1px solid #bbf7d0;color:#166534;padding:.5rem 1rem;border-radius:6px;margin-bottom:1rem;font-size:.9rem}
.note{background:#fff7ed;border:1px solid #fed7aa;border-radius:6px;padding:.75rem 1rem;margin-bottom:1.5rem;font-size:.9rem}
</style>
</head>
<body>
<p><a href="/inkay/admin/">← Back to admin</a></p>
<h1>Banned Users</h1>
{{if .Msg}}<div class="msg">{{.Msg}}</div>{{end}}
<div class="note">Banned users are always proxied to Pretendo, regardless of game server access mode. The ban applies globally across all game servers.</div>

<table>
<tr><th>PNID</th><th>Reason</th><th>Banned at</th><th></th></tr>
{{range .Bans}}
<tr>
  <td>{{if .PNID}}<strong>{{.PNID}}</strong><br><span style="font-family:monospace;color:#999;font-size:.75rem">{{.PID}}</span>{{else}}<span style="font-family:monospace;font-size:.85rem">{{.PID}}</span>{{end}}</td>
  <td>{{if .Reason}}{{.Reason}}{{else}}<span style="color:#aaa">—</span>{{end}}</td>
  <td style="font-size:.8rem;color:#666">{{.CreatedAt.Format "2006-01-02 15:04"}}</td>
  <td>
    <form method="post" action="/inkay/admin/bans/remove" onsubmit="return confirm('Unban PID {{.PID}}?')">
      <input type="hidden" name="pid" value="{{.PID}}">
      <button class="btn-del" type="submit">Unban</button>
    </form>
  </td>
</tr>
{{else}}<tr><td colspan="4" style="color:#aaa">No banned users</td></tr>
{{end}}
</table>

<fieldset>
<legend>Ban user</legend>
<form method="post" action="/inkay/admin/bans/add">
  <div class="row">
    <div class="field" style="max-width:200px">
      <label>PID</label>
      <input type="text" name="pid" placeholder="1435853600" required>
    </div>
    <div class="field">
      <label>Reason (optional)</label>
      <input type="text" name="reason" placeholder="Cheating">
    </div>
    <div class="field" style="max-width:100px">
      <label>&nbsp;</label>
      <button class="submit" type="submit">Ban</button>
    </div>
  </div>
</form>
</fieldset>
</body>
</html>`))

func adminBans(w http.ResponseWriter, r *http.Request) {
	msg := r.URL.Query().Get("msg")
	data := struct {
		Bans []BannedUser
		Msg  string
	}{allBans(), msg}
	w.Header().Set("Content-Type", "text/html")
	bansTmpl.Execute(w, data)
}

func adminBanAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/inkay/admin/bans/", http.StatusSeeOther)
		return
	}
	pidStr := r.FormValue("pid")
	reason := r.FormValue("reason")
	fromReview := r.FormValue("from_review") // non-empty when triggered from review queue

	pid, err := strconv.ParseInt(pidStr, 10, 64)
	if err != nil || pid <= 0 {
		http.Redirect(w, r, "/inkay/admin/bans/?msg=Invalid+PID", http.StatusSeeOther)
		return
	}
	var reasonVal interface{}
	if reason != "" {
		reasonVal = reason
	}
	_, err = db.Exec(`INSERT INTO banned_users (pid, reason) VALUES ($1, $2) ON CONFLICT (pid) DO UPDATE SET reason = EXCLUDED.reason, created_at = NOW()`,
		pid, reasonVal)
	if err != nil {
		log.Printf("ban insert: %v", err)
		http.Redirect(w, r, "/inkay/admin/bans/?msg=DB+error", http.StatusSeeOther)
		return
	}
	if fromReview != "" {
		db.Exec(`DELETE FROM review_queue WHERE pid = $1 AND UPPER(game_server_id) = UPPER($2)`, pid, fromReview)
		http.Redirect(w, r, "/inkay/admin/review/?msg=Banned+PID+"+pidStr, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/inkay/admin/bans/?msg=User+banned", http.StatusSeeOther)
}

func adminBanRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/inkay/admin/bans/", http.StatusSeeOther)
		return
	}
	pidStr := r.FormValue("pid")
	pid, err := strconv.ParseInt(pidStr, 10, 64)
	if err != nil {
		http.Redirect(w, r, "/inkay/admin/bans/?msg=Invalid+PID", http.StatusSeeOther)
		return
	}
	db.Exec(`DELETE FROM banned_users WHERE pid = $1`, pid)
	http.Redirect(w, r, "/inkay/admin/bans/?msg=User+unbanned", http.StatusSeeOther)
}

// --- Review queue ---

var reviewTmpl = template.Must(template.New("review").Funcs(tmplFuncs).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Review Queue</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
body{font-family:system-ui,sans-serif;max-width:760px;margin:2rem auto;padding:0 1rem;color:#222}
h1{font-size:1.4rem}
a{color:#2563eb}
table{width:100%;border-collapse:collapse;font-size:.9rem;margin-bottom:2rem}
th{text-align:left;border-bottom:2px solid #e4e4e7;padding:.5rem .75rem;color:#666;font-weight:600}
td{padding:.5rem .75rem;border-bottom:1px solid #f0f0f0;vertical-align:middle}
tr:last-child td{border-bottom:none}
button,input{font:inherit}
button{cursor:pointer;border:none;border-radius:4px;padding:.3rem .7rem;font-size:.85rem}
.btn-approve{background:#dcfce7;color:#166534}
.btn-ban{background:#fee2e2;color:#991b1b}
.btn-dismiss{background:#f4f4f5;color:#555}
.note{background:#fefce8;border:1px solid #fde68a;border-radius:6px;padding:.75rem 1rem;margin-bottom:1.5rem;font-size:.9rem}
.msg{background:#dcfce7;border:1px solid #bbf7d0;color:#166534;padding:.5rem 1rem;border-radius:6px;margin-bottom:1rem;font-size:.9rem}
.approve-row{display:flex;gap:.4rem;align-items:center}
input[type=text]{border:1px solid #d1d5db;border-radius:4px;padding:.3rem .5rem;font-size:.85rem;width:120px}
</style>
</head>
<body>
<p><a href="/inkay/admin/">← Back to admin</a></p>
<h1>Review Queue</h1>
{{if .Msg}}<div class="msg">{{.Msg}}</div>{{end}}
<div class="note">These PIDs connected to a whitelisted game server but are not yet approved. Approve to add to the whitelist, ban to add to the global ban list, or dismiss to silently discard.</div>
<table>
<tr><th>PNID</th><th>Game server</th><th>Attempts</th><th>First seen</th><th>Last seen</th><th></th></tr>
{{range .Entries}}
<tr>
  <td>{{if .PNID}}<strong>{{.PNID}}</strong><br><span style="font-family:monospace;color:#999;font-size:.75rem">{{.PID}}</span>{{else}}<span style="font-family:monospace;font-size:.85rem">{{.PID}}</span>{{end}}</td>
  <td><span title="{{.GameServerID}}" style="font-size:.85rem">{{gameTitle .GameServerID}}</span></td>
  <td style="color:#666">{{.Attempts}}</td>
  <td style="font-size:.8rem;color:#666">{{.FirstSeen.Format "2006-01-02 15:04"}}</td>
  <td style="font-size:.8rem;color:#666">{{.LastSeen.Format "2006-01-02 15:04"}}</td>
  <td>
    <div class="approve-row">
      <form method="post" action="/inkay/admin/review/approve" style="display:flex;gap:.3rem;align-items:center">
        <input type="hidden" name="pid" value="{{.PID}}">
        <input type="hidden" name="game" value="{{.GameServerID}}">
        <input type="text" name="note" placeholder="Label (opt)">
        <button class="btn-approve" type="submit">Approve</button>
      </form>
      <form method="post" action="/inkay/admin/bans/add" onsubmit="return confirm('Ban PID {{.PID}}?')">
        <input type="hidden" name="pid" value="{{.PID}}">
        <input type="hidden" name="reason" value="denied via review queue">
        <input type="hidden" name="from_review" value="{{.GameServerID}}">
        <button class="btn-ban" type="submit">Ban</button>
      </form>
      <form method="post" action="/inkay/admin/review/dismiss">
        <input type="hidden" name="pid" value="{{.PID}}">
        <input type="hidden" name="game" value="{{.GameServerID}}">
        <button class="btn-dismiss" type="submit">Dismiss</button>
      </form>
    </div>
  </td>
</tr>
{{else}}<tr><td colspan="6" style="color:#aaa">Queue is empty</td></tr>
{{end}}
</table>
</body>
</html>`))

func adminReview(w http.ResponseWriter, r *http.Request) {
	msg := r.URL.Query().Get("msg")
	data := struct {
		Entries []ReviewEntry
		Msg     string
	}{pendingReviews(), msg}
	w.Header().Set("Content-Type", "text/html")
	reviewTmpl.Execute(w, data)
}

func adminReviewApprove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/inkay/admin/review/", http.StatusSeeOther)
		return
	}
	pidStr := r.FormValue("pid")
	game := r.FormValue("game")
	note := r.FormValue("note")

	pid, err := strconv.ParseInt(pidStr, 10, 64)
	if err != nil || pid <= 0 || game == "" {
		http.Redirect(w, r, "/inkay/admin/review/?msg=Invalid+input", http.StatusSeeOther)
		return
	}
	var noteVal interface{}
	if note != "" {
		noteVal = note
	}
	tx, err := db.Begin()
	if err != nil {
		http.Redirect(w, r, "/inkay/admin/review/?msg=DB+error", http.StatusSeeOther)
		return
	}
	tx.Exec(`INSERT INTO user_access (pid, game_server_id, note) VALUES ($1, UPPER($2), $3) ON CONFLICT (pid, game_server_id) DO UPDATE SET note = EXCLUDED.note`, pid, game, noteVal)
	tx.Exec(`DELETE FROM review_queue WHERE pid = $1 AND UPPER(game_server_id) = UPPER($2)`, pid, game)
	if err := tx.Commit(); err != nil {
		tx.Rollback()
		http.Redirect(w, r, "/inkay/admin/review/?msg=DB+error", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/inkay/admin/review/?msg=Approved+PID+"+pidStr, http.StatusSeeOther)
}

func adminReviewDismiss(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/inkay/admin/review/", http.StatusSeeOther)
		return
	}
	pidStr := r.FormValue("pid")
	game := r.FormValue("game")
	pid, err := strconv.ParseInt(pidStr, 10, 64)
	if err != nil || game == "" {
		http.Redirect(w, r, "/inkay/admin/review/?msg=Invalid+input", http.StatusSeeOther)
		return
	}
	db.Exec(`DELETE FROM review_queue WHERE pid = $1 AND UPPER(game_server_id) = UPPER($2)`, pid, game)
	http.Redirect(w, r, "/inkay/admin/review/?msg=Dismissed", http.StatusSeeOther)
}

func adminClientCert(w http.ResponseWriter, r *http.Request) {
	cert, err := latestAdminCert()
	if err != nil || cert == nil {
		http.Error(w, "no cert available yet", 503)
		return
	}
	p12, err := certToP12([]byte(cert.CertPEM), []byte(cert.KeyPEM))
	if err != nil {
		log.Printf("certToP12: %v", err)
		http.Error(w, "p12 conversion failed", 500)
		return
	}
	w.Header().Set("Content-Type", "application/x-pkcs12")
	w.Header().Set("Content-Disposition", `attachment; filename="inkay-admin.p12"`)
	w.Write(p12)
}

func adminCertsRotate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/inkay/admin/", http.StatusSeeOther)
		return
	}
	if err := rotateCert(); err != nil {
		log.Printf("manual rotate: %v", err)
		http.Redirect(w, r, "/inkay/admin/?msg=Rotation+failed:+"+err.Error(), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/inkay/admin/?msg=New+cert+generated+—+re-download+client-cert.p12", http.StatusSeeOther)
}

// --- My Status (per-user page, web password auth) ---

var mySessionStore sync.Map // token string → pid int64
var mySyncTime    sync.Map // pid int64 → time.Time of last Pretendo sync

func myNewSession(pid int64) string {
	b := make([]byte, 16)
	rand.Read(b)
	token := hex.EncodeToString(b)
	mySessionStore.Store(token, pid)
	return token
}

func mySessionPID(r *http.Request) (int64, bool) {
	c, err := r.Cookie("my_session")
	if err != nil {
		return 0, false
	}
	v, ok := mySessionStore.Load(c.Value)
	if !ok {
		return 0, false
	}
	return v.(int64), true
}

type myFriendEntry struct {
	PID           int64
	PNID          string
	MiiName       string
	IsOnline      bool
	TitleID       int64
	GameServerHex string
	LastOnline    sql.NullTime
}

type myLoginData struct {
	Error string
	PNID  string
}

type myStatusData struct {
	PID           int64
	PNID          string
	MiiName       string
	IsOnline      bool
	TitleID       int64
	GameServerHex string
	Friends       []myFriendEntry
}

// --- Discord Link page ---

var myDiscordTmpl = template.Must(template.New("my-discord").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Discord Link — Revivetendo</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:system-ui,sans-serif;background:#0f0f11;color:#e4e4e7;min-height:100vh;display:flex;align-items:center;justify-content:center;padding:2rem}
.card{background:#18181b;border:1px solid #27272a;border-radius:12px;padding:2rem;max-width:480px;width:100%}
h1{font-size:1.4rem;margin-bottom:.5rem}
.sub{color:#71717a;font-size:.9rem;margin-bottom:1.5rem}
.code-box{background:#09090b;border:1px solid #27272a;border-radius:8px;padding:1rem 1.2rem;font-family:monospace;font-size:1.5rem;letter-spacing:.15em;text-align:center;color:#a78bfa;margin:1.2rem 0;user-select:all}
.expiry{color:#71717a;font-size:.82rem;text-align:center;margin-bottom:1.2rem}
.steps{list-style:none;counter-reset:s}
.steps li{counter-increment:s;padding:.6rem 0 .6rem 2.2rem;position:relative;color:#d4d4d8;font-size:.9rem;border-bottom:1px solid #27272a}
.steps li:last-child{border-bottom:none}
.steps li::before{content:counter(s);position:absolute;left:0;top:.55rem;background:#7c3aed;color:#fff;width:1.4rem;height:1.4rem;border-radius:50%;display:flex;align-items:center;justify-content:center;font-size:.75rem;font-weight:700}
.steps code{background:#27272a;padding:.1em .4em;border-radius:4px;font-family:monospace;color:#a78bfa}
.linked{background:#14532d;border:1px solid #16a34a;border-radius:8px;padding:.8rem 1rem;color:#86efac;font-size:.9rem;margin-bottom:1.2rem}
.btn{display:block;width:100%;padding:.7rem;margin-top:1.2rem;background:#7c3aed;color:#fff;border:none;border-radius:8px;font-size:.95rem;cursor:pointer;text-decoration:none;text-align:center}
.btn:hover{background:#6d28d9}
.btn-sm{display:inline-block;padding:.4rem .9rem;background:#27272a;color:#e4e4e7;border:none;border-radius:6px;font-size:.82rem;cursor:pointer;margin-top:.6rem;text-decoration:none}
</style>
</head>
<body>
<div class="card">
  <h1>Discord Link</h1>
  <p class="sub">Link your PNID to Discord to receive WiiU Chat call notifications via DM.</p>
  {{if .LinkedDiscordID}}
  <div class="linked">✅ Your account is linked to Discord user ID <strong>{{.LinkedDiscordID}}</strong>.</div>
  {{end}}
  <p style="color:#a1a1aa;font-size:.88rem;margin-bottom:.8rem">Signed in as <strong>{{.PNID}}</strong></p>
  <div class="code-box">{{.Code}}</div>
  <p class="expiry">Expires in {{.ExpiresIn}} · <a href="/inkay/my/discord" style="color:#a78bfa">Refresh</a></p>
  <ol class="steps">
    <li>Join the Revivetendo Discord server</li>
    <li>Run the slash command:<br><code>/link_pnid {{.Code}}</code></li>
    <li>Done! Call notifications will arrive as Discord DMs.</li>
  </ol>
  <a class="btn-sm" href="/inkay/my/">← Back to My Status</a>
  <a class="btn-sm" href="/inkay/my/logout" style="margin-left:.4rem">Sign out</a>
</div>
</body>
</html>`))

type myDiscordData struct {
	PNID           string
	Code           string
	ExpiresIn      string
	LinkedDiscordID string
}

func myDiscordHandler(w http.ResponseWriter, r *http.Request) {
	pid, ok := mySessionPID(r)
	if !ok {
		http.Redirect(w, r, "/inkay/my/login", http.StatusFound)
		return
	}

	// Look up PNID and current Discord link.
	var pnid, discordID string
	// Use user_settings.nnid as the canonical PNID — nex_accounts.username can be a numeric string.
	db.QueryRow(`SELECT COALESCE(NULLIF(us.nnid,''), na.username), COALESCE(wd.discord_id,'')
		FROM nex_accounts na
		LEFT JOIN user_settings us ON us.pid = na.pid
		LEFT JOIN wii_devices wd ON wd.username = COALESCE(NULLIF(us.nnid,''), na.username)
		WHERE na.pid = $1`, pid).Scan(&pnid, &discordID)
	if pnid == "" {
		http.Error(w, "account not found", http.StatusInternalServerError)
		return
	}

	// Clean up expired codes for this PNID, then get or create a fresh one.
	db.Exec(`DELETE FROM discord_link_codes WHERE pnid = $1 AND expires_at <= now()`, pnid)

	var code string
	var expiresAt time.Time
	db.QueryRow(`SELECT code, expires_at FROM discord_link_codes WHERE pnid = $1`, pnid).Scan(&code, &expiresAt)

	if code == "" {
		b := make([]byte, 8)
		rand.Read(b)
		code = strings.ToUpper(hex.EncodeToString(b))
		expiresAt = time.Now().Add(10 * time.Minute)
		db.Exec(`INSERT INTO discord_link_codes (code, pnid, expires_at) VALUES ($1, $2, $3)`, code, pnid, expiresAt)
	}

	remaining := time.Until(expiresAt).Round(time.Second)
	mins := int(remaining.Minutes())
	secs := int(remaining.Seconds()) % 60
	expiresIn := fmt.Sprintf("%d:%02d", mins, secs)

	myDiscordTmpl.Execute(w, myDiscordData{
		PNID:            pnid,
		Code:            code,
		ExpiresIn:       expiresIn,
		LinkedDiscordID: discordID,
	})
}

var myLoginTmpl = template.Must(template.New("my-login").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>My Status — Pretendo Bridge</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
body{font-family:system-ui,sans-serif;max-width:400px;margin:4rem auto;padding:0 1rem;color:#222}
h1{font-size:1.4rem;margin-bottom:.2rem}
p.sub{color:#666;font-size:.875rem;margin-top:0;margin-bottom:2rem}
.error{background:#fee2e2;border:1px solid #fca5a5;color:#991b1b;border-radius:6px;padding:.6rem 1rem;margin-bottom:1rem;font-size:.875rem}
label{display:block;font-size:.8rem;font-weight:600;color:#555;margin-bottom:.3rem;margin-top:1rem}
input{width:100%;box-sizing:border-box;border:1px solid #d1d5db;border-radius:6px;padding:.5rem .75rem;font:inherit}
input:focus{outline:none;border-color:#6366f1;box-shadow:0 0 0 2px rgba(99,102,241,.15)}
button.submit{margin-top:1.5rem;width:100%;background:#6366f1;color:#fff;border:none;border-radius:6px;padding:.65rem;font:inherit;cursor:pointer}
button.submit:hover{background:#4f46e5}
.hint{font-size:.8rem;color:#888;margin-top:2rem;text-align:center}
</style>
</head>
<body>
<p><a href="/">← Back</a></p>
<h1>My Status</h1>
<p class="sub">Sign in with your PNID and web password.</p>
{{if .Error}}<div class="error">{{.Error}}</div>{{end}}
<form method="post" action="/inkay/my/login">
  <label for="pnid">PNID</label>
  <input id="pnid" type="text" name="pnid" autocomplete="username" value="{{.PNID}}" required>
  <label for="password">Web Password</label>
  <input id="password" type="password" name="password" autocomplete="current-password" required>
  <button class="submit" type="submit">Sign in</button>
</form>
<p class="hint">Set your web password in the Juxt console portal.</p>
</body>
</html>`))

var myStatusTmpl = template.Must(template.New("my-status").Funcs(tmplFuncs).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>My Status — Pretendo Bridge</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
body{font-family:system-ui,sans-serif;max-width:620px;margin:2rem auto;padding:0 1rem;color:#222}
h1{font-size:1.3rem;margin-bottom:1.5rem}
.me{display:flex;align-items:center;gap:1rem;background:#f8fafc;border:1px solid #e2e8f0;border-radius:10px;padding:1rem 1.25rem;margin-bottom:2rem}
.mii{width:52px;height:52px;border-radius:50%;background:#e2e8f0;object-fit:cover}
.me-info{flex:1;min-width:0}
.me-name{font-weight:700;font-size:1rem}
.me-pnid{font-size:.8rem;color:#888;margin-top:.1rem}
.status-line{display:flex;align-items:center;gap:.4rem;margin-top:.4rem;font-size:.85rem}
.dot{width:8px;height:8px;border-radius:50%;flex-shrink:0}
.dot-on{background:#22c55e}.dot-off{background:#d1d5db}
.game{color:#555}
h2{font-size:.95rem;color:#666;font-weight:600;margin-bottom:.75rem;margin-top:0}
table{width:100%;border-collapse:collapse;font-size:.9rem}
th{text-align:left;border-bottom:1px solid #e4e4e7;padding:.4rem .6rem;color:#888;font-size:.8rem;font-weight:600}
td{padding:.5rem .6rem;border-bottom:1px solid #f4f4f5;vertical-align:middle}
tr:last-child td{border-bottom:none}
.friend-mii{width:30px;height:30px;border-radius:50%;background:#e2e8f0;object-fit:cover}
.badge-on{display:inline-block;color:#166534;background:#dcfce7;border-radius:999px;padding:.15rem .55rem;font-size:.75rem;font-weight:600}
.badge-off{display:inline-block;color:#666;background:#f4f4f5;border-radius:999px;padding:.15rem .55rem;font-size:.75rem}
.empty{color:#aaa;font-size:.875rem;padding:.5rem 0}
.nav{display:flex;align-items:center;gap:.5rem;margin-bottom:1.5rem;font-size:.875rem}
.nav a{color:#2563eb}
form.logout-form{display:inline;margin:0}
button.logout-btn{background:none;border:none;color:#dc2626;font-size:.875rem;cursor:pointer;padding:0;text-decoration:underline}
.refresh{font-size:.75rem;color:#aaa;margin-left:auto}
</style>
</head>
<body>
<div class="nav">
  <a href="/">← Back</a>
  <span style="color:#d1d5db">|</span>
  <form class="logout-form" method="post" action="/inkay/my/logout">
    <button class="logout-btn" type="submit">Sign out</button>
  </form>
  <span class="refresh" id="refresh-label">refreshes in 30s</span>
</div>
<h1>My Status</h1>
<div class="me">
  <img class="mii" src="https://cdn.pretendo.cc/miiverse/mii/{{.PID}}/normal_face.png" alt="" onerror="this.style.display='none'">
  <div class="me-info">
    <div class="me-name">{{if .MiiName}}{{.MiiName}}{{else}}{{.PNID}}{{end}}</div>
    <div class="me-pnid">@{{.PNID}}</div>
    <div class="status-line">
      <span class="dot {{if .IsOnline}}dot-on{{else}}dot-off{{end}}"></span>
      {{if .IsOnline}}Online{{with resolveGameName .TitleID .GameServerHex}} · <span class="game">{{.}}</span>{{end}}{{else}}Offline{{end}}
    </div>
  </div>
</div>
<h2>Friends ({{len .Friends}})</h2>
{{if .Friends}}
<table>
<tr><th></th><th>Name</th><th>PNID</th><th>Status</th><th>Game</th><th>Last Online</th></tr>
{{range .Friends}}
<tr>
  <td style="width:38px;padding-right:0">
    <img class="friend-mii" src="https://cdn.pretendo.cc/miiverse/mii/{{.PID}}/normal_face.png" alt="" onerror="this.style.display='none'">
  </td>
  <td>{{if .MiiName}}{{.MiiName}}{{else}}<span style="color:#aaa">—</span>{{end}}</td>
  <td style="font-size:.8rem;color:#888">@{{.PNID}}</td>
  <td>{{if .IsOnline}}<span class="badge-on">Online</span>{{else}}<span class="badge-off">Offline</span>{{end}}</td>
  <td style="font-size:.85rem">{{with resolveGameName .TitleID .GameServerHex}}{{.}}{{else}}<span style="color:#aaa">—</span>{{end}}</td>
  <td style="font-size:.8rem;color:#aaa">{{if .LastOnline.Valid}}{{.LastOnline.Time.Format "Jan 2, 15:04"}}{{else}}—{{end}}</td>
</tr>
{{end}}
</table>
{{else}}<p class="empty">No friends yet.</p>{{end}}
<script>
var countdown = 30;
function tick() {
  countdown--;
  if (countdown <= 0) { location.reload(); return; }
  document.getElementById('refresh-label').textContent = 'refreshes in ' + countdown + 's';
}
setInterval(tick, 1000);
</script>
</body>
</html>`))

func myHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/my/" {
		http.NotFound(w, r)
		return
	}
	pid, ok := mySessionPID(r)
	if !ok {
		w.Header().Set("Content-Type", "text/html")
		myLoginTmpl.Execute(w, myLoginData{})
		return
	}

	// Kick off a fresh Pretendo sync in the background, rate-limited to once per minute.
	// External friends' online status will be up to date on the next 30-second auto-refresh.
	const syncInterval = 60 * time.Second
	now := time.Now()
	if last, ok := mySyncTime.Load(pid); !ok || now.Sub(last.(time.Time)) >= syncInterval {
		mySyncTime.Store(pid, now)
		go func() {
			resp, err := http.Post("http://127.0.0.1:9191/internal/sync/"+strconv.FormatInt(pid, 10), "", nil)
			if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}

	var pnid, miiName, gameServerHex string
	var isOnline bool
	var titleID int64
	db.QueryRow(`
		SELECT COALESCE(NULLIF(p.pnid,''), ''), COALESCE(NULLIF(s.mii_name,''), ''),
		       (s.is_online IS TRUE),
		       COALESCE(s.presence_title_id, 0),
		       COALESCE(LPAD(UPPER(TO_HEX(NULLIF(s.presence_game_server_id, 0))), 8, '0'), '')
		FROM pnid_cache p
		LEFT JOIN user_settings s ON s.pid = p.pid
		WHERE p.pid = $1`, pid).Scan(&pnid, &miiName, &isOnline, &titleID, &gameServerHex)

	rows, err := db.Query(`
		SELECT f.friend_pid,
		       COALESCE(NULLIF(p.pnid,''), NULLIF(f.friend_nnid,''), ''),
		       COALESCE(NULLIF(s.mii_name,''), NULLIF(f.mii_name,''), NULLIF(m.mii_name,''), ''),
		       ((s.is_online IS TRUE) OR f.is_online IS TRUE),
		       COALESCE(NULLIF(s.presence_title_id, 0), NULLIF(f.title_id, 0), 0),
		       COALESCE(LPAD(UPPER(TO_HEX(NULLIF(f.game_server_id, 0))), 8, '0'), ''),
		       GREATEST(s.last_online_at, f.last_online)
		FROM pretendo_friends f
		LEFT JOIN pnid_cache p ON p.pid = f.friend_pid
		LEFT JOIN user_settings s ON s.pid = f.friend_pid
		LEFT JOIN mii_names m ON m.pid = f.friend_pid
		WHERE f.owner_pid = $1
		ORDER BY (s.is_online IS TRUE OR f.is_online IS TRUE) DESC, f.friend_pid`, pid)
	var friends []myFriendEntry
	if err != nil {
		log.Printf("[my] friends query error for pid=%d: %v", pid, err)
	} else {
		defer rows.Close()
		for rows.Next() {
			var f myFriendEntry
			if err := rows.Scan(&f.PID, &f.PNID, &f.MiiName, &f.IsOnline, &f.TitleID, &f.GameServerHex, &f.LastOnline); err != nil {
				log.Printf("[my] scan error: %v", err)
			} else {
				friends = append(friends, f)
			}
		}
	}

	w.Header().Set("Content-Type", "text/html")
	myStatusTmpl.Execute(w, myStatusData{
		PID:           pid,
		PNID:          pnid,
		MiiName:       miiName,
		IsOnline:      isOnline,
		TitleID:       titleID,
		GameServerHex: gameServerHex,
		Friends:       friends,
	})
}

func myLoginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/inkay/my/", http.StatusSeeOther)
		return
	}
	pnid := strings.TrimSpace(r.FormValue("pnid"))
	password := r.FormValue("password")

	fail := func(msg string) {
		w.Header().Set("Content-Type", "text/html")
		myLoginTmpl.Execute(w, myLoginData{Error: msg, PNID: pnid})
	}

	if pnid == "" || password == "" {
		fail("PNID and password are required.")
		return
	}

	var storedHash string
	if err := db.QueryRow(`SELECT web_password_hash FROM wii_devices WHERE username = $1`, pnid).Scan(&storedHash); err != nil || storedHash == "" {
		fail("Invalid PNID or no web password set.")
		return
	}

	h := sha256.Sum256([]byte(password))
	if hex.EncodeToString(h[:]) != storedHash {
		fail("Incorrect password.")
		return
	}

	var pid int64
	if err := db.QueryRow(`SELECT pid FROM pnid_cache WHERE pnid = $1`, pnid).Scan(&pid); err != nil || pid == 0 {
		fail("Account not found in local database.")
		return
	}

	token := myNewSession(pid)
	http.SetCookie(w, &http.Cookie{
		Name:     "my_session",
		Value:    token,
		Path:     "/inkay/my/",
		HttpOnly: true,
		MaxAge:   86400 * 7,
	})
	http.Redirect(w, r, "/inkay/my/", http.StatusSeeOther)
}

func myLogoutHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/inkay/my/", http.StatusSeeOther)
		return
	}
	if c, err := r.Cookie("my_session"); err == nil {
		mySessionStore.Delete(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:   "my_session",
		Value:  "",
		Path:   "/inkay/my/",
		MaxAge: -1,
	})
	http.Redirect(w, r, "/inkay/my/", http.StatusSeeOther)
}
