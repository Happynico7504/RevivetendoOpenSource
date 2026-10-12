package main

// Revivetendo TV channels. A channel is one entry in catalog.json's
// "channels" list (a samurai "directory" on the 3DS, a row on the Wii U page);
// its icon (102x76) and banner (400x168) are jpgs in eshop-video/media/i,
// which account-proxy serves straight from disk.
//
// Staff add and edit channels on /admin/videos/?status=channels. Players can
// submit their own on My Videos: a submission (eshop_channels) keeps its
// images in channel-submissions/<id>/ until staff approve it, which copies
// them into media/i and adds the channel to the catalog. Once a player's
// channel is live they can pick it for their uploads; staff still approve
// every video.
//
// Uploaded images are scaled and cropped to the console sizes with ffmpeg and
// always written as 4:2:0: ffmpeg otherwise keeps the source's chroma layout,
// and the 3DS shows nothing for e.g. a 4:4:0 jpg (sampling 1x2, 2026-10-12).
// Images in media/i get a new file name on every change, so consoles never
// show a cached old one.
//
//	POST /my/videos/api/channel          multipart {name, description, icon, banner} -> {id}
//	POST /my/videos/api/channel-delete   {id}
//	GET  /my/videos/channel-media/{id}/{icon,banner}.jpg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const videoChannelSchema = `
CREATE TABLE IF NOT EXISTS eshop_channels (
	id           BIGSERIAL   PRIMARY KEY,
	pid          BIGINT      NOT NULL,
	name         TEXT        NOT NULL,
	description  TEXT        NOT NULL DEFAULT '',
	status       TEXT        NOT NULL DEFAULT 'submitted'
	             CHECK (status IN ('submitted', 'live', 'rejected')),
	catalog_id   BIGINT,
	review_note  TEXT        NOT NULL DEFAULT '',
	created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	reviewed_at  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS eshop_channels_pid ON eshop_channels (pid);
CREATE INDEX IF NOT EXISTS eshop_channels_status ON eshop_channels (status, created_at);
`

const (
	// Both images go in one request, and nginx allows 10 MB per request.
	videoChannelImageMaxBytes = 4 << 20
	videoChannelMaxName       = 60
	videoChannelMaxPerAccount = 3
	videoChannelsPerDay       = 5
)

// videoChannelImageRe matches the media/i files this code made, the only ones
// it deletes when they are replaced (hand-made ones like channel-icon.jpg stay).
var videoChannelImageRe = regexp.MustCompile(`^i/ch\d+-(icon|banner)-\d+\.jpg$`)

var videoChannelImageSizes = map[string][2]int{"icon": {102, 76}, "banner": {400, 168}}

func registerVideoChannels() {
	if _, err := db.Exec(videoChannelSchema); err != nil {
		log.Printf("video channels schema: %v", err)
	}
	http.HandleFunc("/my/videos/api/channel", myVideosChannelSubmit)
	http.HandleFunc("/my/videos/api/channel-delete", myVideosChannelDelete)
	http.HandleFunc("/my/videos/channel-media/", myVideosChannelMedia)
	http.HandleFunc("/admin/videos/channel", requireClientCert(adminVideosChannel))
	http.HandleFunc("/admin/videos/channel-review", requireClientCert(adminVideosChannelReview))
	http.HandleFunc("/admin/videos/chimg/", requireClientCert(adminVideosChannelImage))
	http.HandleFunc("/admin/videos/chsub/", requireClientCert(adminVideosChannelSubmissionImage))
}

func videoChannelSubmissionDir(id int64) string {
	return filepath.Join(videoRoot, "channel-submissions", strconv.FormatInt(id, 10))
}

var errVideoChannelImage = errors.New("not an image")

// videoChannelImage converts the uploaded image in form field kind ("icon" or
// "banner") to a jpg of exactly the console size at dst. ok is false when no
// file was sent.
func videoChannelImage(r *http.Request, kind, dst string) (ok bool, err error) {
	f, _, err := r.FormFile(kind)
	if err == http.ErrMissingFile {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	tmp, err := os.CreateTemp("", "rvt-channel-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, io.LimitReader(f, videoChannelImageMaxBytes+1))
	tmp.Close()
	if err != nil {
		return false, err
	}
	if n > videoChannelImageMaxBytes {
		return false, fmt.Errorf("the %s is larger than 4 MB", kind)
	}
	size := videoChannelImageSizes[kind]
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	vf := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d,setsar=1", size[0], size[1], size[0], size[1])
	out, err := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-i", tmp.Name(),
		"-frames:v", "1", "-vf", vf, "-pix_fmt", "yuvj420p", "-q:v", "3", "-f", "image2", dst).CombinedOutput()
	if err != nil {
		log.Printf("videos: channel %s: ffmpeg: %v: %s", kind, err, strings.TrimSpace(string(out)))
		os.Remove(dst)
		return false, fmt.Errorf("%w: %s", errVideoChannelImage, kind)
	}
	return true, nil
}

// videoChannelInstallImage copies a converted image into media/i under a new
// name and returns its catalog path.
func videoChannelInstallImage(src string, chID int64, kind string) (string, error) {
	raw, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	rel := fmt.Sprintf("i/ch%d-%s-%d.jpg", chID, kind, time.Now().UnixNano())
	if err := os.WriteFile(filepath.Join(videoRoot, "media", filepath.FromSlash(rel)), raw, 0o644); err != nil {
		return "", err
	}
	return rel, nil
}

type videoChannelEdit struct {
	Name, Description string
	New               bool
	// Converted images to install; "" keeps the current one.
	IconSrc, BannerSrc string
}

// videoCatalogPutChannel adds (id 0) or edits a catalog channel and returns
// its id. A new channel needs both images.
func videoCatalogPutChannel(id int64, e videoChannelEdit) (int64, error) {
	videoCatalogMu.Lock()
	defer videoCatalogMu.Unlock()
	cat, err := videoReadCatalog()
	if err != nil {
		return 0, fmt.Errorf("read catalog: %w", err)
	}
	chans, _ := cat["channels"].([]any)
	var ch map[string]any
	if id == 0 {
		if e.IconSrc == "" || e.BannerSrc == "" {
			return 0, fmt.Errorf("a new channel needs both an icon and a banner")
		}
		for _, c := range chans {
			if m, _ := c.(map[string]any); jsonInt(m["id"]) > id {
				id = jsonInt(m["id"])
			}
		}
		id++
		ch = map[string]any{"id": id, "videos": []any{}}
		cat["channels"] = append(chans, ch)
	} else {
		for _, c := range chans {
			if m, _ := c.(map[string]any); jsonInt(m["id"]) == id {
				ch = m
			}
		}
		if ch == nil {
			return 0, fmt.Errorf("unknown channel %d", id)
		}
	}
	var installed, replaced []string
	removeAll := func(paths []string) {
		for _, p := range paths {
			os.Remove(filepath.Join(videoRoot, "media", filepath.FromSlash(p)))
		}
	}
	for _, img := range []struct{ kind, src string }{{"icon", e.IconSrc}, {"banner", e.BannerSrc}} {
		if img.src == "" {
			continue
		}
		rel, err := videoChannelInstallImage(img.src, id, img.kind)
		if err != nil {
			removeAll(installed)
			return 0, err
		}
		installed = append(installed, rel)
		if old, _ := ch[img.kind].(string); videoChannelImageRe.MatchString(old) {
			replaced = append(replaced, old)
		}
		ch[img.kind] = rel
	}
	ch["name"] = e.Name
	ch["description"] = e.Description
	ch["new"] = e.New
	if err := videoWriteCatalog(cat); err != nil {
		removeAll(installed)
		return 0, err
	}
	removeAll(replaced)
	return id, nil
}

// videoCatalogRemoveChannel takes a channel out of the catalog. Live uploads
// that were in it move to the first channel.
func videoCatalogRemoveChannel(id int64) error {
	videoCatalogMu.Lock()
	cat, err := videoReadCatalog()
	if err != nil {
		videoCatalogMu.Unlock()
		return fmt.Errorf("read catalog: %w", err)
	}
	chans, _ := cat["channels"].([]any)
	var keep []any
	var images []string
	for _, c := range chans {
		m, _ := c.(map[string]any)
		if jsonInt(m["id"]) != id {
			keep = append(keep, c)
			continue
		}
		for _, k := range []string{"icon", "banner"} {
			if p, _ := m[k].(string); videoChannelImageRe.MatchString(p) {
				images = append(images, p)
			}
		}
	}
	if keep == nil {
		keep = []any{}
	}
	cat["channels"] = keep
	err = videoWriteCatalog(cat)
	videoCatalogMu.Unlock()
	if err != nil {
		return err
	}
	for _, p := range images {
		os.Remove(filepath.Join(videoRoot, "media", filepath.FromSlash(p)))
	}
	return videoSyncCatalog()
}

// videoChannelOwners maps catalog channel ids to the players whose live
// submissions they are.
func videoChannelOwners() map[int64]int64 {
	out := map[int64]int64{}
	rows, err := db.Query(`SELECT catalog_id, pid FROM eshop_channels WHERE status = 'live' AND catalog_id IS NOT NULL`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var cid, pid int64
		if rows.Scan(&cid, &pid) == nil {
			out[cid] = pid
		}
	}
	return out
}

// --- staff ---

func adminVideosChannel(w http.ResponseWriter, r *http.Request) {
	redirect := func(msg string) {
		http.Redirect(w, r, "/inkay/admin/videos/?status=channels&msg="+url.QueryEscape(msg), http.StatusSeeOther)
	}
	if r.Method != http.MethodPost {
		redirect("")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2*videoChannelImageMaxBytes+1<<20)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		redirect("Upload failed (images up to 4 MB each).")
		return
	}
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	e := videoChannelEdit{
		Name:        strings.TrimSpace(r.FormValue("name")),
		Description: strings.TrimSpace(strings.ReplaceAll(r.FormValue("description"), "\r", "")),
		New:         r.FormValue("new") != "",
	}
	if e.Name == "" || utf8.RuneCountInString(e.Name) > videoChannelMaxName {
		redirect("A channel needs a name of up to 60 characters.")
		return
	}
	if utf8.RuneCountInString(e.Description) > videoMaxDescription {
		redirect("The description can be at most 500 characters.")
		return
	}
	tmp, err := os.MkdirTemp("", "rvt-channel-*")
	if err != nil {
		redirect("Error: " + err.Error())
		return
	}
	defer os.RemoveAll(tmp)
	for _, kind := range []string{"icon", "banner"} {
		dst := filepath.Join(tmp, kind+".jpg")
		ok, err := videoChannelImage(r, kind, dst)
		if err != nil {
			redirect("Image failed: " + err.Error())
			return
		}
		if ok && kind == "icon" {
			e.IconSrc = dst
		} else if ok {
			e.BannerSrc = dst
		}
	}
	newID, err := videoCatalogPutChannel(id, e)
	if err != nil {
		redirect("Saving failed: " + err.Error())
		return
	}
	// Keep a player's submission in step with what staff changed.
	db.Exec(`UPDATE eshop_channels SET name = $2, description = $3, updated_at = NOW() WHERE catalog_id = $1 AND status = 'live'`, newID, e.Name, e.Description)
	if id == 0 {
		log.Printf("videos: channel %d %q added by staff", newID, e.Name)
		redirect(fmt.Sprintf("Channel %q added.", e.Name))
		return
	}
	log.Printf("videos: channel %d %q edited by staff", newID, e.Name)
	redirect(fmt.Sprintf("Channel %q saved.", e.Name))
}

func adminVideosChannelReview(w http.ResponseWriter, r *http.Request) {
	redirect := func(msg string) {
		http.Redirect(w, r, "/inkay/admin/videos/?status=channels&msg="+url.QueryEscape(msg), http.StatusSeeOther)
	}
	if r.Method != http.MethodPost {
		redirect("")
		return
	}
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	note := strings.TrimSpace(r.FormValue("note"))
	if len([]rune(note)) > 500 {
		note = string([]rune(note)[:500])
	}
	var (
		pid               int64
		name, desc, state string
		catalogID         sql.NullInt64
	)
	if err := db.QueryRow(`SELECT pid, name, description, status, catalog_id FROM eshop_channels WHERE id = $1`, id).
		Scan(&pid, &name, &desc, &state, &catalogID); err != nil {
		redirect("Unknown channel submission.")
		return
	}
	switch r.FormValue("action") {
	case "approve":
		if state == "live" {
			redirect("That channel is already live.")
			return
		}
		dir := videoChannelSubmissionDir(id)
		cid, err := videoCatalogPutChannel(0, videoChannelEdit{Name: name, Description: desc, New: true,
			IconSrc: filepath.Join(dir, "icon.jpg"), BannerSrc: filepath.Join(dir, "banner.jpg")})
		if err != nil {
			redirect("Publishing failed: " + err.Error())
			return
		}
		if _, err := db.Exec(`UPDATE eshop_channels SET status = 'live', catalog_id = $2, review_note = $3, reviewed_at = NOW(), updated_at = NOW() WHERE id = $1`, id, cid, note); err != nil {
			videoCatalogRemoveChannel(cid)
			redirect("Database error: " + err.Error())
			return
		}
		log.Printf("videos: channel submission %d (PID %d) approved as channel %d", id, pid, cid)
		redirect(fmt.Sprintf("Channel %q is live.", name))
	case "reject":
		if note == "" {
			redirect("A rejection needs a note for the player.")
			return
		}
		if state == "live" && catalogID.Valid {
			if err := videoCatalogRemoveChannel(catalogID.Int64); err != nil {
				redirect("Taking it down failed: " + err.Error())
				return
			}
		}
		db.Exec(`UPDATE eshop_channels SET status = 'rejected', catalog_id = NULL, review_note = $2, reviewed_at = NOW(), updated_at = NOW() WHERE id = $1`, id, note)
		log.Printf("videos: channel submission %d (PID %d) rejected (was %s)", id, pid, state)
		redirect(fmt.Sprintf("Channel %q rejected.", name))
	default:
		redirect("Unknown action.")
	}
}

// adminVideosChannelImage previews a catalog channel's icon or banner.
func adminVideosChannelImage(w http.ResponseWriter, r *http.Request) {
	rel := path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/admin/videos/chimg/"))
	if !strings.HasPrefix(rel, "/i/") || !strings.HasSuffix(rel, ".jpg") {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(videoRoot, "media", filepath.FromSlash(rel)))
}

// adminVideosChannelSubmissionImage previews a player's submitted images.
func adminVideosChannelSubmissionImage(w http.ResponseWriter, r *http.Request) {
	id, name, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/admin/videos/chsub/"), "/")
	videoServeChannelSubmissionImage(w, r, id, name, 0)
}

// videoServeChannelSubmissionImage serves icon.jpg or banner.jpg of a
// submission; owner != 0 limits it to that player's own.
func videoServeChannelSubmissionImage(w http.ResponseWriter, r *http.Request, idStr, name string, owner int64) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || (name != "icon.jpg" && name != "banner.jpg") {
		http.NotFound(w, r)
		return
	}
	if owner != 0 {
		var pid int64
		if db.QueryRow(`SELECT pid FROM eshop_channels WHERE id = $1`, id).Scan(&pid) != nil || pid != owner {
			http.NotFound(w, r)
			return
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, filepath.Join(videoChannelSubmissionDir(id), name))
}

type adminChannelSubmission struct {
	ID          int64
	PID         int64
	PNID        string
	Name        string
	Description string
	CreatedAt   time.Time
}

func adminChannelSubmissions() []adminChannelSubmission {
	rows, err := db.Query(`SELECT id, pid, name, description, created_at FROM eshop_channels WHERE status = 'submitted' ORDER BY created_at LIMIT 100`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []adminChannelSubmission
	for rows.Next() {
		var s adminChannelSubmission
		if rows.Scan(&s.ID, &s.PID, &s.Name, &s.Description, &s.CreatedAt) == nil {
			s.PNID, _ = pnidForPID(s.PID)
			out = append(out, s)
		}
	}
	return out
}

// --- players ---

type videoChannelSummary struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      string `json:"status"`
	ReviewNote  string `json:"reviewNote"`
	CatalogID   int64  `json:"catalogId"`
	Created     string `json:"created"`
}

func videoChannelListFor(pid int64) []videoChannelSummary {
	rows, err := db.Query(`SELECT id, name, description, status, review_note, COALESCE(catalog_id, 0), created_at
		FROM eshop_channels WHERE pid = $1 ORDER BY created_at DESC`, pid)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []videoChannelSummary{}
	for rows.Next() {
		var c videoChannelSummary
		var created time.Time
		if rows.Scan(&c.ID, &c.Name, &c.Description, &c.Status, &c.ReviewNote, &c.CatalogID, &created) == nil {
			c.Created = created.UTC().Format("2006-01-02")
			out = append(out, c)
		}
	}
	return out
}

// videoOwnsLiveChannel reports whether catalog channel cid is pid's own live
// channel, the only channels a player may pick for an upload.
func videoOwnsLiveChannel(pid, cid int64) bool {
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM eshop_channels WHERE pid = $1 AND catalog_id = $2 AND status = 'live'`, pid, cid).Scan(&n)
	return n > 0
}

func myVideosChannelSubmit(w http.ResponseWriter, r *http.Request) {
	pid, ok := videoAPIAuth(w, r, http.MethodPost)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2*videoChannelImageMaxBytes+1<<20)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		videoError(w, http.StatusBadRequest, "vid.ch_err_image_size", "Images can be at most 4 MB each.")
		return
	}
	defer r.MultipartForm.RemoveAll()
	name := strings.TrimSpace(r.FormValue("name"))
	desc := strings.TrimSpace(strings.ReplaceAll(r.FormValue("description"), "\r", ""))
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM eshop_channels WHERE pid = $1`, pid).Scan(&count)
	switch {
	case badgeEditorBanned(pid):
		videoError(w, http.StatusForbidden, "vid.err_banned", "Your account can't submit videos.")
		return
	case name == "" || utf8.RuneCountInString(name) > videoChannelMaxName:
		videoError(w, http.StatusBadRequest, "vid.ch_err_name", "The channel name must be 1 to 60 characters.")
		return
	case utf8.RuneCountInString(desc) > videoMaxDescription:
		videoError(w, http.StatusBadRequest, "vid.err_desc", "The description can be at most 500 characters.")
		return
	case r.FormValue("rights") == "":
		videoError(w, http.StatusBadRequest, "vid.ch_err_rights", "Please confirm you have the right to use these images.")
		return
	case count >= videoChannelMaxPerAccount:
		videoError(w, http.StatusTooManyRequests, "vid.ch_err_max", "You can have at most 3 channels. Delete one to make a new one.")
		return
	}
	if badgeEditorRateLimited("video-channel:"+strconv.FormatInt(pid, 10), videoChannelsPerDay, 24*time.Hour) {
		videoError(w, http.StatusTooManyRequests, "vid.err_rate", "Too many uploads, please try again later.")
		return
	}
	// Convert next to the submissions so the finished images can be renamed
	// into place (/tmp is another filesystem).
	parent := filepath.Join(videoRoot, "channel-submissions")
	os.MkdirAll(parent, 0o755)
	tmp, err := os.MkdirTemp(parent, ".tmp-*")
	if err != nil {
		log.Printf("videos: channel submit temp dir: %v", err)
		videoError(w, http.StatusInternalServerError, "vid.err_generic", "Something went wrong, please try again.")
		return
	}
	defer os.RemoveAll(tmp)
	for _, kind := range []string{"icon", "banner"} {
		ok, err := videoChannelImage(r, kind, filepath.Join(tmp, kind+".jpg"))
		if errors.Is(err, errVideoChannelImage) || (err == nil && !ok) {
			videoError(w, http.StatusBadRequest, "vid.ch_err_image", "Please choose an icon and a banner image (JPG or PNG).")
			return
		}
		if err != nil {
			videoError(w, http.StatusBadRequest, "vid.ch_err_image_size", "Images can be at most 4 MB each.")
			return
		}
	}
	var id int64
	if err := db.QueryRow(`INSERT INTO eshop_channels (pid, name, description) VALUES ($1, $2, $3) RETURNING id`, pid, name, desc).Scan(&id); err != nil {
		log.Printf("videos: channel submit: %v", err)
		videoError(w, http.StatusInternalServerError, "vid.err_generic", "Something went wrong, please try again.")
		return
	}
	dir := videoChannelSubmissionDir(id)
	err = os.MkdirAll(dir, 0o755)
	for _, kind := range []string{"icon", "banner"} {
		if err == nil {
			err = os.Rename(filepath.Join(tmp, kind+".jpg"), filepath.Join(dir, kind+".jpg"))
		}
	}
	if err != nil {
		log.Printf("videos: channel submission %d files: %v", id, err)
		db.Exec(`DELETE FROM eshop_channels WHERE id = $1`, id)
		os.RemoveAll(dir)
		videoError(w, http.StatusInternalServerError, "vid.err_generic", "Something went wrong, please try again.")
		return
	}
	log.Printf("videos: channel submission %d by PID %d (%q)", id, pid, name)
	videoJSON(w, http.StatusOK, map[string]any{"id": id})
}

func myVideosChannelDelete(w http.ResponseWriter, r *http.Request) {
	pid, ok := videoAPIAuth(w, r, http.MethodPost)
	if !ok {
		return
	}
	var req struct {
		ID int64 `json:"id"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req) != nil {
		videoError(w, http.StatusBadRequest, "vid.err_generic", "invalid request")
		return
	}
	var status string
	var catalogID sql.NullInt64
	if db.QueryRow(`SELECT status, catalog_id FROM eshop_channels WHERE id = $1 AND pid = $2`, req.ID, pid).Scan(&status, &catalogID) != nil {
		videoError(w, http.StatusNotFound, "vid.err_generic", "unknown channel")
		return
	}
	if _, err := db.Exec(`DELETE FROM eshop_channels WHERE id = $1`, req.ID); err != nil {
		videoError(w, http.StatusInternalServerError, "vid.err_generic", "Something went wrong, please try again.")
		return
	}
	if status == "live" && catalogID.Valid {
		if err := videoCatalogRemoveChannel(catalogID.Int64); err != nil {
			log.Printf("videos: removing channel %d from the catalog: %v", catalogID.Int64, err)
		}
	}
	os.RemoveAll(videoChannelSubmissionDir(req.ID))
	log.Printf("videos: channel submission %d deleted by its owner (PID %d, was %s)", req.ID, pid, status)
	videoJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func myVideosChannelMedia(w http.ResponseWriter, r *http.Request) {
	pid, ok := mySessionPID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	id, name, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/my/videos/channel-media/"), "/")
	videoServeChannelSubmissionImage(w, r, id, name, pid)
}
