package relaylink

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// GenerateMainKey creates the main's RSA key pair (3072 bits recommended).
func GenerateMainKey(bits int) (*rsa.PrivateKey, error) {
	if bits < 2048 {
		return nil, errors.New("relaylink: RSA key must be at least 2048 bits")
	}
	return rsa.GenerateKey(rand.Reader, bits)
}

func MarshalPrivatePEM(k *rsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func ParsePrivatePEM(b []byte) (*rsa.PrivateKey, error) {
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("relaylink: no PEM block in private key")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("relaylink: private key is not RSA")
	}
	return rk, nil
}

func MarshalPublicPEM(k *rsa.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(k)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

func ParsePublicPEM(b []byte) (*rsa.PublicKey, error) {
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("relaylink: no PEM block in public key")
	}
	k, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("relaylink: public key is not RSA")
	}
	return rk, nil
}

// RelayBundle is everything one relay needs to talk to the main. It contains
// the relay's PRIVATE signing key: treat the file like a password and copy it
// to the relay over a secure channel.
type RelayBundle struct {
	RelayID       string `json:"relay_id"`
	SigningKey    string `json:"signing_key"`     // base64 Ed25519 private key
	MainPublicKey string `json:"main_public_key"` // PEM
	MainURL       string `json:"main_url"`        // e.g. http://main.example:7777
	// ReleasePublicKey pins the key OTA updates must be signed with (base64
	// Ed25519). Empty disables over-the-air updates on that relay.
	ReleasePublicKey string `json:"release_public_key,omitempty"`
}

// NewRelayIdentity creates a fresh signing key pair for a relay.
func NewRelayIdentity() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

func NewBundle(relayID string, priv ed25519.PrivateKey, mainPub *rsa.PublicKey, mainURL string) (*RelayBundle, error) {
	pemPub, err := MarshalPublicPEM(mainPub)
	if err != nil {
		return nil, err
	}
	return &RelayBundle{
		RelayID:       relayID,
		SigningKey:    base64.StdEncoding.EncodeToString(priv),
		MainPublicKey: string(pemPub),
		MainURL:       mainURL,
	}, nil
}

func ParseBundle(raw []byte) (*RelayBundle, error) {
	var b RelayBundle
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	if b.RelayID == "" || b.SigningKey == "" || b.MainPublicKey == "" || b.MainURL == "" {
		return nil, errors.New("relaylink: incomplete relay bundle")
	}
	return &b, nil
}

// Client builds the relay's Client from the bundle.
func (b *RelayBundle) Client() (*Client, error) {
	sk, err := base64.StdEncoding.DecodeString(b.SigningKey)
	if err != nil || len(sk) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("relaylink: bad signing key in bundle")
	}
	pub, err := ParsePublicPEM([]byte(b.MainPublicKey))
	if err != nil {
		return nil, err
	}
	return &Client{
		RelayID: b.RelayID, MainPub: pub, Sign: ed25519.PrivateKey(sk), BaseURL: b.MainURL,
		HTTP: &http.Client{Timeout: 30 * time.Second},
	}, nil
}
