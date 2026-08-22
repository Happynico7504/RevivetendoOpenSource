package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/joho/godotenv"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func connectTestMongo(t *testing.T) {
	t.Helper()
	if wscMongoDB != nil {
		return
	}
	client, err := mongo.NewClient(options.Client().ApplyURI("mongodb://localhost:27017/"))
	if err != nil {
		t.Fatalf("mongo client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	wscMongoDB = client.Database("wsc")
}

func connectTestPostgres(t *testing.T) {
	t.Helper()
	if db != nil {
		return
	}
	godotenv.Load("../wiiu-chat-secure/.env")
	var err error
	db, err = sql.Open("postgres", os.Getenv("PN_WUC_POSTGRES_URI"))
	if err != nil {
		t.Fatalf("postgres open: %v", err)
	}
}

// decryptBossFileForTest mirrors encryptBossFile's format in reverse - used
// only to verify our own generated files round-trip correctly.
func decryptBossFileForTest(t *testing.T, data []byte) []byte {
	t.Helper()
	if len(data) < 32 || string(data[0:4]) != "boss" {
		t.Fatalf("not a boss file")
	}
	iv := data[12:24]
	ctrIV := append(append([]byte{}, iv...), 0x00, 0x00, 0x00, 0x01)

	aesKey, _, err := loadBossKeys()
	if err != nil {
		t.Fatalf("loadBossKeys: %v", err)
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	stream := cipher.NewCTR(block, ctrIV)
	plaintext := make([]byte, len(data)-32)
	stream.XORKeyStream(plaintext, data[32:])
	return plaintext
}

func TestRankingTemplateSlots(t *testing.T) {
	slots := rankingTemplateSlots()
	if len(slots) == 0 {
		t.Fatal("expected at least one slot in the real template")
	}
	for i, slot := range slots {
		for j, start := range slot {
			if start < 0 || start+rankingSubRecordSize > len(rankingDataTemplate) {
				t.Fatalf("slot %d sub-record %d: start %d out of bounds", i, j, start)
			}
			marker := rankingDataTemplate[start+2 : start+6]
			if !bytes.Equal(marker, rankingSlotMarker) {
				t.Fatalf("slot %d sub-record %d: expected marker at +2, got %x", i, j, marker)
			}
		}
	}
}

func TestPatchMiiNameRoundTrip(t *testing.T) {
	sub := make([]byte, rankingSubRecordSize)
	copy(sub, rankingDataTemplate[:rankingSubRecordSize])

	patchMiiName(sub, "Nico")

	name := sub[rankingNameOffset : rankingNameOffset+rankingNameMaxRunes*2]
	want := []byte{0x00, 'N', 0x00, 'i', 0x00, 'c', 0x00, 'o', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if !bytes.Equal(name, want) {
		t.Fatalf("patched name field = %x, want %x", name, want)
	}

	// Bytes outside the fixed 20-byte name slot must be untouched.
	if !bytes.Equal(sub[:rankingNameOffset], rankingDataTemplate[:rankingNameOffset]) {
		t.Fatal("patchMiiName modified bytes before the name field")
	}
	after := rankingNameOffset + rankingNameMaxRunes*2
	if !bytes.Equal(sub[after:], rankingDataTemplate[after:rankingSubRecordSize]) {
		t.Fatal("patchMiiName modified bytes after the name field")
	}
}

// TestGenerateRankingDataEncryptDecryptRoundTrip confirms a patched template,
// once run through encryptBossFile, decrypts back byte-for-byte with our own
// boss_keys.bin — the same real-key HMAC verification used to confirm the
// captured Nintendo sample, applied to our own generated output.
func TestGenerateRankingDataEncryptDecryptRoundTrip(t *testing.T) {
	content := make([]byte, len(rankingDataTemplate))
	copy(content, rankingDataTemplate)
	slots := rankingTemplateSlots()
	patchMiiName(content[slots[0][0]:slots[0][0]+rankingSubRecordSize], "TestName")

	encrypted, err := encryptBossFile(content)
	if err != nil {
		t.Fatalf("encryptBossFile: %v", err)
	}
	if !bytes.Equal(encrypted[0:4], []byte("boss")) {
		t.Fatalf("missing boss magic header")
	}

	_, hmacKey, err := loadBossKeys()
	if err != nil {
		t.Fatalf("loadBossKeys: %v", err)
	}

	decrypted := decryptBossFileForTest(t, encrypted)
	mac := hmac.New(sha256.New, hmacKey)
	mac.Write(decrypted[32:])
	if !hmac.Equal(mac.Sum(nil), decrypted[0:32]) {
		t.Fatal("HMAC mismatch after round-trip decrypt")
	}
	if !bytes.Equal(decrypted[32:], content) {
		t.Fatal("decrypted content does not match original generated content")
	}
}

// TestGenerateRankingDataRealClub confirms generateRankingData finds and
// patches real players for Nico's own confirmed club (033=GER Hesse, region
// 3=EU) against the live wsc-secure database.
func TestGenerateRankingDataRealClub(t *testing.T) {
	connectTestMongo(t)
	connectTestPostgres(t)

	content, err := generateRankingData(33, 3)
	if err != nil {
		t.Fatalf("generateRankingData: %v", err)
	}

	players, err := topRankedPlayers(33, 3, 8)
	if err != nil {
		t.Fatalf("topRankedPlayers: %v", err)
	}
	t.Logf("found %d ranked player(s) for club=33 region=3: %v", len(players), players)
	if len(players) == 0 {
		t.Skip("no ranking_scores rows for club=33 region=3 right now - nothing to verify patching against")
	}

	slots := rankingTemplateSlots()
	diffs := 0
	for i := range rankingDataTemplate {
		if content[i] != rankingDataTemplate[i] {
			diffs++
		}
	}
	t.Logf("bytes patched vs template: %d", diffs)
	if diffs == 0 {
		t.Fatal("expected at least some bytes to differ from the template once real players were found")
	}

	name := miiNameForPID(players[0])
	t.Logf("top player PID=%d name=%q", players[0], name)
	if name != "" {
		firstSlot := slots[0][0]
		got := content[firstSlot+rankingNameOffset : firstSlot+rankingNameOffset+rankingNameMaxRunes*2]
		t.Logf("patched name field bytes: %x", got)
	}
}
