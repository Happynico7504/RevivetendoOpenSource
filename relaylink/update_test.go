package relaylink

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func signed(t *testing.T, priv ed25519.PrivateKey, ver uint64, data []byte) *UpdateManifest {
	t.Helper()
	sum := sha256.Sum256(data)
	m := &UpdateManifest{Version: ver, Label: "test", OS: "linux", Arch: "amd64", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))}
	m.Sign(priv)
	return m
}

func TestUpdateManifestVerification(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	otherPub, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	bin := []byte("the new relayd binary")
	m := signed(t, priv, 7, bin)

	if err := m.Verify(pub, "linux", "amd64", 6); err != nil {
		t.Fatalf("good manifest rejected: %v", err)
	}
	// Signed by someone else / verified against the wrong pinned key.
	if err := m.Verify(otherPub, "linux", "amd64", 6); err != ErrUpdateSignature {
		t.Errorf("wrong pinned key: %v", err)
	}
	if err := signed(t, otherPriv, 7, bin).Verify(pub, "linux", "amd64", 6); err != ErrUpdateSignature {
		t.Errorf("forged signer accepted: %v", err)
	}
	// No downgrade, no replay of the running version.
	for _, cur := range []uint64{7, 8, 1000} {
		if err := m.Verify(pub, "linux", "amd64", cur); err != ErrUpdateNotNewer {
			t.Errorf("current=%d: %v", cur, err)
		}
	}
	// Wrong platform (an amd64 binary must never be installed on arm64).
	if err := m.Verify(pub, "linux", "arm64", 6); err != ErrUpdatePlatform {
		t.Errorf("arch: %v", err)
	}
	if err := m.Verify(pub, "darwin", "amd64", 6); err != ErrUpdatePlatform {
		t.Errorf("os: %v", err)
	}
	// Every signed field is covered: changing any one invalidates the signature.
	for name, mut := range map[string]func(*UpdateManifest){
		"version": func(x *UpdateManifest) { x.Version = 99 },
		"size":    func(x *UpdateManifest) { x.Size++ },
		"sha":     func(x *UpdateManifest) { x.SHA256 = hex.EncodeToString(make([]byte, 32)) },
		"os":      func(x *UpdateManifest) { x.OS = "linux2" },
		"arch":    func(x *UpdateManifest) { x.Arch = "arm64" },
		"label":   func(x *UpdateManifest) { x.Label = "other" },
	} {
		c := *m
		mut(&c)
		if err := c.Verify(pub, c.OS, c.Arch, 0); err != ErrUpdateSignature {
			t.Errorf("tampered %s not detected: %v", name, err)
		}
	}
	// Field boundaries cannot be shifted ("ab"+"c" vs "a"+"bc").
	a := &UpdateManifest{OS: "ab", Arch: "c", SHA256: "x", Label: "y"}
	b := &UpdateManifest{OS: "a", Arch: "bc", SHA256: "x", Label: "y"}
	if string(a.SigningBytes()) == string(b.SigningBytes()) {
		t.Error("ambiguous canonical encoding")
	}
	// Sanity limits.
	for name, mm := range map[string]*UpdateManifest{
		"zero size": signed(t, priv, 9, nil),
		"huge size": func() *UpdateManifest {
			x := signed(t, priv, 9, bin)
			x.Size = MaxUpdateSize + 1
			x.Sign(priv)
			return x
		}(),
		"short hash": func() *UpdateManifest { x := signed(t, priv, 9, bin); x.SHA256 = "abcd"; x.Sign(priv); return x }(),
		"non-hex": func() *UpdateManifest {
			x := signed(t, priv, 9, bin)
			x.SHA256 = string(make([]byte, 64))
			x.Sign(priv)
			return x
		}(),
	} {
		if err := mm.Verify(pub, "linux", "amd64", 0); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := (&UpdateManifest{}).Verify(nil, "linux", "amd64", 0); err == nil {
		t.Error("nil pinned key accepted")
	}
}

func TestVerifyBinary(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	bin := []byte("payload")
	m := signed(t, priv, 1, bin)
	if err := m.VerifyBinary(bin); err != nil {
		t.Fatal(err)
	}
	if m.VerifyBinary([]byte("payloaD")) == nil {
		t.Error("different bytes accepted")
	}
	if m.VerifyBinary(append([]byte("payload"), 0)) == nil {
		t.Error("longer binary accepted")
	}
	if m.VerifyBinary(bin[:3]) == nil {
		t.Error("truncated binary accepted")
	}
}

func TestReleaseKeyEncoding(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	back, err := ParseReleasePublicKey(EncodeReleasePublicKey(pub))
	if err != nil || !pub.Equal(back) {
		t.Fatalf("round trip: %v", err)
	}
	for _, bad := range []string{"", "not base64!!", EncodeReleasePublicKey(pub)[:10]} {
		if _, err := ParseReleasePublicKey(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// Releases signed before components existed must still verify: the relayd encoding is
// the original v1 byte string. Built here independently of SigningBytes so a change to
// that function cannot silently pass its own test.
func TestRelaydSigningBytesAreTheOriginalV1Encoding(t *testing.T) {
	m := &UpdateManifest{Version: 7, Label: "hello", OS: "linux", Arch: "amd64", SHA256: strings.Repeat("ab", 32), Size: 1234}
	var want bytes.Buffer
	want.WriteString("relayd-release-v1\x00")
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], 7)
	want.Write(n[:])
	binary.BigEndian.PutUint64(n[:], 1234)
	want.Write(n[:])
	for _, s := range []string{"linux", "amd64", strings.Repeat("ab", 32), "hello"} {
		binary.BigEndian.PutUint32(n[:4], uint32(len(s)))
		want.Write(n[:4])
		want.WriteString(s)
	}
	if !bytes.Equal(m.SigningBytes(), want.Bytes()) {
		t.Fatal("relayd signing bytes changed: every release signed so far would stop verifying")
	}
	m.Component = "relayd" // explicit is the same as default
	if !bytes.Equal(m.SigningBytes(), want.Bytes()) {
		t.Fatal(`Component "relayd" must encode like the empty default`)
	}
}

func TestComponentsCannotBeSwapped(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	bin := []byte("edge binary")
	sum := sha256.Sum256(bin)
	edge := &UpdateManifest{Component: "wscedge", Version: 3, OS: "linux", Arch: "amd64", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(bin))}
	edge.Sign(priv)

	if err := edge.VerifyComponent(pub, "wscedge", "linux", "amd64", 2); err != nil {
		t.Fatalf("valid component release refused: %v", err)
	}
	// Asked for as the relay daemon: refused by the component check.
	if err := edge.Verify(pub, "linux", "amd64", 2); !errors.Is(err, ErrUpdateComponent) {
		t.Fatalf("edge build accepted as relayd: %v", err)
	}
	// A hub that relabels the manifest breaks the signature: the name is signed.
	relabelled := *edge
	relabelled.Component = ""
	if err := relabelled.Verify(pub, "linux", "amd64", 2); !errors.Is(err, ErrUpdateSignature) {
		t.Fatalf("relabelled manifest accepted: %v", err)
	}
	other := *edge
	other.Component = "other"
	if err := other.VerifyComponent(pub, "other", "linux", "amd64", 2); !errors.Is(err, ErrUpdateSignature) {
		t.Fatalf("component name is not covered by the signature: %v", err)
	}
	// A relayd manifest is likewise refused when a component is expected.
	rd := &UpdateManifest{Version: 9, OS: "linux", Arch: "amd64", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(bin))}
	rd.Sign(priv)
	if err := rd.VerifyComponent(pub, "wscedge", "linux", "amd64", 0); !errors.Is(err, ErrUpdateComponent) {
		t.Fatalf("relayd build accepted as wscedge: %v", err)
	}
}
