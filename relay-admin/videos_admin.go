package main

// Staff review for Revivetendo TV uploads (client certificate, like the rest
// of /admin/). Approving publishes a video into a channel straight away by
// adding it to the catalog; account-proxy serves a video's files (from S3,
// see videos_s3.go) only while the catalog lists them, so unpublishing is
// just removing it again.
//
// eshop-video/catalog.json stays the one file account-proxy serves. This code
// owns only the entries with ids in [videoCatalogIDBase, +1e6): it removes
// them and adds back every live submission, so hand-made entries, channels,
// telops and news are left exactly as they are.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

func registerVideosAdmin() {
	http.HandleFunc("/admin/videos/", requireClientCert(adminVideos))
	http.HandleFunc("/admin/videos/review", requireClientCert(adminVideosReview))
	http.HandleFunc("/admin/videos/media/", requireClientCert(adminVideosMedia))
}

type videoChannel struct {
	ID   int64
	Name string
}

var videoCatalogMu sync.Mutex

func videoCatalogPath() string { return filepath.Join(videoRoot, "catalog.json") }

func videoReadCatalog() (map[string]any, error) {
	raw, err := os.ReadFile(videoCatalogPath())
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var cat map[string]any
	if err := dec.Decode(&cat); err != nil {
		return nil, err
	}
	return cat, nil
}

func jsonInt(v any) int64 {
	switch n := v.(type) {
	case json.Number:
		i, _ := n.Int64()
		return i
	case float64:
		return int64(n)
	case int64:
		return n
	}
	return 0
}

func isUserVideoID(id int64) bool {
	return id >= videoCatalogIDBase && id < videoCatalogIDBase+1_000_000
}

func videoChannels() []videoChannel {
	cat, err := videoReadCatalog()
	if err != nil {
		return nil
	}
	var out []videoChannel
	chans, _ := cat["channels"].([]any)
	for _, c := range chans {
		m, _ := c.(map[string]any)
		name, _ := m["name"].(string)
		out = append(out, videoChannel{ID: jsonInt(m["id"]), Name: name})
	}
	return out
}

// videoSyncCatalog rewrites the user-video part of catalog.json from the
// database: every live submission, newest first, at the top of its channel.
func videoSyncCatalog() error {
	videoCatalogMu.Lock()
	defer videoCatalogMu.Unlock()
	cat, err := videoReadCatalog()
	if err != nil {
		return fmt.Errorf("read catalog: %w", err)
	}

	type live struct {
		id, channel                   int64
		title, desc, uploader, stereo string
		seconds                       int
	}
	rows, err := db.Query(`SELECT id, pid, title, description, seconds, COALESCE(channel_id, 0), stereo FROM eshop_videos
		WHERE status = 'live' ORDER BY reviewed_at DESC, id DESC`)
	if err != nil {
		return err
	}
	var lives []live
	for rows.Next() {
		var l live
		var pid int64
		if rows.Scan(&l.id, &pid, &l.title, &l.desc, &l.seconds, &l.channel, &l.stereo) == nil {
			l.uploader, _ = pnidForPID(pid)
			lives = append(lives, l)
		}
	}
	rows.Close()

	var videos []any
	old, _ := cat["videos"].([]any)
	for _, v := range old {
		if m, ok := v.(map[string]any); ok && isUserVideoID(jsonInt(m["id"])) {
			continue
		}
		videos = append(videos, v)
	}
	chans, _ := cat["channels"].([]any)
	if len(chans) == 0 && len(lives) > 0 {
		return fmt.Errorf("catalog has no channels to put videos in")
	}
	var firstChannel int64
	byChannel := map[int64][]any{}
	for i, c := range chans {
		m, _ := c.(map[string]any)
		if i == 0 {
			firstChannel = jsonInt(m["id"])
		}
		byChannel[jsonInt(m["id"])] = nil
	}
	for _, l := range lives {
		catID := videoCatalogIDBase + l.id
		u := "u/" + strconv.FormatInt(l.id, 10) + "/"
		desc := l.desc
		if l.uploader != "" {
			if desc != "" {
				desc += "\n\n"
			}
			desc += "Uploaded by " + l.uploader
		}
		videos = append(videos, map[string]any{
			"id": catID, "name": l.title, "description": desc,
			"banner": u + "banner.jpg", "thumbnail": u + "thumb.jpg",
			"file": u + "video.moflex", "mp4": u + "video.mp4",
			"width": 400, "height": 240, "dimension": map[bool]string{false: "2d", true: "3d"}[l.stereo != ""], "seconds": l.seconds, "new": true,
		})
		ch := l.channel
		if _, ok := byChannel[ch]; !ok {
			ch = firstChannel
		}
		byChannel[ch] = append(byChannel[ch], catID)
	}
	if videos == nil {
		videos = []any{}
	}
	cat["videos"] = videos
	for _, c := range chans {
		m, _ := c.(map[string]any)
		ids := append([]any{}, byChannel[jsonInt(m["id"])]...)
		list, _ := m["videos"].([]any)
		for _, v := range list {
			if !isUserVideoID(jsonInt(v)) {
				ids = append(ids, v)
			}
		}
		m["videos"] = ids
	}

	out, err := json.MarshalIndent(cat, "", "  ")
	if err != nil {
		return err
	}
	tmp := videoCatalogPath() + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, videoCatalogPath())
}

type adminVideoItem struct {
	ID          int64
	PID         int64
	PNID        string
	Title       string
	Description string
	Status      string
	Seconds     int
	Channel     int64
	ReviewNote  string
	Error       string
	MoflexMB    string
	Stereo      string
	Views       int64
	Likes       int64
	Comments    []adminVideoComment
	CreatedAt   time.Time
	SubmittedAt sql.NullTime
	ReviewedAt  sql.NullTime
}

type adminVideoComment struct {
	ID        int64
	PID       int64
	PNID      string
	Body      string
	CreatedAt time.Time
}

// adminVideoComments lists a video's comments, newest first.
func adminVideoComments(id int64) []adminVideoComment {
	rows, err := db.Query(`SELECT id, pid, body, created_at FROM eshop_video_comments WHERE video_id = $1 ORDER BY created_at DESC LIMIT 200`, id)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []adminVideoComment
	for rows.Next() {
		var c adminVideoComment
		if rows.Scan(&c.ID, &c.PID, &c.Body, &c.CreatedAt) == nil {
			c.PNID, _ = pnidForPID(c.PID)
			out = append(out, c)
		}
	}
	return out
}

var adminVideoStatuses = []string{"submitted", "live", "rejected", "processing", "failed", "uploading"}

func adminVideos(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/videos/" {
		http.NotFound(w, r)
		return
	}
	status := r.URL.Query().Get("status")
	valid := false
	for _, s := range adminVideoStatuses {
		valid = valid || s == status
	}
	if !valid {
		status = "submitted"
	}
	counts := map[string]int{}
	if rows, err := db.Query(`SELECT status, COUNT(*) FROM eshop_videos GROUP BY status`); err == nil {
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
	rows, err := db.Query(`SELECT id, pid, title, description, status, seconds, COALESCE(channel_id, 0), review_note, error, created_at, submitted_at, reviewed_at,
			views, (SELECT COUNT(*) FROM eshop_video_likes l WHERE l.video_id = v.id), stereo
		FROM eshop_videos v WHERE status = $1 ORDER BY `+order+` LIMIT 200`, status)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var items []adminVideoItem
	for rows.Next() {
		var it adminVideoItem
		if rows.Scan(&it.ID, &it.PID, &it.Title, &it.Description, &it.Status, &it.Seconds, &it.Channel, &it.ReviewNote, &it.Error, &it.CreatedAt, &it.SubmittedAt, &it.ReviewedAt, &it.Views, &it.Likes, &it.Stereo) != nil {
			continue
		}
		it.PNID, _ = pnidForPID(it.PID)
		if st, err := os.Stat(filepath.Join(videoSubmissionDir(it.ID), "video.moflex")); err == nil {
			it.MoflexMB = fmt.Sprintf("%.1f MB", float64(st.Size())/(1<<20))
		}
		items = append(items, it)
	}
	rows.Close()
	for i := range items {
		if items[i].Status == "live" || items[i].Status == "rejected" {
			items[i].Comments = adminVideoComments(items[i].ID)
		}
	}
	w.Header().Set("Content-Type", "text/html")
	if err := adminVideosTmpl.Execute(w, map[string]any{
		"Status": status, "Statuses": adminVideoStatuses, "Counts": counts, "Items": items,
		"Channels": videoChannels(), "Msg": r.URL.Query().Get("msg"),
	}); err != nil {
		log.Printf("admin videos: %v", err)
	}
}

func adminVideosReview(w http.ResponseWriter, r *http.Request) {
	back := "/inkay/admin/videos/"
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
		http.Redirect(w, r, back+"?status="+template.URLQueryEscaper(from)+"&msg="+template.URLQueryEscaper(msg), http.StatusSeeOther)
	}
	action := r.FormValue("action")
	var res sql.Result
	var err error
	switch action {
	case "approve":
		channel, _ := strconv.ParseInt(r.FormValue("channel"), 10, 64)
		res, err = db.Exec(`UPDATE eshop_videos SET status = 'live', channel_id = $2, review_note = $3, reviewed_at = NOW(), updated_at = NOW()
			WHERE id = $1 AND status IN ('submitted', 'rejected')`, id, channel, note)
	case "reject":
		if note == "" {
			redirect("A rejection needs a note for the uploader.")
			return
		}
		res, err = db.Exec(`UPDATE eshop_videos SET status = 'rejected', review_note = $2, reviewed_at = NOW(), updated_at = NOW()
			WHERE id = $1 AND status IN ('submitted', 'live')`, id, note)
	case "reopen":
		res, err = db.Exec(`UPDATE eshop_videos SET status = 'submitted', reviewed_at = NULL, updated_at = NOW(),
			submitted_at = COALESCE(submitted_at, NOW()) WHERE id = $1 AND status = 'rejected'`, id)
	case "delcomment":
		cid, _ := strconv.ParseInt(r.FormValue("comment"), 10, 64)
		if _, err := db.Exec(`DELETE FROM eshop_video_comments WHERE id = $1 AND video_id = $2`, cid, id); err != nil {
			redirect("Delete failed: " + err.Error())
			return
		}
		log.Printf("videos: comment %d on #%d deleted by staff", cid, id)
		redirect("Comment deleted.")
		return
	case "delete":
		var status string
		if err := db.QueryRow(`SELECT status FROM eshop_videos WHERE id = $1`, id).Scan(&status); err != nil {
			redirect("Unknown video.")
			return
		}
		if status == "processing" {
			redirect("Wait until the conversion is done.")
			return
		}
		if err := videoRemove(id, status == "live"); err != nil {
			redirect("Delete failed: " + err.Error())
			return
		}
		log.Printf("videos: #%d deleted by staff (was %s)", id, status)
		redirect("Video #" + strconv.FormatInt(id, 10) + " deleted.")
		return
	default:
		redirect("Unknown action.")
		return
	}
	if err != nil {
		redirect("Database error: " + err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		redirect("Nothing changed (the video was changed meanwhile?).")
		return
	}
	if err := videoSyncCatalog(); err != nil {
		log.Printf("videos: catalog sync after %s #%d: %v", action, id, err)
		redirect("Saved, but updating the eShop catalog failed: " + err.Error())
		return
	}
	log.Printf("videos: #%d %s by staff", id, action)
	done := map[string]string{"approve": "is live on Revivetendo TV", "reject": "rejected", "reopen": "back in review"}[action]
	redirect("Video #" + strconv.FormatInt(id, 10) + " " + done + ".")
}

func adminVideosMedia(w http.ResponseWriter, r *http.Request) {
	id, name, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/admin/videos/media/"), "/")
	vid, err := strconv.ParseInt(id, 10, 64)
	if !ok || err != nil {
		http.NotFound(w, r)
		return
	}
	serveVideoFile(w, r, vid, name)
}

var adminVideosTmpl = template.Must(template.New("admin-videos").Funcs(template.FuncMap{
	"count": func(m map[string]int, k string) int { return m[k] },
	"mmss":  func(s int) string { return fmt.Sprintf("%d:%02d", s/60, s%60) },
}).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Revivetendo TV videos</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
body{font-family:system-ui,sans-serif;max-width:1000px;margin:2rem auto;padding:0 1rem;color:#222}
h1{font-size:1.4rem}
a{color:#2563eb}
.tabs{display:flex;gap:.5rem;margin:1rem 0 1.5rem;flex-wrap:wrap}
.tabs a{text-decoration:none;border:1px solid #e4e4e7;border-radius:999px;padding:.3rem .9rem;color:#333;font-size:.9rem}
.tabs a.on{background:#2563eb;border-color:#2563eb;color:#fff}
.msg{background:#dcfce7;border:1px solid #bbf7d0;color:#166534;padding:.5rem 1rem;border-radius:6px;margin-bottom:1rem;font-size:.9rem}
.item{display:grid;grid-template-columns:427px 1fr;gap:1.25rem;border:1px solid #e4e4e7;border-radius:10px;padding:1rem;margin-bottom:1rem}
@media (max-width:800px){.item{grid-template-columns:1fr}}
video{width:427px;max-width:100%;height:240px;background:#000;border-radius:6px}
.imgs{display:flex;gap:.75rem;align-items:flex-end;margin-top:.5rem}
.imgs figure{margin:0;text-align:center;font-size:.7rem;color:#666}
.imgs img{display:block;border-radius:4px;background:#f1f5f9}
h2{font-size:1.05rem;margin:0 0 .25rem;word-break:break-word}
.meta{font-size:.8rem;color:#666;margin-bottom:.5rem}
.desc{font-size:.875rem;white-space:pre-wrap;margin-bottom:.6rem}
form{display:flex;gap:.4rem;align-items:flex-start;margin:.4rem 0 0;flex-wrap:wrap}
textarea{font:inherit;font-size:.85rem;border:1px solid #d1d5db;border-radius:4px;padding:.3rem .5rem;width:260px;min-height:2.2rem}
select{font:inherit;font-size:.85rem}
button{font:inherit;cursor:pointer;border:none;border-radius:4px;padding:.35rem .8rem;font-size:.85rem}
.ok{background:#dcfce7;color:#166534}.no{background:#fee2e2;color:#991b1b}.re{background:#e0e7ff;color:#3730a3}.del{background:#f4f4f5;color:#52525b}
.note{font-size:.85rem;background:#fff7ed;border:1px solid #fed7aa;border-radius:6px;padding:.4rem .7rem;margin-bottom:.5rem;white-space:pre-wrap}
.empty{color:#666}
.comments{margin:.6rem 0;font-size:.85rem}.comments summary{cursor:pointer;color:#2563eb}
.cmt{border-top:1px solid #f1f1f4;padding:.4rem 0;white-space:pre-wrap;word-break:break-word}.cmt .who{color:#666;font-size:.75rem}.cmt form{margin:.2rem 0 0}
</style>
</head>
<body>
<p><a href="/inkay/admin/">← Back to admin</a></p>
<h1>Revivetendo TV videos</h1>
<p class="meta">Players upload these on <a href="/inkay/my/videos" target="_blank">My Videos</a>. Approving puts a video live in the chosen channel right away, in the 3DS and Wii U eShops; rejecting a live video takes it down. The preview is the Wii U version; the 3DS gets the same picture at 400×240.</p>
{{if .Msg}}<div class="msg">{{.Msg}}</div>{{end}}
<div class="tabs">{{$st := .Status}}{{$c := .Counts}}{{range .Statuses}}<a href="?status={{.}}" class="{{if eq . $st}}on{{end}}">{{.}} ({{count $c .}})</a>{{end}}</div>
{{$chans := .Channels}}
{{range .Items}}
<div class="item">
  <div>
    {{if or (eq .Status "submitted") (eq .Status "live") (eq .Status "rejected")}}
    <video controls preload="none" poster="/inkay/admin/videos/media/{{.ID}}/banner.jpg" src="/inkay/admin/videos/media/{{.ID}}/video.mp4"></video>
    <div class="imgs">
      <figure><img src="/inkay/admin/videos/media/{{.ID}}/thumb.jpg" width="120" height="88" alt=""><figcaption>thumbnail</figcaption></figure>
      <figure><img src="/inkay/admin/videos/media/{{.ID}}/banner.jpg" width="200" height="84" alt=""><figcaption>banner</figcaption></figure>
    </div>
    {{else}}<div class="meta">No preview yet ({{.Status}}).</div>{{end}}
  </div>
  <div>
    <h2>#{{.ID}} {{.Title}}</h2>
    <div class="meta">by <strong>{{if .PNID}}{{.PNID}}{{else}}?{{end}}</strong> (PID {{.PID}}){{if .Seconds}} · {{mmss .Seconds}}{{end}}{{if .MoflexMB}} · 3DS file {{.MoflexMB}}{{end}}{{if .Stereo}} · <strong>3D</strong> ({{.Stereo}}; preview shows the left eye){{end}}{{if or (eq .Status "live") (eq .Status "rejected")}} · {{.Views}} views · {{.Likes}} likes · {{len .Comments}} comments{{end}}<br>
      uploaded {{.CreatedAt.Format "2006-01-02 15:04"}}{{if .ReviewedAt.Valid}} · reviewed {{.ReviewedAt.Time.Format "2006-01-02 15:04"}}{{end}}</div>
    {{if .Description}}<div class="desc">{{.Description}}</div>{{end}}
    {{if .Error}}<div class="note">Conversion: {{.Error}}</div>{{end}}
    {{if .ReviewNote}}<div class="note">Note: {{.ReviewNote}}</div>{{end}}
    {{if or (eq .Status "submitted") (eq .Status "rejected")}}
    <form method="post" action="/inkay/admin/videos/review"><input type="hidden" name="id" value="{{.ID}}"><input type="hidden" name="from" value="{{.Status}}"><input type="hidden" name="action" value="approve">
      <select name="channel">{{range $chans}}<option value="{{.ID}}">{{.Name}}</option>{{end}}</select>
      <button class="ok">Approve &amp; publish</button></form>
    {{end}}
    {{if or (eq .Status "submitted") (eq .Status "live")}}
    <form method="post" action="/inkay/admin/videos/review"><input type="hidden" name="id" value="{{.ID}}"><input type="hidden" name="from" value="{{.Status}}"><input type="hidden" name="action" value="reject"><textarea name="note" placeholder="Reason, shown to the uploader" required></textarea><button class="no">{{if eq .Status "live"}}Take down{{else}}Reject{{end}}</button></form>
    {{end}}
    {{if eq .Status "rejected"}}
    <form method="post" action="/inkay/admin/videos/review"><input type="hidden" name="id" value="{{.ID}}"><input type="hidden" name="from" value="{{.Status}}"><input type="hidden" name="action" value="reopen"><button class="re">Back to review</button></form>
    {{end}}
    {{if .Comments}}<details class="comments"><summary>Comments ({{len .Comments}})</summary>
    {{$vid := .ID}}{{$st2 := .Status}}{{range .Comments}}<div class="cmt"><span class="who">{{if .PNID}}{{.PNID}}{{else}}PID {{.PID}}{{end}} · {{.CreatedAt.Format "2006-01-02 15:04"}}</span><div>{{.Body}}</div>
    <form method="post" action="/inkay/admin/videos/review"><input type="hidden" name="id" value="{{$vid}}"><input type="hidden" name="from" value="{{$st2}}"><input type="hidden" name="action" value="delcomment"><input type="hidden" name="comment" value="{{.ID}}"><button class="del">Delete comment</button></form></div>{{end}}
    </details>{{end}}
    {{if ne .Status "processing"}}
    <form method="post" action="/inkay/admin/videos/review" onsubmit="return confirm('Delete video #{{.ID}} and its files for good?')"><input type="hidden" name="id" value="{{.ID}}"><input type="hidden" name="from" value="{{.Status}}"><input type="hidden" name="action" value="delete"><button class="del">Delete</button></form>
    {{end}}
  </div>
</div>
{{else}}<p class="empty">Nothing here.</p>{{end}}
</body>
</html>`))
