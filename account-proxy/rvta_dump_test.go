package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func rvtaDumpRequest(offset, total, body string, mod func(r *http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "https://rvta.nicoch.net/v1/dump", strings.NewReader(body))
	r.Header.Set("X-RvtA-Id", "0123456789abcdef0123456789abcdef")
	r.Header.Set("X-RvtA-FC", "542601732896")
	r.Header.Set("X-RvtA-Dump-Kind", "memory")
	r.Header.Set("X-RvtA-Dump-Name", "mem-0004000000134600-1791700000.bin")
	r.Header.Set("X-RvtA-Dump-Offset", offset)
	r.Header.Set("X-RvtA-Dump-Total", total)
	if mod != nil {
		mod(r)
	}
	w := httptest.NewRecorder()
	handleRvta(w, r)
	return w
}

func TestRvtaDumpResumableUpload(t *testing.T) {
	old, oldFree := rvtaDumpDir, rvtaDumpMinFreeDisk
	rvtaDumpDir, rvtaDumpMinFreeDisk = t.TempDir(), 0
	defer func() { rvtaDumpDir, rvtaDumpMinFreeDisk = old, oldFree }()

	if w := rvtaDumpRequest("0", "10", "hello", nil); w.Code != 200 || w.Body.String() != "ok 5\n" {
		t.Fatalf("first chunk: %d %q", w.Code, w.Body.String())
	}
	// a resent first chunk (reply lost) does not duplicate data: the server says how much it has
	if w := rvtaDumpRequest("0", "10", "hello", nil); w.Code != http.StatusConflict || w.Body.String() != "size 5\n" {
		t.Fatalf("resent chunk: %d %q", w.Code, w.Body.String())
	}
	if w := rvtaDumpRequest("5", "10", "world", nil); w.Code != 200 || w.Body.String() != "ok 10\n" {
		t.Fatalf("second chunk: %d %q", w.Code, w.Body.String())
	}
	got, _ := os.ReadFile(filepath.Join(rvtaDumpDir, "0123456789abcdef0123456789abcdef", "memory-mem-0004000000134600-1791700000.bin"))
	if string(got) != "helloworld" {
		t.Fatalf("stored %q", got)
	}
	if w := rvtaDumpRequest("10", "10", "x", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("chunk past the end accepted: %d", w.Code)
	}
}

func TestRvtaDumpRejectsBadInput(t *testing.T) {
	old := rvtaDumpDir
	rvtaDumpDir = t.TempDir()
	defer func() { rvtaDumpDir = old }()
	cases := map[string]func(r *http.Request){
		"path traversal": func(r *http.Request) { r.Header.Set("X-RvtA-Dump-Name", "../../etc/passwd") },
		"hidden name":    func(r *http.Request) { r.Header.Set("X-RvtA-Dump-Name", ".bashrc") },
		"unknown kind":   func(r *http.Request) { r.Header.Set("X-RvtA-Dump-Kind", "save") },
		"bad id":         func(r *http.Request) { r.Header.Set("X-RvtA-Id", "../x") },
		"bad fc":         func(r *http.Request) { r.Header.Set("X-RvtA-FC", "123") },
		"too large":      func(r *http.Request) { r.Header.Set("X-RvtA-Dump-Total", "999999999999") },
	}
	for name, mod := range cases {
		if w := rvtaDumpRequest("0", "10", "hello", mod); w.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d", name, w.Code)
		}
	}
	entries, _ := os.ReadDir(rvtaDumpDir)
	if len(entries) != 0 {
		t.Fatalf("rejected requests wrote files: %v", entries)
	}
}
