package main

// Badge Arcade editor: a public web editor for custom badges (and later crane
// machines). Anyone can use it; saving needs the /my/ login and stores the
// creation under the player's PID. Admins review and deploy creations later
// (account-proxy builds them into the daily SpotPass package).
//
// Routes live under /my/ so the existing my_session cookie (Path /inkay/my/
// on the public site) reaches them; all URLs in the app are relative because
// the public site sits behind the /inkay/ prefix:
//
//	/my/badge-arcade/                  static editor app (badgeeditor/)
//	/my/badge-arcade/api/me            login state
//	/my/badge-arcade/api/preview       render a badge exactly as the 3DS will
//	/my/badge-arcade/api/creations     list (GET) / create (POST) own creations
//	/my/badge-arcade/api/creations/{id} load (GET) / update (PUT) / delete (DELETE)
//	/my/badge-arcade/api/creations/{id}/art.png
//	/my/badge-arcade/api/creations/{id}/submit    send a draft/rejected creation for review
//	/my/badge-arcade/api/creations/{id}/withdraw  take a submission back

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	_ "image/gif"
	_ "image/jpeg"

	"github.com/Happynico7504/badgearcade"
)

//go:embed badgeeditor
var badgeEditorFiles embed.FS

const badgeEditorSchema = `
CREATE TABLE IF NOT EXISTS badge_arcade_creations (
	id          BIGSERIAL   PRIMARY KEY,
	pid         BIGINT      NOT NULL,
	kind        TEXT        NOT NULL CHECK (kind IN ('badge', 'crane')),
	title       TEXT        NOT NULL DEFAULT '',
	spec        JSONB       NOT NULL DEFAULT '{}',
	art         BYTEA,
	source      BYTEA,
	status      TEXT        NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'submitted', 'approved', 'rejected')),
	review_note TEXT        NOT NULL DEFAULT '',
	created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS badge_arcade_creations_pid ON badge_arcade_creations (pid);
ALTER TABLE badge_arcade_creations ADD COLUMN IF NOT EXISTS submitted_at TIMESTAMPTZ;
ALTER TABLE badge_arcade_creations ADD COLUMN IF NOT EXISTS reviewed_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS badge_arcade_creations_status ON badge_arcade_creations (status, submitted_at);
`

const badgeEditorBase = "/my/badge-arcade/"

const (
	badgeEditorMaxCreations = 100
	badgeEditorMaxArt       = 512 << 10 // composed 128x128 art, PNG
	badgeEditorMaxSource    = 3 << 20   // original upload, kept for re-editing
	badgeEditorMaxBody      = 6 << 20
	badgeEditorMaxTitle     = 60
	badgeEditorMaxPending   = 5 // submissions waiting for review, per account
)

func registerBadgeEditor() {
	if _, err := db.Exec(badgeEditorSchema); err != nil {
		log.Printf("badge editor schema: %v", err)
	}
	static, _ := fs.Sub(badgeEditorFiles, "badgeeditor")
	http.Handle(badgeEditorBase, http.StripPrefix(badgeEditorBase, http.FileServer(http.FS(static))))
	// Without this, ServeMux answers ".../badge-arcade" (no slash) with an
	// absolute redirect to /my/badge-arcade/, which drops the public site's
	// /inkay/ prefix. A relative Location keeps whatever prefix the browser used.
	http.HandleFunc(strings.TrimSuffix(badgeEditorBase, "/"), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "badge-arcade/")
		w.WriteHeader(http.StatusMovedPermanently)
	})
	http.HandleFunc(badgeEditorBase+"api/me", badgeEditorMe)
	http.HandleFunc(badgeEditorBase+"api/preview", badgeEditorPreview)
	http.HandleFunc(badgeEditorBase+"api/creations", badgeEditorCreations)
	http.HandleFunc(badgeEditorBase+"api/creations/", badgeEditorCreation)
	registerBadgeEditorAdmin()
	registerBadgeArcadeDeploy()
}

func badgeEditorJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func badgeEditorError(w http.ResponseWriter, status int, msg string) {
	badgeEditorJSON(w, status, map[string]string{"error": msg})
}

// badgeEditorReadJSON decodes a JSON request body. Requiring the JSON content
// type also keeps other sites from submitting cross-site forms with the
// player's session cookie (that would need a CORS preflight, which we don't
// answer).
func badgeEditorReadJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		badgeEditorError(w, http.StatusUnsupportedMediaType, "expected JSON")
		return false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, badgeEditorMaxBody)).Decode(v); err != nil {
		badgeEditorError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return false
	}
	return true
}

// badgeEditorSpec is what the editor stores for a badge (the art is separate).
type badgeEditorSpec struct {
	// Names per 3DS language slot: 0 Japanese, 1 English, 2 French, 3 German,
	// 4 Italian, 5 Spanish, 6 Chinese, 7 Korean, 8 Dutch, 9 Portuguese,
	// 10 Russian, 11 Traditional Chinese. Empty slots fall back to English.
	Names [badgearcade.DisplayNameLanguages]string `json:"names"`
	// Collision polygons in 128x128 art pixels; nil = automatic.
	Collision []badgearcade.Polygon `json:"collision"`
	// Editor view state (image placement), opaque to the server.
	View json.RawMessage `json:"view,omitempty"`
}

// normalizeBadgeName trims a name, keeps at most two lines (the game breaks
// badge names with CRLF) and fits it into a 0x100-byte UTF-16 slot.
func normalizeBadgeName(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > 2 {
		lines = lines[:2]
	}
	s = strings.Join(lines, "\r\n")
	for len(utf16.Encode([]rune(s))) > 0x7f {
		r := []rune(s)
		s = string(r[:len(r)-1])
	}
	return s
}

func (s *badgeEditorSpec) normalize() error {
	for i := range s.Names {
		s.Names[i] = normalizeBadgeName(s.Names[i])
	}
	if s.Names[1] == "" {
		return errors.New("the badge needs an English name")
	}
	for i := range s.Names {
		if s.Names[i] == "" {
			s.Names[i] = s.Names[1]
		}
	}
	if len(s.Collision) > 8 {
		return errors.New("at most 8 collision shapes")
	}
	for _, p := range s.Collision {
		if len(p) < 3 || len(p) > 8 {
			return errors.New("collision shapes need 3 to 8 points")
		}
		for _, pt := range p {
			if pt[0] < 0 || pt[0] > 128 || pt[1] < 0 || pt[1] > 128 {
				return errors.New("collision points must be inside the badge (0-128)")
			}
		}
	}
	if len(s.View) > 4096 {
		return errors.New("view state too large")
	}
	return nil
}

// decodeDataURL decodes a "data:image/...;base64," URL (or plain base64).
func decodeDataURL(s string, limit int) ([]byte, error) {
	if i := strings.Index(s, ","); strings.HasPrefix(s, "data:") && i >= 0 {
		s = s[i+1:]
	}
	if base64.StdEncoding.DecodedLen(len(s)) > limit {
		return nil, fmt.Errorf("image too large (max %d KB)", limit>>10)
	}
	return base64.StdEncoding.DecodeString(s)
}

func pngBytes(img image.Image) []byte {
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

func dataURL(b []byte) string { return "data:image/png;base64," + base64.StdEncoding.EncodeToString(b) }

// badgeEditorArt decodes uploaded art and normalises it to a 128x128 PNG.
func badgeEditorArt(s string) (*image.NRGBA, []byte, error) {
	raw, err := decodeDataURL(s, badgeEditorMaxArt)
	if err != nil {
		return nil, nil, err
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, nil, errors.New("art is not a PNG, JPEG or GIF image")
	}
	if cfg.Width > 1024 || cfg.Height > 1024 {
		return nil, nil, errors.New("art must be at most 1024x1024")
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, nil, err
	}
	art := badgearcade.FitImage(img, 128, 128, color.Transparent)
	return art, pngBytes(art), nil
}

// badgeEditorMe reports the login state for the editor.
func badgeEditorMe(w http.ResponseWriter, r *http.Request) {
	pid, ok := mySessionPID(r)
	if !ok {
		badgeEditorJSON(w, http.StatusOK, map[string]any{"loggedIn": false})
		return
	}
	pnid, _ := pnidForPID(pid)
	badgeEditorJSON(w, http.StatusOK, map[string]any{"loggedIn": true, "pnid": pnid, "banned": badgeEditorBanned(pid)})
}

func badgeEditorBanned(pid int64) bool {
	var banned bool
	db.QueryRow(`SELECT EXISTS(SELECT 1 FROM banned_users WHERE pid = $1)`, pid).Scan(&banned)
	return banned
}

// badgeEditorClientIP is the visitor's address: the first X-Forwarded-For hop
// (relay-admin's public pages are reached through the Apache2 proxy), else the
// direct peer.
func badgeEditorClientIP(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		ip, _, _ := strings.Cut(f, ",")
		return strings.TrimSpace(ip)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// badgeEditorRateLimited allows n requests per window per key.
func badgeEditorRateLimited(key string, n int, window time.Duration) bool {
	ctx := context.Background()
	k := "badgeeditor:rl:" + key + ":" + strconv.FormatInt(time.Now().Unix()/int64(window.Seconds()), 10)
	v, _ := runtimeCacheGet(ctx, k)
	count, _ := strconv.Atoi(v)
	if count >= n {
		return true
	}
	runtimeCacheSet(ctx, k, strconv.Itoa(count+1), window)
	return false
}

// badgeEditorPreview renders a badge with the same code the server uses to
// build it: the 64x64 HOME Menu image, the in-machine texture (after ETC1
// compression) and shadow, and the collision shapes.
func badgeEditorPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		badgeEditorError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if badgeEditorRateLimited("preview:"+badgeEditorClientIP(r), 60, time.Minute) {
		badgeEditorError(w, http.StatusTooManyRequests, "too many previews, slow down a little")
		return
	}
	var req struct {
		Art       string                `json:"art"`
		Collision []badgearcade.Polygon `json:"collision"`
	}
	if !badgeEditorReadJSON(w, r, &req) {
		return
	}
	art, _, err := badgeEditorArt(req.Art)
	if err != nil {
		badgeEditorError(w, http.StatusBadRequest, err.Error())
		return
	}
	spec := badgeEditorSpec{Collision: req.Collision}
	spec.Names[1] = "Preview"
	if err := spec.normalize(); err != nil {
		badgeEditorError(w, http.StatusBadRequest, err.Error())
		return
	}
	auto := badgearcade.AutoCollision(art, 8)
	p := badgearcade.BuildPrize(badgearcade.PrizeSpec{BadgeID: 1, FileName: "Pr_Preview", Category: "Preview", Image: art, Collision: spec.Collision})
	collision, _ := p.CollisionPolygons()
	badgeEditorJSON(w, http.StatusOK, map[string]any{
		"image64":       dataURL(pngBytes(p.Image64())),
		"image32":       dataURL(pngBytes(badgearcade.DecodeRGB565A4(p.Image[badgearcade.RGB565A4Size(64, 64):], 32, 32))),
		"texture":       dataURL(pngBytes(badgearcade.DecodeETC1A4(p.Texture[:0x4000], 128, 128))),
		"shadow":        dataURL(pngBytes(badgearcade.DecodeETC1A4(p.Texture[0x4000:], 128, 128))),
		"collision":     collision,
		"autoCollision": auto,
	})
}

type badgeEditorSummary struct {
	ID         int64     `json:"id"`
	Kind       string    `json:"kind"`
	Title      string    `json:"title"`
	Status     string    `json:"status"`
	ReviewNote string    `json:"reviewNote"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

func badgeEditorAuth(w http.ResponseWriter, r *http.Request) (int64, bool) {
	pid, ok := mySessionPID(r)
	if !ok {
		badgeEditorError(w, http.StatusUnauthorized, "please log in to save creations")
		return 0, false
	}
	return pid, true
}

// badgeEditorSaveRequest is a create/update body.
type badgeEditorSaveRequest struct {
	Kind   string          `json:"kind"`
	Title  string          `json:"title"`
	Spec   badgeEditorSpec `json:"spec"`
	Art    string          `json:"art"`
	Source string          `json:"source"` // optional original upload
}

func (req *badgeEditorSaveRequest) validate() (art, source []byte, specJSON []byte, err error) {
	if req.Kind != "badge" {
		return nil, nil, nil, errors.New("only badges can be saved for now")
	}
	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" {
		req.Title = strings.SplitN(normalizeBadgeName(req.Spec.Names[1]), "\r\n", 2)[0]
	}
	if r := []rune(req.Title); len(r) > badgeEditorMaxTitle {
		req.Title = string(r[:badgeEditorMaxTitle])
	}
	if err := req.Spec.normalize(); err != nil {
		return nil, nil, nil, err
	}
	if _, art, err = badgeEditorArt(req.Art); err != nil {
		return nil, nil, nil, err
	}
	if req.Source != "" {
		if source, err = decodeDataURL(req.Source, badgeEditorMaxSource); err != nil {
			return nil, nil, nil, err
		}
		if _, _, err := image.DecodeConfig(bytes.NewReader(source)); err != nil {
			return nil, nil, nil, errors.New("source is not a PNG, JPEG or GIF image")
		}
	}
	specJSON, err = json.Marshal(req.Spec)
	return art, source, specJSON, err
}

// badgeEditorCreations lists (GET) or creates (POST) the player's creations.
func badgeEditorCreations(w http.ResponseWriter, r *http.Request) {
	pid, ok := badgeEditorAuth(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		rows, err := db.Query(`SELECT id, kind, title, status, review_note, updated_at FROM badge_arcade_creations
			WHERE pid = $1 ORDER BY updated_at DESC`, pid)
		if err != nil {
			badgeEditorError(w, http.StatusInternalServerError, "database error")
			return
		}
		defer rows.Close()
		list := []badgeEditorSummary{}
		for rows.Next() {
			var s badgeEditorSummary
			if rows.Scan(&s.ID, &s.Kind, &s.Title, &s.Status, &s.ReviewNote, &s.UpdatedAt) == nil {
				list = append(list, s)
			}
		}
		badgeEditorJSON(w, http.StatusOK, list)
	case http.MethodPost:
		if badgeEditorBanned(pid) {
			badgeEditorError(w, http.StatusForbidden, "this account can't save creations")
			return
		}
		if badgeEditorRateLimited("save:"+strconv.FormatInt(pid, 10), 30, time.Minute) {
			badgeEditorError(w, http.StatusTooManyRequests, "too many saves, slow down a little")
			return
		}
		var req badgeEditorSaveRequest
		if !badgeEditorReadJSON(w, r, &req) {
			return
		}
		art, source, spec, err := req.validate()
		if err != nil {
			badgeEditorError(w, http.StatusBadRequest, err.Error())
			return
		}
		var count int
		db.QueryRow(`SELECT COUNT(*) FROM badge_arcade_creations WHERE pid = $1`, pid).Scan(&count)
		if count >= badgeEditorMaxCreations {
			badgeEditorError(w, http.StatusForbidden, fmt.Sprintf("you can keep at most %d creations - delete some first", badgeEditorMaxCreations))
			return
		}
		var id int64
		err = db.QueryRow(`INSERT INTO badge_arcade_creations (pid, kind, title, spec, art, source)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`, pid, req.Kind, req.Title, spec, art, nullBytes(source)).Scan(&id)
		if err != nil {
			log.Printf("badge editor: insert: %v", err)
			badgeEditorError(w, http.StatusInternalServerError, "database error")
			return
		}
		badgeEditorJSON(w, http.StatusCreated, map[string]any{"id": id})
	default:
		badgeEditorError(w, http.StatusMethodNotAllowed, "GET or POST")
	}
}

func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// badgeEditorCreation loads, updates or deletes one of the player's creations.
func badgeEditorCreation(w http.ResponseWriter, r *http.Request) {
	pid, ok := badgeEditorAuth(w, r)
	if !ok {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, badgeEditorBase+"api/creations/")
	idStr, sub, _ := strings.Cut(rest, "/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		badgeEditorError(w, http.StatusNotFound, "not found")
		return
	}
	var (
		kind, title, status, note string
		spec                      []byte
		art, source               []byte
		updated                   time.Time
	)
	err = db.QueryRow(`SELECT kind, title, status, review_note, spec, art, source, updated_at FROM badge_arcade_creations
		WHERE id = $1 AND pid = $2`, id, pid).Scan(&kind, &title, &status, &note, &spec, &art, &source, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		badgeEditorError(w, http.StatusNotFound, "not found")
		return
	} else if err != nil {
		badgeEditorError(w, http.StatusInternalServerError, "database error")
		return
	}

	if sub == "art.png" && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "private, max-age=60")
		w.Write(art)
		return
	}
	if sub == "submit" || sub == "withdraw" {
		if r.Method != http.MethodPost {
			badgeEditorError(w, http.StatusMethodNotAllowed, "POST only")
			return
		}
		var empty struct{}
		if !badgeEditorReadJSON(w, r, &empty) { // JSON only, see badgeEditorReadJSON
			return
		}
		badgeEditorSubmit(w, pid, id, status, sub == "submit")
		return
	}
	if sub != "" {
		badgeEditorError(w, http.StatusNotFound, "not found")
		return
	}

	switch r.Method {
	case http.MethodGet:
		out := map[string]any{
			"id": id, "kind": kind, "title": title, "status": status, "reviewNote": note,
			"spec": json.RawMessage(spec), "art": dataURL(art), "updatedAt": updated,
		}
		if len(source) > 0 {
			out["source"] = "data:" + http.DetectContentType(source) + ";base64," + base64.StdEncoding.EncodeToString(source)
		}
		badgeEditorJSON(w, http.StatusOK, out)
	case http.MethodPut:
		if status == "submitted" || status == "approved" {
			badgeEditorError(w, http.StatusConflict, "this creation is "+status+" and can't be changed - duplicate it instead")
			return
		}
		if badgeEditorBanned(pid) {
			badgeEditorError(w, http.StatusForbidden, "this account can't save creations")
			return
		}
		if badgeEditorRateLimited("save:"+strconv.FormatInt(pid, 10), 30, time.Minute) {
			badgeEditorError(w, http.StatusTooManyRequests, "too many saves, slow down a little")
			return
		}
		var req badgeEditorSaveRequest
		if !badgeEditorReadJSON(w, r, &req) {
			return
		}
		req.Kind = kind
		art, src, spec, err := req.validate()
		if err != nil {
			badgeEditorError(w, http.StatusBadRequest, err.Error())
			return
		}
		if len(src) == 0 {
			src = source // keep the stored original when none is sent
		}
		// Editing a rejected creation turns it back into a draft.
		_, err = db.Exec(`UPDATE badge_arcade_creations SET title = $1, spec = $2, art = $3, source = $4,
			status = 'draft', updated_at = NOW() WHERE id = $5 AND pid = $6`, req.Title, spec, art, nullBytes(src), id, pid)
		if err != nil {
			badgeEditorError(w, http.StatusInternalServerError, "database error")
			return
		}
		badgeEditorJSON(w, http.StatusOK, map[string]any{"id": id})
	case http.MethodDelete:
		if status == "approved" {
			badgeEditorError(w, http.StatusConflict, "approved creations can't be deleted")
			return
		}
		db.Exec(`DELETE FROM badge_arcade_creations WHERE id = $1 AND pid = $2`, id, pid)
		badgeEditorJSON(w, http.StatusOK, map[string]any{"deleted": id})
	default:
		badgeEditorError(w, http.StatusMethodNotAllowed, "GET, PUT or DELETE")
	}
}

// badgeEditorSubmit moves a creation into the review queue, or back out.
func badgeEditorSubmit(w http.ResponseWriter, pid, id int64, status string, submit bool) {
	if !submit {
		if status != "submitted" {
			badgeEditorError(w, http.StatusConflict, "only submitted creations can be withdrawn")
			return
		}
		db.Exec(`UPDATE badge_arcade_creations SET status = 'draft', submitted_at = NULL, updated_at = NOW()
			WHERE id = $1 AND pid = $2 AND status = 'submitted'`, id, pid)
		badgeEditorJSON(w, http.StatusOK, map[string]any{"id": id, "status": "draft"})
		return
	}
	if status != "draft" && status != "rejected" {
		badgeEditorError(w, http.StatusConflict, "this creation is already "+status)
		return
	}
	if badgeEditorBanned(pid) {
		badgeEditorError(w, http.StatusForbidden, "this account can't submit creations")
		return
	}
	var pending int
	db.QueryRow(`SELECT COUNT(*) FROM badge_arcade_creations WHERE pid = $1 AND status = 'submitted'`, pid).Scan(&pending)
	if pending >= badgeEditorMaxPending {
		badgeEditorError(w, http.StatusForbidden, fmt.Sprintf("you already have %d creations waiting for review", pending))
		return
	}
	db.Exec(`UPDATE badge_arcade_creations SET status = 'submitted', submitted_at = NOW(), review_note = '', updated_at = NOW()
		WHERE id = $1 AND pid = $2`, id, pid)
	badgeEditorJSON(w, http.StatusOK, map[string]any{"id": id, "status": "submitted"})
}
