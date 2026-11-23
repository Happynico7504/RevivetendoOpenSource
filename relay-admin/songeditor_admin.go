package main

// Admin review queue for custom songs (client certificate, like the rest
// of /admin/). Mirrors badgeeditor_admin.go's shape. Approving a song here
// does NOT yet put it live for anyone to play — there is no deployment
// pipeline for songs yet (unlike Badge Arcade's deployments), so for now
// an approved song still needs to be wired into a private_songs slot by
// hand. That's the next piece to build, not this one.

import (
	"database/sql"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func registerSongEditorAdmin() {
	http.HandleFunc("/admin/songeditor/", requireClientCert(adminSongEditor))
	http.HandleFunc("/admin/songeditor/review", requireClientCert(adminSongEditorReview))
	http.HandleFunc("/admin/songeditor/download/", requireClientCert(adminSongEditorDownload))
}

type adminSongEditorItem struct {
	ID          int64
	PID         int64
	PNID        string
	Title       string
	Status      string
	ReviewNote  string
	NotesMode   string
	BuildError  string
	HasBuilt    bool
	UpdatedAt   time.Time
	SubmittedAt sql.NullTime
	ReviewedAt  sql.NullTime
}

var adminSongEditorStatuses = []string{"submitted", "approved", "rejected", "draft"}

func adminSongEditor(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/songeditor/" {
		http.NotFound(w, r)
		return
	}
	status := r.URL.Query().Get("status")
	valid := false
	for _, s := range adminSongEditorStatuses {
		valid = valid || s == status
	}
	if !valid {
		status = "submitted"
	}
	counts := map[string]int{}
	if rows, err := db.Query(`SELECT status, COUNT(*) FROM wiikaraoke_song_creations GROUP BY status`); err == nil {
		for rows.Next() {
			var s string
			var n int
			if rows.Scan(&s, &n) == nil {
				counts[s] = n
			}
		}
		rows.Close()
	}
	order := "submitted_at ASC NULLS LAST"
	if status != "submitted" {
		order = "updated_at DESC"
	}
	rows, err := db.Query(`SELECT id, pid, title, status, review_note, spec->>'notes_mode', build_error,
			built IS NOT NULL, updated_at, submitted_at, reviewed_at
		FROM wiikaraoke_song_creations WHERE status = $1 ORDER BY `+order+` LIMIT 200`, status)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var items []adminSongEditorItem
	for rows.Next() {
		var it adminSongEditorItem
		var notesMode sql.NullString
		if err := rows.Scan(&it.ID, &it.PID, &it.Title, &it.Status, &it.ReviewNote, &notesMode, &it.BuildError,
			&it.HasBuilt, &it.UpdatedAt, &it.SubmittedAt, &it.ReviewedAt); err != nil {
			continue
		}
		it.NotesMode = notesMode.String
		it.PNID, _ = pnidForPID(it.PID)
		items = append(items, it)
	}
	w.Header().Set("Content-Type", "text/html")
	if err := adminSongEditorTmpl.Execute(w, map[string]any{
		"Status": status, "Statuses": adminSongEditorStatuses, "Counts": counts, "Items": items, "Msg": r.URL.Query().Get("msg"),
	}); err != nil {
		log.Printf("admin song editor: %v", err)
	}
}

func adminSongEditorReview(w http.ResponseWriter, r *http.Request) {
	back := "/inkay/admin/songeditor/"
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
		res, err = db.Exec(`UPDATE wiikaraoke_song_creations SET status = 'approved', review_note = $2, reviewed_at = NOW()
			WHERE id = $1 AND status IN ('submitted', 'rejected') AND built IS NOT NULL`, id, note)
	case "reject":
		if note == "" {
			redirect("A rejection needs a note for the creator.")
			return
		}
		res, err = db.Exec(`UPDATE wiikaraoke_song_creations SET status = 'rejected', review_note = $2, reviewed_at = NOW()
			WHERE id = $1 AND status IN ('submitted', 'approved')`, id, note)
	case "reopen":
		res, err = db.Exec(`UPDATE wiikaraoke_song_creations SET status = 'submitted', reviewed_at = NULL,
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
		redirect("Nothing changed (missing a successful build, or the song was changed meanwhile?).")
		return
	}
	done := map[string]string{"approve": "approved", "reject": "rejected", "reopen": "back in review"}[r.FormValue("action")]
	redirect("Song #" + strconv.FormatInt(id, 10) + " " + done + ".")
}

func adminSongEditorDownload(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/admin/songeditor/download/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var data []byte
	if err := db.QueryRow(`SELECT built FROM wiikaraoke_song_creations WHERE id = $1`, id).Scan(&data); err != nil || data == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="song-`+idStr+`.ujk"`)
	w.Write(data)
}

var adminSongEditorTmpl = template.Must(template.New("admin-songeditor").Funcs(template.FuncMap{
	"count": func(m map[string]int, k string) int { return m[k] },
}).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Wii Karaoke U songs</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
body{font-family:system-ui,sans-serif;max-width:900px;margin:2rem auto;padding:0 1rem;color:#222}
h1{font-size:1.4rem}
a{color:#2563eb}
.tabs{display:flex;gap:.5rem;margin:1rem 0 1.5rem;flex-wrap:wrap}
.tabs a{text-decoration:none;border:1px solid #e4e4e7;border-radius:999px;padding:.3rem .9rem;color:#333;font-size:.9rem}
.tabs a.on{background:#2563eb;border-color:#2563eb;color:#fff}
.msg{background:#dcfce7;border:1px solid #bbf7d0;color:#166534;padding:.5rem 1rem;border-radius:6px;margin-bottom:1rem;font-size:.9rem}
.item{border:1px solid #e4e4e7;border-radius:10px;padding:1rem;margin-bottom:1rem}
h2{font-size:1.05rem;margin:0 0 .25rem}
.meta{font-size:.8rem;color:#666;margin-bottom:.5rem}
form{display:inline-flex;gap:.4rem;align-items:flex-start;margin:.25rem .4rem 0 0}
textarea{font:inherit;font-size:.85rem;border:1px solid #d1d5db;border-radius:4px;padding:.3rem .5rem;width:260px;min-height:2.2rem}
button{font:inherit;cursor:pointer;border:none;border-radius:4px;padding:.35rem .8rem;font-size:.85rem}
.ok{background:#dcfce7;color:#166534}.no{background:#fee2e2;color:#991b1b}.re{background:#e0e7ff;color:#3730a3}
.note{font-size:.85rem;background:#fff7ed;border:1px solid #fed7aa;border-radius:6px;padding:.4rem .7rem;margin-bottom:.5rem}
.err{font-size:.85rem;background:#fee2e2;border:1px solid #fecaca;color:#991b1b;border-radius:6px;padding:.4rem .7rem;margin-bottom:.5rem}
.empty{color:#666}
</style>
</head>
<body>
<p><a href="/inkay/admin/">← Back to admin</a></p>
<h1>Wii Karaoke U songs</h1>
<p class="meta">Players make these in the <a href="/inkay/my/songeditor/" target="_blank">song editor</a>. There is no deployment pipeline yet — an approved song still needs to be wired into a private_songs slot by hand.</p>
{{if .Msg}}<div class="msg">{{.Msg}}</div>{{end}}
<div class="tabs">{{$st := .Status}}{{$c := .Counts}}{{range .Statuses}}<a href="?status={{.}}" class="{{if eq . $st}}on{{end}}">{{.}} ({{count $c .}})</a>{{end}}</div>
{{range .Items}}
<div class="item">
  <h2>#{{.ID}} {{.Title}}</h2>
  <div class="meta">by <strong>{{if .PNID}}{{.PNID}}{{else}}?{{end}}</strong> (PID {{.PID}}) · notes mode: {{if .NotesMode}}{{.NotesMode}}{{else}}?{{end}} ·
    {{if .HasBuilt}}<a href="/inkay/admin/songeditor/download/{{.ID}}">download .ujk</a>{{else}}not built{{end}}<br>
    {{if .SubmittedAt.Valid}}submitted {{.SubmittedAt.Time.Format "2006-01-02 15:04"}} · {{end}}{{if .ReviewedAt.Valid}}reviewed {{.ReviewedAt.Time.Format "2006-01-02 15:04"}} · {{end}}updated {{.UpdatedAt.Format "2006-01-02 15:04"}}</div>
  {{if .BuildError}}<div class="err">Build error: {{.BuildError}}</div>{{end}}
  {{if .ReviewNote}}<div class="note">Note: {{.ReviewNote}}</div>{{end}}
  {{if or (eq .Status "submitted") (eq .Status "rejected")}}
  <form method="post" action="/inkay/admin/songeditor/review"><input type="hidden" name="id" value="{{.ID}}"><input type="hidden" name="from" value="{{.Status}}"><input type="hidden" name="action" value="approve"><button class="ok">Approve</button></form>
  {{end}}
  {{if or (eq .Status "submitted") (eq .Status "approved")}}
  <form method="post" action="/inkay/admin/songeditor/review"><input type="hidden" name="id" value="{{.ID}}"><input type="hidden" name="from" value="{{.Status}}"><input type="hidden" name="action" value="reject"><textarea name="note" placeholder="Reason, shown to the creator" required></textarea><button class="no">Reject</button></form>
  {{end}}
  {{if or (eq .Status "approved") (eq .Status "rejected")}}
  <form method="post" action="/inkay/admin/songeditor/review"><input type="hidden" name="id" value="{{.ID}}"><input type="hidden" name="from" value="{{.Status}}"><input type="hidden" name="action" value="reopen"><button class="re">Back to review</button></form>
  {{end}}
</div>
{{else}}<p class="empty">Nothing here.</p>{{end}}
</body>
</html>`))
