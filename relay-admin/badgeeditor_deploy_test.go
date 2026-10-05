package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestBadgeArcadeDeployLive drives the deployment handlers against the real
// database with a temporary approved badge for PID 1 (removed afterwards).
func TestBadgeArcadeDeployLive(t *testing.T) {
	if os.Getenv("PN_WUC_POSTGRES_URI") == "" || os.Getenv("BADGE_ARCADE_DEPLOY_LIVE") == "" {
		t.Skip("set PN_WUC_POSTGRES_URI and BADGE_ARCADE_DEPLOY_LIVE=1")
	}
	var err error
	if db, err = sql.Open("postgres", os.Getenv("PN_WUC_POSTGRES_URI")); err != nil {
		t.Fatal(err)
	}
	db.Exec(badgeEditorSchema)
	db.Exec(badgeArcadeDeploySchema)
	var cid int64
	if err := db.QueryRow(`INSERT INTO badge_arcade_creations (pid, kind, title, spec, art, status)
		VALUES (1, 'badge', 'deploy test', '{}', '\x89504e47', 'approved') RETURNING id`).Scan(&cid); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(`DELETE FROM badge_arcade_creations WHERE id = $1`, cid)

	tmpls, err := badgeArcadeDeployTemplates()
	if err != nil || len(tmpls) == 0 {
		t.Fatalf("templates: %d, %v", len(tmpls), err)
	}
	all := 0
	for _, tp := range tmpls {
		if tp.AllRegions() {
			all++
		}
	}
	t.Logf("%d templates, %d in all regions (first: %s %v, %d spots)", len(tmpls), all, tmpls[0].Name, tmpls[0].Regions, tmpls[0].PrizeSlots)

	post := func(h http.HandlerFunc, path string, form url.Values) string {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		h(rec, req)
		loc, _ := url.QueryUnescape(rec.Header().Get("Location"))
		return loc
	}
	page := func() string {
		rec := httptest.NewRecorder()
		adminBadgeArcadeDeployments(rec, httptest.NewRequest(http.MethodGet, "/admin/badge-arcade/deployments/", nil))
		if rec.Code != 200 {
			t.Fatalf("page: %d %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	if !strings.Contains(page(), "deploy test") {
		t.Fatal("approved badge not offered")
	}
	base := url.Values{"name": {"Test machine"}, "slot": {"hall"}, "status": {"test"}, "start": {"2026-10-06"}, "end": {"2026-10-12"},
		"template": {tmpls[0].Name}, "badge": {itoa64(cid)}}
	for name, bad := range map[string]url.Values{
		"no badges":    with(base, "badge", ""),
		"bad template": with(base, "template", "NoSuchMachine"),
		"bad dates":    with(base, "end", "2026-10-01"),
	} {
		if loc := post(adminBadgeArcadeDeploySave, "/admin/badge-arcade/deployments/save", bad); !strings.Contains(loc, "Not saved") {
			t.Fatalf("%s accepted: %s", name, loc)
		}
	}
	t.Log(post(adminBadgeArcadeDeploySave, "/admin/badge-arcade/deployments/save", base))
	var did int64
	var status string
	db.QueryRow(`SELECT id, status FROM badge_arcade_deployments WHERE name = 'Test machine' ORDER BY id DESC LIMIT 1`).Scan(&did, &status)
	if did == 0 || status != "test" {
		t.Fatal("deployment not created")
	}
	defer db.Exec(`DELETE FROM badge_arcade_deployments WHERE id = $1`, did)
	if !strings.Contains(page(), "Test machine") {
		t.Fatal("deployment not listed")
	}
	post(adminBadgeArcadeDeployStatus, "/admin/badge-arcade/deployments/status", url.Values{"id": {itoa64(did)}, "status": {"off"}})
	db.QueryRow(`SELECT status FROM badge_arcade_deployments WHERE id = $1`, did).Scan(&status)
	if status != "off" {
		t.Fatalf("status %s", status)
	}
	post(adminBadgeArcadeDeployDelete, "/admin/badge-arcade/deployments/delete", url.Values{"id": {itoa64(did)}})
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM badge_arcade_deployment_badges WHERE deployment_id = $1`, did).Scan(&n)
	if n != 0 {
		t.Fatal("badge links not deleted with the deployment")
	}
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

func with(v url.Values, k, val string) url.Values {
	out := url.Values{}
	for kk, vv := range v {
		out[kk] = append([]string{}, vv...)
	}
	if val == "" {
		delete(out, k)
	} else {
		out.Set(k, val)
	}
	return out
}
