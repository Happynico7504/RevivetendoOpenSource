package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"
)

// presenceSharing is what the account chose to share publicly (see the presence_sharing table).
func presenceSharing(pid int64) (overlayOnline, publicList bool) {
	db.QueryRow(`SELECT overlay_online, public_list FROM presence_sharing WHERE pid = $1`, pid).Scan(&overlayOnline, &publicList)
	return
}

func setPresenceSharing(pid int64, overlayOnline, publicList bool) error {
	_, err := db.Exec(`
		INSERT INTO presence_sharing (pid, overlay_online, public_list, updated_at) VALUES ($1, $2, $3, NOW())
		ON CONFLICT (pid) DO UPDATE SET overlay_online = EXCLUDED.overlay_online,
			public_list = EXCLUDED.public_list, updated_at = NOW()`, pid, overlayOnline, publicList)
	return err
}

func myPresenceSharingHandler(w http.ResponseWriter, r *http.Request) {
	pid, ok := mySessionPID(r)
	if !ok {
		http.Redirect(w, r, "/inkay/my/login", http.StatusFound)
		return
	}
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/inkay/my/account#sharing", http.StatusSeeOther)
		return
	}
	if err := setPresenceSharing(pid, r.FormValue("overlay_online") == "1", r.FormValue("public_list") == "1"); err != nil {
		log.Printf("myPresenceSharingHandler: pid=%d: %v", pid, err)
		renderMyAccount(w, pid, myAccountData{Error: "Failed to save the sharing settings, try again later.", Key: "acct.ov_err"})
		return
	}
	renderMyAccount(w, pid, myAccountData{Success: "Sharing settings saved.", Key: "acct.ov_ok"})
}

// sharedPresence is one opted-in account's status. Online is the bridge's own Friends
// presence or, for players on Pretendo's Friends server, Pretendo's.
//
// Pretendo's side is only what other accounts' friend syncs saw (one pretendo_friends row
// per friend, refreshed only when that friend syncs), and while a player is on the bridge
// Pretendo sees our own keep-alive login, plus the one-shot sync account-proxy runs right
// after their console disconnects. So only the freshest row counts, only when it is recent
// and newer than the bridge's last disconnect plus a grace period; otherwise a stale "online"
// row kept bridge players online for hours after their Wii U was off.
type sharedPresence struct {
	PID        int64     `json:"pid"`
	PNID       string    `json:"pnid"`
	MiiName    string    `json:"mii_name,omitempty"`
	Online     bool      `json:"online"`
	Game       string    `json:"game,omitempty"` // only while online; "" = no game / unknown
	LastOnline time.Time `json:"last_online,omitempty"`
}

// loadSharedPresence returns the accounts that opted in through the given presence_sharing
// column, optionally only the one with the given PNID.
func loadSharedPresence(column, pnid string) []sharedPresence {
	q := `
		SELECT p.pid, p.pnid, COALESCE(NULLIF(s.mii_name,''), NULLIF(m.mii_name,''), ''),
		       (s.is_online IS TRUE OR pt.online IS TRUE),
		       CASE WHEN s.is_online IS TRUE THEN COALESCE(s.presence_title_id, 0)
		            WHEN pt.online THEN pf.title_id ELSE 0 END,
		       CASE WHEN s.is_online IS TRUE THEN COALESCE(s.presence_game_server_id::BIGINT, 0)
		            WHEN pt.online THEN pf.game_server_id ELSE 0 END,
		       GREATEST(s.last_online_at, pf.last_online)
		FROM presence_sharing ps
		JOIN pnid_cache p ON p.pid = ps.pid AND p.pnid <> ''
		LEFT JOIN user_settings s ON s.pid = ps.pid
		LEFT JOIN mii_names m ON m.pid = ps.pid
		LEFT JOIN LATERAL (
			SELECT is_online, title_id, game_server_id, last_online, updated_at FROM pretendo_friends
			WHERE friend_pid = ps.pid ORDER BY updated_at DESC LIMIT 1
		) pf ON TRUE
		CROSS JOIN LATERAL (
			SELECT pf.is_online AND pf.updated_at > NOW() - INTERVAL '10 minutes'
			       AND (s.last_online_at IS NULL OR pf.updated_at > s.last_online_at + INTERVAL '5 minutes') AS online
		) pt
		WHERE ps.` + column
	args := []any{}
	if pnid != "" {
		q += ` AND LOWER(p.pnid) = LOWER($1)`
		args = append(args, pnid)
	}
	rows, err := db.Query(q+` ORDER BY 4 DESC, LOWER(p.pnid)`, args...)
	if err != nil {
		log.Printf("loadSharedPresence: %v", err)
		return nil
	}
	defer rows.Close()
	var list []sharedPresence
	for rows.Next() {
		var e sharedPresence
		var titleID, gsid int64
		var last *time.Time
		if rows.Scan(&e.PID, &e.PNID, &e.MiiName, &e.Online, &titleID, &gsid, &last) != nil {
			continue
		}
		if last != nil {
			e.LastOnline = *last
		}
		if e.Online {
			hex := ""
			if gsid != 0 {
				hex = fmt.Sprintf("%08X", uint32(gsid))
			}
			e.Game = mcpGame(true, titleID, hex)
		}
		list = append(list, e)
	}
	return list
}

// apiWSCOverlayOnline is the overlay's Wii U online/offline feed. It only answers for
// accounts that opted in on My Account; for every other PNID (known or not) it says
// {"shared":false}, so it can't be used to find out who is online or which PNIDs exist.
func apiWSCOverlayOnline(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	resp := struct {
		Shared bool `json:"shared"`
		Online bool `json:"online,omitempty"`
	}{}
	if pnid := strings.TrimSpace(r.URL.Query().Get("pnid")); pnid != "" {
		if l := loadSharedPresence("overlay_online", pnid); len(l) > 0 {
			resp.Shared, resp.Online = true, l[0].Online
		}
	}
	json.NewEncoder(w).Encode(resp)
}

func apiPublicOnline(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	list := loadSharedPresence("public_list", "")
	if list == nil {
		list = []sharedPresence{}
	}
	json.NewEncoder(w).Encode(list)
}

func publicOnlineHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/online/" {
		http.NotFound(w, r)
		return
	}
	list := loadSharedPresence("public_list", "")
	online := 0
	for _, e := range list {
		if e.Online {
			online++
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	publicOnlineTmpl.Execute(w, struct {
		Users  []sharedPresence
		Online int
	}{list, online})
}

var publicOnlineTmpl = template.Must(template.New("online").Funcs(tmplFuncs).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
` + i18nScript + `
<meta charset="utf-8">
<title>Who's Online — Revivetendo</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
body{font-family:system-ui,sans-serif;max-width:900px;margin:2rem auto;padding:0 1rem;color:#222}
h1{font-size:1.4rem}
a{color:#2563eb}
.bar{display:flex;align-items:center;gap:.6rem;margin-bottom:1.5rem;font-size:.9rem;color:#666;flex-wrap:wrap}
.refresh{margin-left:auto;font-size:.8rem;color:#aaa}
.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(240px,1fr));gap:.75rem}
.user{display:flex;align-items:center;gap:.75rem;border:1px solid #e4e4e7;border-radius:10px;padding:.7rem .8rem;min-width:0}
.user.off{opacity:.6}
.mii{width:48px;height:48px;border-radius:50%;background:#f4f4f5;flex-shrink:0;overflow:hidden;position:relative}
.mii img{width:100%;height:100%}
.dot{width:10px;height:10px;border-radius:50%;flex-shrink:0;background:#d4d4d8}
.user.on .dot{background:#22c55e}
.info{min-width:0}
.name{font-weight:600;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.pnid{font-size:.8rem;color:#888}
.status{font-size:.85rem;color:#166534;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.user.off .status{color:#888}
.empty{color:#888;font-size:.9rem}
</style>
</head>
<body data-i18n-page-title="online.title">
<div data-lang-slot></div>
<h1 data-i18n="online.h1">Who's Online</h1>
<div class="bar">
  <span data-i18n="online.count" data-i18n-args='{"n":{{.Online}},"total":{{len .Users}}}'>{{.Online}} of {{len .Users}} online</span>
  <span>·</span>
  <a href="/inkay/my/account#sharing" data-i18n="online.optin">Get listed here</a>
  <span class="refresh" id="refresh-label" data-i18n="my.refresh" data-i18n-args='{"n":30}'>refreshes in 30s</span>
</div>
{{if .Users}}<div class="grid">
{{range .Users}}<div class="user {{if .Online}}on{{else}}off{{end}}">
  <div class="mii"><img src="https://sos-de-fra-1.exo.io/olv-data/mii/{{.PID}}/normal_face.png" alt="" loading="lazy" onerror="this.style.display='none'"></div>
  <div class="info">
    <div class="name">{{if .MiiName}}{{.MiiName}}{{else}}{{.PNID}}{{end}}</div>
    <div class="pnid">@{{.PNID}}</div>
    {{if .Online}}<div class="status">{{if .Game}}{{.Game}}{{else}}<span data-i18n="online.online">Online</span>{{end}}</div>
    {{else}}<div class="status"><span data-i18n="online.offline">Offline</span>{{if not .LastOnline.IsZero}} · <span data-i18n="online.last_seen">last seen</span> {{localTime .LastOnline "match"}}{{end}}</div>{{end}}
  </div>
  <span class="dot"></span>
</div>
{{end}}</div>
{{else}}<p class="empty" data-i18n="online.empty">Nobody has opted in yet.</p>{{end}}
<script>
var countdown = 30;
setInterval(function () {
  countdown--;
  if (countdown <= 0) { location.reload(); return; }
  document.getElementById('refresh-label').textContent = I18N.fill(I18N.t('my.refresh', 'refreshes in {n}s'), {n: countdown});
}, 1000);
</script>
` + localTimeScript + `
</body>
</html>`))
