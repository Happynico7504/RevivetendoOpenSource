package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Wii Karaoke U by JOYSOUND (wiikaraoke-server/). The game sends this service token to its
// JOYSOUND HTTP server, which reads the PID out of it to find the player's tickets.
const (
	wiiKaraokeGameServerID = "10100D01"
	wiiKaraokeClientID     = "471ac6f9b1368cba77a15da69d5b0241"
	// The console may hold on to a service token, and the server refuses expired ones.
	wiiKaraokeTokenLifetime = 7 * 24 * time.Hour
)

var wiiKaraokeTokenKey = loadWiiKaraokeTokenKey()

// A single-service restart keeps the environment start.sh had when the bridge started, so also
// read the key from the file start.sh sources it from.
func loadWiiKaraokeTokenKey() string {
	if key := os.Getenv("WAHP_SERVICE_TOKEN_KEY"); key != "" {
		return key
	}
	data, err := os.ReadFile("/nico-pretendo-bridge/wiikaraoke-server/.env")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if key, ok := strings.CutPrefix(line, "WAHP_SERVICE_TOKEN_KEY="); ok {
			return strings.TrimSpace(key)
		}
	}
	return ""
}

// generateWiiKaraokeServiceToken builds the token wiikaraoke-server/wahp/identity.py expects:
// base64(u32 pid | u64 title_id | u64 issued_ms | u64 expires_ms | HMAC-SHA256), big endian.
func generateWiiKaraokeServiceToken(pid uint32, r *http.Request) string {
	titleID, _ := strconv.ParseUint(r.Header.Get("X-Nintendo-Title-ID"), 16, 64)
	now := time.Now()
	data := make([]byte, 28)
	binary.BigEndian.PutUint32(data[0:4], pid)
	binary.BigEndian.PutUint64(data[4:12], titleID)
	binary.BigEndian.PutUint64(data[12:20], uint64(now.UnixMilli()))
	binary.BigEndian.PutUint64(data[20:28], uint64(now.Add(wiiKaraokeTokenLifetime).UnixMilli()))
	mac := hmac.New(sha256.New, []byte(wiiKaraokeTokenKey))
	mac.Write(data)
	return base64.StdEncoding.EncodeToString(mac.Sum(data))
}

// handleWiiKaraokeNexToken: the server checks passwords via account-grpc GetNEXPassword, which
// prefers friends_nex_password, so the token must carry exactly that value. Always the main
// server: there is no Wii Karaoke server on the regional relays.
func handleWiiKaraokeNexToken(w http.ResponseWriter, r *http.Request, host string, port uint16) {
	ip := realIP(r)
	pid := fetchRealPID(r)

	if pid != 0 && checkBanned(pid) {
		log.Printf("WiiKaraoke: PID=%d is banned, proxying to Pretendo", pid)
		proxyAndCachePID(w, r)
		return
	}

	var password string
	if pid != 0 {
		db.QueryRow(`SELECT COALESCE(NULLIF(friends_nex_password, ''), nex_password, '') FROM nex_accounts WHERE pid = $1`, pid).Scan(&password)
	}
	if password == "" {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			http.Error(w, "token error", 500)
			return
		}
		password = fmt.Sprintf("%x", b)
		if pid != 0 {
			if _, err := db.Exec(`
				INSERT INTO nex_accounts (pid, username, nex_password)
				VALUES ($1, $2, $3)
				ON CONFLICT (pid) DO UPDATE SET nex_password = EXCLUDED.nex_password
			`, pid, fmt.Sprintf("%d", pid), password); err != nil {
				log.Printf("upsert nex_account: %v", err)
			}
		}
	}
	if pid != 0 {
		db.Exec(`INSERT INTO relay_requests (pid, game_server_id) VALUES ($1, $2)`, pid, wiiKaraokeGameServerID)
	}
	log.Printf("wii_karaoke_token for %s: PID=%d -> %s:%d", ip, pid, host, port)

	body, err := xml.MarshalIndent(nexToken{
		Host:        host,
		NexPassword: password,
		PID:         pid,
		Port:        port,
		Token:       password,
	}, "", "  ")
	if err != nil {
		http.Error(w, "encode error", 500)
		return
	}
	body = append([]byte(xml.Header), body...)
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}
