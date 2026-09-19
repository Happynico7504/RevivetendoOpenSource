package relaylink

// Forwarding: a relay terminates a console's TLS connection locally and sends
// the decrypted HTTP request to the main through the sealed channel; the main
// replays it against one of its own local backends and returns the result.

// ForwardPath is the API path for forwarded requests (POST).
const ForwardPath = "/relay/v1/forward"

// ForwardRequest is one HTTP request from a console. Backend names a target
// the MAIN has configured (relays cannot make the main connect anywhere else).
type ForwardRequest struct {
	Backend  string              `json:"backend"`
	SNI      string              `json:"sni,omitempty"` // TLS server name the console asked for
	Method   string              `json:"method"`
	Path     string              `json:"path"` // request URI, including query
	Headers  map[string][]string `json:"headers,omitempty"`
	Body     []byte              `json:"body,omitempty"`
	ClientIP string              `json:"client_ip"`
}

// ForwardResponse is the backend's answer, replayed to the console verbatim.
type ForwardResponse struct {
	Close   bool                `json:"close,omitempty"` // backend answered "Connection: close": the relay must too (WSC BOSS depends on it)
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    []byte              `json:"body,omitempty"`
}

// Certificate sync API (GET). Responses are never cacheable (TTL 0), so private
// keys are never placed in a relay's cache store.
const (
	CertManifestPath = "/relay/v1/certs/manifest"
	CertPairPrefix   = "/relay/v1/certs/pair/"
)

type CertInfo struct {
	Name     string   `json:"name"`
	SHA256   string   `json:"sha256"` // over cert PEM + key PEM: changes when either is replaced
	NotAfter int64    `json:"not_after"`
	DNSNames []string `json:"dns_names"`
}

// CertRoute says which certificate answers which TLS server name. Relays must
// NOT guess from the names inside certificates: several certificates can list
// the same name (olv-nicochristmann-net and the 3DS cert both list
// ctr.olv.nicochristmann.net) and the wrong pick breaks consoles that verify
// certificates. Routes are evaluated in order; the first match wins.
type CertRoute struct {
	Match string `json:"match"` // "exact" or "suffix" (suffix includes the leading dot)
	Value string `json:"value"`
	Cert  string `json:"cert"`
}

type CertManifest struct {
	Certs   []CertInfo  `json:"certs"`
	Routes  []CertRoute `json:"routes,omitempty"`
	Default string      `json:"default,omitempty"` // served when no route matches (or the SNI is empty)
}

// SelectCert applies the routes to a TLS server name and returns the
// certificate name to serve.
func (m *CertManifest) SelectCert(sni string) string {
	sni = lowerASCII(sni)
	for _, r := range m.Routes {
		switch r.Match {
		case "exact":
			if sni == r.Value {
				return r.Cert
			}
		case "suffix":
			if len(sni) > len(r.Value) && sni[len(sni)-len(r.Value):] == r.Value {
				return r.Cert
			}
		}
	}
	return m.Default
}

func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

type CertPair struct {
	Name    string `json:"name"`
	CertPEM string `json:"cert_pem"`
	KeyPEM  string `json:"key_pem"`
}
