package main

// Background ML note-detection queue for the song editor: a separate,
// parallel pipeline from songeditor_lyrics_sync.go - the user explicitly
// chose NOT to generalize the two into one shared queue (2026-11-14),
// so this mirrors that file's structure closely rather than reusing its
// code. notes_mode="acapella_ml" runs tools/songeditor/notes.py's
// from_acapella_ml (torchcrepe, a neural CPU pitch detector,
// meaningfully more accurate than notes_mode="acapella"'s autocorrelation
// heuristic but confirmed ~55-66s / ~560MB peak RSS per song - the same
// "too slow/heavy for a synchronous request" reasoning as lyrics' ML
// sync, so it gets the same one-at-a-time, lowest-CPU-priority queue
// treatment (videoCmd's chrt/nice/ionice wrapper, same as lyrics sync).
//
// Unlike lyrics sync, this reuses the EXISTING aux/aux_name upload (the
// same vocal recording notes_mode="acapella" already uses) rather than
// a separate upload slot - there's no reason for the same song to need
// two different vocal recordings for its two acapella-based modes.
//
//	POST /my/songeditor/api/creations/{id}/notes-sync  enqueue a sync job
//
// GET .../creations/{id} already reports notesSyncStatus/notesQueueAhead/
// notesSyncError (songeditor.go).

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

const songEditorNotesSyncTimeout = 5 * time.Minute

var (
	songEditorNotesSyncWake   = make(chan struct{}, 1)
	songEditorNotesSyncActive atomic.Int64
)

const songEditorNotesSyncQueueOrder = `(COALESCE(notes_queued_at, created_at), id)`

func songEditorNotesSyncEnqueue(id int64) {
	select {
	case songEditorNotesSyncWake <- struct{}{}:
	default:
	}
}

// songEditorNotesSyncQueueAhead returns how many jobs will be synced
// before id, or -1 while id itself is being synced - same shape as
// videoQueueAhead/songEditorLyricsSyncQueueAhead.
func songEditorNotesSyncQueueAhead(id int64) int {
	if songEditorNotesSyncActive.Load() == id {
		return -1
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM wiikaraoke_song_creations q, wiikaraoke_song_creations me
		WHERE me.id = $1 AND q.notes_sync_status = 'queued'
		AND (COALESCE(q.notes_queued_at, q.created_at), q.id) < (COALESCE(me.notes_queued_at, me.created_at), me.id)`, id).Scan(&n)
	return n
}

// songEditorNotesSyncStart validates and enqueues a job (POST only - the
// actual sync happens in the background worker, this just marks it queued).
func songEditorNotesSyncStart(w http.ResponseWriter, r *http.Request, pid, id int64) {
	if r.Method != http.MethodPost {
		songEditorError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var empty struct{}
	if !songEditorReadJSON(w, r, &empty) {
		return
	}
	var status, notesSyncStatus string
	var hasAux bool
	err := db.QueryRow(`SELECT status, notes_sync_status, aux IS NOT NULL
		FROM wiikaraoke_song_creations WHERE id = $1 AND pid = $2`, id, pid).Scan(
		&status, &notesSyncStatus, &hasAux)
	if err != nil {
		songEditorError(w, http.StatusNotFound, "not found")
		return
	}
	if status == "submitted" || status == "approved" {
		songEditorError(w, http.StatusConflict, "this song is "+status+" and can't be changed - duplicate it instead")
		return
	}
	if notesSyncStatus == "queued" || notesSyncStatus == "processing" {
		songEditorError(w, http.StatusConflict, "a notes sync is already running for this song")
		return
	}
	if !hasAux {
		songEditorError(w, http.StatusBadRequest, "upload an a cappella recording (in the Notes section) before syncing notes to it")
		return
	}
	if songEditorRateLimited("notes-sync:"+fmt.Sprint(pid), 10, time.Hour) {
		songEditorError(w, http.StatusTooManyRequests, "too many notes syncs, slow down a little")
		return
	}
	_, err = db.Exec(`UPDATE wiikaraoke_song_creations SET notes_sync_status = 'queued', notes_queued_at = NOW(),
		notes_sync_error = '', updated_at = NOW() WHERE id = $1`, id)
	if err != nil {
		songEditorError(w, http.StatusInternalServerError, "database error")
		return
	}
	log.Printf("song editor: notes sync #%d queued by PID %d", id, pid)
	songEditorNotesSyncEnqueue(id)
	songEditorJSON(w, http.StatusOK, map[string]any{"ok": true, "queueAhead": songEditorNotesSyncQueueAhead(id)})
}

func songEditorNotesSyncWorker() {
	for {
		var id int64
		err := db.QueryRow(`SELECT id FROM wiikaraoke_song_creations WHERE notes_sync_status = 'queued'
			ORDER BY ` + songEditorNotesSyncQueueOrder + ` LIMIT 1`).Scan(&id)
		if err != nil {
			select {
			case <-songEditorNotesSyncWake:
			case <-time.After(time.Minute):
			}
			continue
		}
		songEditorNotesSyncActive.Store(id)
		db.Exec(`UPDATE wiikaraoke_song_creations SET notes_sync_status = 'processing', updated_at = NOW() WHERE id = $1`, id)
		songEditorNotesSyncProcess(id)
		songEditorNotesSyncActive.Store(0)
		// Never pick the same job forever if its status somehow didn't move on.
		if res, err := db.Exec(`UPDATE wiikaraoke_song_creations SET notes_sync_status = 'failed',
			notes_sync_error = $2, updated_at = NOW() WHERE id = $1 AND notes_sync_status = 'processing'`,
			id, "the sync worker exited unexpectedly - this has been logged"); err != nil {
			time.Sleep(time.Minute) // the database is unreachable: don't spin
		} else if n, _ := res.RowsAffected(); n > 0 {
			log.Printf("song editor: notes sync #%d was still 'processing' after the worker returned, marked failed", id)
		}
	}
}

func songEditorNotesSyncProcess(id int64) {
	var specRaw, auxData []byte
	var auxName string
	if err := db.QueryRow(`SELECT spec, aux, aux_name FROM wiikaraoke_song_creations WHERE id = $1`, id).
		Scan(&specRaw, &auxData, &auxName); err != nil {
		db.Exec(`UPDATE wiikaraoke_song_creations SET notes_sync_status = 'failed', notes_sync_error = $2,
			updated_at = NOW() WHERE id = $1`, id, "could not load this song's data")
		return
	}
	var spec songEditorSpec
	if err := json.Unmarshal(specRaw, &spec); err != nil {
		db.Exec(`UPDATE wiikaraoke_song_creations SET notes_sync_status = 'failed', notes_sync_error = $2,
			updated_at = NOW() WHERE id = $1`, id, "the saved spec is not valid")
		return
	}

	dir, err := os.MkdirTemp("", fmt.Sprintf("notessync-%d-", id))
	if err != nil {
		db.Exec(`UPDATE wiikaraoke_song_creations SET notes_sync_status = 'failed', notes_sync_error = $2,
			updated_at = NOW() WHERE id = $1`, id, "could not allocate a work directory")
		return
	}
	defer os.RemoveAll(dir)
	auxPath := filepath.Join(dir, "aux"+safeExt(auxName))
	if err := os.WriteFile(auxPath, auxData, 0o600); err != nil {
		db.Exec(`UPDATE wiikaraoke_song_creations SET notes_sync_status = 'failed', notes_sync_error = $2,
			updated_at = NOW() WHERE id = $1`, id, "could not stage the recording")
		return
	}

	reqSpec := map[string]any{
		"rpx_path":      songEditorRPXPath,
		"template_path": songEditorTemplatePath,
		"event3_ms":     spec.Event3MS,
		"lead_in_ms":    spec.LeadInMS,
		"aux_path":      auxPath,
	}
	reqJSON, err := json.Marshal(reqSpec)
	if err != nil {
		db.Exec(`UPDATE wiikaraoke_song_creations SET notes_sync_status = 'failed', notes_sync_error = $2,
			updated_at = NOW() WHERE id = $1`, id, "could not encode the sync request")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), songEditorNotesSyncTimeout)
	defer cancel()
	// videoCmd (videos.go): chrt --idle + nice 19 + ionice -c 3 - the same
	// lowest-priority treatment as video conversion and lyrics sync, so
	// a sync job never competes with the live game servers for CPU.
	cmd := videoCmd(ctx, songEditorPythonBin, "-m", "songeditor.notes_sync_cli")
	cmd.Dir = songEditorToolsDir
	cmd.Stdin = bytes.NewReader(reqJSON)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	var out struct {
		OK       bool            `json:"ok"`
		Error    string          `json:"error"`
		Notes    json.RawMessage `json:"notes"`
		LeadInMS *int            `json:"lead_in_ms"`
	}
	if jsonErr := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); jsonErr != nil {
		log.Printf("song editor: notes sync #%d produced no JSON (run err: %v): stderr=%q stdout=%q", id, runErr, stderr.String(), stdout.String())
		db.Exec(`UPDATE wiikaraoke_song_creations SET notes_sync_status = 'failed', notes_sync_error = $2,
			updated_at = NOW() WHERE id = $1`, id, "the sync worker crashed unexpectedly - this has been logged")
		return
	}
	if !out.OK {
		db.Exec(`UPDATE wiikaraoke_song_creations SET notes_sync_status = 'failed', notes_sync_error = $2,
			updated_at = NOW() WHERE id = $1`, id, out.Error)
		return
	}
	db.Exec(`UPDATE wiikaraoke_song_creations SET notes_sync_status = 'done', notes_sync_error = '',
		spec = jsonb_set(spec, '{manual_notes}', $1::jsonb), updated_at = NOW() WHERE id = $2`,
		string(out.Notes), id)
	// Only ever caches a freshly auto-detected value (spec.LeadInMS was
	// nil going in) - never overwrites a player's own manual touch-up.
	if spec.LeadInMS == nil && out.LeadInMS != nil {
		db.Exec(`UPDATE wiikaraoke_song_creations SET spec = jsonb_set(spec, '{lead_in_ms}', $1::jsonb) WHERE id = $2`,
			*out.LeadInMS, id)
	}
	log.Printf("song editor: notes sync #%d done", id)
}
