package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func withAssignServer(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	old := relayAssignURL
	relayAssignURL = srv.URL
	t.Cleanup(func() { relayAssignURL = old; srv.Close() })
}

func TestRelayAssignUsesTheHubsAnswer(t *testing.T) {
	var got map[string]any
	withAssignServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(map[string]any{"relay": "us-1", "host": "107.173.31.124", "port": 60014})
	})
	host, port, ok := relayAssign("wsc", 1435853600, "tok", "8.8.8.8")
	if !ok || host != "107.173.31.124" || port != 60014 {
		t.Fatalf("got %q %d %v", host, port, ok)
	}
	if got["game"] != "wsc" || uint32(got["pid"].(float64)) != 1435853600 || got["password"] != "tok" || got["client_ip"] != "8.8.8.8" {
		t.Fatalf("the hub received %v", got)
	}
}

func TestRelayAssignFailsOpenInEveryWay(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"no relay for this client (204)": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) },
		"hub error (500)":                func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", 500) },
		"garbage body":                   func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("nope")) },
		"hostname instead of an IP": func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"host": "evil.example.com", "port": 60014})
		},
		"IPv6 address": func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"host": "2001:db8::1", "port": 60014})
		},
		"port zero": func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"host": "203.0.113.5", "port": 0})
		},
		"port too large": func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"host": "203.0.113.5", "port": 70000})
		},
		"empty object": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("{}")) },
	}
	for name, h := range cases {
		withAssignServer(t, h)
		if host, port, ok := relayAssign("wsc", 1, "tok", "8.8.8.8"); ok {
			t.Errorf("%s: used %q:%d", name, host, port)
		}
	}
	// Hub not running at all: an immediate refusal, never a delay for the console.
	relayAssignURL = "http://127.0.0.1:1/assign"
	start := time.Now()
	if _, _, ok := relayAssign("wsc", 1, "tok", "8.8.8.8"); ok || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("hub down: ok=%v after %v", ok, time.Since(start))
	}
}

func TestRelayAssignNeverHoldsAConsoleForLong(t *testing.T) {
	withAssignServer(t, func(w http.ResponseWriter, r *http.Request) { time.Sleep(3 * time.Second) })
	start := time.Now()
	if _, _, ok := relayAssign("wsc", 1, "tok", "8.8.8.8"); ok || time.Since(start) > 2*time.Second {
		t.Fatalf("a slow hub held the nex_token answer for %v", time.Since(start))
	}
}
