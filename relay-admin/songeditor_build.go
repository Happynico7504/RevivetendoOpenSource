package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
)

// songEditorSpec is what's stored in wiikaraoke_song_creations.spec, set by
// the editor UI. notesMode selects which of the other fields matter.
type songEditorSpec struct {
	NotesMode   string          `json:"notes_mode"`
	ManualNotes json.RawMessage `json:"manual_notes,omitempty"`
	Event3MS    int             `json:"event3_ms,omitempty"`
	// LeadInMS: total target preamble length in ms, scaled across all 8
	// preamble events (track.scale_preamble_overrides, see cli.py) -
	// unset means auto-detect from whichever aux recording is available
	// (notes.detect_lead_in_ms). Set either by the player touching it up,
	// or cached back automatically once a sync job/build auto-detects it.
	LeadInMS   *int   `json:"lead_in_ms,omitempty"`
	MirrorReal bool   `json:"mirror_real,omitempty"`
	Title      string `json:"title,omitempty"`
	Artist     string `json:"artist,omitempty"`
	Writer     string `json:"writer,omitempty"`
	Composer   string `json:"composer,omitempty"`
	// LyricsMode selects which of the three fields below matters, same
	// pattern as NotesMode - see lyrics_sync.py. Omitted entirely means
	// no custom lyrics (borrows the template's, as before this feature).
	LyricsMode string `json:"lyrics_mode,omitempty"`
	// LyricsLines ([text, startMS, endMS] per element) for "manual".
	LyricsLines json.RawMessage `json:"lyrics_lines,omitempty"`
	// LyricsLRC: raw .lrc file text for "lrc".
	LyricsLRC string `json:"lyrics_lrc,omitempty"`
	// LyricsRawLines: plain lyric strings, no timestamps, for "acapella" -
	// auto-synced against the SAME aux upload notes_mode="acapella" uses
	// (voice-activity only, not pitch, so it works regardless of what
	// notes_mode actually is - but there is only one aux upload today,
	// not a separate one for lyrics specifically).
	LyricsRawLines json.RawMessage `json:"lyrics_raw_lines,omitempty"`
}

// cliSpec is what actually goes to cli.py over stdin: the player's spec
// plus server-controlled paths the player never gets to set directly.
type cliSpec struct {
	RPXPath        string          `json:"rpx_path"`
	TemplatePath   string          `json:"template_path"`
	NotesMode      string          `json:"notes_mode"`
	ManualNotes    json.RawMessage `json:"manual_notes,omitempty"`
	Event3MS       int             `json:"event3_ms,omitempty"`
	LeadInMS       *int            `json:"lead_in_ms,omitempty"`
	MirrorReal     bool            `json:"mirror_real,omitempty"`
	Title          string          `json:"title,omitempty"`
	Artist         string          `json:"artist,omitempty"`
	Writer         string          `json:"writer,omitempty"`
	Composer       string          `json:"composer,omitempty"`
	LyricsMode     string          `json:"lyrics_mode,omitempty"`
	LyricsLines    json.RawMessage `json:"lyrics_lines,omitempty"`
	LyricsLRC      string          `json:"lyrics_lrc,omitempty"`
	LyricsRawLines json.RawMessage `json:"lyrics_raw_lines,omitempty"`
	AudioPath      string          `json:"audio_path"`
	AuxPath        string          `json:"aux_path,omitempty"`
	OutPath        string          `json:"out_path"`
}

func songEditorBuild(w http.ResponseWriter, r *http.Request, pid, id int64, status string) {
	if r.Method != http.MethodPost {
		songEditorError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var empty struct{}
	if !songEditorReadJSON(w, r, &empty) {
		return
	}
	if status == "submitted" || status == "approved" {
		songEditorError(w, http.StatusConflict, "this song is "+status+" and can't be rebuilt - duplicate it instead")
		return
	}
	if songEditorRateLimited("build:"+fmt.Sprint(pid), 10, songEditorBuildTimeout) {
		songEditorError(w, http.StatusTooManyRequests, "too many builds, slow down a little")
		return
	}

	var specRaw []byte
	var audioData, auxData []byte
	var audioName, auxName string
	err := db.QueryRow(`SELECT spec, audio, aux, audio_name, aux_name FROM wiikaraoke_song_creations
		WHERE id = $1 AND pid = $2`, id, pid).Scan(&specRaw, &audioData, &auxData, &audioName, &auxName)
	if err != nil {
		songEditorError(w, http.StatusNotFound, "not found")
		return
	}

	var spec songEditorSpec
	if err := json.Unmarshal(specRaw, &spec); err != nil {
		songEditorError(w, http.StatusBadRequest, "saved spec is not valid - fix it in the editor and save again")
		return
	}
	if spec.NotesMode != "manual" && spec.NotesMode != "midi" && spec.NotesMode != "acapella" && spec.NotesMode != "acapella_ml" {
		songEditorError(w, http.StatusBadRequest, "pick a notes mode (manual, midi, a cappella or a cappella ML) before building")
		return
	}
	if len(audioData) == 0 {
		songEditorError(w, http.StatusBadRequest, "upload a backing track before building")
		return
	}
	// notes_mode="acapella_ml" doesn't need anything checked here - like
	// lyrics_mode="acapella", it's resolved entirely by the separate
	// background sync queue (songeditor_notes_sync.go), using the
	// existing aux upload. This build endpoint just consumes whatever
	// spec.manual_notes that already wrote back (cli.py refuses to
	// build without it, with a clear error naming the Sync notes button).
	if spec.NotesMode != "manual" && spec.NotesMode != "acapella_ml" && len(auxData) == 0 {
		songEditorError(w, http.StatusBadRequest, "upload a "+spec.NotesMode+" file before building")
		return
	}
	if spec.LyricsMode != "" && spec.LyricsMode != "manual" && spec.LyricsMode != "lrc" && spec.LyricsMode != "acapella" {
		songEditorError(w, http.StatusBadRequest, "pick a valid lyrics mode (manual, lrc or acapella)")
		return
	}
	// lyrics_mode="acapella" doesn't need anything checked here - it's
	// resolved entirely by the separate background sync queue
	// (songeditor_lyrics_sync.go), using its own aux upload. This build
	// endpoint just consumes whatever spec.lyrics_lines that already
	// wrote back (cli.py refuses to build without it, with a clear
	// error naming the Sync lyrics button).

	result, buildErr := songEditorRunBuild(id, spec, audioData, auxData, audioName, auxName)
	if buildErr != nil {
		db.Exec(`UPDATE wiikaraoke_song_creations SET built = NULL, build_error = $1, updated_at = NOW() WHERE id = $2`,
			buildErr.Error(), id)
		songEditorError(w, http.StatusBadRequest, buildErr.Error())
		return
	}
	db.Exec(`UPDATE wiikaraoke_song_creations SET built = $1, build_error = '', updated_at = NOW() WHERE id = $2`,
		result.built, id)
	// MIDI/a cappella builds only ever extract notes in Python, which never
	// otherwise reaches storage - without this there is nothing for "Manual"
	// mode to show if the player wants to touch up an automatic result.
	// Manual mode is already its own source of truth, so don't overwrite it
	// with itself.
	if spec.NotesMode != "manual" && len(result.notes) > 0 {
		db.Exec(`UPDATE wiikaraoke_song_creations SET spec = jsonb_set(spec, '{manual_notes}', $1::jsonb) WHERE id = $2`,
			string(result.notes), id)
	}
	// Same reasoning as manual_notes above: lrc/acapella lyric syncing
	// only ever happens in Python, so cache the resolved per-line timing
	// back for "manual" lyrics mode to show/touch up.
	if spec.LyricsMode != "" && spec.LyricsMode != "manual" && len(result.lyricsLines) > 0 {
		db.Exec(`UPDATE wiikaraoke_song_creations SET spec = jsonb_set(spec, '{lyrics_lines}', $1::jsonb) WHERE id = $2`,
			string(result.lyricsLines), id)
	}
	// Caches a freshly auto-detected lead-in the same way - only ever
	// returned by cli.py when the player hadn't already set one, so this
	// never overwrites a manual touch-up with an auto-detected value.
	if spec.LeadInMS == nil && result.leadInMS != nil {
		db.Exec(`UPDATE wiikaraoke_song_creations SET spec = jsonb_set(spec, '{lead_in_ms}', $1::jsonb) WHERE id = $2`,
			*result.leadInMS, id)
	}
	songEditorJSON(w, http.StatusOK, map[string]any{
		"ok": true, "totalMS": result.totalMS, "actualAudioMS": result.actualAudioMS, "noteCount": result.noteCount,
		"notes": result.notes, "lyricsLines": result.lyricsLines,
	})
}

type songEditorBuildResult struct {
	built         []byte
	totalMS       float64
	actualAudioMS float64
	noteCount     int
	notes         json.RawMessage // [[duration_ms, pitch], ...] - same shape as manual_notes
	lyricsLines   json.RawMessage // [[text, start_ms, end_ms], ...] - same shape as lyrics_lines
	leadInMS      *int            // auto-detected lead-in, when cli.py computed one fresh
}

func songEditorRunBuild(id int64, spec songEditorSpec, audioData, auxData []byte, audioName, auxName string) (*songEditorBuildResult, error) {
	dir, err := os.MkdirTemp("", fmt.Sprintf("songeditor-%d-", id))
	if err != nil {
		return nil, fmt.Errorf("could not allocate a build directory")
	}
	defer os.RemoveAll(dir)

	audioPath := filepath.Join(dir, "audio"+safeExt(audioName))
	if err := os.WriteFile(audioPath, audioData, 0o600); err != nil {
		return nil, fmt.Errorf("could not stage the backing track")
	}
	cli := cliSpec{
		RPXPath:        songEditorRPXPath,
		TemplatePath:   songEditorTemplatePath,
		NotesMode:      spec.NotesMode,
		ManualNotes:    spec.ManualNotes,
		Event3MS:       spec.Event3MS,
		LeadInMS:       spec.LeadInMS,
		MirrorReal:     spec.MirrorReal,
		Title:          spec.Title,
		Artist:         spec.Artist,
		Writer:         spec.Writer,
		Composer:       spec.Composer,
		LyricsMode:     spec.LyricsMode,
		LyricsLines:    spec.LyricsLines,
		LyricsLRC:      spec.LyricsLRC,
		LyricsRawLines: spec.LyricsRawLines,
		AudioPath:      audioPath,
		OutPath:        filepath.Join(dir, "out.ujk"),
	}
	if spec.NotesMode != "manual" {
		auxPath := filepath.Join(dir, "aux"+safeExt(auxName))
		if err := os.WriteFile(auxPath, auxData, 0o600); err != nil {
			return nil, fmt.Errorf("could not stage the aux file")
		}
		cli.AuxPath = auxPath
	}

	reqJSON, err := json.Marshal(cli)
	if err != nil {
		return nil, fmt.Errorf("could not encode the build request")
	}

	ctx, cancel := context.WithTimeout(context.Background(), songEditorBuildTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, songEditorPythonBin, "-m", "songeditor.cli")
	cmd.Dir = songEditorToolsDir
	cmd.Stdin = bytes.NewReader(reqJSON)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	var out struct {
		OK            bool            `json:"ok"`
		Error         string          `json:"error"`
		TotalMS       float64         `json:"total_ms"`
		ActualAudioMS float64         `json:"actual_audio_ms"`
		NoteCount     int             `json:"note_count"`
		Notes         json.RawMessage `json:"notes"`
		LyricsLines   json.RawMessage `json:"lyrics_lines"`
		LeadInMS      *int            `json:"lead_in_ms"`
	}
	if jsonErr := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); jsonErr != nil {
		log.Printf("song editor: build %d produced no JSON (run err: %v): stderr=%q stdout=%q", id, runErr, stderr.String(), stdout.String())
		return nil, fmt.Errorf("the builder crashed unexpectedly - this has been logged")
	}
	if !out.OK {
		return nil, fmt.Errorf("%s", out.Error)
	}

	built, err := os.ReadFile(cli.OutPath)
	if err != nil {
		return nil, fmt.Errorf("the builder reported success but no file was produced")
	}
	return &songEditorBuildResult{
		built: built, totalMS: out.TotalMS, actualAudioMS: out.ActualAudioMS, noteCount: out.NoteCount,
		notes: out.Notes, lyricsLines: out.LyricsLines, leadInMS: out.LeadInMS,
	}, nil
}

// safeExt keeps ffmpeg/our own parsers happy about format sniffing by
// extension while never passing the player's raw filename to exec.Command.
func safeExt(name string) string {
	ext := filepath.Ext(name)
	if len(ext) < 2 || len(ext) > 6 {
		return ""
	}
	for _, c := range ext[1:] {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
			return ""
		}
	}
	return ext
}
