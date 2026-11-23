package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"net/http/httptest"
	"testing"
)

func TestWiiKaraokeServiceTokenFormat(t *testing.T) {
	wiiKaraokeTokenKey = "testkey"
	r := httptest.NewRequest("GET", "/v1/api/provider/service_token/@me", nil)
	r.Header.Set("X-Nintendo-Title-ID", "0005000010100D00")

	token := generateWiiKaraokeServiceToken(1435853600, r)
	t.Log(token)
	raw, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 60 {
		t.Fatalf("length = %d, identity.py only accepts 60", len(raw))
	}
	if pid := binary.BigEndian.Uint32(raw[0:4]); pid != 1435853600 {
		t.Errorf("pid = %d", pid)
	}
	if title := binary.BigEndian.Uint64(raw[4:12]); title != 0x0005000010100D00 {
		t.Errorf("title = %x", title)
	}
	mac := hmac.New(sha256.New, []byte("testkey"))
	mac.Write(raw[:28])
	if !hmac.Equal(mac.Sum(nil), raw[28:]) {
		t.Error("bad HMAC")
	}
}
