package relayhub

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Happynico7504/relaylink"
)

// CertStore serves the main's leaf certificates (with their private keys) to
// relays. Safety rules, enforced by construction:
//
//   - A key is only ever served next to a certificate that is NOT a CA. The CA
//     private keys (which can mint certificates for anything the consoles
//     trust) are never reachable, whatever they are named.
//   - Only <name>.crt + <name>.key pairs whose key actually matches the
//     certificate are served; CSRs, serial files, backups (.bak*) and orphans
//     are ignored.
//   - A relay can only ask for names that appear in the scan; there is no
//     path handling, so no way to read other files.
type CertStore struct {
	Dir     string
	Default string // name of the fallback cert
	Routes  []relaylink.CertRoute
	TTL     time.Duration

	mu      sync.Mutex
	scanned time.Time
	pairs   map[string]*scannedPair
	list    []relaylink.CertInfo
}

type scannedPair struct {
	pair relaylink.CertPair
	info relaylink.CertInfo
}

func (s *CertStore) ttl() time.Duration {
	if s.TTL > 0 {
		return s.TTL
	}
	return 30 * time.Second
}

func (s *CertStore) scan() error {
	pairs := map[string]*scannedPair{}
	var list []relaylink.CertInfo
	err := filepath.WalkDir(s.Dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".crt") {
			return nil
		}
		rel, _ := filepath.Rel(s.Dir, path)
		name := strings.TrimSuffix(filepath.ToSlash(rel), ".crt")
		if strings.Contains(name, ".bak") || strings.Contains(name, "fullchain") {
			return nil
		}
		certPEM, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		keyPEM, err := os.ReadFile(strings.TrimSuffix(path, ".crt") + ".key")
		if err != nil {
			return nil // no key: a public-only certificate, nothing to sync
		}
		if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
			return nil // key does not match the certificate
		}
		blk, _ := pem.Decode(certPEM)
		if blk == nil {
			return nil
		}
		leaf, err := x509.ParseCertificate(blk.Bytes)
		if err != nil || leaf.IsCA || leaf.KeyUsage&x509.KeyUsageCertSign != 0 {
			return nil // NEVER serve the key of a CA
		}
		sum := sha256.Sum256(append(append([]byte{}, certPEM...), keyPEM...))
		info := relaylink.CertInfo{
			Name: name, SHA256: hex.EncodeToString(sum[:]),
			NotAfter: leaf.NotAfter.Unix(), DNSNames: leaf.DNSNames,
		}
		if len(info.DNSNames) == 0 && leaf.Subject.CommonName != "" {
			info.DNSNames = []string{leaf.Subject.CommonName}
		}
		pairs[name] = &scannedPair{
			pair: relaylink.CertPair{Name: name, CertPEM: string(certPEM), KeyPEM: string(keyPEM)},
			info: info,
		}
		list = append(list, info)
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	s.pairs, s.list, s.scanned = pairs, list, time.Now()
	return nil
}

func (s *CertStore) refresh() error {
	if s.pairs != nil && time.Since(s.scanned) < s.ttl() {
		return nil
	}
	return s.scan()
}

// Manifest lists the certificates available to relays.
func (s *CertStore) Manifest() (*relaylink.CertManifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refresh(); err != nil {
		return nil, err
	}
	m := &relaylink.CertManifest{Certs: append([]relaylink.CertInfo(nil), s.list...)}
	if _, ok := s.pairs[s.Default]; ok {
		m.Default = s.Default
	}
	for _, r := range s.Routes {
		if _, ok := s.pairs[r.Cert]; ok { // never advertise a route to a cert that is not served
			m.Routes = append(m.Routes, r)
		}
	}
	return m, nil
}

// Pair returns one certificate and key by manifest name.
func (s *CertStore) Pair(name string) (*relaylink.CertPair, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refresh(); err != nil {
		return nil, false
	}
	p, ok := s.pairs[name]
	if !ok {
		return nil, false
	}
	cp := p.pair
	return &cp, true
}

// DefaultCertRoutes reproduces account-proxy's getCert() for its SNI-routed
// listener (nginx :443 -> 7443): explicit names first, then the wildcard
// suffixes, and the default certificate for everything else. Keep it in sync
// with startOLVProxy in account-proxy/main.go (see the comment there about the
// 2026-08-23 Wii U 116-1097 regression before reordering anything).
func DefaultCertRoutes() []relaylink.CertRoute {
	exact := func(sni, cert string) relaylink.CertRoute {
		return relaylink.CertRoute{Match: "exact", Value: sni, Cert: cert}
	}
	suffix := func(s, cert string) relaylink.CertRoute {
		return relaylink.CertRoute{Match: "suffix", Value: s, Cert: cert}
	}
	return []relaylink.CertRoute{
		exact("discovery.olv.nintendo.net", "discovery-olv-nintendo-net"),
		exact("api.olv.nintendo.net", "api-olv-nintendo-net"),
		exact("discovery.olv.pretendo.cc", "discovery-olv-pretendo-cc"),
		exact("boss.nicochristmann.net", "boss-nicochristmann-net"),
		exact("olv3ds.nicochristmann.net", "3ds/olv3ds-nicochristmann-net"),
		exact("ctr.olv.nicochristmann.net", "3ds/ctr-olv-nicochristmann-net"),
		exact("hpp-relay.nicochristmann.net", "hpp-relay-nicochristmann-net"),
		exact("npdl.cdn.pretendo.cc", "npdl-cdn-pretendo-cc"),
		exact("olv.nicochristmann.net", "olv-nicochristmann-net"),
		exact("portal.olv.nicochristmann.net", "olv-nicochristmann-net"),
		suffix(".pretendo.cc", "wildcard-pretendo-cc"),
		suffix(".nicoch.net", "wildcard-nicoch-net"),
		suffix(".nicochristmann.net", "wildcard-nicochristmann-net"),
	}
}
