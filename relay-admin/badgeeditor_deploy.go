package main

// Admin deployments for Badge Arcade: approved badges placed into a machine
// cloned from a Nintendo layout, for a date range. account-proxy builds them
// into the daily SpotPass package (see account-proxy/badgearcade_deploy.go).

import (
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Happynico7504/badgearcade"
)

const badgeArcadeDeploySchema = `
CREATE TABLE IF NOT EXISTS badge_arcade_deployments (
	id         BIGSERIAL   PRIMARY KEY,
	name       TEXT        NOT NULL,
	template   TEXT        NOT NULL,
	slot       TEXT        NOT NULL CHECK (slot IN ('hall', 'training')),
	status     TEXT        NOT NULL DEFAULT 'test' CHECK (status IN ('test', 'live', 'off')),
	start_date DATE        NOT NULL,
	end_date   DATE        NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE IF NOT EXISTS badge_arcade_deployment_badges (
	deployment_id BIGINT NOT NULL REFERENCES badge_arcade_deployments (id) ON DELETE CASCADE,
	creation_id   BIGINT NOT NULL REFERENCES badge_arcade_creations (id) ON DELETE CASCADE,
	position      INT    NOT NULL DEFAULT 0,
	PRIMARY KEY (deployment_id, creation_id)
);
`

const badgeArcadeDeployBase = "/inkay/admin/badge-arcade/deployments/"

func registerBadgeArcadeDeploy() {
	if _, err := db.Exec(badgeArcadeDeploySchema); err != nil {
		log.Printf("badge arcade deploy schema: %v", err)
	}
	http.HandleFunc("/admin/badge-arcade/deployments/", requireClientCert(adminBadgeArcadeDeployments))
	http.HandleFunc("/admin/badge-arcade/deployments/save", requireClientCert(adminBadgeArcadeDeploySave))
	http.HandleFunc("/admin/badge-arcade/deployments/status", requireClientCert(adminBadgeArcadeDeployStatus))
	http.HandleFunc("/admin/badge-arcade/deployments/delete", requireClientCert(adminBadgeArcadeDeployDelete))
	http.HandleFunc("/admin/badge-arcade/deployments/publish", requireClientCert(adminBadgeArcadeDeployPublish))
}

// --- templates (Nintendo machines to copy layouts from) ---

// badgeArcadeDeployTemplate is a template machine plus the regions whose
// weekly archive contains it. Nintendo's EUR, USA and JPN archives hold
// different machines; account-proxy copies a template (and the parts it
// needs) into the regions that lack it, so deployments reach every region.
type badgeArcadeDeployTemplate struct {
	badgearcade.CraneTemplate
	Regions []string
}

func (t badgeArcadeDeployTemplate) AllRegions() bool {
	return len(t.Regions) == len(badgeArcadeDeployRegions)
}

var badgeArcadeDeployRegions = []struct{ Prefix, Name string }{{"GB_en", "EUR"}, {"US_en", "USA"}, {"JP_ja", "JPN"}}

var (
	badgeArcadeTemplatesOnce sync.Once
	badgeArcadeTemplates     []badgeArcadeDeployTemplate
	badgeArcadeTemplatesErr  error
)

// badgeArcadeDeployTemplates lists every usable machine across the regional
// weekly archives: all-region ones first, then without "Difficult" obstacles.
func badgeArcadeDeployTemplates() ([]badgeArcadeDeployTemplate, error) {
	badgeArcadeTemplatesOnce.Do(func() {
		keyPath := os.Getenv("BOSS3DS_KEY_FILE")
		if keyPath == "" {
			keyPath = "/home/nico/boss3ds_key.hex"
		}
		key, err := badgearcade.LoadBOSSKey(keyPath)
		if err != nil {
			badgeArcadeTemplatesErr = err
			return
		}
		byName := map[string]*badgeArcadeDeployTemplate{}
		var order []string
		for _, region := range badgeArcadeDeployRegions {
			raw, err := os.ReadFile("/home/nico/badgearcade-boss-data/" + region.Prefix + "_data_data_v131.dat.boss")
			if err != nil {
				badgeArcadeTemplatesErr = err
				return
			}
			_, payloads, err := badgearcade.ParseBOSS(key, raw)
			if err != nil {
				badgeArcadeTemplatesErr = err
				return
			}
			entries, err := badgearcade.ParseSARC(payloads[0].Content)
			if err != nil {
				badgeArcadeTemplatesErr = err
				return
			}
			wi, err := badgearcade.WeeklyArchiveIndex(entries)
			if err != nil {
				badgeArcadeTemplatesErr = err
				return
			}
			ts, err := badgearcade.CraneTemplates(entries[wi].Data)
			if err != nil {
				badgeArcadeTemplatesErr = err
				return
			}
			for _, t := range ts {
				if byName[t.Name] == nil {
					byName[t.Name] = &badgeArcadeDeployTemplate{CraneTemplate: t}
					order = append(order, t.Name)
				}
				byName[t.Name].Regions = append(byName[t.Name].Regions, region.Name)
			}
		}
		for _, n := range order {
			badgeArcadeTemplates = append(badgeArcadeTemplates, *byName[n])
		}
		sort.SliceStable(badgeArcadeTemplates, func(i, j int) bool {
			a, b := badgeArcadeTemplates[i], badgeArcadeTemplates[j]
			if len(a.Regions) != len(b.Regions) {
				return len(a.Regions) > len(b.Regions)
			}
			if a.Difficult != b.Difficult {
				return !a.Difficult
			}
			return a.Name < b.Name
		})
	})
	return badgeArcadeTemplates, badgeArcadeTemplatesErr
}

// --- page ---

type adminDeployBadge struct {
	ID       int64
	Title    string
	PNID     string
	Selected bool
}

type adminDeployment struct {
	ID        int64
	Name      string
	Template  string
	Slot      string
	Status    string
	Start     time.Time
	End       time.Time
	BadgeIDs  []int64
	ActiveNow bool
}

func badgeArcadeDeploymentsList() []adminDeployment {
	rows, err := db.Query(`SELECT id, name, template, slot, status, start_date, end_date FROM badge_arcade_deployments ORDER BY id DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	today := time.Now().UTC().Truncate(24 * time.Hour)
	var out []adminDeployment
	for rows.Next() {
		var d adminDeployment
		if rows.Scan(&d.ID, &d.Name, &d.Template, &d.Slot, &d.Status, &d.Start, &d.End) == nil {
			d.ActiveNow = d.Status != "off" && !today.Before(d.Start) && !today.After(d.End)
			out = append(out, d)
		}
	}
	for i := range out {
		if br, err := db.Query(`SELECT creation_id FROM badge_arcade_deployment_badges WHERE deployment_id = $1 ORDER BY position, creation_id`, out[i].ID); err == nil {
			for br.Next() {
				var id int64
				if br.Scan(&id) == nil {
					out[i].BadgeIDs = append(out[i].BadgeIDs, id)
				}
			}
			br.Close()
		}
	}
	return out
}

func adminBadgeArcadeDeployments(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/badge-arcade/deployments/" {
		http.NotFound(w, r)
		return
	}
	templates, terr := badgeArcadeDeployTemplates()
	deps := badgeArcadeDeploymentsList()
	today := time.Now().UTC()
	form := adminDeployment{Slot: "hall", Status: "test", Start: today, End: today.AddDate(0, 0, 6), Template: "Pokemon_091"}
	if id, err := strconv.ParseInt(r.URL.Query().Get("edit"), 10, 64); err == nil {
		for _, d := range deps {
			if d.ID == id {
				form = d
			}
		}
	}
	selected := map[int64]bool{}
	for _, id := range form.BadgeIDs {
		selected[id] = true
	}
	var badges []adminDeployBadge
	if rows, err := db.Query(`SELECT id, pid, title FROM badge_arcade_creations WHERE status = 'approved' AND kind = 'badge' ORDER BY id DESC`); err == nil {
		for rows.Next() {
			var b adminDeployBadge
			var pid int64
			if rows.Scan(&b.ID, &pid, &b.Title) == nil {
				b.PNID, _ = pnidForPID(pid)
				b.Selected = selected[b.ID]
				badges = append(badges, b)
			}
		}
		rows.Close()
	}
	data := map[string]any{
		"Deployments": deps, "Templates": templates, "Badges": badges, "Form": form,
		"Msg": r.URL.Query().Get("msg"), "Today": today.Format("2006-01-02"),
	}
	if terr != nil {
		data["TemplateError"] = terr.Error()
	}
	w.Header().Set("Content-Type", "text/html")
	if err := adminDeployTmpl.Execute(w, data); err != nil {
		log.Printf("admin deployments: %v", err)
	}
}

func adminDeployRedirect(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, badgeArcadeDeployBase+"?msg="+template.URLQueryEscaper(msg), http.StatusSeeOther)
}

func adminBadgeArcadeDeploySave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, badgeArcadeDeployBase, http.StatusSeeOther)
		return
	}
	r.ParseForm()
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	name := strings.TrimSpace(r.FormValue("name"))
	slot, status, tmpl := r.FormValue("slot"), r.FormValue("status"), r.FormValue("template")
	start, err1 := time.Parse("2006-01-02", r.FormValue("start"))
	end, err2 := time.Parse("2006-01-02", r.FormValue("end"))
	fail := func(msg string) { adminDeployRedirect(w, r, "Not saved: "+msg) }
	switch {
	case name == "" || len([]rune(name)) > 60:
		fail("the name must be 1-60 characters")
		return
	case slot != "hall" && slot != "training":
		fail("unknown slot")
		return
	case status != "test" && status != "live" && status != "off":
		fail("unknown status")
		return
	case err1 != nil || err2 != nil || end.Before(start) || end.Sub(start) > 366*24*time.Hour:
		fail("the dates must be a range of at most a year")
		return
	}
	templates, err := badgeArcadeDeployTemplates()
	known := false
	var slots int
	for _, t := range templates {
		if t.Name == tmpl {
			known, slots = true, t.PrizeSlots
		}
	}
	if err != nil || !known {
		fail("unknown template machine")
		return
	}
	var badgeIDs []int64
	for _, v := range r.Form["badge"] {
		if bid, err := strconv.ParseInt(v, 10, 64); err == nil {
			var ok bool
			db.QueryRow(`SELECT EXISTS(SELECT 1 FROM badge_arcade_creations WHERE id = $1 AND status = 'approved' AND kind = 'badge')`, bid).Scan(&ok)
			if ok {
				badgeIDs = append(badgeIDs, bid)
			}
		}
	}
	if len(badgeIDs) == 0 || len(badgeIDs) > 20 {
		fail("pick 1-20 approved badges")
		return
	}
	if len(badgeIDs) > slots {
		fail(strconv.Itoa(len(badgeIDs)) + " badges but " + tmpl + " only has " + strconv.Itoa(slots) + " prize spots")
		return
	}
	tx, err := db.Begin()
	if err != nil {
		fail("database error")
		return
	}
	defer tx.Rollback()
	if id == 0 {
		err = tx.QueryRow(`INSERT INTO badge_arcade_deployments (name, template, slot, status, start_date, end_date)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`, name, tmpl, slot, status, start, end).Scan(&id)
	} else {
		_, err = tx.Exec(`UPDATE badge_arcade_deployments SET name = $1, template = $2, slot = $3, status = $4,
			start_date = $5, end_date = $6, updated_at = NOW() WHERE id = $7`, name, tmpl, slot, status, start, end, id)
	}
	if err == nil {
		_, err = tx.Exec(`DELETE FROM badge_arcade_deployment_badges WHERE deployment_id = $1`, id)
	}
	for pos, bid := range badgeIDs {
		if err == nil {
			_, err = tx.Exec(`INSERT INTO badge_arcade_deployment_badges (deployment_id, creation_id, position) VALUES ($1, $2, $3)`, id, bid, pos)
		}
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		fail("database error: " + err.Error())
		return
	}
	adminDeployRedirect(w, r, "Deployment #"+strconv.FormatInt(id, 10)+" saved. Use “Publish now” to rebuild today's package, or it applies from the next daily build.")
}

func adminBadgeArcadeDeployStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, badgeArcadeDeployBase, http.StatusSeeOther)
		return
	}
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	status := r.FormValue("status")
	if status != "test" && status != "live" && status != "off" {
		adminDeployRedirect(w, r, "Unknown status.")
		return
	}
	db.Exec(`UPDATE badge_arcade_deployments SET status = $1, updated_at = NOW() WHERE id = $2`, status, id)
	adminDeployRedirect(w, r, "Deployment #"+strconv.FormatInt(id, 10)+" is now "+status+". Publish to apply it today.")
}

func adminBadgeArcadeDeployDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, badgeArcadeDeployBase, http.StatusSeeOther)
		return
	}
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	db.Exec(`DELETE FROM badge_arcade_deployments WHERE id = $1`, id)
	adminDeployRedirect(w, r, "Deployment #"+strconv.FormatInt(id, 10)+" deleted. Publish to remove it from today's package.")
}

// adminBadgeArcadeDeployPublish asks account-proxy to rebuild today's packages.
func adminBadgeArcadeDeployPublish(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, badgeArcadeDeployBase, http.StatusSeeOther)
		return
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post("http://127.0.0.1:9191/internal/badge-arcade/rebuild", "text/plain", nil)
	if err != nil {
		adminDeployRedirect(w, r, "Publishing failed: "+err.Error())
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		adminDeployRedirect(w, r, "Publishing failed: account-proxy answered "+resp.Status)
		return
	}
	adminDeployRedirect(w, r, "Rebuilding today's packages (takes a few seconds). Consoles pick it up on their next full download (the second pass after starting the game). At most 9 rebuilds per day.")
}

var adminDeployTmpl = template.Must(template.New("admin-deploy").Funcs(template.FuncMap{
	"date":     func(t time.Time) string { return t.Format("2006-01-02") },
	"badgeid":  func(id int64) uint32 { return BadgeArcadeBadgeID(id) },
	"statuses": func() []string { return []string{"test", "live", "off"} },
}).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Badge Arcade deployments</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
body{font-family:system-ui,sans-serif;max-width:1000px;margin:2rem auto;padding:0 1rem;color:#222}
h1{font-size:1.4rem}h2{font-size:1.05rem;margin:1.5rem 0 .6rem}
a{color:#2563eb}
.msg{background:#dcfce7;border:1px solid #bbf7d0;color:#166534;padding:.5rem 1rem;border-radius:6px;margin-bottom:1rem;font-size:.9rem}
.err{background:#fee2e2;border:1px solid #fca5a5;color:#991b1b;padding:.5rem 1rem;border-radius:6px;margin-bottom:1rem;font-size:.9rem}
.note{background:#fff7ed;border:1px solid #fed7aa;border-radius:6px;padding:.75rem 1rem;margin-bottom:1.25rem;font-size:.88rem;line-height:1.45}
table{width:100%;border-collapse:collapse;font-size:.88rem}
th{text-align:left;border-bottom:2px solid #e4e4e7;padding:.45rem .6rem;color:#666;font-weight:600}
td{padding:.45rem .6rem;border-bottom:1px solid #f0f0f0;vertical-align:middle}
.thumbs img{width:40px;height:40px;border-radius:4px;background:repeating-conic-gradient(#e5e7eb 0 25%,#fff 0 50%) 0 0/8px 8px;margin-right:2px}
.pill{display:inline-block;border-radius:999px;padding:.05rem .55rem;font-size:.75rem;font-weight:700}
.pill.live{background:#dcfce7;color:#166534}.pill.test{background:#e0e7ff;color:#3730a3}.pill.off{background:#f3f4f6;color:#6b7280}
.now{font-size:.72rem;color:#166534;font-weight:600}
form.inline{display:inline}
button{font:inherit;cursor:pointer;border:none;border-radius:4px;padding:.3rem .7rem;font-size:.82rem;background:#e0e7ff;color:#3730a3}
button.del{background:#fee2e2;color:#991b1b}
button.primary{background:#2563eb;color:#fff;padding:.45rem 1.1rem}
fieldset{border:1px solid #e4e4e7;border-radius:8px;padding:1rem 1.25rem}
legend{font-weight:600;padding:0 .4rem}
.row{display:flex;gap:1rem;flex-wrap:wrap;margin-bottom:.8rem}
.row label{display:flex;flex-direction:column;gap:.3rem;font-size:.8rem;color:#555;font-weight:600}
input[type=text],input[type=date],select{font:inherit;font-size:.9rem;border:1px solid #d1d5db;border-radius:4px;padding:.35rem .5rem}
.badges{display:grid;grid-template-columns:repeat(auto-fill,minmax(140px,1fr));gap:.5rem;margin:.4rem 0 1rem}
.badges label{display:flex;flex-direction:column;align-items:center;gap:.2rem;border:1px solid #e4e4e7;border-radius:8px;padding:.5rem;font-size:.78rem;cursor:pointer;text-align:center}
.badges label:has(input:checked){border-color:#2563eb;box-shadow:0 0 0 2px rgba(37,99,235,.2)}
.badges img{width:64px;height:64px;border-radius:4px;background:repeating-conic-gradient(#e5e7eb 0 25%,#fff 0 50%) 0 0/8px 8px}
.muted{color:#666;font-size:.8rem}
</style>
</head>
<body>
<p><a href="/inkay/admin/badge-arcade/">← Badge Arcade creations</a></p>
<h1>Badge Arcade deployments</h1>
{{if .Msg}}<div class="msg">{{.Msg}}</div>{{end}}
{{if .TemplateError}}<div class="err">Template machines unavailable: {{.TemplateError}}</div>{{end}}
<div class="note">
A deployment is one machine: a copy of a Nintendo layout filled with approved badges, for an inclusive UTC date range,
either in the <strong>hall</strong> or as the daily <strong>training crane</strong> (one free try per day).
Deployments appear in all three regions (EUR, USA, JPN): when a region's package lacks the template machine, it is copied in from a region that has it, together with the stage and objects it needs.
<strong>test</strong> = only consoles in <code>test-consoles.txt</code> (experimental package), <strong>live</strong> = everyone, <strong>off</strong> = disabled.<br>
The weekly lineup is the same in every region: <strong>Monday–Thursday</strong> show 30 random official machines a day (from all regions), <strong>Friday–Sunday</strong> only the deployments active that day, in random order (the hall can hold at most 30). A deployment's date range decides which weekends it appears on; training deployments also only run Friday–Sunday. Changes apply at the next daily build (00:02 UTC) or right away with <em>Publish now</em>.
</div>

<form method="post" action="/inkay/admin/badge-arcade/deployments/publish" style="margin-bottom:1rem"><button class="primary">Publish now</button> <span class="muted">rebuilds today's packages with the current deployments</span></form>

<h2>Deployments</h2>
<table>
<tr><th>#</th><th>Name</th><th>Where</th><th>Template</th><th>Dates (UTC)</th><th>Badges</th><th>Status</th><th></th></tr>
{{range .Deployments}}
<tr>
<td>{{.ID}}</td><td>{{.Name}}</td><td>{{.Slot}}</td><td>{{.Template}}</td>
<td>{{date .Start}} – {{date .End}}{{if .ActiveNow}}<br><span class="now">active today</span>{{end}}</td>
<td class="thumbs">{{range .BadgeIDs}}<img src="/inkay/admin/badge-arcade/img/{{.}}/home.png" alt="" title="#{{.}}">{{end}}</td>
<td><span class="pill {{.Status}}">{{.Status}}</span></td>
<td>
  <a href="?edit={{.ID}}">edit</a>
  {{$id := .ID}}{{$st := .Status}}{{range $s := statuses}}{{if ne $s $st}}<form class="inline" method="post" action="/inkay/admin/badge-arcade/deployments/status"><input type="hidden" name="id" value="{{$id}}"><input type="hidden" name="status" value="{{$s}}"><button>{{$s}}</button></form>{{end}}{{end}}
  <form class="inline" method="post" action="/inkay/admin/badge-arcade/deployments/delete" onsubmit="return confirm('Delete deployment #{{.ID}}?')"><input type="hidden" name="id" value="{{.ID}}"><button class="del">delete</button></form>
</td>
</tr>
{{else}}<tr><td colspan="8" class="muted">No deployments yet.</td></tr>{{end}}
</table>

<h2>{{if .Form.ID}}Edit deployment #{{.Form.ID}} <a href="./" class="muted">(new instead)</a>{{else}}New deployment{{end}}</h2>
<form method="post" action="/inkay/admin/badge-arcade/deployments/save">
<fieldset>
<input type="hidden" name="id" value="{{.Form.ID}}">
<div class="row">
  <label>Name<input type="text" name="name" value="{{.Form.Name}}" maxlength="60" required placeholder="e.g. Community picks #1"></label>
  <label>Where<select name="slot"><option value="hall" {{if eq .Form.Slot "hall"}}selected{{end}}>Hall machine</option><option value="training" {{if eq .Form.Slot "training"}}selected{{end}}>Training crane</option></select></label>
  <label>Status<select name="status">{{$fs := .Form.Status}}{{range $s := statuses}}<option value="{{$s}}" {{if eq $s $fs}}selected{{end}}>{{$s}}</option>{{end}}</select></label>
</div>
<div class="row">
  <label>From (UTC)<input type="date" name="start" value="{{date .Form.Start}}" required></label>
  <label>To (UTC, inclusive)<input type="date" name="end" value="{{date .Form.End}}" required></label>
  <label>Template machine<select name="template">{{$ft := .Form.Template}}{{range .Templates}}<option value="{{.Name}}" {{if eq .Name $ft}}selected{{end}}>{{.Name}} — {{.PrizeSlots}} spots · {{if .AllRegions}}Nintendo used it in all regions{{else}}from {{range $i, $r := .Regions}}{{if $i}}+{{end}}{{$r}}{{end}}{{end}}{{if .Difficult}} · ⚠ difficult{{end}}</option>{{end}}</select></label>
</div>
<div class="muted">Approved badges (they fill the template's prize spots in turn; one badge fills every spot):</div>
<div class="badges">
{{range .Badges}}<label><input type="checkbox" name="badge" value="{{.ID}}" {{if .Selected}}checked{{end}}><img src="/inkay/admin/badge-arcade/img/{{.ID}}/home.png" alt="">#{{.ID}} {{.Title}}<span class="muted">by {{if .PNID}}{{.PNID}}{{else}}?{{end}} · badge ID {{badgeid .ID}}</span></label>
{{else}}<p class="muted">No approved badges yet — approve some in <a href="/inkay/admin/badge-arcade/">Badge Arcade creations</a>.</p>{{end}}
</div>
<button class="primary">{{if .Form.ID}}Save changes{{else}}Create deployment{{end}}</button>
</fieldset>
</form>
</body>
</html>`))
