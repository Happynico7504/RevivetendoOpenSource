package main

// 3DS HOME Menu notifications ("SpotPass notifications" from the notification
// module, title 0004013000003502), built from n3ds_system_messages.
//
// Format recovered 2026-10-07 from Nintendo's own Swapdoodle ring files
// (RNG_NT1/nt1, RNG_NT2/nt2), decrypted with the 3DS BOSS key: a 3DS BOSS file
// can carry payloads for several titles, and a payload addressed to the
// notification module (ContentDataType 0x00020001) becomes a HOME Menu
// notification. Its content:
//
//	0x00   flags: 01 01 <has image> 01, then 4 zero bytes
//	0x08   u64 title ID the notification can launch (0 = none)
//	0x20   title, UTF-16LE, 0x40 bytes (31 characters + terminator)
//	0x60   message, UTF-16LE, up to 0x17E0
//	0x17E0 optional JPEG (not used here)
//
// The 3DS never asks us for a generic system-message channel (the old
// sysmsg-tasksheet guess was never requested), so the notifications ride along
// in files of tasks consoles do poll: Swapdoodle's RNG_NT1 (the path Nintendo
// itself used) and NZOffPg.

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/Happynico7504/badgearcade"
)

const (
	n3dsNewsTitleID         = 0x0004013000003502
	n3dsNewsContentDataType = 0x00020001
	n3dsNewsContentSize     = 0x17E0
	// n3dsNewsNsDataIDBase + message id identifies a notification to the
	// console, so each message is shown once (Nintendo's own were in the 39000s).
	n3dsNewsNsDataIDBase = 0x00A00000
)

func putUTF16(dst []byte, s string) {
	for i, c := range utf16.Encode([]rune(s)) {
		if 2*i+2 > len(dst)-2 { // keep the terminator
			break
		}
		binary.LittleEndian.PutUint16(dst[2*i:], c)
	}
}

// buildN3DSNews encodes one notification (no image).
func buildN3DSNews(title, message string, launchTitleID uint64) []byte {
	b := make([]byte, n3dsNewsContentSize)
	copy(b, []byte{0x01, 0x01, 0x00, 0x01})
	binary.LittleEndian.PutUint64(b[0x08:], launchTitleID)
	putUTF16(b[0x20:0x60], title)
	putUTF16(b[0x60:], strings.ReplaceAll(message, "\r\n", "\n"))
	return b
}

// n3dsNewsPayloads are the active 3DS system messages as notification payloads.
// title_id is the title a notification can launch; the old sysmsg placeholder
// IDs (000400300000a102/b102) mean none.
func n3dsNewsPayloads() []badgearcade.BOSSPayload {
	var out []badgearcade.BOSSPayload
	for _, m := range active3DSSysMsgs() {
		var launch uint64
		if !n3dsHomeMenuTitleIDs[strings.ToLower(m.titleID)] {
			launch, _ = strconv.ParseUint(m.titleID, 16, 64)
		}
		out = append(out, badgearcade.BOSSPayload{
			ProgramID:       n3dsNewsTitleID,
			ContentDataType: n3dsNewsContentDataType,
			NsDataID:        uint32(n3dsNewsNsDataIDBase + m.id),
			Version:         1,
			Content:         buildN3DSNews(m.subject, m.body, launch),
		})
	}
	return out
}

var n3dsNewsCache sync.Map // sha256(base file + payloads) -> []byte

// withN3DSNews returns base (a 3DS BOSS file) with the active notifications
// appended as extra payloads; base unchanged when there are none or anything
// fails. Re-encryption is cached per content.
func withN3DSNews(base []byte) []byte {
	news := n3dsNewsPayloads()
	if len(news) == 0 {
		return base
	}
	h := sha256.New()
	h.Write(base)
	for _, p := range news {
		fmt.Fprintf(h, "|%d|%x", p.NsDataID, p.Content)
	}
	key := string(h.Sum(nil))
	if v, ok := n3dsNewsCache.Load(key); ok {
		return v.([]byte)
	}
	k, err := loadBoss3DSKey()
	if err != nil {
		log.Printf("3ds news: %v", err)
		return base
	}
	serial, payloads, err := badgearcade.ParseBOSS(k, base)
	if err != nil {
		log.Printf("3ds news: base file: %v", err)
		return base
	}
	out, err := badgearcade.BuildBOSS(k, serial, append(payloads, news...), nil)
	if err != nil {
		log.Printf("3ds news: build: %v", err)
		return base
	}
	n3dsNewsCache.Store(key, out)
	return out
}

// n3dsNewsOnlyFile is a BOSS file carrying just the notifications, for tasks
// we have no content of our own for (NZOffPg); nil when there are none.
func n3dsNewsOnlyFile() []byte {
	news := n3dsNewsPayloads()
	if len(news) == 0 {
		return nil
	}
	h := sha256.New()
	for _, p := range news {
		fmt.Fprintf(h, "|%d|%x", p.NsDataID, p.Content)
	}
	key := "only" + string(h.Sum(nil))
	if v, ok := n3dsNewsCache.Load(key); ok {
		return v.([]byte)
	}
	k, err := loadBoss3DSKey()
	if err != nil {
		log.Printf("3ds news: %v", err)
		return nil
	}
	out, err := badgearcade.BuildBOSS(k, uint64(time.Now().Unix()), news, nil)
	if err != nil {
		log.Printf("3ds news: build: %v", err)
		return nil
	}
	n3dsNewsCache.Store(key, out)
	return out
}
