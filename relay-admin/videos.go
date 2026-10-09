package main

// Revivetendo TV uploads: players submit videos for the video channel the
// Nintendo eShop has become (account-proxy/samurai.go serves it to the 3DS
// and Wii U eShops from eshop-video/catalog.json). Each upload is converted
// in the background to everything the consoles need - a 400x240 .moflex for
// the 3DS (mobipeg, tools/mobipeg), an .mp4 for the Wii U eShop browser, a
// 120x88 thumbnail and a 400x168 banner - and then waits for staff review
// (videos_admin.go). Nothing reaches a console until staff approve it.
//
// Uploads arrive in chunks so no proxy in front of us (nginx here, Apache2 on
// the dashboard box) needs a body limit anywhere near the file size, and a
// dropped chunk can simply be sent again:
//
//	POST /my/videos/api/start                 {title, description, size} -> {id}
//	PUT  /my/videos/api/chunk?id=N&offset=O   raw bytes, O must equal what we already have
//	POST /my/videos/api/delete                {id}
//	GET  /my/videos/api/list                  own videos (the page polls it while converting)
//	GET  /my/videos/media/{id}/{file}         own previews (video.mp4, thumb.jpg, banner.jpg)
//
// The API only accepts requests carrying X-Requested-With, which a browser
// will not send cross-site without a CORS preflight we never answer, so other
// sites cannot submit with a player's my_session cookie.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const videoSchema = `
CREATE TABLE IF NOT EXISTS eshop_videos (
	id           BIGSERIAL   PRIMARY KEY,
	pid          BIGINT      NOT NULL,
	title        TEXT        NOT NULL,
	description  TEXT        NOT NULL DEFAULT '',
	status       TEXT        NOT NULL DEFAULT 'uploading'
	             CHECK (status IN ('uploading', 'processing', 'failed', 'submitted', 'live', 'rejected')),
	upload_size  BIGINT      NOT NULL,
	received     BIGINT      NOT NULL DEFAULT 0,
	seconds      INT         NOT NULL DEFAULT 0,
	channel_id   BIGINT,
	review_note  TEXT        NOT NULL DEFAULT '',
	error        TEXT        NOT NULL DEFAULT '',
	created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	submitted_at TIMESTAMPTZ,
	reviewed_at  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS eshop_videos_pid ON eshop_videos (pid);
CREATE INDEX IF NOT EXISTS eshop_videos_status ON eshop_videos (status, submitted_at);
`

const (
	videoRoot            = "/nico-pretendo-bridge/eshop-video"
	videoMaxUpload       = 300 << 20
	videoChunkSize       = 8 << 20 // the page sends this much per request; nginx allows 10 MB
	videoMaxSeconds      = 600
	videoMaxPending      = 3 // uploading + converting + waiting for review, per account
	videoMaxTitle        = 60
	videoMaxDescription  = 500
	videoUploadsPerHour  = 5
	videoStaleUploadTime = 24 * time.Hour
	// videoCatalogIDBase + submission id is the eShop movie id (3DS movie ids
	// are 5004xxxxxxxxxx; hand-made catalog entries use 50040099...).
	videoCatalogIDBase = 50040091000000
)

func videoSubmissionDir(id int64) string {
	return filepath.Join(videoRoot, "submissions", strconv.FormatInt(id, 10))
}

func mobipegFFmpeg() string {
	if p := os.Getenv("MOBIPEG_FFMPEG"); p != "" {
		return p
	}
	return "/nico-pretendo-bridge/tools/mobipeg/ffmpeg"
}

func registerVideos() {
	if _, err := db.Exec(videoSchema); err != nil {
		log.Printf("videos schema: %v", err)
	}
	http.HandleFunc("/my/videos", myVideosPage)
	http.HandleFunc("/my/videos/", myVideosPage)
	http.HandleFunc("/my/videos/api/start", myVideosStart)
	http.HandleFunc("/my/videos/api/chunk", myVideosChunk)
	http.HandleFunc("/my/videos/api/delete", myVideosDelete)
	http.HandleFunc("/my/videos/api/list", myVideosList)
	http.HandleFunc("/my/videos/media/", myVideosMedia)
	registerVideosAdmin()
	go videoWorker()
	go videoJanitor()
}

func videoJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// videoError answers with an English message and an i18n key the page
// translates (vid.err_*).
func videoError(w http.ResponseWriter, status int, key, msg string) {
	videoJSON(w, status, map[string]string{"error": msg, "key": key})
}

// videoAPIAuth checks the session and the anti-CSRF header.
func videoAPIAuth(w http.ResponseWriter, r *http.Request, method string) (int64, bool) {
	if r.Method != method {
		videoError(w, http.StatusMethodNotAllowed, "vid.err_generic", method+" only")
		return 0, false
	}
	if r.Header.Get("X-Requested-With") == "" {
		videoError(w, http.StatusForbidden, "vid.err_generic", "missing X-Requested-With")
		return 0, false
	}
	pid, ok := mySessionPID(r)
	if !ok {
		videoError(w, http.StatusUnauthorized, "vid.err_login", "Please sign in again.")
		return 0, false
	}
	return pid, true
}

type videoSummary struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Status      string `json:"status"`
	Received    int64  `json:"received"`
	Size        int64  `json:"size"`
	Seconds     int    `json:"seconds"`
	ReviewNote  string `json:"reviewNote"`
	HasPreview  bool   `json:"hasPreview"`
	Error       string `json:"error,omitempty"`
	ErrorKey    string `json:"errorKey,omitempty"`
	Created     string `json:"created"`
}

func videoListFor(pid int64) []videoSummary {
	rows, err := db.Query(`SELECT id, title, description, status, received, upload_size, seconds, review_note, error, created_at
		FROM eshop_videos WHERE pid = $1 ORDER BY created_at DESC LIMIT 100`, pid)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []videoSummary
	for rows.Next() {
		var v videoSummary
		var created time.Time
		if rows.Scan(&v.ID, &v.Title, &v.Description, &v.Status, &v.Received, &v.Size, &v.Seconds, &v.ReviewNote, &v.Error, &created) != nil {
			continue
		}
		v.Created = created.UTC().Format("2006-01-02")
		v.HasPreview = v.Status == "submitted" || v.Status == "live" || v.Status == "rejected"
		v.ErrorKey = videoFailKeys[v.Error]
		out = append(out, v)
	}
	return out
}

func myVideosList(w http.ResponseWriter, r *http.Request) {
	pid, ok := videoAPIAuth(w, r, http.MethodGet)
	if !ok {
		return
	}
	videoJSON(w, http.StatusOK, map[string]any{"videos": videoListFor(pid)})
}

func myVideosStart(w http.ResponseWriter, r *http.Request) {
	pid, ok := videoAPIAuth(w, r, http.MethodPost)
	if !ok {
		return
	}
	var req struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Size        int64  `json:"size"`
		Rights      bool   `json:"rights"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		videoError(w, http.StatusBadRequest, "vid.err_generic", "invalid request")
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	req.Description = strings.TrimSpace(strings.ReplaceAll(req.Description, "\r", ""))
	switch {
	case badgeEditorBanned(pid):
		videoError(w, http.StatusForbidden, "vid.err_banned", "Your account can't submit videos.")
	case req.Title == "" || utf8.RuneCountInString(req.Title) > videoMaxTitle:
		videoError(w, http.StatusBadRequest, "vid.err_title", "The title must be 1 to 60 characters.")
	case utf8.RuneCountInString(req.Description) > videoMaxDescription:
		videoError(w, http.StatusBadRequest, "vid.err_desc", "The description can be at most 500 characters.")
	case !req.Rights:
		videoError(w, http.StatusBadRequest, "vid.err_rights", "Please confirm you have the right to share this video.")
	case req.Size <= 0 || req.Size > videoMaxUpload:
		videoError(w, http.StatusBadRequest, "vid.err_size", "Videos can be at most 300 MB.")
	default:
		var pending int
		db.QueryRow(`SELECT COUNT(*) FROM eshop_videos WHERE pid = $1 AND status IN ('uploading', 'processing', 'submitted')`, pid).Scan(&pending)
		if pending >= videoMaxPending {
			videoError(w, http.StatusTooManyRequests, "vid.err_pending", "You already have 3 videos waiting. Please wait until they are reviewed.")
			return
		}
		if badgeEditorRateLimited("video-upload:"+strconv.FormatInt(pid, 10), videoUploadsPerHour, time.Hour) {
			videoError(w, http.StatusTooManyRequests, "vid.err_rate", "Too many uploads, please try again later.")
			return
		}
		var id int64
		if err := db.QueryRow(`INSERT INTO eshop_videos (pid, title, description, upload_size) VALUES ($1, $2, $3, $4) RETURNING id`,
			pid, req.Title, req.Description, req.Size).Scan(&id); err != nil {
			log.Printf("videos: start: %v", err)
			videoError(w, http.StatusInternalServerError, "vid.err_generic", "Something went wrong, please try again.")
			return
		}
		if err := os.MkdirAll(videoSubmissionDir(id), 0o755); err != nil {
			log.Printf("videos: mkdir %d: %v", id, err)
		}
		log.Printf("videos: #%d started by PID %d (%d bytes, %q)", id, pid, req.Size, req.Title)
		videoJSON(w, http.StatusOK, map[string]any{"id": id, "chunkSize": videoChunkSize})
	}
}

// videoUploadLocks serializes chunks per submission, so a retried chunk that
// overlaps a slow original can't interleave writes.
var videoUploadLocks sync.Map

func myVideosChunk(w http.ResponseWriter, r *http.Request) {
	pid, ok := videoAPIAuth(w, r, http.MethodPut)
	if !ok {
		return
	}
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if id <= 0 || err != nil || offset < 0 {
		videoError(w, http.StatusBadRequest, "vid.err_generic", "bad id or offset")
		return
	}
	mu, _ := videoUploadLocks.LoadOrStore(id, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	var status string
	var size, received int64
	err = db.QueryRow(`SELECT status, upload_size, received FROM eshop_videos WHERE id = $1 AND pid = $2`, id, pid).Scan(&status, &size, &received)
	if err != nil {
		videoError(w, http.StatusNotFound, "vid.err_generic", "unknown upload")
		return
	}
	if status != "uploading" {
		videoJSON(w, http.StatusOK, map[string]any{"received": received, "done": true})
		return
	}
	if offset != received {
		// The client resumes from what we really have.
		videoJSON(w, http.StatusConflict, map[string]any{"received": received})
		return
	}
	src := filepath.Join(videoSubmissionDir(id), "source")
	f, err := os.OpenFile(src, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("videos: open %d: %v", id, err)
		videoError(w, http.StatusInternalServerError, "vid.err_generic", "Something went wrong, please try again.")
		return
	}
	defer f.Close()
	// Drop anything past what the database says arrived (a chunk cut off mid-write).
	if err := f.Truncate(received); err != nil {
		videoError(w, http.StatusInternalServerError, "vid.err_generic", "Something went wrong, please try again.")
		return
	}
	if _, err := f.Seek(received, io.SeekStart); err != nil {
		videoError(w, http.StatusInternalServerError, "vid.err_generic", "Something went wrong, please try again.")
		return
	}
	limit := size - received
	if limit > videoChunkSize {
		limit = videoChunkSize
	}
	n, err := io.Copy(f, io.LimitReader(r.Body, limit+1))
	if err != nil || n == 0 || n > limit {
		f.Truncate(received)
		videoError(w, http.StatusBadRequest, "vid.err_generic", "chunk incomplete or too large, send it again")
		return
	}
	received += n
	done := received == size
	if done {
		db.Exec(`UPDATE eshop_videos SET received = $2, status = 'processing', updated_at = NOW() WHERE id = $1`, id, received)
		log.Printf("videos: #%d upload complete (%d bytes), converting", id, received)
		videoEnqueue(id)
	} else {
		db.Exec(`UPDATE eshop_videos SET received = $2, updated_at = NOW() WHERE id = $1`, id, received)
	}
	videoJSON(w, http.StatusOK, map[string]any{"received": received, "done": done})
}

func myVideosDelete(w http.ResponseWriter, r *http.Request) {
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
	if err := db.QueryRow(`SELECT status FROM eshop_videos WHERE id = $1 AND pid = $2`, req.ID, pid).Scan(&status); err != nil {
		videoError(w, http.StatusNotFound, "vid.err_generic", "unknown video")
		return
	}
	if status == "processing" {
		videoError(w, http.StatusConflict, "vid.err_busy", "This video is still being converted, please wait a moment.")
		return
	}
	if err := videoRemove(req.ID, status == "live"); err != nil {
		log.Printf("videos: delete #%d: %v", req.ID, err)
		videoError(w, http.StatusInternalServerError, "vid.err_generic", "Something went wrong, please try again.")
		return
	}
	log.Printf("videos: #%d deleted by its uploader (PID %d, was %s)", req.ID, pid, status)
	videoJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// videoRemove deletes a submission, its files and (when it was live) its
// catalog entry.
func videoRemove(id int64, wasLive bool) error {
	if _, err := db.Exec(`DELETE FROM eshop_videos WHERE id = $1`, id); err != nil {
		return err
	}
	if wasLive {
		if err := videoSyncCatalog(); err != nil {
			return err
		}
	}
	return videoDeleteStored(id)
}

// videoFileTypes are a conversion's output files: what owners and staff may
// preview, what goes to S3, and what the catalog points the consoles at.
var videoFileTypes = map[string]string{
	"video.moflex": "application/octet-stream",
	"video.mp4":    "video/mp4",
	"thumb.jpg":    "image/jpeg",
	"banner.jpg":   "image/jpeg",
}

func myVideosMedia(w http.ResponseWriter, r *http.Request) {
	pid, ok := mySessionPID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	id, name, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/my/videos/media/"), "/")
	vid, err := strconv.ParseInt(id, 10, 64)
	if !ok || err != nil {
		http.NotFound(w, r)
		return
	}
	var owner int64
	if db.QueryRow(`SELECT pid FROM eshop_videos WHERE id = $1`, vid).Scan(&owner) != nil || owner != pid {
		http.NotFound(w, r)
		return
	}
	serveVideoFile(w, r, vid, name)
}

// --- conversion ---

var videoQueue = make(chan int64, 256)

func videoEnqueue(id int64) {
	go func() { videoQueue <- id }()
}

// videoWorker converts one upload at a time, so uploads can't crowd out the
// game servers on this machine. Uploads interrupted by a restart are picked
// up again here.
func videoWorker() {
	if rows, err := db.Query(`SELECT id FROM eshop_videos WHERE status = 'processing' ORDER BY id`); err == nil {
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				videoEnqueue(id)
			}
		}
		rows.Close()
	}
	for id := range videoQueue {
		seconds, err := videoConvert(id)
		if err != nil {
			log.Printf("videos: #%d conversion failed: %v", id, err)
			msg := videoFailGeneric
			if errors.Is(err, errVideoTooLong) {
				msg = videoFailTooLong
			} else if errors.Is(err, errVideoNoVideo) {
				msg = videoFailNoVideo
			}
			db.Exec(`UPDATE eshop_videos SET status = 'failed', error = $2, updated_at = NOW() WHERE id = $1 AND status = 'processing'`, id, msg)
			continue
		}
		if err := videoStoreConverted(id); err != nil {
			log.Printf("videos: #%d storing in S3 failed: %v", id, err)
			db.Exec(`UPDATE eshop_videos SET status = 'failed', error = $2, updated_at = NOW() WHERE id = $1 AND status = 'processing'`, id, videoFailStorage)
			os.RemoveAll(videoSubmissionDir(id))
			continue
		}
		db.Exec(`UPDATE eshop_videos SET status = 'submitted', seconds = $2, error = '', submitted_at = NOW(), updated_at = NOW()
			WHERE id = $1 AND status = 'processing'`, id, seconds)
		log.Printf("videos: #%d converted (%d s), waiting for review", id, seconds)
	}
}

// Conversion failures as stored in eshop_videos.error (shown to staff) and
// the i18n keys the upload page shows the uploader instead.
const (
	videoFailGeneric = "This video could not be converted. Try another file (MP4 works best)."
	videoFailTooLong = "This video is longer than 10 minutes."
	videoFailNoVideo = "This file has no video in it."
	videoFailStorage = "The video could not be saved to storage. Please upload it again later."
)

var videoFailKeys = map[string]string{
	videoFailGeneric: "vid.failed_help",
	videoFailTooLong: "vid.fail_long",
	videoFailNoVideo: "vid.fail_novideo",
	videoFailStorage: "vid.fail_storage",
}

var (
	errVideoTooLong = errors.New("video too long")
	errVideoNoVideo = errors.New("no video stream")
)

// videoRun runs a converter at low priority with a time limit and returns
// the end of its output for the log when it fails.
func videoRun(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, "nice", append([]string{"-n", "15", name}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		tail := string(out)
		if len(tail) > 600 {
			tail = tail[len(tail)-600:]
		}
		return fmt.Errorf("%s: %v: %s", filepath.Base(name), err, strings.TrimSpace(tail))
	}
	return nil
}

func videoConvert(id int64) (int, error) {
	dir := videoSubmissionDir(id)
	src := filepath.Join(dir, "source")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	// Probe with the system ffprobe; it reads everything a browser upload could be.
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_entries", "format=duration:stream=codec_type", "-of", "json", src).Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe: %v", err)
	}
	var probe struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	json.Unmarshal(out, &probe)
	hasVideo, hasAudio := false, false
	for _, s := range probe.Streams {
		hasVideo = hasVideo || s.CodecType == "video"
		hasAudio = hasAudio || s.CodecType == "audio"
	}
	if !hasVideo {
		return 0, errVideoNoVideo
	}
	dur, _ := strconv.ParseFloat(probe.Format.Duration, 64)
	if dur <= 0 {
		return 0, fmt.Errorf("unknown duration %q", probe.Format.Duration)
	}
	if dur > videoMaxSeconds+1 {
		return 0, errVideoTooLong
	}

	// 3DS: settings mobipeg's own encode.py uses for fmt=moflex (400x240,
	// keyframe at least every 90 frames, ADPCM, 4096-byte blocks); 30 fps like
	// Nintendo's trailers. Letterboxed, since the top screen is 5:3.
	moflex := []string{"-hide_banner", "-loglevel", "error", "-y", "-threads", "2", "-i", src, "-t", strconv.Itoa(videoMaxSeconds),
		"-vf", "scale=400:240:force_original_aspect_ratio=decrease,pad=400:240:(ow-iw)/2:(oh-ih)/2,setsar=1,fps=30",
		"-map", "0:v:0", "-c:v", "mobiclip", "-mobiclip", "1", "-g", "90", "-b:v", "1000k", "-mo_block", "4096"}
	if hasAudio {
		moflex = append(moflex, "-map", "0:a:0", "-mo_audio", "adpcm", "-ar", "48000", "-ac", "2")
	} else {
		moflex = append(moflex, "-an")
	}
	if err := videoRun(ctx, mobipegFFmpeg(), append(moflex, filepath.Join(dir, "video.moflex"))...); err != nil {
		return 0, err
	}

	// Wii U eShop browser: baseline H.264 + AAC with the index up front, so it
	// starts playing before the download finishes.
	mp4 := []string{"-hide_banner", "-loglevel", "error", "-y", "-threads", "2", "-i", src, "-t", strconv.Itoa(videoMaxSeconds),
		"-vf", "scale=854:480:force_original_aspect_ratio=decrease,pad=854:480:(ow-iw)/2:(oh-ih)/2,setsar=1",
		"-map", "0:v:0", "-c:v", "libx264", "-profile:v", "baseline", "-level", "3.1", "-pix_fmt", "yuv420p", "-crf", "23", "-r", "30"}
	if hasAudio {
		mp4 = append(mp4, "-map", "0:a:0", "-c:a", "aac", "-b:a", "128k", "-ar", "48000", "-ac", "2")
	} else {
		mp4 = append(mp4, "-an")
	}
	if err := videoRun(ctx, "ffmpeg", append(mp4, "-movflags", "+faststart", filepath.Join(dir, "video.mp4"))...); err != nil {
		return 0, err
	}

	// Thumbnail and banner from a frame a third of the way in, cropped to fill.
	at := fmt.Sprintf("%.2f", dur/3)
	for _, img := range []struct{ name, size string }{{"thumb.jpg", "120:88"}, {"banner.jpg", "400:168"}} {
		w, h, _ := strings.Cut(img.size, ":")
		vf := "scale=" + img.size + ":force_original_aspect_ratio=increase,crop=" + w + ":" + h + ",setsar=1"
		if err := videoRun(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-ss", at, "-i", src, "-frames:v", "1", "-vf", vf, "-q:v", "3", filepath.Join(dir, img.name)); err != nil {
			return 0, err
		}
	}
	os.Remove(src)
	seconds := int(dur + 0.5)
	if seconds > videoMaxSeconds {
		seconds = videoMaxSeconds
	}
	return seconds, nil
}

// videoJanitor drops uploads abandoned half-way.
func videoJanitor() {
	for {
		rows, err := db.Query(`SELECT id FROM eshop_videos WHERE status = 'uploading' AND updated_at < NOW() - $1::interval`,
			fmt.Sprintf("%d seconds", int(videoStaleUploadTime.Seconds())))
		if err == nil {
			var ids []int64
			for rows.Next() {
				var id int64
				if rows.Scan(&id) == nil {
					ids = append(ids, id)
				}
			}
			rows.Close()
			for _, id := range ids {
				log.Printf("videos: #%d abandoned mid-upload, removing", id)
				videoRemove(id, false)
			}
		}
		time.Sleep(time.Hour)
	}
}
