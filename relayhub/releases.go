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
	"strings"
	"sync"

	"github.com/Happynico7504/relaylink"
)

// ReleaseStore serves pre-signed releases of the relay daemon and of separately
// shipped components (for example the WSC edge). Layout:
//
//	<dir>/<os>-<arch>/manifest.json              relayd: signed manifest
//	<dir>/<os>-<arch>/relayd                     relayd: the binary
//	<dir>/<component>/<os>-<arch>/manifest.json  any other component
//	<dir>/<component>/<os>-<arch>/<component>    its binary
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
	stamps   [4]int64 // mtime+size of manifest and binary: mtime alone misses fast rewrites
}

var (
	platformRe  = regexp.MustCompile(`^[a-z0-9]{1,16}$`)
	componentRe = regexp.MustCompile(`^[a-z][a-z0-9]{0,31}$`) // no hyphen: never collides with a <os>-<arch> directory
)

// releaseDir returns where a component's platform release lives and what its binary
// file is called. The relay daemon keeps the original flat layout.
func (s *ReleaseStore) releaseDir(component, goos, goarch string) (dir, binName string, err error) {
	if component == "" {
		component = relaylink.ComponentRelayd
	}
	if !componentRe.MatchString(component) || !platformRe.MatchString(goos) || !platformRe.MatchString(goarch) {
		return "", "", ErrNotFound
	}
	if component == relaylink.ComponentRelayd {
		return filepath.Join(s.Dir, goos+"-"+goarch), "relayd", nil
	}
	return filepath.Join(s.Dir, component, goos+"-"+goarch), component, nil
}

func (s *ReleaseStore) load(component, goos, goarch string) (*releaseEntry, error) {
	if component == "" {
		component = relaylink.ComponentRelayd
	}
	dir, binName, err := s.releaseDir(component, goos, goarch)
	if err != nil {
		return nil, err
	}
	mi, err := os.Stat(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, ErrNotFound
	}
	bi, err := os.Stat(filepath.Join(dir, binName))
	if err != nil {
		return nil, ErrNotFound
	}
	key := component + "/" + goos + "-" + goarch
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache == nil {
		s.cache = map[string]*releaseEntry{}
	}
	if e, ok := s.cache[key]; ok && e.stamps == [4]int64{mi.ModTime().UnixNano(), mi.Size(), bi.ModTime().UnixNano(), bi.Size()} {
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
	bin, err := os.ReadFile(filepath.Join(dir, binName))
	if err != nil {
		return nil, ErrNotFound
	}
	// Guard against a half-published release: never serve a manifest whose
	// binary is not the one it describes, or one filed under the wrong component.
	if m.ComponentName() != component || m.OS != goos || m.Arch != goarch || m.VerifyBinary(bin) != nil {
		return nil, ErrNotFound
	}
	e := &releaseEntry{manifest: &m, raw: raw, binary: bin, stamps: [4]int64{mi.ModTime().UnixNano(), mi.Size(), bi.ModTime().UnixNano(), bi.Size()}}
	s.cache[key] = e
	return e, nil
}

// Manifest returns the signed relayd manifest for a platform.
func (s *ReleaseStore) Manifest(goos, goarch string) (*relaylink.UpdateManifest, error) {
	return s.ManifestFor(relaylink.ComponentRelayd, goos, goarch)
}

// ManifestFor returns the signed manifest of a component for a platform.
func (s *ReleaseStore) ManifestFor(component, goos, goarch string) (*relaylink.UpdateManifest, error) {
	e, err := s.load(component, goos, goarch)
	if err != nil {
		return nil, err
	}
	m := *e.manifest
	return &m, nil
}

// Chunk returns the slice of the relayd binary starting at offset.
func (s *ReleaseStore) Chunk(goos, goarch string, version uint64, offset int64) (*relaylink.UpdateChunk, error) {
	return s.ChunkFor(relaylink.ComponentRelayd, goos, goarch, version, offset)
}

// ChunkFor returns the slice of a component's binary starting at offset.
func (s *ReleaseStore) ChunkFor(component, goos, goarch string, version uint64, offset int64) (*relaylink.UpdateChunk, error) {
	e, err := s.load(component, goos, goarch)
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

// PublishRelease signs a relayd binary and writes <dir>/<os>-<arch>/{relayd,manifest.json}.
// It refuses to publish a version that is not newer than what is already there
// (relays would ignore it anyway) unless force is set.
func PublishRelease(dir string, binary []byte, version uint64, label, goos, goarch string, priv ed25519.PrivateKey, force bool) (*relaylink.UpdateManifest, error) {
	return PublishComponent(dir, relaylink.ComponentRelayd, binary, version, label, goos, goarch, priv, force)
}

// PublishComponent is PublishRelease for any component. Each component has its own
// version line, so publishing one never affects another.
func PublishComponent(dir, component string, binary []byte, version uint64, label, goos, goarch string, priv ed25519.PrivateKey, force bool) (*relaylink.UpdateManifest, error) {
	if component == "" {
		component = relaylink.ComponentRelayd
	}
	if !componentRe.MatchString(component) {
		return nil, fmt.Errorf("bad component name %q", component)
	}
	if !platformRe.MatchString(goos) || !platformRe.MatchString(goarch) {
		return nil, fmt.Errorf("bad platform %q/%q", goos, goarch)
	}
	if len(binary) == 0 || int64(len(binary)) > relaylink.MaxUpdateSize {
		return nil, fmt.Errorf("binary size %d outside 1..%d bytes", len(binary), relaylink.MaxUpdateSize)
	}
	if version == 0 {
		return nil, errors.New("version must be greater than 0")
	}
	store := &ReleaseStore{Dir: dir}
	pdir, binName, err := store.releaseDir(component, goos, goarch)
	if err != nil {
		return nil, err
	}
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
	if component != relaylink.ComponentRelayd {
		m.Component = component // relayd manifests keep the original, component-less form
	}
	m.Sign(priv)
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		return nil, err
	}
	// Binary first, manifest last; each replaced atomically. (A reader that sees
	// the new binary with the old manifest simply finds no match and serves 404.)
	if err := writeFileAtomic(filepath.Join(pdir, binName), binary, 0o755); err != nil {
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

// Components lists every component that has something published: relayd (if its flat
// release exists) plus each subdirectory holding a platform release.
func (s *ReleaseStore) Components() []string {
	out := []string{}
	if ents, err := os.ReadDir(s.Dir); err == nil {
		for _, e := range ents {
			if !e.IsDir() || !componentRe.MatchString(e.Name()) {
				continue
			}
			if sub, err := os.ReadDir(filepath.Join(s.Dir, e.Name())); err == nil {
				for _, p := range sub {
					if p.IsDir() && strings.Contains(p.Name(), "-") {
						out = append(out, e.Name())
						break
					}
				}
			}
		}
	}
	return append([]string{relaylink.ComponentRelayd}, out...)
}
