package relaylink

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
)

// Over-the-air updates for relayd.
//
// Trust model: releases are signed with a dedicated Ed25519 RELEASE key that is
// separate from the main's RSA key. A relay pins the release PUBLIC key (it is
// part of the relay's bundle) and installs a binary only if its signed manifest
// verifies, matches the relay's OS/arch, has a strictly higher version than the
// running one (no downgrades) and the binary's SHA-256 and size match the
// manifest. The signing key can live off the main (sign on a laptop, copy the
// result over), in which case even a fully compromised main cannot push code to
// the relays; it can only withhold updates.

const (
	UpdateManifestPath = "/relay/v1/update/manifest" // GET ?os=&arch=
	UpdateChunkPath    = "/relay/v1/update/chunk"    // GET ?os=&arch=&version=&offset=
	UpdateChunkSize    = 1 << 20
	MaxUpdateSize      = 64 << 20
)

// UpdateManifest describes one release binary for one platform.
type UpdateManifest struct {
	Version uint64 `json:"version"` // strictly increasing
	Label   string `json:"label,omitempty"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	SHA256  string `json:"sha256"` // hex, of the binary
	Size    int64  `json:"size"`
	Sig     []byte `json:"sig"` // Ed25519 over SigningBytes()
}

// UpdateChunk is one slice of the binary.
type UpdateChunk struct {
	Offset int64  `json:"offset"`
	Data   []byte `json:"data"`
	EOF    bool   `json:"eof"`
}

// SigningBytes is the canonical, unambiguous byte string that gets signed:
// every field is length- or width-delimited, and a fixed prefix separates this
// use of the key from any other.
func (m *UpdateManifest) SigningBytes() []byte {
	var b bytes.Buffer
	b.WriteString("relayd-release-v1\x00")
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], m.Version)
	b.Write(n[:])
	binary.BigEndian.PutUint64(n[:], uint64(m.Size))
	b.Write(n[:])
	for _, s := range []string{m.OS, m.Arch, m.SHA256, m.Label} {
		binary.BigEndian.PutUint32(n[:4], uint32(len(s)))
		b.Write(n[:4])
		b.WriteString(s)
	}
	return b.Bytes()
}

// Sign fills in Sig.
func (m *UpdateManifest) Sign(priv ed25519.PrivateKey) {
	m.Sig = ed25519.Sign(priv, m.SigningBytes())
}

var (
	ErrUpdateSignature = errors.New("relaylink: release signature invalid")
	ErrUpdatePlatform  = errors.New("relaylink: release is for a different platform")
	ErrUpdateNotNewer  = errors.New("relaylink: release is not newer than the running version")
	ErrUpdateBad       = errors.New("relaylink: malformed release manifest")
)

// Verify checks a manifest against the pinned key and the relay's platform and
// current version. It does not look at the binary itself (see VerifyBinary).
func (m *UpdateManifest) Verify(pub ed25519.PublicKey, goos, goarch string, current uint64) error {
	if len(pub) != ed25519.PublicKeySize {
		return ErrUpdateSignature
	}
	if !ed25519.Verify(pub, m.SigningBytes(), m.Sig) {
		return ErrUpdateSignature
	}
	if m.OS != goos || m.Arch != goarch {
		return ErrUpdatePlatform
	}
	if m.Size <= 0 || m.Size > MaxUpdateSize || len(m.SHA256) != 64 {
		return ErrUpdateBad
	}
	if _, err := hex.DecodeString(m.SHA256); err != nil {
		return ErrUpdateBad
	}
	if m.Version <= current {
		return ErrUpdateNotNewer
	}
	return nil
}

// VerifyBinary checks downloaded bytes against the manifest.
func (m *UpdateManifest) VerifyBinary(data []byte) error {
	if int64(len(data)) != m.Size {
		return fmt.Errorf("relaylink: binary size %d does not match manifest (%d)", len(data), m.Size)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != m.SHA256 {
		return errors.New("relaylink: binary hash does not match manifest")
	}
	return nil
}

// ---- key helpers ------------------------------------------------------------

// EncodeReleasePublicKey / ParseReleasePublicKey: the format stored in bundles.
func EncodeReleasePublicKey(pub ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(pub)
}

func ParseReleasePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("relaylink: bad release public key")
	}
	return ed25519.PublicKey(b), nil
}
