package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	_ "github.com/lib/pq"
)

// TestBadgeArcadeAdminLive drives the admin review handlers against the real
// database for one existing creation (BADGE_ARCADE_TEST_CREATION=<id>,
// PN_WUC_POSTGRES_URI set). The handlers are called directly, without the
// client-certificate wrapper.
func TestBadgeArcadeAdminLive(t *testing.T) {
	id := os.Getenv("BADGE_ARCADE_TEST_CREATION")
	if id == "" || os.Getenv("PN_WUC_POSTGRES_URI") == "" {
		t.Skip("set BADGE_ARCADE_TEST_CREATION and PN_WUC_POSTGRES_URI")
	}
	var err error
	if db, err = sql.Open("postgres", os.Getenv("PN_WUC_POSTGRES_URI")); err != nil {
		t.Fatal(err)
	}
	get := func(h http.HandlerFunc, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	post := func(form url.Values) string {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/admin/badge-arcade/review", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		adminBadgeArcadeReview(rec, req)
		loc, _ := url.QueryUnescape(rec.Header().Get("Location"))
		return loc
	}
	status := func() (s, note string) {
		db.QueryRow(`SELECT status, review_note FROM badge_arcade_creations WHERE id = $1`, id).Scan(&s, &note)
		return
	}

	page := get(adminBadgeArcade, "/admin/badge-arcade/?status=submitted")
	if page.Code != 200 || !strings.Contains(page.Body.String(), "#"+id+" ") {
		t.Fatalf("queue page %d does not list #%s", page.Code, id)
	}
	for _, img := range []string{"art.png", "home.png", "machine.png"} {
		rec := get(adminBadgeArcadeImage, "/admin/badge-arcade/img/"+id+"/"+img)
		if rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), "\x89PNG") {
			t.Fatalf("%s: %d", img, rec.Code)
		}
	}
	t.Logf("reject without note -> %s", post(url.Values{"id": {id}, "action": {"reject"}, "from": {"submitted"}}))
	if s, _ := status(); s != "submitted" {
		t.Fatalf("rejected without a note: %s", s)
	}
	t.Logf("reject -> %s", post(url.Values{"id": {id}, "action": {"reject"}, "note": {"Please use a transparent background."}, "from": {"submitted"}}))
	if s, n := status(); s != "rejected" || n == "" {
		t.Fatalf("after reject: %s %q", s, n)
	}
	t.Logf("reopen -> %s", post(url.Values{"id": {id}, "action": {"reopen"}, "from": {"rejected"}}))
	t.Logf("approve -> %s", post(url.Values{"id": {id}, "action": {"approve"}, "from": {"submitted"}}))
	if s, _ := status(); s != "approved" {
		t.Fatalf("after approve: %s", s)
	}
	if !strings.Contains(get(adminBadgeArcade, "/admin/badge-arcade/?status=approved").Body.String(), "badge ID 9000") {
		t.Fatal("approved tab does not show the badge ID")
	}
}
