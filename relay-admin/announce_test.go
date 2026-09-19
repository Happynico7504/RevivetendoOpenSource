package main

import (
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"
)

// The tag names below must match relayhub's TagRedirects/TagBans; the hub
// rejects nothing, but a typo would make relays silently keep stale entries.
func TestAnnounceInvalidationPayload(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:9401")
	if err != nil {
		t.Skipf("hub port busy (a real relayhub is running): %v", err)
	}
	got := make(chan []string, 4)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Tags []string `json:"tags"`
		}
		json.NewDecoder(r.Body).Decode(&b)
		if r.Method != http.MethodPost || r.URL.Path != "/invalidate" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		got <- b.Tags
		w.Write([]byte(`{"seq":1}`))
	})}
	go srv.Serve(ln)
	defer srv.Close()

	invalidateRedirectsCache()
	announceInvalidation("bans")
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case tags := <-got:
			for _, tg := range tags {
				seen[tg] = true
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("announcement %d never arrived (saw %v)", i+1, seen)
		}
	}
	if !seen["config:redirects"] || !seen["bans"] {
		t.Fatalf("wrong tags: %v", seen)
	}
}

func TestAnnounceWithoutHubIsHarmless(t *testing.T) {
	if ln, err := net.Listen("tcp", "127.0.0.1:9401"); err == nil {
		ln.Close() // nothing is listening: the hub is down
	} else {
		t.Skip("a real relayhub is running")
	}
	done := make(chan struct{})
	go func() { announceInvalidation("bans"); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("announceInvalidation blocked the caller")
	}
	time.Sleep(1200 * time.Millisecond) // let the background attempt fail quietly
}
