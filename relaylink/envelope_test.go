package relaylink

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type rig struct {
	main   *Server
	client *Client
	relayP ed25519.PublicKey
	now    time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048) // 2048 keeps tests fast
	if err != nil {
		t.Fatal(err)
	}
	pub, sk, _ := ed25519.GenerateKey(rand.Reader)
	r := &rig{relayP: pub, now: time.Unix(1_800_000_000, 0)}
	clock := func() time.Time { return r.now }
	r.main = &Server{
		Priv: priv,
		RelayKey: func(id string) (ed25519.PublicKey, bool) {
			if id == "us-1" {
				return pub, true
			}
			return nil, false
		},
		Replay: &MemoryReplay{Now: clock},
		Now:    clock,
	}
	r.client = &Client{RelayID: "us-1", MainPub: &priv.PublicKey, Sign: sk, Now: clock}
	return r
}

func (r *rig) roundTrip(t *testing.T, method, path string, body []byte) (*Request, int, []byte) {
	t.Helper()
	env, pending, err := r.client.Seal(method, path, body)
	if err != nil {
		t.Fatal(err)
	}
	req, reply, err := r.main.Open(context.Background(), env)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	out, err := reply.Seal(201, []byte("pong:"+string(body)))
	if err != nil {
		t.Fatal(err)
	}
	status, resp, err := pending.OpenResponse(out)
	if err != nil {
		t.Fatalf("OpenResponse: %v", err)
	}
	return req, status, resp
}

func TestRoundTrip(t *testing.T) {
	r := newRig(t)
	req, status, resp := r.roundTrip(t, "POST", "/relay/v1/nex-token", []byte(`{"pid":1}`))
	if req.RelayID != "us-1" || req.Method != "POST" || req.Path != "/relay/v1/nex-token" || string(req.Body) != `{"pid":1}` {
		t.Fatalf("bad request: %+v", req)
	}
	if status != 201 || string(resp) != `pong:{"pid":1}` {
		t.Fatalf("bad response: %d %q", status, resp)
	}
}

func TestEmptyAndLargeBody(t *testing.T) {
	r := newRig(t)
	r.roundTrip(t, "GET", "/x", nil)
	big := bytes.Repeat([]byte{0xAB}, 3<<20)
	req, _, _ := r.roundTrip(t, "POST", "/big", big)
	if !bytes.Equal(req.Body, big) {
		t.Fatal("large body corrupted")
	}
}

func TestPlaintextNotOnTheWire(t *testing.T) {
	r := newRig(t)
	env, _, _ := r.client.Seal("POST", "/secret-path", []byte("SECRET-BODY-MARKER"))
	raw, _ := json.Marshal(env)
	for _, needle := range []string{"SECRET-BODY-MARKER", "/secret-path"} {
		if bytes.Contains(raw, []byte(needle)) {
			t.Fatalf("%q visible in envelope", needle)
		}
	}
}

func TestReplayRejected(t *testing.T) {
	r := newRig(t)
	env, _, _ := r.client.Seal("POST", "/a", []byte("x"))
	if _, _, err := r.main.Open(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.main.Open(context.Background(), env); err != ErrReplay {
		t.Fatalf("want ErrReplay, got %v", err)
	}
}

func TestExpiredAndFutureTimestamps(t *testing.T) {
	r := newRig(t)
	env, _, _ := r.client.Seal("POST", "/a", nil)
	r.now = r.now.Add(MaxClockSkew + time.Second)
	if _, _, err := r.main.Open(context.Background(), env); err != ErrExpired {
		t.Fatalf("stale: want ErrExpired, got %v", err)
	}
	r.now = r.now.Add(-2*MaxClockSkew - 2*time.Second)
	env2, _, _ := r.client.Seal("POST", "/a", nil)
	r.now = r.now.Add(-MaxClockSkew - time.Second) // main clock now far behind the relay
	if _, _, err := r.main.Open(context.Background(), env2); err != ErrExpired {
		t.Fatalf("future: want ErrExpired, got %v", err)
	}
}

func TestUnknownOrRevokedRelay(t *testing.T) {
	r := newRig(t)
	r.client.RelayID = "jp-9"
	env, _, _ := r.client.Seal("POST", "/a", nil)
	if _, _, err := r.main.Open(context.Background(), env); err != ErrUnknownRelay {
		t.Fatalf("want ErrUnknownRelay, got %v", err)
	}
}

func TestWrongSigningKeyRejected(t *testing.T) {
	r := newRig(t)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	r.client.Sign = other // knows the main's public key, but is not the real relay
	env, _, _ := r.client.Seal("POST", "/a", nil)
	if _, _, err := r.main.Open(context.Background(), env); err != ErrBadSignature {
		t.Fatalf("want ErrBadSignature, got %v", err)
	}
}

func TestRelayIDSwapRejected(t *testing.T) {
	// Re-labelling an envelope as another relay must fail (AAD binds relay_id).
	r := newRig(t)
	env, _, _ := r.client.Seal("POST", "/a", nil)
	env.RelayID = "us-1 " // different string, same key lookup would fail anyway
	if _, _, err := r.main.Open(context.Background(), env); err == nil {
		t.Fatal("tampered relay_id accepted")
	}
}

func TestTamperedCiphertextAndKey(t *testing.T) {
	r := newRig(t)
	for _, mutate := range []func(*Envelope){
		func(e *Envelope) { e.Ciphertext[len(e.Ciphertext)/2] ^= 1 },
		func(e *Envelope) { e.Nonce[0] ^= 1 },
		func(e *Envelope) { e.WrappedKey[10] ^= 1 },
		func(e *Envelope) { e.V = 2 },
	} {
		env, _, _ := r.client.Seal("POST", "/a", []byte("x"))
		mutate(env)
		if _, _, err := r.main.Open(context.Background(), env); err == nil {
			t.Fatal("tampered envelope accepted")
		}
	}
}

func TestTamperedOrForeignResponseRejected(t *testing.T) {
	r := newRig(t)
	env, pending, _ := r.client.Seal("POST", "/a", nil)
	_, reply, _ := r.main.Open(context.Background(), env)
	out, _ := reply.Seal(200, []byte("ok"))
	bad := append([]byte(nil), out...)
	bad[len(bad)-1] ^= 1
	if _, _, err := pending.OpenResponse(bad); err == nil {
		t.Fatal("tampered response accepted")
	}
	// A response to a different request (different key/nonce) must not open.
	env2, _, _ := r.client.Seal("POST", "/a", nil)
	_, reply2, _ := r.main.Open(context.Background(), env2)
	out2, _ := reply2.Seal(200, []byte("ok"))
	if _, _, err := pending.OpenResponse(out2); err == nil {
		t.Fatal("response from another request accepted")
	}
	if _, _, err := pending.OpenResponse([]byte("short")); err == nil {
		t.Fatal("short response accepted")
	}
}

func TestWrongMainKeyCannotReadOrSpoof(t *testing.T) {
	r := newRig(t)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	imposter := &Server{Priv: other, RelayKey: r.main.RelayKey, Replay: &MemoryReplay{}, Now: r.main.Now}
	env, _, _ := r.client.Seal("POST", "/a", nil)
	if _, _, err := imposter.Open(context.Background(), env); err == nil {
		t.Fatal("a server without the main's private key opened the request")
	}
}

func TestHTTPEndToEnd(t *testing.T) {
	r := newRig(t)
	r.client.Now = nil // real clock over HTTP
	r.main.Now = nil
	r.main.Replay = &MemoryReplay{}
	srv := httptest.NewServer(r.main.Handler(func(q *Request) (int, []byte) {
		return http.StatusOK, []byte(q.RelayID + " " + q.Method + " " + q.Path + " " + string(q.Body))
	}))
	defer srv.Close()
	r.client.BaseURL = srv.URL

	status, body, err := r.client.Do(context.Background(), "POST", "/sync/config", []byte("v=3"))
	if err != nil || status != 200 || string(body) != "us-1 POST /sync/config v=3" {
		t.Fatalf("got %d %q %v", status, body, err)
	}

	// A plain (non-envelope) request and a wrong path are opaque failures.
	for _, path := range []string{RPCPath, "/other"} {
		resp, _ := http.Post(srv.URL+path, "application/json", bytes.NewReader([]byte(`{"v":1}`)))
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("garbage accepted on %s", path)
		}
		resp.Body.Close()
	}
}

type failingReplay struct{}

func (failingReplay) Seen(context.Context, string, []byte, time.Duration) (bool, error) {
	return false, context.DeadlineExceeded
}

func TestFailsClosedWhenReplayStoreDown(t *testing.T) {
	r := newRig(t)
	r.main.Replay = failingReplay{}
	env, _, _ := r.client.Seal("POST", "/a", nil)
	if _, _, err := r.main.Open(context.Background(), env); err == nil {
		t.Fatal("request accepted while replay store unavailable")
	}
}
