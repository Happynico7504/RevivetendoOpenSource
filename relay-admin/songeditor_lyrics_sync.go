package main

// Background a cappella lyrics-sync queue for the song editor: syncing
// pasted lyric lines against an isolated vocal recording runs a local,
// CPU-only speech-recognition model (tools/songeditor/lyrics_sync.py's
// from_acapella - confirmed 2026-11-14 to take ~25-30s per song on this
// box). That's long and CPU-heavy enough to need the exact same treatment
// as eshop_videos' conversion queue (videos.go): one job at a time, at
// the lowest CPU priority (videoCmd's chrt --idle/nice/ionice wrapper,
// reused directly - same reasoning, same package), queued in the
// database so the order survives a restart, with players able to see
// their place in line the same way the video upload page already does.
//
//	POST /my/songeditor/api/creations/{id}/lyricsAux    upload the vocal recording (own slot, not shared with notes_mode's own aux)
//	POST /my/songeditor/api/creations/{id}/lyrics-sync  enqueue a sync job
//
// GET .../creations/{id} already reports lyricsSyncStatus/lyricsQueueAhead/
// lyricsSyncError/hasLyricsAux/lyricsAuxName (songeditor.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

const songEditorLyricsSyncTimeout = 5 * time.Minute

var (
	songEditorLyricsSyncWake   = make(chan struct{}, 1)
	songEditorLyricsSyncActive atomic.Int64
)

const songEditorLyricsSyncQueueOrder = `(COALESCE(lyrics_queued_at, created_at), id)`

func songEditorLyricsSyncEnqueue(id int64) {
	select {
	case songEditorLyricsSyncWake <- struct{}{}:
	default:
	}
}

// songEditorLyricsSyncQueueAhead returns how many jobs will be synced
// before id, or -1 while id itself is being synced - same shape as
// videoQueueAhead.
func songEditorLyricsSyncQueueAhead(id int64) int {
	if songEditorLyricsSyncActive.Load() == id {
		return -1
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM wiikaraoke_song_creations q, wiikaraoke_song_creations me
		WHERE me.id = $1 AND q.lyrics_sync_status = 'queued'
		AND (COALESCE(q.lyrics_queued_at, q.created_at), q.id) < (COALESCE(me.lyrics_queued_at, me.created_at), me.id)`, id).Scan(&n)
	return n
}

// songEditorLyricsSyncStart validates and enqueues a job (POST only - the
// actual sync happens in the background worker, this just marks it queued).
func songEditorLyricsSyncStart(w http.ResponseWriter, r *http.Request, pid, id int64) {
	if r.Method != http.MethodPost {
		songEditorError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var empty struct{}
	if !songEditorReadJSON(w, r, &empty) {
		return
	}
	var specRaw []byte
	var status, lyricsSyncStatus string
	var hasLyricsAux bool
	err := db.QueryRow(`SELECT spec, status, lyrics_sync_status, lyrics_aux IS NOT NULL
		FROM wiikaraoke_song_creations WHERE id = $1 AND pid = $2`, id, pid).Scan(
		&specRaw, &status, &lyricsSyncStatus, &hasLyricsAux)
	if err != nil {
		songEditorError(w, http.StatusNotFound, "not found")
		return
	}
	if status == "submitted" || status == "approved" {
		songEditorError(w, http.StatusConflict, "this song is "+status+" and can't be changed - duplicate it instead")
		return
	}
	if lyricsSyncStatus == "queued" || lyricsSyncStatus == "processing" {
		songEditorError(w, http.StatusConflict, "a lyrics sync is already running for this song")
		return
	}
	var spec songEditorSpec
	if err := json.Unmarshal(specRaw, &spec); err != nil {
		songEditorError(w, http.StatusBadRequest, "saved spec is not valid - fix it in the editor and save again")
		return
	}
	var rawLines []string
	if err := json.Unmarshal(spec.LyricsRawLines, &rawLines); err != nil || len(rawLines) == 0 {
		songEditorError(w, http.StatusBadRequest, "paste at least one lyric line before syncing")
		return
	}
	if !hasLyricsAux {
		songEditorError(w, http.StatusBadRequest, "upload an a cappella recording before syncing lyrics to it")
		return
	}
	if songEditorRateLimited("lyrics-sync:"+fmt.Sprint(pid), 10, time.Hour) {
		songEditorError(w, http.StatusTooManyRequests, "too many lyrics syncs, slow down a little")
		return
	}
	_, err = db.Exec(`UPDATE wiikaraoke_song_creations SET lyrics_sync_status = 'queued', lyrics_queued_at = NOW(),
		lyrics_sync_error = '', updated_at = NOW() WHERE id = $1`, id)
	if err != nil {
		songEditorError(w, http.StatusInternalServerError, "database error")
		return
	}
	log.Printf("song editor: lyrics sync #%d queued by PID %d (%d lines)", id, pid, len(rawLines))
	songEditorLyricsSyncEnqueue(id)
	songEditorJSON(w, http.StatusOK, map[string]any{"ok": true, "queueAhead": songEditorLyricsSyncQueueAhead(id)})
}

func songEditorLyricsSyncWorker() {
	for {
		var id int64
		err := db.QueryRow(`SELECT id FROM wiikaraoke_song_creations WHERE lyrics_sync_status = 'queued'
			ORDER BY ` + songEditorLyricsSyncQueueOrder + ` LIMIT 1`).Scan(&id)
		if err != nil {
			select {
			case <-songEditorLyricsSyncWake:
			case <-time.After(time.Minute):
			}
			continue
		}
		songEditorLyricsSyncActive.Store(id)
		db.Exec(`UPDATE wiikaraoke_song_creations SET lyrics_sync_status = 'processing', updated_at = NOW() WHERE id = $1`, id)
		songEditorLyricsSyncProcess(id)
		songEditorLyricsSyncActive.Store(0)
		// Never pick the same job forever if its status somehow didn't move on.
		if res, err := db.Exec(`UPDATE wiikaraoke_song_creations SET lyrics_sync_status = 'failed',
			lyrics_sync_error = $2, updated_at = NOW() WHERE id = $1 AND lyrics_sync_status = 'processing'`,
			id, "the sync worker exited unexpectedly - this has been logged"); err != nil {
			time.Sleep(time.Minute) // the database is unreachable: don't spin
		} else if n, _ := res.RowsAffected(); n > 0 {
			log.Printf("song editor: lyrics sync #%d was still 'processing' after the worker returned, marked failed", id)
		}
	}
}

func songEditorLyricsSyncProcess(id int64) {
	var specRaw, auxData []byte
	var auxName string
	if err := db.QueryRow(`SELECT spec, lyrics_aux, lyrics_aux_name FROM wiikaraoke_song_creations WHERE id = $1`, id).
		Scan(&specRaw, &auxData, &auxName); err != nil {
		db.Exec(`UPDATE wiikaraoke_song_creations SET lyrics_sync_status = 'failed', lyrics_sync_error = $2,
			updated_at = NOW() WHERE id = $1`, id, "could not load this song's data")
		return
	}
	var spec songEditorSpec
	if err := json.Unmarshal(specRaw, &spec); err != nil {
		db.Exec(`UPDATE wiikaraoke_song_creations SET lyrics_sync_status = 'failed', lyrics_sync_error = $2,
			updated_at = NOW() WHERE id = $1`, id, "the saved spec is not valid")
		return
	}
	var rawLines []string
	json.Unmarshal(spec.LyricsRawLines, &rawLines)

	dir, err := os.MkdirTemp("", fmt.Sprintf("lyricssync-%d-", id))
	if err != nil {
		db.Exec(`UPDATE wiikaraoke_song_creations SET lyrics_sync_status = 'failed', lyrics_sync_error = $2,
			updated_at = NOW() WHERE id = $1`, id, "could not allocate a work directory")
		return
	}
	defer os.RemoveAll(dir)
	auxPath := filepath.Join(dir, "aux"+safeExt(auxName))
	if err := os.WriteFile(auxPath, auxData, 0o600); err != nil {
		db.Exec(`UPDATE wiikaraoke_song_creations SET lyrics_sync_status = 'failed', lyrics_sync_error = $2,
			updated_at = NOW() WHERE id = $1`, id, "could not stage the recording")
		return
	}

	reqSpec := map[string]any{
		"rpx_path":      songEditorRPXPath,
		"template_path": songEditorTemplatePath,
		"event3_ms":     spec.Event3MS,
		"lead_in_ms":    spec.LeadInMS,
		"raw_lines":     rawLines,
		"aux_path":      auxPath,
		"use_ml":        true,
	}
	reqJSON, err := json.Marshal(reqSpec)
	if err != nil {
		db.Exec(`UPDATE wiikaraoke_song_creations SET lyrics_sync_status = 'failed', lyrics_sync_error = $2,
			updated_at = NOW() WHERE id = $1`, id, "could not encode the sync request")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), songEditorLyricsSyncTimeout)
	defer cancel()
	// videoCmd (videos.go): chrt --idle + nice 19 + ionice -c 3 - the same
	// lowest-priority treatment as video conversion, so a sync job never
	// competes with the live game servers for CPU.
	cmd := videoCmd(ctx, songEditorPythonBin, "-m", "songeditor.lyrics_sync_cli")
	cmd.Dir = songEditorToolsDir
	cmd.Stdin = bytes.NewReader(reqJSON)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	var out struct {
		OK          bool            `json:"ok"`
		Error       string          `json:"error"`
		LyricsLines json.RawMessage `json:"lyrics_lines"`
		LeadInMS    *int            `json:"lead_in_ms"`
	}
	if jsonErr := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); jsonErr != nil {
		log.Printf("song editor: lyrics sync #%d produced no JSON (run err: %v): stderr=%q stdout=%q", id, runErr, stderr.String(), stdout.String())
		db.Exec(`UPDATE wiikaraoke_song_creations SET lyrics_sync_status = 'failed', lyrics_sync_error = $2,
			updated_at = NOW() WHERE id = $1`, id, "the sync worker crashed unexpectedly - this has been logged")
		return
	}
	if !out.OK {
		db.Exec(`UPDATE wiikaraoke_song_creations SET lyrics_sync_status = 'failed', lyrics_sync_error = $2,
			updated_at = NOW() WHERE id = $1`, id, out.Error)
		return
	}
	db.Exec(`UPDATE wiikaraoke_song_creations SET lyrics_sync_status = 'done', lyrics_sync_error = '',
		spec = jsonb_set(spec, '{lyrics_lines}', $1::jsonb), updated_at = NOW() WHERE id = $2`,
		string(out.LyricsLines), id)
	// Only ever caches a freshly auto-detected value (spec.LeadInMS was
	// nil going in) - never overwrites a player's own manual touch-up.
	if spec.LeadInMS == nil && out.LeadInMS != nil {
		db.Exec(`UPDATE wiikaraoke_song_creations SET spec = jsonb_set(spec, '{lead_in_ms}', $1::jsonb) WHERE id = $2`,
			*out.LeadInMS, id)
	}
	log.Printf("song editor: lyrics sync #%d done", id)
}
