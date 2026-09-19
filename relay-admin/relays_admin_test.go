package main

import (
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Runs the real handlers against a real Postgres, in a scratch schema the caller
// created (RELAYS_TEST_URI = the connection URI with an existing query string,
// RELAYS_TEST_SCHEMA = the empty schema to use). Skipped otherwise.
func TestRelayAdminHandlersAgainstPostgres(t *testing.T) {
	uri, schema := os.Getenv("RELAYS_TEST_URI"), os.Getenv("RELAYS_TEST_SCHEMA")
	if uri == "" || schema == "" {
		t.Skip("set RELAYS_TEST_URI and RELAYS_TEST_SCHEMA to run")
	}
	d, err := sql.Open("postgres", uri+"&search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	old := db
	db = d
	defer func() { db = old }()
	if _, err := db.Exec(relaysSchema); err != nil {
		t.Fatal(err)
	}
	db.Exec(`DELETE FROM relays`)

	pubFile := filepath.Join(t.TempDir(), "main-public.pem")
	os.WriteFile(pubFile, []byte("-----BEGIN PUBLIC KEY-----\nTESTKEY\n-----END PUBLIC KEY-----\n"), 0o644)
	wantURL := "http://main.test:7777"
	if real := os.Getenv("RELAYS_TEST_PUBKEY_FILE"); real != "" { // end-to-end smoke against a real hub
		pubFile, wantURL = real, os.Getenv("RELAYS_TEST_MAIN_URL")
	}
	t.Setenv("RELAYHUB_PUBLIC_KEY_FILE", pubFile)
	t.Setenv("RELAYHUB_PUBLIC_URL", wantURL)

	do := func(h http.HandlerFunc, method, target string, form url.Values) *httptest.ResponseRecorder {
		var body *strings.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		} else {
			body = strings.NewReader("")
		}
		req := httptest.NewRequest(method, target, body)
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}
	count := func() int {
		var n int
		db.QueryRow(`SELECT count(*) FROM relays`).Scan(&n)
		return n
	}
	add := func(id, region, host, port string) *httptest.ResponseRecorder {
		return do(adminRelaysAdd, "POST", "/admin/relays/add", url.Values{
			"id": {id}, "region": {region}, "host": {host}, "health_port": {port}, "name": {"Test"}})
	}

	if rec := do(adminRelays, "GET", "/admin/relays/", nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), "No relays registered yet") {
		t.Fatalf("empty page: %d", rec.Code)
	}

	// Valid add: the page carries the secret bundle exactly once.
	rec := add("us-1", "na", "relay-us.example.net", "443")
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("add: %d cache=%q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	page := rec.Body.String()
	i, j := strings.Index(page, "<textarea"), strings.Index(page, "</textarea>")
	if i < 0 || j < 0 {
		t.Fatal("no bundle in the response")
	}
	raw := page[strings.Index(page[i:], ">")+i+1 : j]
	raw = html.UnescapeString(raw)
	var bundle struct {
		RelayID, SigningKey, MainPublicKey, MainURL string
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("bundle is not JSON: %v\n%s", err, raw)
	}
	bundle.RelayID, bundle.SigningKey, bundle.MainPublicKey, bundle.MainURL = m["relay_id"], m["signing_key"], m["main_public_key"], m["main_url"]
	priv, err := base64.StdEncoding.DecodeString(bundle.SigningKey)
	if bundle.RelayID != "us-1" || err != nil || len(priv) != ed25519.PrivateKeySize ||
		!strings.Contains(bundle.MainPublicKey, "BEGIN PUBLIC KEY") || bundle.MainURL != wantURL {
		t.Fatalf("bad bundle: %+v (%v)", bundle, err)
	}
	// The stored public key must be the one belonging to the bundle's private key.
	var stored []byte
	db.QueryRow(`SELECT public_key FROM relays WHERE id='us-1'`).Scan(&stored)
	if string(stored) != string(ed25519.PrivateKey(priv).Public().(ed25519.PublicKey)) {
		t.Fatal("registered public key does not match the bundle's private key")
	}
	if out := os.Getenv("RELAYS_TEST_BUNDLE_OUT"); out != "" {
		os.WriteFile(out, []byte(raw), 0o600)
	}

	// The secret is gone from every later view.
	later := do(adminRelays, "GET", "/admin/relays/", nil).Body.String()
	if strings.Contains(later, bundle.SigningKey) || strings.Contains(later, "Secret bundle") || !strings.Contains(later, "us-1") {
		t.Fatal("secret bundle shown again (or relay missing)")
	}

	// Validation: nothing gets registered.
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"bad id":         add("US_1", "na", "h.example.net", "0"),
		"upper id":       add("Us-1", "na", "h.example.net", "0"),
		"bad region":     add("x-1", "mars", "h.example.net", "0"),
		"bad host":       add("x-1", "na", "a b;rm", "0"),
		"empty host":     add("x-1", "na", "", "0"),
		"bad port":       add("x-1", "na", "h.example.net", "70000"),
		"duplicate id":   add("us-1", "jp", "other.example.net", "0"),
		"injection host": add("x-1", "na", "h.example.net'; DROP TABLE relays;--", "0"),
	} {
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `class="err"`) || strings.Contains(rec.Body.String(), "Secret bundle") {
			t.Errorf("%s: not rejected cleanly (%d)", name, rec.Code)
		}
	}
	if n := count(); n != 1 {
		t.Fatalf("relays after failed adds: %d, want 1", n)
	}
	if rec := do(adminRelaysAdd, "GET", "/admin/relays/add", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("GET add: %d", rec.Code)
	}

	// Missing main public key: a clear error, no half-created relay.
	t.Setenv("RELAYHUB_PUBLIC_KEY_FILE", filepath.Join(t.TempDir(), "absent.pem"))
	if rec := add("eu-1", "eu", "h.example.net", "0"); !strings.Contains(rec.Body.String(), "relayhub pubkey") || count() != 1 {
		t.Fatal("missing public key not handled")
	}

	// Toggle and delete.
	do(adminRelaysToggle, "POST", "/admin/relays/toggle", url.Values{"id": {"us-1"}})
	var enabled bool
	db.QueryRow(`SELECT enabled FROM relays WHERE id='us-1'`).Scan(&enabled)
	if enabled {
		t.Fatal("toggle did not disable")
	}
	do(adminRelaysToggle, "POST", "/admin/relays/toggle", url.Values{"id": {"us-1"}})
	db.QueryRow(`SELECT enabled FROM relays WHERE id='us-1'`).Scan(&enabled)
	if !enabled {
		t.Fatal("toggle did not re-enable")
	}
	if rec := do(adminRelaysDelete, "POST", "/admin/relays/delete", url.Values{"id": {"../etc"}}); rec.Code != http.StatusSeeOther || count() != 1 {
		t.Fatal("invalid id not rejected")
	}
	// Keep the row for the caller's follow-up smoke test when a bundle was requested.
	if os.Getenv("RELAYS_TEST_BUNDLE_OUT") == "" {
		do(adminRelaysDelete, "POST", "/admin/relays/delete", url.Values{"id": {"us-1"}})
		if count() != 0 {
			t.Fatal("delete did not remove the relay")
		}
	}
}
