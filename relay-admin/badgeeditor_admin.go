package main

// Admin review queue for Badge Arcade creations (client certificate, like the
// rest of /admin/). Approved creations are what deployments put into the
// arcade.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"html/template"
	"image"
	"image/color"
	"image/draw"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Happynico7504/badgearcade"
)

// BadgeArcadeBadgeID is the badge ID an approved creation gets in the game:
// custom badges use 900,000,000+ (Nintendo's highest is 80,100,000).
func BadgeArcadeBadgeID(creationID int64) uint32 { return uint32(900000000 + creationID) }

func registerBadgeEditorAdmin() {
	http.HandleFunc("/admin/badge-arcade/", requireClientCert(adminBadgeArcade))
	http.HandleFunc("/admin/badge-arcade/review", requireClientCert(adminBadgeArcadeReview))
	http.HandleFunc("/admin/badge-arcade/img/", requireClientCert(adminBadgeArcadeImage))
}

var badgeArcadeLangNames = map[int]string{
	0: "Japanese", 1: "English", 2: "French", 3: "German", 4: "Italian", 5: "Spanish",
	6: "Chinese", 7: "Korean", 8: "Dutch", 9: "Portuguese", 10: "Russian", 11: "Chinese (Trad.)",
}

type adminBadgeArcadeName struct{ Lang, Name string }

type adminBadgeArcadeItem struct {
	ID          int64
	PID         int64
	PNID        string
	Kind        string
	Title       string
	Status      string
	ReviewNote  string
	Names       []adminBadgeArcadeName // English first, then languages that differ
	Outline     string                 // "automatic" / "N custom shapes"
	BadgeID     uint32
	UpdatedAt   time.Time
	SubmittedAt sql.NullTime
	ReviewedAt  sql.NullTime
}

var adminBadgeArcadeStatuses = []string{"submitted", "approved", "rejected", "draft"}

func adminBadgeArcade(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/badge-arcade/" {
		http.NotFound(w, r)
		return
	}
	status := r.URL.Query().Get("status")
	valid := false
	for _, s := range adminBadgeArcadeStatuses {
		valid = valid || s == status
	}
	if !valid {
		status = "submitted"
	}
	counts := map[string]int{}
	if rows, err := db.Query(`SELECT status, COUNT(*) FROM badge_arcade_creations GROUP BY status`); err == nil {
		for rows.Next() {
			var s string
			var n int
			if rows.Scan(&s, &n) == nil {
				counts[s] = n
			}
		}
		rows.Close()
	}
	order := "submitted_at ASC NULLS LAST" // oldest submission first
	if status != "submitted" {
		order = "updated_at DESC"
	}
	rows, err := db.Query(`SELECT id, pid, kind, title, status, review_note, spec, updated_at, submitted_at, reviewed_at
		FROM badge_arcade_creations WHERE status = $1 ORDER BY `+order+` LIMIT 200`, status)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var items []adminBadgeArcadeItem
	for rows.Next() {
		var it adminBadgeArcadeItem
		var spec []byte
		if err := rows.Scan(&it.ID, &it.PID, &it.Kind, &it.Title, &it.Status, &it.ReviewNote, &spec, &it.UpdatedAt, &it.SubmittedAt, &it.ReviewedAt); err != nil {
			continue
		}
		it.PNID, _ = pnidForPID(it.PID)
		it.BadgeID = BadgeArcadeBadgeID(it.ID)
		var sp badgeEditorSpec
		json.Unmarshal(spec, &sp)
		en := sp.Names[1]
		it.Names = append(it.Names, adminBadgeArcadeName{"English", en})
		for i := 0; i < 12; i++ {
			if i != 1 && sp.Names[i] != "" && sp.Names[i] != en {
				it.Names = append(it.Names, adminBadgeArcadeName{badgeArcadeLangNames[i], sp.Names[i]})
			}
		}
		it.Outline = "automatic"
		if len(sp.Collision) > 0 {
			it.Outline = strconv.Itoa(len(sp.Collision)) + " custom shape(s)"
		}
		items = append(items, it)
	}
	w.Header().Set("Content-Type", "text/html")
	if err := adminBadgeArcadeTmpl.Execute(w, map[string]any{
		"Status": status, "Statuses": adminBadgeArcadeStatuses, "Counts": counts, "Items": items, "Msg": r.URL.Query().Get("msg"),
	}); err != nil {
		log.Printf("admin badge arcade: %v", err)
	}
}

func adminBadgeArcadeReview(w http.ResponseWriter, r *http.Request) {
	back := "/inkay/admin/badge-arcade/"
	if r.Method != http.MethodPost {
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	note := strings.TrimSpace(r.FormValue("note"))
	if len([]rune(note)) > 500 {
		note = string([]rune(note)[:500])
	}
	from := r.FormValue("from")
	redirect := func(msg string) {
		http.Redirect(w, r, back+"?status="+from+"&msg="+template.URLQueryEscaper(msg), http.StatusSeeOther)
	}
	var res sql.Result
	var err error
	switch r.FormValue("action") {
	case "approve":
		res, err = db.Exec(`UPDATE badge_arcade_creations SET status = 'approved', review_note = $2, reviewed_at = NOW()
			WHERE id = $1 AND status IN ('submitted', 'rejected')`, id, note)
	case "reject":
		if note == "" {
			redirect("A rejection needs a note for the creator.")
			return
		}
		res, err = db.Exec(`UPDATE badge_arcade_creations SET status = 'rejected', review_note = $2, reviewed_at = NOW()
			WHERE id = $1 AND status IN ('submitted', 'approved')`, id, note)
	case "reopen":
		res, err = db.Exec(`UPDATE badge_arcade_creations SET status = 'submitted', reviewed_at = NULL,
			submitted_at = COALESCE(submitted_at, NOW()) WHERE id = $1 AND status IN ('approved', 'rejected')`, id)
	default:
		redirect("Unknown action.")
		return
	}
	if err != nil {
		redirect("Database error: " + err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		redirect("Nothing changed (the creation was changed meanwhile?).")
		return
	}
	done := map[string]string{"approve": "approved", "reject": "rejected", "reopen": "back in review"}[r.FormValue("action")]
	redirect("Creation #" + strconv.FormatInt(id, 10) + " " + done + ".")
}

// adminBadgeArcadeImage renders /admin/badge-arcade/img/{id}/{art|home|machine}.png.
func adminBadgeArcadeImage(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/admin/badge-arcade/img/"), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var art, spec []byte
	if err := db.QueryRow(`SELECT art, spec FROM badge_arcade_creations WHERE id = $1`, id).Scan(&art, &spec); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=60")
	if parts[1] == "art.png" {
		w.Write(art)
		return
	}
	img, _, err := image.Decode(bytes.NewReader(art))
	if err != nil {
		http.Error(w, "bad art", http.StatusInternalServerError)
		return
	}
	var sp badgeEditorSpec
	json.Unmarshal(spec, &sp)
	p := badgearcade.BuildPrize(badgearcade.PrizeSpec{BadgeID: BadgeArcadeBadgeID(id), FileName: "Pr_Review", Category: "Review", Image: img, Collision: sp.Collision})
	switch parts[1] {
	case "home.png":
		w.Write(pngBytes(p.Image64()))
	case "machine.png":
		// The badge as it sits in a machine: texture over its shadow.
		out := image.NewNRGBA(image.Rect(0, 0, 128, 128))
		draw.Draw(out, out.Bounds(), &image.Uniform{color.NRGBA{0xd8, 0xee, 0xff, 0xff}}, image.Point{}, draw.Src)
		draw.Draw(out, out.Bounds(), badgearcade.DecodeETC1A4(p.Texture[0x4000:], 128, 128), image.Point{}, draw.Over)
		draw.Draw(out, out.Bounds(), badgearcade.DecodeETC1A4(p.Texture[:0x4000], 128, 128), image.Point{}, draw.Over)
		w.Write(pngBytes(out))
	default:
		http.NotFound(w, r)
	}
}

var adminBadgeArcadeTmpl = template.Must(template.New("admin-badge-arcade").Funcs(template.FuncMap{
	"count": func(m map[string]int, k string) int { return m[k] },
}).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Badge Arcade creations</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
body{font-family:system-ui,sans-serif;max-width:1000px;margin:2rem auto;padding:0 1rem;color:#222}
h1{font-size:1.4rem}
a{color:#2563eb}
.tabs{display:flex;gap:.5rem;margin:1rem 0 1.5rem;flex-wrap:wrap}
.tabs a{text-decoration:none;border:1px solid #e4e4e7;border-radius:999px;padding:.3rem .9rem;color:#333;font-size:.9rem}
.tabs a.on{background:#2563eb;border-color:#2563eb;color:#fff}
.msg{background:#dcfce7;border:1px solid #bbf7d0;color:#166534;padding:.5rem 1rem;border-radius:6px;margin-bottom:1rem;font-size:.9rem}
.item{display:grid;grid-template-columns:auto 1fr;gap:1.25rem;border:1px solid #e4e4e7;border-radius:10px;padding:1rem;margin-bottom:1rem}
.imgs{display:flex;gap:.75rem;align-items:flex-end}
.imgs figure{margin:0;text-align:center;font-size:.7rem;color:#666}
.imgs img{display:block;border-radius:6px;background:repeating-conic-gradient(#e5e7eb 0 25%,#fff 0 50%) 0 0/12px 12px}
.imgs .big{width:128px;height:128px}.imgs .home{width:128px;height:128px;image-rendering:pixelated}
h2{font-size:1.05rem;margin:0 0 .25rem}
.meta{font-size:.8rem;color:#666;margin-bottom:.5rem}
table{border-collapse:collapse;font-size:.85rem;margin-bottom:.6rem}
td{padding:.15rem .6rem .15rem 0;vertical-align:top;white-space:pre-wrap}
td.l{color:#666;font-weight:600}
form{display:inline-flex;gap:.4rem;align-items:flex-start;margin:.25rem .4rem 0 0}
textarea{font:inherit;font-size:.85rem;border:1px solid #d1d5db;border-radius:4px;padding:.3rem .5rem;width:260px;min-height:2.2rem}
button{font:inherit;cursor:pointer;border:none;border-radius:4px;padding:.35rem .8rem;font-size:.85rem}
.ok{background:#dcfce7;color:#166534}.no{background:#fee2e2;color:#991b1b}.re{background:#e0e7ff;color:#3730a3}
.note{font-size:.85rem;background:#fff7ed;border:1px solid #fed7aa;border-radius:6px;padding:.4rem .7rem;margin-bottom:.5rem}
.empty{color:#666}
</style>
</head>
<body>
<p><a href="/inkay/admin/">← Back to admin</a></p>
<h1>Badge Arcade creations</h1>
<p class="meta">Players make these in the <a href="/inkay/my/badge-arcade/" target="_blank">badge editor</a>. Approved creations can be deployed to the arcade.</p>
{{if .Msg}}<div class="msg">{{.Msg}}</div>{{end}}
<div class="tabs">{{$st := .Status}}{{$c := .Counts}}{{range .Statuses}}<a href="?status={{.}}" class="{{if eq . $st}}on{{end}}">{{.}} ({{count $c .}})</a>{{end}}</div>
{{range .Items}}
<div class="item">
  <div class="imgs">
    <figure><img class="big" src="/inkay/admin/badge-arcade/img/{{.ID}}/art.png" alt=""><figcaption>artwork</figcaption></figure>
    <figure><img class="home" src="/inkay/admin/badge-arcade/img/{{.ID}}/home.png" alt=""><figcaption>HOME Menu (64×64)</figcaption></figure>
    <figure><img class="big" src="/inkay/admin/badge-arcade/img/{{.ID}}/machine.png" alt=""><figcaption>in the machine</figcaption></figure>
  </div>
  <div>
    <h2>#{{.ID}} {{.Title}}</h2>
    <div class="meta">by <strong>{{if .PNID}}{{.PNID}}{{else}}?{{end}}</strong> (PID {{.PID}}) · {{.Kind}} · outline {{.Outline}} · badge ID {{.BadgeID}}<br>
      {{if .SubmittedAt.Valid}}submitted {{.SubmittedAt.Time.Format "2006-01-02 15:04"}} · {{end}}{{if .ReviewedAt.Valid}}reviewed {{.ReviewedAt.Time.Format "2006-01-02 15:04"}} · {{end}}updated {{.UpdatedAt.Format "2006-01-02 15:04"}}</div>
    <table>{{range .Names}}<tr><td class="l">{{.Lang}}</td><td>{{.Name}}</td></tr>{{end}}</table>
    {{if .ReviewNote}}<div class="note">Note: {{.ReviewNote}}</div>{{end}}
    {{if or (eq .Status "submitted") (eq .Status "rejected")}}
    <form method="post" action="/inkay/admin/badge-arcade/review"><input type="hidden" name="id" value="{{.ID}}"><input type="hidden" name="from" value="{{.Status}}"><input type="hidden" name="action" value="approve"><button class="ok">Approve</button></form>
    {{end}}
    {{if or (eq .Status "submitted") (eq .Status "approved")}}
    <form method="post" action="/inkay/admin/badge-arcade/review"><input type="hidden" name="id" value="{{.ID}}"><input type="hidden" name="from" value="{{.Status}}"><input type="hidden" name="action" value="reject"><textarea name="note" placeholder="Reason, shown to the creator" required></textarea><button class="no">Reject</button></form>
    {{end}}
    {{if or (eq .Status "approved") (eq .Status "rejected")}}
    <form method="post" action="/inkay/admin/badge-arcade/review"><input type="hidden" name="id" value="{{.ID}}"><input type="hidden" name="from" value="{{.Status}}"><input type="hidden" name="action" value="reopen"><button class="re">Back to review</button></form>
    {{end}}
  </div>
</div>
{{else}}<p class="empty">Nothing here.</p>{{end}}
</body>
</html>`))
