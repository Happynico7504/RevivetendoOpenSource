package globals

import (
	"encoding/pem"
	"os"
	"sync"
)

var (
	nnCACertOnce sync.Once
	nnCACertDER  []byte
)

// NNCACertDER returns the DER-encoded bytes of the CA that signs
// hpp-001a2c00-l1.n.app.nicoch.net's certificate (the S3-upload relay host -
// see globals.RelayThroughHPPHost). Badge Arcade's own PrepareGetObject
// download succeeded with an empty RootCA field, but PreparePostObject/
// PrepareUpdateObject uploads never even attempted a network connection to
// the relay host (no TLS handshake, successful or failed, ever showed up in
// account-proxy's logs) - consistent with many real NEX titles requiring the
// RootCaCert field to be populated before their upload path will trust a
// non-default server at all, unlike the download path. Confirmed 2026-09-16.
func NNCACertDER() []byte {
	nnCACertOnce.Do(func() {
		data, err := os.ReadFile("/nico-pretendo-bridge/certs/nicochristmann-nn-ca.crt")
		if err != nil {
			Logger.Error("NNCACertDER: read: " + err.Error())
			return
		}
		block, _ := pem.Decode(data)
		if block == nil {
			Logger.Error("NNCACertDER: PEM decode failed")
			return
		}
		nnCACertDER = block.Bytes
	})
	return nnCACertDER
}
