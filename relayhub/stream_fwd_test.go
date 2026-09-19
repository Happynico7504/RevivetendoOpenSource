package relayhub

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/Happynico7504/relaylink"
	"time"
)

const bigSize = 40<<20 + 12345 // over the 16 MiB cap the old path had

// bigByte returns n deterministic bytes starting at offset off, so any dropped,
// duplicated or reordered chunk changes the hash.
func bigByte(off, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		x := uint32(off + i)
		b[i] = byte(x*2654435761>>13) ^ byte(x>>9)
	}
	return b
}

func writeBig(w io.Writer) {
	for off := 0; off < bigSize; off += 1 << 20 {
		n := 1 << 20
		if off+n > bigSize {
			n = bigSize - off
		}
		w.Write(bigByte(off, n))
	}
}

func bigHash() [32]byte {
	h := sha256.New()
	writeBig(h)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func TestLargeResponseStreamsIntact(t *testing.T) {
	r := newContentRig(t)
	relay := r.newRelay(t, "a", true)

	req, _ := http.NewRequest("GET", relay.url+"/big", nil)
	req.Host = olvHost
	req.Header.Set("X-Nintendo-Servicetoken", "alice")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.ContentLength != bigSize {
		t.Fatalf("Content-Length %d, want %d", resp.ContentLength, bigSize)
	}
	h := sha256.New()
	n, err := io.Copy(h, resp.Body)
	if err != nil || n != bigSize {
		t.Fatalf("read %d bytes, err %v", n, err)
	}
	want := bigHash()
	if !bytes.Equal(h.Sum(nil), want[:]) {
		t.Fatal("streamed body differs from the backend's")
	}
	// The stream is released once the download finishes.
	deadline := time.Now().Add(3 * time.Second)
	for r.hub.Fwd.ActiveStreams() != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := r.hub.Fwd.ActiveStreams(); n != 0 {
		t.Fatalf("%d streams left open", n)
	}
	// A streamed answer is never cached.
	if s := relay.content.Stats(); s.Stores != 0 {
		t.Fatalf("stored a streamed body: %+v", s)
	}
}

func TestChunkBoundaryExactLength(t *testing.T) {
	r := newContentRig(t)
	relay := r.newRelay(t, "a", true)
	code, body, _ := relay.do(t, "GET", olvHost, "/bigexact", "alice")
	if code != 200 || !bytes.Equal([]byte(body), bigByte(0, 3<<20)) {
		t.Fatalf("code %d, %d bytes", code, len(body))
	}
}

func TestHeadReportsRealLength(t *testing.T) {
	r := newContentRig(t)
	relay := r.newRelay(t, "a", true)
	code, body, hdr := relay.do(t, "HEAD", olvHost, "/big", "alice")
	if code != 200 || body != "" || hdr.Get("Content-Length") != fmt.Sprint(bigSize) {
		t.Fatalf("code %d body %d Content-Length %q", code, len(body), hdr.Get("Content-Length"))
	}
}

func TestChunkOfAnotherRelaysStreamIsRefused(t *testing.T) {
	r := newContentRig(t)
	a := r.newRelay(t, "a", true)
	req, _ := http.NewRequest("GET", a.url+"/big", nil)
	req.Host = olvHost
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() // leave it half read: the session is open
	time.Sleep(200 * time.Millisecond)
	r.hub.Fwd.smu.Lock()
	var id string
	for k := range r.hub.Fwd.sessions {
		id = k
	}
	r.hub.Fwd.smu.Unlock()
	if id == "" {
		t.Skip("session already finished")
	}
	if _, err := r.hub.Fwd.Chunk(t.Context(), "b", relaylink.ForwardChunkRequest{ID: id, Index: 1}); err == nil {
		t.Fatal("relay b read relay a's stream")
	}
}
