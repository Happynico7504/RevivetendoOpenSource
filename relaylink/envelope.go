// Package relaylink is the encrypted, authenticated request channel between a
// regional relay and the authoritative main instance.
//
// Design (hybrid encryption, standard library only):
//
//	relay -> main: a fresh random AES-256 key encrypts the request with
//	AES-GCM; that key is wrapped to the main's RSA public key with RSA-OAEP
//	(SHA-256). The relay also signs the request with its own Ed25519 key, so
//	the main knows WHICH relay is calling (the public key alone authenticates
//	nobody) and can revoke a single relay.
//
//	main -> relay: the response is encrypted with the SAME AES key under a
//	fresh nonce. Only the holder of the RSA private key can recover that key,
//	so a response that decrypts is proof it came from the main.
//
// Method, path and body all travel inside the ciphertext; the outer HTTP
// request is always POST /relay/v1/rpc. Replay protection: every request
// carries a timestamp and a random nonce (both signed and encrypted); the main
// rejects stale timestamps and nonces it has already seen.
//
// Not provided: forward secrecy. If the main's RSA private key leaks, recorded
// traffic can be decrypted. (Use WireGuard underneath, or move to an ephemeral
// key exchange such as HPKE, if that matters.)
package relaylink

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	envelopeVersion = 1
	oaepLabel       = "relaylink-v1-key"
	// MaxClockSkew is how far a request timestamp may differ from the main's clock.
	MaxClockSkew = 60 * time.Second
	// RPCPath is the only HTTP path the transport uses.
	RPCPath = "/relay/v1/rpc"
	// MaxBodyBytes bounds a single envelope on the wire.
	MaxBodyBytes = 16 << 20
)

var (
	ErrBadEnvelope  = errors.New("relaylink: bad envelope")
	ErrUnknownRelay = errors.New("relaylink: unknown or revoked relay")
	ErrBadSignature = errors.New("relaylink: bad relay signature")
	ErrExpired      = errors.New("relaylink: request timestamp outside allowed skew")
	ErrReplay       = errors.New("relaylink: replayed request")
	ErrBadResponse  = errors.New("relaylink: bad response")
)

// Envelope is the JSON body of the outer HTTP request. []byte fields are
// base64 on the wire.
type Envelope struct {
	V          int    `json:"v"`
	RelayID    string `json:"relay_id"`
	WrappedKey []byte `json:"k"`
	Nonce      []byte `json:"n"`
	Ciphertext []byte `json:"c"`
}

type plainRequest struct {
	TS       int64  `json:"ts"` // unix milliseconds
	ReqNonce []byte `json:"rn"`
	Method   string `json:"m"`
	Path     string `json:"p"`
	Body     []byte `json:"b"`
	Sig      []byte `json:"s"`
}

// Request is a verified, decrypted relay request.
type Request struct {
	RelayID string
	Method  string
	Path    string
	Body    []byte
	Ctx     context.Context // set by Handler; the outer HTTP request's context
}

func reqAAD(relayID string) []byte { return []byte("req|" + relayID) }
func respAAD(reqNonce []byte) []byte {
	return append([]byte("resp|"), reqNonce...)
}

func sigMessage(relayID string, ts int64, reqNonce []byte, method, path string, body []byte) []byte {
	h := sha256.Sum256(body)
	var b bytes.Buffer
	b.WriteString("relaylink-v1-sig\x00")
	b.WriteString(relayID)
	b.WriteByte(0)
	var t [8]byte
	binary.BigEndian.PutUint64(t[:], uint64(ts))
	b.Write(t[:])
	b.Write(reqNonce)
	b.WriteString(method)
	b.WriteByte(0)
	b.WriteString(path)
	b.WriteByte(0)
	b.Write(h[:])
	return b.Bytes()
}

func newGCM(key []byte) (cipher.AEAD, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(blk)
}

// ---- relay side ----------------------------------------------------------

// Client is a relay's handle for calling the main.
type Client struct {
	RelayID string
	MainPub *rsa.PublicKey     // the main's RSA public key
	Sign    ed25519.PrivateKey // this relay's signing key
	BaseURL string             // e.g. "http://main.example:7777"
	HTTP    *http.Client
	Now     func() time.Time // test hook
}

// Pending is the state needed to open the response to one sealed request.
type Pending struct {
	key      []byte
	reqNonce []byte
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Seal encrypts and signs one request.
func (c *Client) Seal(method, path string, body []byte) (*Envelope, *Pending, error) {
	key := make([]byte, 32)
	reqNonce := make([]byte, 16)
	gcmNonce := make([]byte, 12)
	for _, b := range [][]byte{key, reqNonce, gcmNonce} {
		if _, err := io.ReadFull(rand.Reader, b); err != nil {
			return nil, nil, err
		}
	}
	ts := c.now().UnixMilli()
	pr := plainRequest{
		TS: ts, ReqNonce: reqNonce, Method: method, Path: path, Body: body,
		Sig: ed25519.Sign(c.Sign, sigMessage(c.RelayID, ts, reqNonce, method, path, body)),
	}
	plain, err := json.Marshal(pr)
	if err != nil {
		return nil, nil, err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, nil, err
	}
	ct := gcm.Seal(nil, gcmNonce, plain, reqAAD(c.RelayID))
	wrapped, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, c.MainPub, key, []byte(oaepLabel))
	if err != nil {
		return nil, nil, err
	}
	return &Envelope{V: envelopeVersion, RelayID: c.RelayID, WrappedKey: wrapped, Nonce: gcmNonce, Ciphertext: ct},
		&Pending{key: key, reqNonce: reqNonce}, nil
}

// OpenResponse decrypts the main's response (nonce || ciphertext).
func (p *Pending) OpenResponse(resp []byte) (int, []byte, error) {
	if len(resp) < 12+16 {
		return 0, nil, ErrBadResponse
	}
	gcm, err := newGCM(p.key)
	if err != nil {
		return 0, nil, err
	}
	plain, err := gcm.Open(nil, resp[:12], resp[12:], respAAD(p.reqNonce))
	if err != nil || len(plain) < 2 {
		return 0, nil, ErrBadResponse
	}
	return int(binary.BigEndian.Uint16(plain[:2])), plain[2:], nil
}

// Do seals a request, POSTs it to the main and returns the inner status/body.
func (c *Client) Do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	env, pending, err := c.Seal(method, path, body)
	if err != nil {
		return 0, nil, err
	}
	raw, _ := json.Marshal(env)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+RPCPath, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes))
	if err != nil {
		return 0, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		// Transport-level failure (bad envelope, unknown relay, replay...).
		// Deliberately opaque: the main does not say why.
		return 0, nil, fmt.Errorf("relaylink: transport rejected (HTTP %d)", resp.StatusCode)
	}
	return pending.OpenResponse(data)
}

// ---- main side -----------------------------------------------------------

// ReplayStore records request nonces so a captured request cannot be replayed.
// Seen returns true if (relayID, nonce) was already recorded, and records it
// for ttl otherwise. The main should back this with Redis (SET NX EX).
type ReplayStore interface {
	Seen(ctx context.Context, relayID string, nonce []byte, ttl time.Duration) (bool, error)
}

// Server verifies and opens relay requests on the main.
type Server struct {
	Priv     *rsa.PrivateKey
	RelayKey func(relayID string) (ed25519.PublicKey, bool) // false = unknown/revoked
	Replay   ReplayStore
	Now      func() time.Time // test hook
}

// Reply seals the response to one opened request.
type Reply struct {
	key      []byte
	reqNonce []byte
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Open decrypts and verifies an envelope. Every failure returns an error
// without saying which check failed to the caller's peer (the HTTP handler maps
// them all to the same response).
func (s *Server) Open(ctx context.Context, env *Envelope) (*Request, *Reply, error) {
	if env == nil || env.V != envelopeVersion || env.RelayID == "" ||
		len(env.Nonce) != 12 || len(env.WrappedKey) == 0 || len(env.Ciphertext) == 0 {
		return nil, nil, ErrBadEnvelope
	}
	pub, ok := s.RelayKey(env.RelayID)
	if !ok {
		return nil, nil, ErrUnknownRelay
	}
	key, err := rsa.DecryptOAEP(sha256.New(), nil, s.Priv, env.WrappedKey, []byte(oaepLabel))
	if err != nil || len(key) != 32 {
		return nil, nil, ErrBadEnvelope
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, nil, ErrBadEnvelope
	}
	plain, err := gcm.Open(nil, env.Nonce, env.Ciphertext, reqAAD(env.RelayID))
	if err != nil {
		return nil, nil, ErrBadEnvelope
	}
	var pr plainRequest
	if json.Unmarshal(plain, &pr) != nil || len(pr.ReqNonce) != 16 {
		return nil, nil, ErrBadEnvelope
	}
	if !ed25519.Verify(pub, sigMessage(env.RelayID, pr.TS, pr.ReqNonce, pr.Method, pr.Path, pr.Body), pr.Sig) {
		return nil, nil, ErrBadSignature
	}
	skew := s.now().Sub(time.UnixMilli(pr.TS))
	if skew > MaxClockSkew || skew < -MaxClockSkew {
		return nil, nil, ErrExpired
	}
	// Nonces only need remembering for as long as their timestamp is valid.
	seen, err := s.Replay.Seen(ctx, env.RelayID, pr.ReqNonce, 2*MaxClockSkew)
	if err != nil {
		return nil, nil, err // fail closed if the replay store is down
	}
	if seen {
		return nil, nil, ErrReplay
	}
	return &Request{RelayID: env.RelayID, Method: pr.Method, Path: pr.Path, Body: pr.Body},
		&Reply{key: key, reqNonce: pr.ReqNonce}, nil
}

// Seal encrypts a response for the relay that sent the request.
func (r *Reply) Seal(status int, body []byte) ([]byte, error) {
	nonce := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	gcm, err := newGCM(r.key)
	if err != nil {
		return nil, err
	}
	plain := make([]byte, 2+len(body))
	binary.BigEndian.PutUint16(plain[:2], uint16(status))
	copy(plain[2:], body)
	return append(nonce, gcm.Seal(nil, nonce, plain, respAAD(r.reqNonce))...), nil
}

// Handler serves POST /relay/v1/rpc. dispatch receives the verified request
// and returns the inner status and body.
func (s *Server) Handler(dispatch func(*Request) (int, []byte)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != RPCPath {
			http.NotFound(w, r)
			return
		}
		var env Envelope
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBodyBytes)).Decode(&env); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		req, reply, err := s.Open(r.Context(), &env)
		if err != nil {
			// One opaque answer for every failure (no oracle for an attacker).
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		req.Ctx = r.Context()
		status, body := dispatch(req)
		out, err := reply.Seal(status, body)
		if err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(out)
	})
}
