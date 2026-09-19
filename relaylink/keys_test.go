package relaylink

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestKeyPEMRoundTripAndBundle(t *testing.T) {
	priv, err := GenerateMainKey(2048)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateMainKey(1024); err == nil {
		t.Fatal("weak key accepted")
	}
	pp, _ := MarshalPrivatePEM(priv)
	back, err := ParsePrivatePEM(pp)
	if err != nil || back.N.Cmp(priv.N) != 0 {
		t.Fatalf("private PEM round trip: %v", err)
	}
	pubPEM, _ := MarshalPublicPEM(&priv.PublicKey)
	pub, err := ParsePublicPEM(pubPEM)
	if err != nil || pub.N.Cmp(priv.N) != 0 {
		t.Fatalf("public PEM round trip: %v", err)
	}
	if _, err := ParsePrivatePEM(pubPEM); err == nil {
		t.Fatal("public PEM parsed as private")
	}
	if _, err := ParsePublicPEM([]byte("garbage")); err == nil {
		t.Fatal("garbage parsed")
	}

	// A bundle produced for a relay yields a working Client against a server.
	relayPub, relayPriv, _ := NewRelayIdentity()
	b, _ := NewBundle("jp-1", relayPriv, &priv.PublicKey, "http://placeholder")
	srv := &Server{
		Priv:     priv,
		RelayKey: func(id string) (ed25519.PublicKey, bool) { return relayPub, id == "jp-1" },
		Replay:   &MemoryReplay{},
	}
	ts := httptest.NewServer(srv.Handler(func(q *Request) (int, []byte) { return http.StatusOK, []byte(q.RelayID) }))
	defer ts.Close()
	b.MainURL = ts.URL
	raw := mustJSON(t, b)
	parsed, err := ParseBundle(raw)
	if err != nil {
		t.Fatal(err)
	}
	c, err := parsed.Client()
	if err != nil {
		t.Fatal(err)
	}
	status, body, err := c.Do(context.Background(), "GET", "/x", nil)
	if err != nil || status != 200 || string(body) != "jp-1" {
		t.Fatalf("bundle client: %d %q %v", status, body, err)
	}
	if _, err := ParseBundle([]byte(`{"relay_id":"x"}`)); err == nil {
		t.Fatal("incomplete bundle accepted")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
