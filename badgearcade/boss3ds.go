package badgearcade

// 3DS BOSS (SpotPass) container, ported from @pretendonetwork/boss-crypto's
// 3ds.ts. AES-128-CTR with the console's slot 0x38 normal key; the IV is a
// random 12-byte nonce plus a 32-bit counter starting at 1. Both RSA signature
// blocks are left zeroed - confirmed 2026-10-05 that a real 3DS accepts that
// (it only checks the SHA-256 hashes).

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

const (
	bossHeaderSize        = 40
	bossContentHeaderSize = 306
	bossPayloadHeaderSize = 316
)

var bossKeyHash, _ = hex.DecodeString("86fbc2bb4cb703b2a4c6cc9961319926")

// BOSSPayload is one payload of a 3DS BOSS file.
type BOSSPayload struct {
	ProgramID       uint64
	ContentDataType uint32
	NsDataID        uint32
	Version         uint32
	Content         []byte
}

// LoadBOSSKey reads the hex-encoded 3DS BOSS AES key (slot 0x38 normal key)
// from path and checks it against the known key's MD5. The key is
// console-derived, so callers keep it outside the repo.
func LoadBOSSKey(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, err
	}
	if sum := md5.Sum(key); !bytes.Equal(sum[:], bossKeyHash) {
		return nil, errors.New("badgearcade: key does not match the known 3DS BOSS key hash")
	}
	return key, nil
}

func bossStream(key, nonce []byte) (cipher.Stream, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	iv := append(append([]byte{}, nonce...), 0, 0, 0, 1)
	return cipher.NewCTR(block, iv), nil
}

func bossHash(header, content []byte) []byte {
	h := sha256.New()
	h.Write(header)
	h.Write([]byte{0, 0})
	h.Write(content)
	return h.Sum(nil)
}

// ParseBOSS decrypts a 3DS BOSS file and verifies its SHA-256 hashes.
func ParseBOSS(key, data []byte) (serial uint64, payloads []BOSSPayload, err error) {
	if len(data) < bossHeaderSize+bossContentHeaderSize || string(data[:4]) != "boss" {
		return 0, nil, errors.New("badgearcade: not a BOSS file")
	}
	if binary.BigEndian.Uint16(data[24:]) != 2 {
		return 0, nil, errors.New("badgearcade: unknown hash type")
	}
	serial = binary.BigEndian.Uint64(data[12:])
	stream, err := bossStream(key, data[28:40])
	if err != nil {
		return 0, nil, err
	}
	pt := make([]byte, len(data)-bossHeaderSize)
	stream.XORKeyStream(pt, data[bossHeaderSize:])

	ch := pt[:bossContentHeaderSize]
	if !bytes.Equal(bossHash(ch[:18], nil), ch[18:50]) {
		return 0, nil, errors.New("badgearcade: content header hash mismatch (wrong key?)")
	}
	count := int(binary.BigEndian.Uint16(ch[16:]))
	off := bossContentHeaderSize
	for i := 0; i < count; i++ {
		if off+bossPayloadHeaderSize > len(pt) {
			return 0, nil, errors.New("badgearcade: truncated payload header")
		}
		ph := pt[off : off+bossPayloadHeaderSize]
		length := int(binary.BigEndian.Uint32(ph[16:]))
		start := off + bossPayloadHeaderSize
		if start+length > len(pt) {
			return 0, nil, errors.New("badgearcade: truncated payload")
		}
		content := pt[start : start+length]
		if !bytes.Equal(bossHash(ph[:28], content), ph[28:60]) {
			return 0, nil, fmt.Errorf("badgearcade: payload %d hash mismatch", i)
		}
		payloads = append(payloads, BOSSPayload{
			ProgramID:       binary.BigEndian.Uint64(ph[0:]),
			ContentDataType: binary.BigEndian.Uint32(ph[12:]),
			NsDataID:        binary.BigEndian.Uint32(ph[20:]),
			Version:         binary.BigEndian.Uint32(ph[24:]),
			Content:         content,
		})
		off = start + length
	}
	return serial, payloads, nil
}

// BuildBOSS encrypts payloads into a 3DS BOSS file. It always sets content
// header byte 0 to 0x80 (boss-crypto's default, "not privileged"); Nintendo's
// Badge Arcade files have 0x00 there, but 0x80 is what was confirmed working on
// real hardware. nonce may be nil for a random one.
func BuildBOSS(key []byte, serial uint64, payloads []BOSSPayload, nonce []byte) ([]byte, error) {
	var body bytes.Buffer
	for _, p := range payloads {
		ph := make([]byte, 28)
		binary.BigEndian.PutUint64(ph[0:], p.ProgramID)
		binary.BigEndian.PutUint32(ph[12:], p.ContentDataType)
		binary.BigEndian.PutUint32(ph[16:], uint32(len(p.Content)))
		binary.BigEndian.PutUint32(ph[20:], p.NsDataID)
		binary.BigEndian.PutUint32(ph[24:], p.Version)
		body.Write(ph)
		body.Write(bossHash(ph, p.Content))
		body.Write(make([]byte, 256)) // RSA signature: not checked by the console
		body.Write(p.Content)
	}
	ch := make([]byte, 18)
	ch[0] = 0x80
	binary.BigEndian.PutUint16(ch[16:], uint16(len(payloads)))
	container := append(append(append(ch, bossHash(ch, nil)...), make([]byte, 256)...), body.Bytes()...)

	if nonce == nil {
		nonce = make([]byte, 12)
		if _, err := rand.Read(nonce); err != nil {
			return nil, err
		}
	}
	header := make([]byte, bossHeaderSize)
	copy(header, "boss")
	binary.BigEndian.PutUint32(header[4:], 0x10001)
	binary.BigEndian.PutUint32(header[8:], uint32(bossHeaderSize+len(container)))
	binary.BigEndian.PutUint64(header[12:], serial)
	binary.BigEndian.PutUint16(header[20:], 1)
	binary.BigEndian.PutUint16(header[24:], 2)
	binary.BigEndian.PutUint16(header[26:], 2)
	copy(header[28:], nonce)

	stream, err := bossStream(key, nonce)
	if err != nil {
		return nil, err
	}
	out := make([]byte, bossHeaderSize+len(container))
	copy(out, header)
	stream.XORKeyStream(out[bossHeaderSize:], container)
	return out, nil
}
