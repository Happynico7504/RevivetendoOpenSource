package relayhub

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"github.com/Happynico7504/relaylink"
)

// ReleaseStore serves pre-signed relayd releases. Layout:
//
//	<dir>/<os>-<arch>/manifest.json   signed manifest (relaylink.UpdateManifest)
//	<dir>/<os>-<arch>/relayd          the binary
//
// The hub never signs anything: it only serves what `relayhub release sign`
// produced (possibly on another machine), so a compromised hub cannot forge an
// update, it can only withhold or roll back to an older signed one.
type ReleaseStore struct {
	Dir string

	mu    sync.Mutex
	cache map[string]*releaseEntry
}

type releaseEntry struct {
	manifest *relaylink.UpdateManifest
	raw      []byte
	binary   []byte
	mtimes   [2]int64
}

var platformRe = regexp.MustCompile(`^[a-z0-9]{1,16}$`)

func (s *ReleaseStore) load(goos, goarch string) (*releaseEntry, error) {
	if !platformRe.MatchString(goos) || !platformRe.MatchString(goarch) {
		return nil, ErrNotFound
	}
	dir := filepath.Join(s.Dir, goos+"-"+goarch)
	mi, err := os.Stat(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, ErrNotFound
	}
	bi, err := os.Stat(filepath.Join(dir, "relayd"))
	if err != nil {
		return nil, ErrNotFound
	}
	key := goos + "-" + goarch
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache == nil {
		s.cache = map[string]*releaseEntry{}
	}
	if e, ok := s.cache[key]; ok && e.mtimes == [2]int64{mi.ModTime().UnixNano(), bi.ModTime().UnixNano()} {
		return e, nil
	}
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, ErrNotFound
	}
	var m relaylink.UpdateManifest
	if json.Unmarshal(raw, &m) != nil {
		return nil, ErrNotFound
	}
	bin, err := os.ReadFile(filepath.Join(dir, "relayd"))
	if err != nil {
		return nil, ErrNotFound
	}
	// Guard against a half-published release: never serve a manifest whose
	// binary is not the one it describes.
	if m.OS != goos || m.Arch != goarch || m.VerifyBinary(bin) != nil {
		return nil, ErrNotFound
	}
	e := &releaseEntry{manifest: &m, raw: raw, binary: bin, mtimes: [2]int64{mi.ModTime().UnixNano(), bi.ModTime().UnixNano()}}
	s.cache[key] = e
	return e, nil
}

// Manifest returns the signed manifest for a platform.
func (s *ReleaseStore) Manifest(goos, goarch string) (*relaylink.UpdateManifest, error) {
	e, err := s.load(goos, goarch)
	if err != nil {
		return nil, err
	}
	m := *e.manifest
	return &m, nil
}

// Chunk returns the slice of the binary starting at offset.
func (s *ReleaseStore) Chunk(goos, goarch string, version uint64, offset int64) (*relaylink.UpdateChunk, error) {
	e, err := s.load(goos, goarch)
	if err != nil || e.manifest.Version != version {
		return nil, ErrNotFound
	}
	size := int64(len(e.binary))
	if offset < 0 || offset >= size {
		return nil, ErrNotFound
	}
	end := offset + relaylink.UpdateChunkSize
	if end > size {
		end = size
	}
	return &relaylink.UpdateChunk{Offset: offset, Data: e.binary[offset:end], EOF: end == size}, nil
}

// PublishRelease signs a binary and writes <dir>/<os>-<arch>/{relayd,manifest.json}.
// It refuses to publish a version that is not newer than what is already there
// (relays would ignore it anyway) unless force is set.
func PublishRelease(dir string, binary []byte, version uint64, label, goos, goarch string, priv ed25519.PrivateKey, force bool) (*relaylink.UpdateManifest, error) {
	if !platformRe.MatchString(goos) || !platformRe.MatchString(goarch) {
		return nil, fmt.Errorf("bad platform %q/%q", goos, goarch)
	}
	if len(binary) == 0 || int64(len(binary)) > relaylink.MaxUpdateSize {
		return nil, fmt.Errorf("binary size %d outside 1..%d bytes", len(binary), relaylink.MaxUpdateSize)
	}
	if version == 0 {
		return nil, errors.New("version must be greater than 0")
	}
	pdir := filepath.Join(dir, goos+"-"+goarch)
	if !force {
		if raw, err := os.ReadFile(filepath.Join(pdir, "manifest.json")); err == nil {
			var old relaylink.UpdateManifest
			if json.Unmarshal(raw, &old) == nil && version <= old.Version {
				return nil, fmt.Errorf("version %d is not newer than the published %d (use -force to override)", version, old.Version)
			}
		}
	}
	sum := sha256.Sum256(binary)
	m := &relaylink.UpdateManifest{
		Version: version, Label: label, OS: goos, Arch: goarch,
		SHA256: hex.EncodeToString(sum[:]), Size: int64(len(binary)),
	}
	m.Sign(priv)
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		return nil, err
	}
	// Binary first, manifest last; each replaced atomically. (A reader that sees
	// the new binary with the old manifest simply finds no match and serves 404.)
	if err := writeFileAtomic(filepath.Join(pdir, "relayd"), binary, 0o755); err != nil {
		return nil, err
	}
	raw, _ := json.MarshalIndent(m, "", "  ")
	if err := writeFileAtomic(filepath.Join(pdir, "manifest.json"), raw, 0o644); err != nil {
		return nil, err
	}
	return m, nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
