package main

// Wii Karaoke U song editor: a public web editor for custom songs (melody
// notes + a backing track), following the same shape as the Badge Arcade
// editor (badgeeditor.go) — player-owned drafts, submit for review, admin
// approves/rejects. Unlike Badge Arcade, the actual song-format logic
// (SSZL compression, the UJK0 container, JOY-U1 track events, the
// streamed-audio packet format — all reverse-engineered and proven on
// real hardware this project cycle) stays in Python
// (wiikaraoke-server/tools/songeditor/): porting it to Go would mean a
// second implementation of a format we only just got right. songEditorBuild
// shells out to it per creation.
//
// Unlike Badge Arcade's editor, this one's static files are NOT embedded
// in the binary — nginx serves them directly from songEditorStaticDir, so
// this file only ever answers under .../api/. See the nginx config on the
// dashboard host for the matching location blocks.
//
//	/my/songeditor/api/me                        login state
//	/my/songeditor/api/creations                 list (GET) / create (POST) own drafts
//	/my/songeditor/api/creations/{id}             load (GET) / update (PUT) / delete (DELETE)
//	/my/songeditor/api/creations/{id}/build        (re)build the .ujk from the current spec
//	/my/songeditor/api/creations/{id}/audio        the stored backing track
//	/my/songeditor/api/creations/{id}/download     the built .ujk, if any
//	/my/songeditor/api/creations/{id}/submit       send a draft/rejected creation for review
//	/my/songeditor/api/creations/{id}/withdraw     take a submission back

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const songEditorSchema = `
CREATE TABLE IF NOT EXISTS wiikaraoke_song_creations (
	id            BIGSERIAL   PRIMARY KEY,
	pid           BIGINT      NOT NULL,
	title         TEXT        NOT NULL DEFAULT '',
	spec          JSONB       NOT NULL DEFAULT '{}',
	audio         BYTEA,
	audio_name    TEXT        NOT NULL DEFAULT '',
	aux           BYTEA,
	aux_name      TEXT        NOT NULL DEFAULT '',
	built         BYTEA,
	build_error   TEXT        NOT NULL DEFAULT '',
	status        TEXT        NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'submitted', 'approved', 'rejected')),
	review_note   TEXT        NOT NULL DEFAULT '',
	created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	submitted_at  TIMESTAMPTZ,
	reviewed_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS wiikaraoke_song_creations_pid ON wiikaraoke_song_creations (pid);
CREATE INDEX IF NOT EXISTS wiikaraoke_song_creations_status ON wiikaraoke_song_creations (status, submitted_at);
-- A separate upload slot for lyrics_mode="acapella" syncing (its own
-- file, not forced to share notes_mode's own midi/acapella upload), and
-- the background sync queue's own state - see songeditor_lyrics_sync.go.
-- Mirrors eshop_videos' queued_at/status pattern (videos.go) for the same
-- reason: a CPU-heavy background job that must run one at a time, at the
-- lowest priority, with players able to see their place in line.
ALTER TABLE wiikaraoke_song_creations ADD COLUMN IF NOT EXISTS lyrics_aux BYTEA;
ALTER TABLE wiikaraoke_song_creations ADD COLUMN IF NOT EXISTS lyrics_aux_name TEXT NOT NULL DEFAULT '';
ALTER TABLE wiikaraoke_song_creations ADD COLUMN IF NOT EXISTS lyrics_sync_status TEXT NOT NULL DEFAULT 'idle'
	CHECK (lyrics_sync_status IN ('idle', 'queued', 'processing', 'done', 'failed'));
ALTER TABLE wiikaraoke_song_creations ADD COLUMN IF NOT EXISTS lyrics_queued_at TIMESTAMPTZ;
ALTER TABLE wiikaraoke_song_creations ADD COLUMN IF NOT EXISTS lyrics_sync_error TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS wiikaraoke_song_creations_lyrics_sync ON wiikaraoke_song_creations (lyrics_sync_status, lyrics_queued_at);
-- A separate, parallel background queue for ML note detection
-- (notes_mode="acapella_ml", tools/songeditor/notes.from_acapella_ml) -
-- deliberately NOT generalized together with the lyrics sync queue
-- above (the user explicitly chose keeping them independent over a
-- single shared queue, 2026-11-14). Reuses the existing aux/aux_name
-- upload (the same vocal recording notes_mode="acapella" already uses),
-- unlike lyrics sync which got its own separate upload slot.
ALTER TABLE wiikaraoke_song_creations ADD COLUMN IF NOT EXISTS notes_sync_status TEXT NOT NULL DEFAULT 'idle'
	CHECK (notes_sync_status IN ('idle', 'queued', 'processing', 'done', 'failed'));
ALTER TABLE wiikaraoke_song_creations ADD COLUMN IF NOT EXISTS notes_queued_at TIMESTAMPTZ;
ALTER TABLE wiikaraoke_song_creations ADD COLUMN IF NOT EXISTS notes_sync_error TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS wiikaraoke_song_creations_notes_sync ON wiikaraoke_song_creations (notes_sync_status, notes_queued_at);
`

const songEditorBase = "/my/songeditor/"

const (
	songEditorMaxCreations = 50
	songEditorMaxAudio     = 60 << 20 // uploaded backing track - uncompressed WAV can be large
	songEditorMaxAux       = 30 << 20 // .mid files are tiny, but an a cappella WAV reference can be sizeable too
	songEditorMaxBody      = 25 << 20
	songEditorMaxTitle     = 60
	songEditorMaxPending   = 5 // submissions waiting for review, per account

	// Game dump + template used to build every song. Server-controlled —
	// never derived from user input. See project_wii_karaoke.md /
	// project_wk_audio_streaming_format.md for why these specific files.
	songEditorRPXPath      = "/home/nico/wiikaraokeu/code/app.rpx"
	songEditorTemplatePath = "/home/nico/ujk-files/0000113.ujk"

	songEditorPythonBin    = "/nico-pretendo-bridge/wiikaraoke-server/.venv/bin/python3"
	songEditorToolsDir     = "/nico-pretendo-bridge/wiikaraoke-server/tools"
	songEditorBuildTimeout = 60 * time.Second
)

func registerSongEditor() {
	if _, err := db.Exec(songEditorSchema); err != nil {
		log.Printf("song editor schema: %v", err)
	}
	http.HandleFunc(songEditorBase+"api/me", songEditorMe)
	http.HandleFunc(songEditorBase+"api/creations", songEditorCreations)
	http.HandleFunc(songEditorBase+"api/creations/", songEditorCreation)
	registerSongEditorAdmin()
	go songEditorLyricsSyncWorker()
	go songEditorNotesSyncWorker()
}

func songEditorJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func songEditorError(w http.ResponseWriter, status int, msg string) {
	songEditorJSON(w, status, map[string]string{"error": msg})
}

func songEditorReadJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		songEditorError(w, http.StatusUnsupportedMediaType, "expected JSON")
		return false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, songEditorMaxBody)).Decode(v); err != nil {
		songEditorError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return false
	}
	return true
}

func songEditorAuth(w http.ResponseWriter, r *http.Request) (int64, bool) {
	pid, ok := mySessionPID(r)
	if !ok {
		songEditorError(w, http.StatusUnauthorized, "please log in to save creations")
		return 0, false
	}
	return pid, true
}

func songEditorBanned(pid int64) bool {
	var banned bool
	db.QueryRow(`SELECT EXISTS(SELECT 1 FROM banned_users WHERE pid = $1)`, pid).Scan(&banned)
	return banned
}

func songEditorRateLimited(key string, n int, window time.Duration) bool {
	ctx := context.Background()
	k := "songeditor:rl:" + key + ":" + strconv.FormatInt(time.Now().Unix()/int64(window.Seconds()), 10)
	v, _ := runtimeCacheGet(ctx, k)
	count, _ := strconv.Atoi(v)
	if count >= n {
		return true
	}
	runtimeCacheSet(ctx, k, strconv.Itoa(count+1), window)
	return false
}

func songEditorMe(w http.ResponseWriter, r *http.Request) {
	pid, ok := mySessionPID(r)
	if !ok {
		songEditorJSON(w, http.StatusOK, map[string]any{"loggedIn": false})
		return
	}
	pnid, _ := pnidForPID(pid)
	songEditorJSON(w, http.StatusOK, map[string]any{"loggedIn": true, "pnid": pnid, "banned": songEditorBanned(pid)})
}

type songEditorSummary struct {
	ID         int64     `json:"id"`
	Title      string    `json:"title"`
	Status     string    `json:"status"`
	ReviewNote string    `json:"reviewNote"`
	HasBuilt   bool      `json:"hasBuilt"`
	BuildError string    `json:"buildError"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

func songEditorCreations(w http.ResponseWriter, r *http.Request) {
	pid, ok := songEditorAuth(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		rows, err := db.Query(`SELECT id, title, status, review_note, built IS NOT NULL, build_error, updated_at
			FROM wiikaraoke_song_creations WHERE pid = $1 ORDER BY updated_at DESC`, pid)
		if err != nil {
			songEditorError(w, http.StatusInternalServerError, "database error")
			return
		}
		defer rows.Close()
		list := []songEditorSummary{}
		for rows.Next() {
			var s songEditorSummary
			if rows.Scan(&s.ID, &s.Title, &s.Status, &s.ReviewNote, &s.HasBuilt, &s.BuildError, &s.UpdatedAt) == nil {
				list = append(list, s)
			}
		}
		songEditorJSON(w, http.StatusOK, list)
	case http.MethodPost:
		if songEditorBanned(pid) {
			songEditorError(w, http.StatusForbidden, "this account can't save creations")
			return
		}
		if songEditorRateLimited("save:"+strconv.FormatInt(pid, 10), 30, time.Minute) {
			songEditorError(w, http.StatusTooManyRequests, "too many saves, slow down a little")
			return
		}
		var count int
		db.QueryRow(`SELECT COUNT(*) FROM wiikaraoke_song_creations WHERE pid = $1`, pid).Scan(&count)
		if count >= songEditorMaxCreations {
			songEditorError(w, http.StatusForbidden, fmt.Sprintf("you can keep at most %d songs - delete some first", songEditorMaxCreations))
			return
		}
		var id int64
		err := db.QueryRow(`INSERT INTO wiikaraoke_song_creations (pid, title) VALUES ($1, '') RETURNING id`, pid).Scan(&id)
		if err != nil {
			log.Printf("song editor: insert: %v", err)
			songEditorError(w, http.StatusInternalServerError, "database error")
			return
		}
		songEditorJSON(w, http.StatusCreated, map[string]any{"id": id})
	default:
		songEditorError(w, http.StatusMethodNotAllowed, "GET or POST")
	}
}

// songEditorSaveRequest is the body for a metadata/spec update (PUT). The
// backing track and any aux file (MIDI / a cappella reference) are
// uploaded separately as multipart form posts to audio/aux sub-paths,
// since they can be sizeable binary files.
type songEditorSaveRequest struct {
	Title string          `json:"title"`
	Spec  json.RawMessage `json:"spec"`
}

func songEditorCreation(w http.ResponseWriter, r *http.Request) {
	pid, ok := songEditorAuth(w, r)
	if !ok {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, songEditorBase+"api/creations/")
	idStr, sub, _ := strings.Cut(rest, "/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		songEditorError(w, http.StatusNotFound, "not found")
		return
	}

	var (
		title, status, note, buildError, audioName, auxName string
		lyricsAuxName, lyricsSyncStatus, lyricsSyncError    string
		notesSyncStatus, notesSyncError                     string
		spec                                                []byte
		hasAudio, hasAux, hasBuilt, hasLyricsAux            bool
		updated                                             time.Time
	)
	err = db.QueryRow(`SELECT title, status, review_note, build_error, audio_name, aux_name, spec,
			audio IS NOT NULL, aux IS NOT NULL, built IS NOT NULL, updated_at,
			lyrics_aux_name, lyrics_aux IS NOT NULL, lyrics_sync_status, lyrics_sync_error,
			notes_sync_status, notes_sync_error
		FROM wiikaraoke_song_creations WHERE id = $1 AND pid = $2`, id, pid).Scan(
		&title, &status, &note, &buildError, &audioName, &auxName, &spec, &hasAudio, &hasAux, &hasBuilt, &updated,
		&lyricsAuxName, &hasLyricsAux, &lyricsSyncStatus, &lyricsSyncError,
		&notesSyncStatus, &notesSyncError,
	)
	if errors.Is(err, sql.ErrNoRows) {
		songEditorError(w, http.StatusNotFound, "not found")
		return
	} else if err != nil {
		songEditorError(w, http.StatusInternalServerError, "database error")
		return
	}

	switch sub {
	case "audio", "aux", "lyricsAux":
		songEditorServeBlob(w, r, id, sub)
		return
	case "lyrics-sync":
		songEditorLyricsSyncStart(w, r, pid, id)
		return
	case "notes-sync":
		songEditorNotesSyncStart(w, r, pid, id)
		return
	case "download":
		songEditorDownload(w, id, hasBuilt)
		return
	case "build":
		songEditorBuild(w, r, pid, id, status)
		return
	case "submit", "withdraw":
		if r.Method != http.MethodPost {
			songEditorError(w, http.StatusMethodNotAllowed, "POST only")
			return
		}
		var empty struct{}
		if !songEditorReadJSON(w, r, &empty) {
			return
		}
		songEditorSubmit(w, pid, id, status, hasBuilt, sub == "submit")
		return
	case "":
		// fall through to the plain CRUD below
	default:
		songEditorError(w, http.StatusNotFound, "not found")
		return
	}

	switch r.Method {
	case http.MethodGet:
		out := map[string]any{
			"id": id, "title": title, "status": status, "reviewNote": note,
			"buildError": buildError, "spec": json.RawMessage(spec), "updatedAt": updated,
			"audioName": audioName, "auxName": auxName, "hasAudio": hasAudio, "hasAux": hasAux, "hasBuilt": hasBuilt,
			"lyricsAuxName": lyricsAuxName, "hasLyricsAux": hasLyricsAux,
			"lyricsSyncStatus": lyricsSyncStatus, "lyricsSyncError": lyricsSyncError,
			"notesSyncStatus": notesSyncStatus, "notesSyncError": notesSyncError,
		}
		if lyricsSyncStatus == "queued" || lyricsSyncStatus == "processing" {
			out["lyricsQueueAhead"] = songEditorLyricsSyncQueueAhead(id)
		}
		if notesSyncStatus == "queued" || notesSyncStatus == "processing" {
			out["notesQueueAhead"] = songEditorNotesSyncQueueAhead(id)
		}
		songEditorJSON(w, http.StatusOK, out)
	case http.MethodPut:
		if status == "submitted" || status == "approved" {
			songEditorError(w, http.StatusConflict, "this song is "+status+" and can't be changed - duplicate it instead")
			return
		}
		if songEditorBanned(pid) {
			songEditorError(w, http.StatusForbidden, "this account can't save creations")
			return
		}
		var req songEditorSaveRequest
		if !songEditorReadJSON(w, r, &req) {
			return
		}
		if len(req.Title) > songEditorMaxTitle {
			songEditorError(w, http.StatusBadRequest, "title is too long")
			return
		}
		if len(req.Spec) == 0 {
			req.Spec = []byte("{}")
		}
		var parsed any
		if err := json.Unmarshal(req.Spec, &parsed); err != nil {
			songEditorError(w, http.StatusBadRequest, "spec is not valid JSON")
			return
		}
		// editing a rejected song turns it back into a draft, and a successful
		// edit invalidates any previous build until the next "build" call
		_, err = db.Exec(`UPDATE wiikaraoke_song_creations SET title = $1, spec = $2,
			status = 'draft', built = NULL, build_error = '', updated_at = NOW() WHERE id = $3 AND pid = $4`,
			req.Title, string(req.Spec), id, pid)
		if err != nil {
			songEditorError(w, http.StatusInternalServerError, "database error")
			return
		}
		songEditorJSON(w, http.StatusOK, map[string]any{"id": id})
	case http.MethodDelete:
		if status == "approved" {
			songEditorError(w, http.StatusConflict, "approved songs can't be deleted")
			return
		}
		db.Exec(`DELETE FROM wiikaraoke_song_creations WHERE id = $1 AND pid = $2`, id, pid)
		songEditorJSON(w, http.StatusOK, map[string]any{"deleted": true})
	default:
		songEditorError(w, http.StatusMethodNotAllowed, "GET, PUT or DELETE")
	}
}

// songEditorServeBlob accepts a multipart upload (POST) or serves the
// stored file back (GET) for the audio/aux sub-path.
func songEditorServeBlob(w http.ResponseWriter, r *http.Request, id int64, which string) {
	column, nameColumn, maxSize := "audio", "audio_name", songEditorMaxAudio
	if which == "aux" {
		column, nameColumn, maxSize = "aux", "aux_name", songEditorMaxAux
	} else if which == "lyricsAux" {
		// a separate upload slot from "aux" (notes_mode's own midi/acapella
		// file) - the user wants lyrics acapella-sync to have its own file,
		// not forced to reuse whatever notes_mode happens to be using.
		column, nameColumn, maxSize = "lyrics_aux", "lyrics_aux_name", songEditorMaxAux
	}
	switch r.Method {
	case http.MethodGet:
		var data []byte
		var name string
		err := db.QueryRow(`SELECT `+column+`, `+nameColumn+` FROM wiikaraoke_song_creations WHERE id = $1`, id).Scan(&data, &name)
		if err != nil || data == nil {
			songEditorError(w, http.StatusNotFound, "not found")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `inline; filename="`+name+`"`)
		w.Write(data)
	case http.MethodPost:
		// Cap the raw body BEFORE parsing - without this, ParseMultipartForm
		// will happily spend a long time reading/spooling an oversized body
		// to disk instead of failing fast, and a retry while that's still in
		// flight can degrade the whole service (seen in testing: 413 on an
		// oversized upload, then 502s on every retry until relay-admin was
		// restarted). MaxBytesReader rejects it immediately instead.
		r.Body = http.MaxBytesReader(w, r.Body, int64(maxSize)+(2<<20))
		if err := r.ParseMultipartForm(int64(maxSize) + (1 << 20)); err != nil {
			songEditorError(w, http.StatusRequestEntityTooLarge, "upload too large or malformed")
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			songEditorError(w, http.StatusBadRequest, "missing file")
			return
		}
		defer file.Close()
		data := make([]byte, maxSize+1)
		n, _ := file.Read(data)
		for n <= maxSize {
			more, err := file.Read(data[n:])
			n += more
			if err != nil {
				break
			}
		}
		if n > maxSize {
			songEditorError(w, http.StatusRequestEntityTooLarge, "file is too large")
			return
		}
		_, err = db.Exec(`UPDATE wiikaraoke_song_creations SET `+column+` = $1, `+nameColumn+` = $2,
			built = NULL, build_error = '', updated_at = NOW() WHERE id = $3`, data[:n], header.Filename, id)
		if err != nil {
			songEditorError(w, http.StatusInternalServerError, "database error")
			return
		}
		songEditorJSON(w, http.StatusOK, map[string]any{"ok": true, "bytes": n})
	default:
		songEditorError(w, http.StatusMethodNotAllowed, "GET or POST")
	}
}

func songEditorDownload(w http.ResponseWriter, id int64, hasBuilt bool) {
	if !hasBuilt {
		songEditorError(w, http.StatusNotFound, "not built yet")
		return
	}
	var data []byte
	if err := db.QueryRow(`SELECT built FROM wiikaraoke_song_creations WHERE id = $1`, id).Scan(&data); err != nil {
		songEditorError(w, http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="song-%d.ujk"`, id))
	w.Write(data)
}

func songEditorSubmit(w http.ResponseWriter, pid, id int64, status string, hasBuilt, submit bool) {
	if !submit {
		if status != "submitted" {
			songEditorError(w, http.StatusConflict, "only submitted songs can be withdrawn")
			return
		}
		db.Exec(`UPDATE wiikaraoke_song_creations SET status = 'draft', submitted_at = NULL, updated_at = NOW()
			WHERE id = $1 AND pid = $2 AND status = 'submitted'`, id, pid)
		songEditorJSON(w, http.StatusOK, map[string]any{"id": id, "status": "draft"})
		return
	}
	if status != "draft" && status != "rejected" {
		songEditorError(w, http.StatusConflict, "this song is already "+status)
		return
	}
	if !hasBuilt {
		songEditorError(w, http.StatusConflict, "build the song successfully before submitting it")
		return
	}
	if songEditorBanned(pid) {
		songEditorError(w, http.StatusForbidden, "this account can't submit songs")
		return
	}
	var pending int
	db.QueryRow(`SELECT COUNT(*) FROM wiikaraoke_song_creations WHERE pid = $1 AND status = 'submitted'`, pid).Scan(&pending)
	if pending >= songEditorMaxPending {
		songEditorError(w, http.StatusForbidden, fmt.Sprintf("you already have %d songs waiting for review", songEditorMaxPending))
		return
	}
	_, err := db.Exec(`UPDATE wiikaraoke_song_creations SET status = 'submitted', submitted_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND pid = $2`, id, pid)
	if err != nil {
		songEditorError(w, http.StatusInternalServerError, "database error")
		return
	}
	songEditorJSON(w, http.StatusOK, map[string]any{"id": id, "status": "submitted"})
}
