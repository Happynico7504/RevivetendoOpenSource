package relayd

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/Happynico7504/relaylink"
)

// CertSet holds the certificates synced from the main. They live on disk
// (0600, so a restart works even if the main is briefly unreachable) and in
// memory. Certificates are only ever fetched with Client.Call, never through
// the caching Fetcher, and the main marks them uncacheable as well.
type CertSet struct {
	Client *relaylink.Client
	Dir    string // <data_dir>/certs
	Logf   func(string, ...any)

	mu       sync.RWMutex
	manifest relaylink.CertManifest
	certs    map[string]*tls.Certificate
	hashes   map[string]string
}

var safeName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*(/[a-z0-9][a-z0-9._-]*)*$`)

func (s *CertSet) logf(f string, a ...any) {
	if s.Logf != nil {
		s.Logf(f, a...)
	}
}

func (s *CertSet) path(name, ext string) (string, error) {
	if !safeName.MatchString(name) {
		return "", fmt.Errorf("unsafe certificate name %q", name)
	}
	return filepath.Join(s.Dir, filepath.FromSlash(name)+ext), nil
}

// LoadFromDisk restores the last synced state (manifest + certs).
func (s *CertSet) LoadFromDisk() error {
	raw, err := os.ReadFile(filepath.Join(s.Dir, "manifest.json"))
	if err != nil {
		return err
	}
	var m relaylink.CertManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	certs, hashes := map[string]*tls.Certificate{}, map[string]string{}
	for _, ci := range m.Certs {
		cp, _ := s.path(ci.Name, ".crt")
		kp, _ := s.path(ci.Name, ".key")
		if cp == "" {
			continue
		}
		c, err := tls.LoadX509KeyPair(cp, kp)
		if err != nil {
			s.logf("cert %s on disk unusable: %v", ci.Name, err)
			continue
		}
		certs[ci.Name], hashes[ci.Name] = &c, ci.SHA256
	}
	s.mu.Lock()
	s.manifest, s.certs, s.hashes = m, certs, hashes
	s.mu.Unlock()
	return nil
}

func writeAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Sync fetches the manifest and any new or changed certificates.
func (s *CertSet) Sync(ctx context.Context) error {
	resp, err := s.Client.Call(ctx, http.MethodGet, relaylink.CertManifestPath, nil)
	if err != nil {
		return err
	}
	if resp.Status != http.StatusOK {
		return fmt.Errorf("cert manifest: status %d", resp.Status)
	}
	var m relaylink.CertManifest
	if err := json.Unmarshal(resp.Body, &m); err != nil {
		return err
	}
	s.mu.RLock()
	oldCerts, oldHashes := s.certs, s.hashes
	s.mu.RUnlock()

	certs, hashes := map[string]*tls.Certificate{}, map[string]string{}
	changed := 0
	for _, ci := range m.Certs {
		if _, err := s.path(ci.Name, ".crt"); err != nil {
			s.logf("%v (ignored)", err)
			continue
		}
		if c, ok := oldCerts[ci.Name]; ok && oldHashes[ci.Name] == ci.SHA256 {
			certs[ci.Name], hashes[ci.Name] = c, ci.SHA256
			continue
		}
		pr, err := s.Client.Call(ctx, http.MethodGet, relaylink.CertPairPrefix+ci.Name, nil)
		if err != nil || pr.Status != http.StatusOK {
			s.logf("fetching cert %s failed (status/err: %v)", ci.Name, err)
			if c, ok := oldCerts[ci.Name]; ok { // keep serving the previous version
				certs[ci.Name], hashes[ci.Name] = c, oldHashes[ci.Name]
			}
			continue
		}
		var pair relaylink.CertPair
		if json.Unmarshal(pr.Body, &pair) != nil || pair.Name != ci.Name {
			s.logf("cert %s: malformed answer", ci.Name)
			continue
		}
		c, err := tls.X509KeyPair([]byte(pair.CertPEM), []byte(pair.KeyPEM))
		if err != nil {
			s.logf("cert %s: pair does not load: %v", ci.Name, err)
			continue
		}
		cp, _ := s.path(ci.Name, ".crt")
		kp, _ := s.path(ci.Name, ".key")
		if err := writeAtomic(kp, []byte(pair.KeyPEM), 0o600); err != nil {
			return err
		}
		if err := writeAtomic(cp, []byte(pair.CertPEM), 0o600); err != nil {
			return err
		}
		certs[ci.Name], hashes[ci.Name] = &c, ci.SHA256
		changed++
	}
	// Drop files for certificates the main no longer serves.
	for name := range oldCerts {
		if _, ok := certs[name]; !ok {
			cp, _ := s.path(name, ".crt")
			kp, _ := s.path(name, ".key")
			os.Remove(cp)
			os.Remove(kp)
		}
	}
	mj, _ := json.Marshal(m)
	if err := writeAtomic(filepath.Join(s.Dir, "manifest.json"), mj, 0o600); err != nil {
		return err
	}
	s.mu.Lock()
	s.manifest, s.certs, s.hashes = m, certs, hashes
	s.mu.Unlock()
	if changed > 0 {
		s.logf("certificates synced: %d updated, %d total", changed, len(certs))
	}
	return nil
}

// Select returns the certificate for an SNI using the main's route table.
func (s *CertSet) Select(sni string) (*tls.Certificate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	name := s.manifest.SelectCert(sni)
	if c, ok := s.certs[name]; ok {
		return c, nil
	}
	if c, ok := s.certs[s.manifest.Default]; ok {
		return c, nil
	}
	return nil, errors.New("no certificate available")
}

// Named returns one certificate by manifest name (for single-cert listeners).
func (s *CertSet) Named(name string) (*tls.Certificate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if c, ok := s.certs[name]; ok {
		return c, nil
	}
	return nil, fmt.Errorf("certificate %q not synced", name)
}

func (s *CertSet) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.certs)
}

// Run re-syncs periodically until ctx is done.
func (s *CertSet) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Sync(ctx); err != nil {
				s.logf("cert sync failed (keeping the current certificates): %v", err)
			}
		}
	}
}
