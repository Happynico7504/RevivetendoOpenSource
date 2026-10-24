package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestThemeHTMLRewritesColors(t *testing.T) {
	page := `<!DOCTYPE html><html><head><style>
body{color:#222;max-width:400px}
a:hover{color:#2563eb}
.note{background:#fff7ed;border:1px solid #fed7aa;border-radius:6px}
.x{background:url("data:image/svg+xml,%23fff") #fff;box-shadow:0 0 0 2px rgba(37,99,235,.2)}
</style></head><body><header>h</header><span style="color:#aaa">x</span></body></html>`
	out := themeHTML(page)
	for _, want := range []string{
		`color:var(--tf-222222,#222)`,
		`a:hover{color:var(--tf-2563eb,#2563eb)}`,
		`background:var(--tb-fff7ed,#fff7ed)`,
		`border:1px solid var(--tl-fed7aa,#fed7aa)`,
		`border-radius:6px`,
		`url("data:image/svg+xml,%23fff") var(--tb-ffffff,#fff)`,
		`box-shadow:0 0 0 2px rgba(37,99,235,.2)`,
		`style="color:var(--tf-aaaaaa,#aaa)"`,
		`:root[data-theme=dark]{`,
		`--tb-ffffff:#121212;`,
		`<head><style>`,
		`<header>h</header>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Count(out, "rvt-theme") == 0 {
		t.Error("theme switch not injected")
	}
}

func TestThemeHTMLOptOutAndFragment(t *testing.T) {
	page := `<html><head>` + themeOptOut + `<style>body{color:#222}</style></head></html>`
	if themeHTML(page) != page {
		t.Error("opted-out page was changed")
	}
	frag := `<td style="color:#aaa">x</td>`
	out := themeHTML(frag)
	if strings.Contains(out, "<script") || !strings.Contains(out, "--tf-aaaaaa:") {
		t.Errorf("fragment: %s", out)
	}
}

func TestThemeDark(t *testing.T) {
	for _, c := range []struct {
		role byte
		in   string
		want string
	}{
		{'f', "#fff", "#ffffff"},    // white text on a coloured button stays
		{'b', "#0f172a", "#0d1424"}, // dark code block stays dark
		{'f', "#000", "#ebebeb"},
		{'b', "rgba(255,255,255,.5)", "rgba(18,18,18,0.5)"},
	} {
		col, ok := parseThemeColor(c.in)
		if !ok {
			t.Fatalf("parse %s", c.in)
		}
		if got := themeDark(c.role, col).css(); got != c.want {
			t.Errorf("themeDark(%c, %s) = %s, want %s", c.role, c.in, got, c.want)
		}
	}
}

func TestThemeMiddleware(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Length", "999")
		w.Write([]byte(`<html><head></head><body style="color:#222"></body></html>`))
	})
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"color":"#222"}`))
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	h := themeMiddleware(mux)

	get := func(path, ua string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("User-Agent", ua)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := get("/page", "Mozilla/5.0"); !strings.Contains(rec.Body.String(), "var(--tf-222222,#222)") || rec.Header().Get("Content-Length") != "" {
		t.Errorf("page not themed: %q %v", rec.Body.String(), rec.Header())
	}
	if rec := get("/page", "Mozilla/5.0 (Nintendo WiiU) AppleWebKit/536.30"); strings.Contains(rec.Body.String(), "var(") {
		t.Error("console page was themed")
	}
	if rec := get("/json", "Mozilla/5.0"); rec.Body.String() != `{"color":"#222"}` {
		t.Errorf("json changed: %s", rec.Body.String())
	}
	if rec := get("/missing", "Mozilla/5.0"); rec.Code != 404 {
		t.Errorf("status %d", rec.Code)
	}
}
