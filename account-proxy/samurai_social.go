package main

// Views, likes and comments for uploaded Revivetendo TV videos (the tables
// live with eshop_videos in relay-admin/videos.go, same database).
//
// Views come from both consoles: one per IP per video every 6 hours, counted
// when a console starts downloading the video file from its first byte.
// Likes and comments are Wii U only; the 3DS eShop has no room for them.
//
// Who is liking or commenting: the Wii U eShop page sends the signed-in
// account (wiiuNNA.principalId) as X-TV-PID, and we accept it only if that
// account logged in from the same IP within the last week (nex_sessions).
// The custom header also means other sites can't make these requests (a
// cross-site request with it needs a CORS preflight we never answer).

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	samuraiUploadIDBase    = 50040091000000 // relay-admin videoCatalogIDBase
	tvCommentMaxRunes      = 200
	tvCommentsPerMinute    = 3
	tvCommentsPerDay       = 30
	tvViewDedupe           = 6 * time.Hour
	tvViewerLoginFreshness = "7 days"
	tvCommentsShown        = 50
)

// tvUploadID maps an eShop movie id to its eshop_videos id.
func tvUploadID(catalogID int64) (int64, bool) {
	if catalogID > samuraiUploadIDBase && catalogID < samuraiUploadIDBase+1_000_000 {
		return catalogID - samuraiUploadIDBase, true
	}
	return 0, false
}

// tvCountView counts a view of upload id from this request's IP, at most once
// per tvViewDedupe. Only requests that start at the first byte count, so the
// players' follow-up range requests don't.
func tvCountView(r *http.Request, id string) {
	if rg := r.Header.Get("Range"); rg != "" && !strings.HasPrefix(rg, "bytes=0-") {
		return
	}
	ctx := context.Background()
	key := "tvview:" + id + ":" + realIP(r)
	if _, seen := runtimeCacheGet(ctx, key); seen {
		return
	}
	runtimeCacheSet(ctx, key, "1", tvViewDedupe)
	if _, err := db.Exec(`UPDATE eshop_videos SET views = views + 1 WHERE id = $1 AND status = 'live'`, id); err != nil {
		log.Printf("tv: count view of #%s: %v", id, err)
	}
}

// tvViewer returns the verified PID behind a request, or 0.
func tvViewer(r *http.Request) int64 {
	pid, err := strconv.ParseInt(r.Header.Get("X-TV-PID"), 10, 64)
	if err != nil || pid <= 0 {
		return 0
	}
	var ok bool
	db.QueryRow(`SELECT EXISTS(SELECT 1 FROM nex_sessions WHERE pid = $1 AND ip = $2 AND expires_at > NOW() - $3::interval)`,
		pid, realIP(r), tvViewerLoginFreshness).Scan(&ok)
	if !ok {
		log.Printf("tv: X-TV-PID %d not recently logged in from %s, ignoring", pid, realIP(r))
		return 0
	}
	return pid
}

func tvJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// tvDisplayName is the Mii name, else the PNID.
func tvDisplayName(pid int64) string {
	var name string
	if db.QueryRow(`SELECT mii_name FROM mii_names WHERE pid = $1`, pid).Scan(&name) == nil && strings.TrimSpace(name) != "" {
		return name
	}
	db.QueryRow(`SELECT pnid FROM pnid_cache WHERE pid = $1`, pid).Scan(&name)
	return name
}

type tvStats struct {
	Views, Likes, Comments int64
}

// tvStatsFor returns stats for uploaded videos, keyed by eshop_videos id.
func tvStatsFor(ids []int64) map[int64]tvStats {
	out := map[int64]tvStats{}
	if len(ids) == 0 {
		return out
	}
	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = strconv.FormatInt(id, 10)
	}
	rows, err := db.Query(`SELECT id, views,
			(SELECT COUNT(*) FROM eshop_video_likes l WHERE l.video_id = v.id),
			(SELECT COUNT(*) FROM eshop_video_comments c WHERE c.video_id = v.id)
		FROM eshop_videos v WHERE id = ANY(string_to_array($1, ',')::bigint[])`, strings.Join(strs, ","))
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var s tvStats
		if rows.Scan(&id, &s.Views, &s.Likes, &s.Comments) == nil {
			out[id] = s
		}
	}
	return out
}

// handleTVAPI serves /ninja/tv/{video,like,comment} for the Wii U page.
func handleTVAPI(w http.ResponseWriter, r *http.Request) {
	catID, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	id, ok := tvUploadID(catID)
	if !ok {
		tvJSON(w, http.StatusNotFound, map[string]string{"error": "This video has no comments."})
		return
	}
	var live bool
	db.QueryRow(`SELECT EXISTS(SELECT 1 FROM eshop_videos WHERE id = $1 AND status = 'live')`, id).Scan(&live)
	if !live {
		tvJSON(w, http.StatusNotFound, map[string]string{"error": "This video is no longer available."})
		return
	}
	pid := tvViewer(r)
	switch strings.TrimPrefix(r.URL.Path, "/ninja/tv/") {
	case "video":
		tvVideoDetails(w, id, pid)
	case "like":
		if r.Method != http.MethodPost {
			tvJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		if pid == 0 {
			tvJSON(w, http.StatusForbidden, map[string]string{"error": "We couldn't tell which account you're using. Restart the eShop and try again."})
			return
		}
		res, err := db.Exec(`INSERT INTO eshop_video_likes (video_id, pid) VALUES ($1, $2) ON CONFLICT DO NOTHING`, id, pid)
		if err == nil {
			if n, _ := res.RowsAffected(); n == 0 {
				_, err = db.Exec(`DELETE FROM eshop_video_likes WHERE video_id = $1 AND pid = $2`, id, pid)
			}
		}
		if err != nil {
			log.Printf("tv: like #%d by %d: %v", id, pid, err)
			tvJSON(w, http.StatusInternalServerError, map[string]string{"error": "Something went wrong, please try again."})
			return
		}
		tvVideoDetails(w, id, pid)
	case "comment":
		tvPostComment(w, r, id, pid)
	default:
		http.NotFound(w, r)
	}
}

func tvVideoDetails(w http.ResponseWriter, id, pid int64) {
	type comment struct {
		Name string `json:"name"`
		Body string `json:"body"`
		Date string `json:"date"`
	}
	st := tvStatsFor([]int64{id})[id]
	var liked bool
	if pid != 0 {
		db.QueryRow(`SELECT EXISTS(SELECT 1 FROM eshop_video_likes WHERE video_id = $1 AND pid = $2)`, id, pid).Scan(&liked)
	}
	comments := []comment{}
	rows, err := db.Query(`SELECT pid, body, created_at FROM eshop_video_comments WHERE video_id = $1 ORDER BY created_at DESC LIMIT $2`, id, tvCommentsShown)
	if err == nil {
		for rows.Next() {
			var cpid int64
			var c comment
			var at time.Time
			if rows.Scan(&cpid, &c.Body, &at) == nil {
				c.Name = tvDisplayName(cpid)
				c.Date = at.UTC().Format("2006-01-02")
				comments = append(comments, c)
			}
		}
		rows.Close()
	}
	tvJSON(w, http.StatusOK, map[string]any{
		"views": st.Views, "likes": st.Likes, "commentCount": st.Comments,
		"liked": liked, "signedIn": pid != 0, "comments": comments,
	})
}

func tvPostComment(w http.ResponseWriter, r *http.Request, id, pid int64) {
	if r.Method != http.MethodPost {
		tvJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	if pid == 0 {
		tvJSON(w, http.StatusForbidden, map[string]string{"error": "We couldn't tell which account you're using. Restart the eShop and try again."})
		return
	}
	var banned bool
	db.QueryRow(`SELECT EXISTS(SELECT 1 FROM banned_users WHERE pid = $1)`, pid).Scan(&banned)
	if banned {
		tvJSON(w, http.StatusForbidden, map[string]string{"error": "Your account can't post comments."})
		return
	}
	var req struct {
		Body string `json:"body"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req) != nil {
		tvJSON(w, http.StatusBadRequest, map[string]string{"error": "Something went wrong, please try again."})
		return
	}
	body := strings.TrimSpace(strings.ReplaceAll(req.Body, "\r", ""))
	if body == "" || utf8.RuneCountInString(body) > tvCommentMaxRunes {
		tvJSON(w, http.StatusBadRequest, map[string]string{"error": "Comments can be 1 to 200 characters."})
		return
	}
	ctx := context.Background()
	p := strconv.FormatInt(pid, 10)
	for _, lim := range []struct {
		window time.Duration
		max    int
	}{{time.Minute, tvCommentsPerMinute}, {24 * time.Hour, tvCommentsPerDay}} {
		k := "tvcomment:" + p + ":" + strconv.FormatInt(time.Now().Unix()/int64(lim.window.Seconds()), 10)
		v, _ := runtimeCacheGet(ctx, k)
		n, _ := strconv.Atoi(v)
		if n >= lim.max {
			tvJSON(w, http.StatusTooManyRequests, map[string]string{"error": "You're commenting too fast. Please wait a little."})
			return
		}
		runtimeCacheSet(ctx, k, strconv.Itoa(n+1), lim.window)
	}
	if _, err := db.Exec(`INSERT INTO eshop_video_comments (video_id, pid, body) VALUES ($1, $2, $3)`, id, pid, body); err != nil {
		log.Printf("tv: comment on #%d by %d: %v", id, pid, err)
		tvJSON(w, http.StatusInternalServerError, map[string]string{"error": "Something went wrong, please try again."})
		return
	}
	log.Printf("tv: comment on #%d by PID %d (%d chars)", id, pid, utf8.RuneCountInString(body))
	tvVideoDetails(w, id, pid)
}
