package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"bytes"
	_ "embed"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
	"github.com/pires/go-proxyproto"
	minio "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// pidCache maps remote IP → Pretendo PID, populated from any passing nex_token response.
var pidCache sync.Map

// redirectCache maps uppercased game_server_id → activeRedirect, refreshed from DB every 30s.
var redirectCache sync.Map

// bannedPIDs is a set of globally banned PIDs, refreshed every 30s alongside redirects.
var bannedPIDs sync.Map

type activeRedirect struct {
	ToHost     string
	Port       uint16
	AccessMode string
}

var mongoDB *mongo.Database
var wscMongoDB *mongo.Database

type nexToken struct {
	XMLName     xml.Name `xml:"nex_token"`
	Host        string   `xml:"host"`
	NexPassword string   `xml:"nex_password"`
	PID         uint32   `xml:"pid"`
	Port        uint16   `xml:"port"`
	Token       string   `xml:"token"`
}

type profilePersonMii struct {
	Name     string `xml:"name"`
	Data     string `xml:"data"`
	ImageURL string `xml:"mii_images>mii_image>cached_url"`
}

type profilePerson struct {
	XMLName xml.Name         `xml:"person"`
	PID     uint32           `xml:"pid"`
	PNID    string           `xml:"user_id"`
	Mii     profilePersonMii `xml:"mii"`
}

type serviceTokenResp struct {
	XMLName xml.Name `xml:"service_token"`
	Token   string   `xml:"token"`
}

const juxtAESKeyHex = "6014b79d9a50f090f4b7342d00d33a53cd97760ee3e50c35ad40ab7e01dbddcc"


var db *sql.DB

// swapdoodleDB is a separate connection to swapdoodle-server's own Postgres
// DB (not wiiuchat) - needed by handleNpdlCDN's RNG_EC1 stub to look up
// whether the requesting recipient has a real pending note. Same approach
// relay-admin already uses for its own swapdoodleDB.
var swapdoodleDB *sql.DB

// handshakeStaggerState/staggerHandshakePerIP are shared by every TLS
// listener that wants the same per-remote-IP handshake desync used by the
// OLV/HPP listener (port 7443, see is3DSSensitiveHost's doc comment for the
// original 2026-08-27 finding: a real 3DS's ssl:C module throwing genuine
// "tls: unexpected message" alerts when multiple handshakes to us land in
// the same second). Extracted to package scope 2026-08-27 so the ACT/account
// listener (port 6666) can reuse the exact same throttle - the 3DS's act
// module hits that listener too (see nimbus's account_url.s patch), and
// unlike the OLV listener there's no per-hostname SNI signal there to gate
// on (Wii U and 3DS both present the same "act.nicochristmann.net" SNI), so
// this applies unconditionally per remote IP rather than being 3DS-gated.
// Low risk: account/OAuth traffic on 6666 is not bursty the way Swap
// Doodle's HPP/BOSS calls are, so a real Wii U will essentially never see
// more than one connection per 300ms window from the same IP anyway.
var handshakeStaggerState = struct {
	mu   sync.Mutex
	next map[string]time.Time
}{next: make(map[string]time.Time)}

func staggerHandshakePerIP(remoteAddr net.Addr) {
	remote := "unknown"
	if remoteAddr != nil {
		if host, _, err := net.SplitHostPort(remoteAddr.String()); err == nil {
			remote = host
		}
	}
	const minGap = 300 * time.Millisecond
	handshakeStaggerState.mu.Lock()
	now := time.Now()
	next := handshakeStaggerState.next[remote]
	if next.Before(now) {
		next = now
	}
	wait := next.Sub(now)
	handshakeStaggerState.next[remote] = next.Add(minGap)
	handshakeStaggerState.mu.Unlock()
	if wait > 0 {
		time.Sleep(wait)
	}
}

func main() {
	godotenv.Load("../wiiu-chat-secure/.env")
	// Prefixed load (godotenv.Load does not override already-set vars) so
	// this can't collide with wiiu-chat-secure's own env - see
	// swapdoodleS3Client's doc comment for why account-proxy needs these.
	if env, err := godotenv.Read("/nico-pretendo-bridge/swapdoodle/.env"); err == nil {
		for _, k := range []string{"PN_SD_CONFIG_S3_ENDPOINT", "PN_SD_CONFIG_S3_ACCESS_KEY", "PN_SD_CONFIG_S3_ACCESS_SECRET", "PN_SD_CONFIG_S3_BUCKET", "PN_SD_POSTGRES_URI"} {
			if os.Getenv(k) == "" {
				os.Setenv(k, env[k])
			}
		}
	}
	if sdDB, err := sql.Open("postgres", os.Getenv("PN_SD_POSTGRES_URI")); err != nil {
		log.Printf("swapdoodle db: %v", err)
	} else {
		swapdoodleDB = sdDB
	}

	var err error
	db, err = sql.Open("postgres", os.Getenv("PN_WUC_POSTGRES_URI"))
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer db.Close()

	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS relay_requests (
		id             BIGSERIAL   PRIMARY KEY,
		pid            BIGINT      NOT NULL,
		game_server_id TEXT        NOT NULL,
		requested_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		log.Fatalf("schema: %v", err)
	}
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS pid_cache (
		auth_hash TEXT        PRIMARY KEY,
		pid       BIGINT      NOT NULL,
		cached_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		log.Fatalf("schema: %v", err)
	}
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS wiiu_system_messages (
		id             SERIAL      PRIMARY KEY,
		subject        TEXT        NOT NULL,
		body           TEXT        NOT NULL,
		title_id       TEXT        NOT NULL DEFAULT '000500101004d100',
		high_priority  BOOLEAN     NOT NULL DEFAULT false,
		active         BOOLEAN     NOT NULL DEFAULT true,
		region         TEXT        NOT NULL DEFAULT '',
		created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		log.Fatalf("schema: %v", err)
	}
	if _, err = db.Exec(`ALTER TABLE wiiu_system_messages ADD COLUMN IF NOT EXISTS region TEXT NOT NULL DEFAULT ''`); err != nil {
		log.Fatalf("schema: %v", err)
	}
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS n3ds_system_messages (
		id             SERIAL      PRIMARY KEY,
		subject        TEXT        NOT NULL,
		body           TEXT        NOT NULL,
		title_id       TEXT        NOT NULL DEFAULT '000400300000a102',
		high_priority  BOOLEAN     NOT NULL DEFAULT false,
		active         BOOLEAN     NOT NULL DEFAULT true,
		region         TEXT        NOT NULL DEFAULT '',
		created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		log.Fatalf("schema: %v", err)
	}
	if _, err = db.Exec(`ALTER TABLE n3ds_system_messages ADD COLUMN IF NOT EXISTS region TEXT NOT NULL DEFAULT ''`); err != nil {
		log.Fatalf("schema: %v", err)
	}
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS user_access (
		pid            BIGINT      NOT NULL,
		game_server_id TEXT        NOT NULL,
		note           TEXT,
		created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (pid, game_server_id)
	)`); err != nil {
		log.Fatalf("schema: %v", err)
	}
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS banned_users (
		pid            BIGINT      PRIMARY KEY,
		reason         TEXT,
		created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		log.Fatalf("schema: %v", err)
	}
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS review_queue (
		pid            BIGINT      NOT NULL,
		game_server_id TEXT        NOT NULL,
		first_seen     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		last_seen      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		attempt_count  INTEGER     NOT NULL DEFAULT 1,
		PRIMARY KEY (pid, game_server_id)
	)`); err != nil {
		log.Fatalf("schema: %v", err)
	}
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS pnid_cache (
		pid        BIGINT      PRIMARY KEY,
		pnid       TEXT        NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		log.Fatalf("schema: %v", err)
	}
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS wii_devices (
		username          TEXT        PRIMARY KEY,
		device_id         TEXT        NOT NULL DEFAULT '',
		serial            TEXT        NOT NULL DEFAULT '',
		device_cert       TEXT        NOT NULL DEFAULT '',
		pw_hash           TEXT        NOT NULL DEFAULT '',
		web_password_hash TEXT        NOT NULL DEFAULT '',
		updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		log.Fatalf("schema: %v", err)
	}
	db.Exec(`ALTER TABLE wii_devices ADD COLUMN IF NOT EXISTS pw_hash TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE wii_devices ADD COLUMN IF NOT EXISTS web_password_hash TEXT NOT NULL DEFAULT ''`)
	db.Exec(`CREATE TABLE IF NOT EXISTS web_logins (id BIGSERIAL PRIMARY KEY, pid BIGINT NOT NULL, ip TEXT NOT NULL, logged_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), success BOOLEAN NOT NULL)`)
	db.Exec(`ALTER TABLE redirects ADD COLUMN IF NOT EXISTS game_server_id TEXT`)
	db.Exec(`ALTER TABLE redirects ADD COLUMN IF NOT EXISTS port INTEGER`)
	db.Exec(`ALTER TABLE redirects ADD COLUMN IF NOT EXISTS access_mode TEXT NOT NULL DEFAULT 'whitelist'`)
	db.Exec(`ALTER TABLE nex_accounts ADD COLUMN IF NOT EXISTS friends_nex_password TEXT`)
	db.Exec(`CREATE TABLE IF NOT EXISTS pretendo_friends (
		owner_pid      BIGINT      NOT NULL,
		friend_pid     BIGINT      NOT NULL,
		friend_nnid    TEXT        NOT NULL DEFAULT '',
		mii_name       TEXT        NOT NULL DEFAULT '',
		mii_data       BYTEA,
		is_online      BOOLEAN     NOT NULL DEFAULT FALSE,
		game_server_id BIGINT      NOT NULL DEFAULT 0,
		befriended_at  TIMESTAMPTZ,
		last_online    TIMESTAMPTZ,
		updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (owner_pid, friend_pid)
	)`)
	db.Exec(`ALTER TABLE pretendo_friends ADD COLUMN IF NOT EXISTS is_online BOOLEAN NOT NULL DEFAULT FALSE`)
	db.Exec(`ALTER TABLE pretendo_friends ADD COLUMN IF NOT EXISTS game_server_id BIGINT NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE pretendo_friends ADD COLUMN IF NOT EXISTS befriended_at TIMESTAMPTZ`)
	db.Exec(`ALTER TABLE pretendo_friends ADD COLUMN IF NOT EXISTS last_online TIMESTAMPTZ`)
	db.Exec(`ALTER TABLE pretendo_friends ADD COLUMN IF NOT EXISTS title_id BIGINT NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE pretendo_friends ADD COLUMN IF NOT EXISTS title_version SMALLINT NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE pretendo_friends ADD COLUMN IF NOT EXISTS presence_flags INT NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE pretendo_friends ADD COLUMN IF NOT EXISTS presence_pid BIGINT NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE pretendo_friends ADD COLUMN IF NOT EXISTS presence_gathering_id INT NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE pretendo_friends ADD COLUMN IF NOT EXISTS presence_unk5 SMALLINT NOT NULL DEFAULT 3`)
	db.Exec(`ALTER TABLE pretendo_friends ADD COLUMN IF NOT EXISTS presence_unk6 SMALLINT NOT NULL DEFAULT 3`)
	db.Exec(`ALTER TABLE pretendo_friends ADD COLUMN IF NOT EXISTS presence_unk7 SMALLINT NOT NULL DEFAULT 3`)
	db.Exec(`CREATE TABLE IF NOT EXISTS mii_cache (
		pid        BIGINT PRIMARY KEY,
		pnid       TEXT NOT NULL DEFAULT '',
		mii_name   TEXT NOT NULL DEFAULT '',
		mii_data   BYTEA NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`)

	mongoClient, err := mongo.NewClient(options.Client().ApplyURI("mongodb://localhost:27017/"))
	if err != nil {
		log.Fatalf("mongo client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = mongoClient.Connect(ctx); err != nil {
		log.Fatalf("mongo connect: %v", err)
	}
	mongoDB = mongoClient.Database("pretendo")
	wscMongoDB = mongoClient.Database("wsc")
	log.Printf("connected to MongoDB")

	refreshRedirects()
	refreshBans()
	go func() {
		for range time.Tick(30 * time.Second) {
			refreshRedirects()
			refreshBans()
		}
	}()

	go func() {
		for range time.Tick(6 * time.Hour) {
			periodicFriendSync()
		}
	}()

	http.HandleFunc("/", handle)

	// Internal listener: gRPC stub auth + web password setup page (proxied via nginx)
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/internal/auth", handleInternalAuth)
		mux.HandleFunc("/internal/mii", handleInternalMii)
		mux.HandleFunc("/internal/web/status", handleWebStatus)
		mux.HandleFunc("/internal/web/set-password", handleWebSetPassword)
		mux.HandleFunc("/internal/friends/", handleInternalFriends)
		mux.HandleFunc("/internal/lookup/", handleInternalLookup)
		mux.HandleFunc("/internal/sync/", handleInternalSync)
		mux.HandleFunc("/internal/presence/start/", handlePresenceStart)
		mux.HandleFunc("/internal/presence/stop/", handlePresenceStop)
		mux.HandleFunc("/internal/presence/command/", handlePresenceCommand)
		log.Printf("account proxy internal listener on 127.0.0.1:9191")
		if err := http.ListenAndServe("127.0.0.1:9191", mux); err != nil {
			log.Fatalf("internal listener: %v", err)
		}
	}()

	go startOLVProxy()
	go initMiiImages()

	addr := "0.0.0.0:6666"
	log.Printf("account proxy listening on %s (TLS)", addr)

	cert, err := tls.LoadX509KeyPair("certs/nicochristmann-nn-cert.crt", "certs/nicochristmann-nn-cert.key")
	if err != nil {
		log.Fatalf("load cert: %v", err)
	}
	tlsCfg := &tls.Config{
		MinVersion:   tls.VersionTLS10,
		Certificates: []tls.Certificate{cert},
		// Shares the OLV/HPP listener's per-remote-IP handshake stagger (see
		// its doc comment on is3DSSensitiveHost/staggerHandshakePerIP) - the
		// 3DS's act module hits this same port (act.nicochristmann.net:6666,
		// see nimbus's account_url.s patch), and unlike the OLV listener
		// there's no per-hostname SNI split between Wii U and 3DS here to
		// gate on, so this applies unconditionally. Returning nil, nil keeps
		// the original tlsCfg for the handshake, per tls.Config's own
		// documented GetConfigForClient behavior.
		GetConfigForClient: func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			if chi.Conn != nil {
				staggerHandshakePerIP(chi.Conn.RemoteAddr())
			}
			return nil, nil
		},
	}

	tcpLn, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	tlsLn := tls.NewListener(tcpLn, tlsCfg)

	srv := &http.Server{
		TLSConfig: tlsCfg,
		ConnState: func(conn net.Conn, state http.ConnState) {
			log.Printf("conn %s → %s", conn.RemoteAddr(), state)
		},
	}
	log.Fatal(srv.Serve(tlsLn))
}

func refreshRedirects() {
	rows, err := db.Query(`
		SELECT UPPER(game_server_id), to_host, port, COALESCE(access_mode, 'whitelist')
		FROM redirects
		WHERE enabled = true AND game_server_id IS NOT NULL AND port IS NOT NULL`)
	if err != nil {
		log.Printf("refreshRedirects: %v", err)
		return
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var gameID, host, accessMode string
		var port uint16
		if err := rows.Scan(&gameID, &host, &port, &accessMode); err != nil {
			continue
		}
		redirectCache.Store(gameID, activeRedirect{host, port, accessMode})
		seen[gameID] = true
	}
	redirectCache.Range(func(k, _ interface{}) bool {
		if !seen[k.(string)] {
			redirectCache.Delete(k)
		}
		return true
	})
	log.Printf("redirects refreshed: %d active", len(seen))
}

func refreshBans() {
	rows, err := db.Query(`SELECT pid FROM banned_users`)
	if err != nil {
		log.Printf("refreshBans: %v", err)
		return
	}
	defer rows.Close()
	seen := map[uint32]bool{}
	for rows.Next() {
		var pid uint32
		if err := rows.Scan(&pid); err != nil {
			continue
		}
		bannedPIDs.Store(pid, struct{}{})
		seen[pid] = true
	}
	bannedPIDs.Range(func(k, _ interface{}) bool {
		if !seen[k.(uint32)] {
			bannedPIDs.Delete(k)
		}
		return true
	})
}

func checkBanned(pid uint32) bool {
	_, ok := bannedPIDs.Load(pid)
	return ok
}

func checkUserAccess(pid uint32, gameServerID, accessMode string) bool {
	switch accessMode {
	case "open":
		return true
	case "whitelist":
		var count int
		db.QueryRow(`SELECT COUNT(*) FROM user_access WHERE pid = $1 AND UPPER(game_server_id) = UPPER($2)`, pid, gameServerID).Scan(&count)
		return count > 0
	case "blacklist":
		var count int
		db.QueryRow(`SELECT COUNT(*) FROM user_access WHERE pid = $1 AND UPPER(game_server_id) = UPPER($2)`, pid, gameServerID).Scan(&count)
		return count == 0
	default:
		return false
	}
}

func queueForReview(pid uint32, gameServerID string) {
	_, err := db.Exec(`
		INSERT INTO review_queue (pid, game_server_id)
		VALUES ($1, UPPER($2))
		ON CONFLICT (pid, game_server_id) DO UPDATE
		SET last_seen = NOW(), attempt_count = review_queue.attempt_count + 1`,
		pid, gameServerID)
	if err != nil {
		log.Printf("queueForReview: %v", err)
	}
}

// queueNewUser queues pid for review on every managed (non-open) game server
// it hasn't already been approved or queued for. Called on first profile seen.
func queueNewUser(pid uint32) {
	if checkBanned(pid) {
		return
	}
	redirectCache.Range(func(k, v interface{}) bool {
		rd := v.(activeRedirect)
		if rd.AccessMode == "open" {
			return true
		}
		gameID := k.(string)
		// Only queue if not already approved.
		if !checkUserAccess(pid, gameID, "whitelist") {
			queueForReview(pid, gameID)
		}
		return true
	})
}

func generateJuxtServiceToken(pid uint32) (string, error) {
	aesKey, err := hex.DecodeString(juxtAESKeyHex)
	if err != nil {
		return "", err
	}
	// system_type(1) + token_type(1) + pid(4 LE) + issue_time(8 LE) + title_id(8 LE) + access_level(1)
	plaintext := make([]byte, 23)
	plaintext[0] = 0x01 // WUP (Wii U)
	plaintext[1] = 0x04 // IndependentService
	binary.LittleEndian.PutUint32(plaintext[2:6], pid)
	binary.LittleEndian.PutUint64(plaintext[6:14], uint64(time.Now().UnixMilli()))
	// title_id[14:22] and access_level[22] stay zero
	checksum := crc32.ChecksumIEEE(plaintext)
	checksumBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(checksumBytes, checksum)
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return "", err
	}
	padLen := aes.BlockSize - (len(plaintext) % aes.BlockSize)
	padded := append(plaintext, bytes.Repeat([]byte{byte(padLen)}, padLen)...)
	encrypted := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, make([]byte, aes.BlockSize)).CryptBlocks(encrypted, padded)
	return base64.StdEncoding.EncodeToString(append(checksumBytes, encrypted...)), nil
}

// handleServiceToken mints a Juxt-scoped service token locally whenever we already have
// a PID for the caller, regardless of which client_id/service requested it. We used to
// proxy this to Pretendo's real account server first and only substitute our own token on
// success (see git history), but the response's only field we ever kept was the token
// itself - everything else got discarded on marshal - and Pretendo's real server outright
// rejects some client_ids it never registered (e.g. Miiverse/OLV, code 0004 "invalid
// application credentials"), which broke those flows with no working fallback to lose.
// Bans are already enforced independently at the NEX/game-token layer (checkBanned), not
// here, so skipping the upstream call entirely doesn't remove that protection.
func handleServiceToken(w http.ResponseWriter, r *http.Request) {
	pid := fetchRealPID(r)

	if pid == 0 {
		// No cached PID (e.g. never seen this device before) - we can't mint a token
		// without one, so this is the one case where asking Pretendo can still help.
		body, status, headers, err := doUpstream(r)
		if err != nil {
			log.Printf("service_token: upstream error: %v", err)
			http.Error(w, "upstream error", 502)
			return
		}
		log.Printf("service_token: no PID cached for %s, passing Pretendo token (Juxt will reject)", realIP(r))
		writeResponse(w, status, headers, body)
		return
	}

	ourToken, err := generateJuxtServiceToken(pid)
	if err != nil {
		log.Printf("service_token: generate error for PID=%d: %v", pid, err)
		http.Error(w, "token generation error", http.StatusInternalServerError)
		return
	}
	body, err := xml.MarshalIndent(serviceTokenResp{Token: ourToken}, "", "  ")
	if err != nil {
		log.Printf("service_token: marshal error for PID=%d: %v", pid, err)
		http.Error(w, "token generation error", http.StatusInternalServerError)
		return
	}
	body = append([]byte(xml.Header), body...)
	log.Printf("service_token: minted local token for PID=%d client_id=%s (Pretendo upstream skipped)", pid, r.URL.Query().Get("client_id"))
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

func storeDeviceHeaders(username, pwHash string, r *http.Request) {
	deviceID := r.Header.Get("X-Nintendo-Device-Id")
	serial := r.Header.Get("X-Nintendo-Serial-Number")
	cert := r.Header.Get("X-Nintendo-Device-Cert")
	if username == "" || (deviceID == "" && cert == "") {
		return
	}
	db.Exec(`INSERT INTO wii_devices (username, device_id, serial, device_cert, pw_hash, updated_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
		ON CONFLICT (username) DO UPDATE SET
			device_id   = EXCLUDED.device_id,
			serial      = EXCLUDED.serial,
			device_cert = EXCLUDED.device_cert,
			pw_hash     = EXCLUDED.pw_hash,
			updated_at  = NOW()`,
		username, deviceID, serial, cert, pwHash)
}

func handle(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s %s", r.Method, r.RequestURI)
	gameServerID := r.URL.Query().Get("game_server_id")

	// Intercept OAuth to cache device headers for web login.
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_token/generate") {
		var bodyBuf []byte
		if r.Body != nil {
			bodyBuf, _ = io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(bodyBuf))
		}
		// TEMPORARY - capture full request to compare a successful real-3DS
		// login against a failing Azahar one, diagnosing intermittent
		// 022-2932/1600 errors. Remove once resolved.
		os.MkdirAll("/nico-pretendo-bridge/act-capture", 0755)
		ts := time.Now().Format("20060102-150405.000")
		reqLog := fmt.Sprintf("From: %s\nHeaders: %v\nBody: %s\n", realIP(r), r.Header, bodyBuf)
		os.WriteFile(fmt.Sprintf("/nico-pretendo-bridge/act-capture/%s_access_token_generate.request.txt", ts), []byte(reqLog), 0644)
		// Parse user_id from form body before proxying
		if vals, err := url.ParseQuery(string(bodyBuf)); err == nil {
			if uid := vals.Get("user_id"); uid != "" {
				storeDeviceHeaders(uid, vals.Get("password"), r)
			}
		}
	}

	if strings.HasSuffix(r.URL.Path, "/service_token/@me") {
		handleServiceToken(w, r)
		return
	}

	if strings.HasSuffix(r.URL.Path, "/nex_token/@me") {
		if v, ok := redirectCache.Load(strings.ToUpper(gameServerID)); ok {
			rd := v.(activeRedirect)
			switch strings.ToUpper(gameServerID) {
			case "1005A000":
				handleNexToken(w, r, rd.ToHost, rd.Port, rd.AccessMode)
			case "1010EB00":
				handleMK8NexToken(w, r, rd.ToHost, rd.Port, rd.AccessMode)
			case "1012F100":
				handleWSCNexToken(w, r, rd.ToHost, rd.Port, rd.AccessMode)
			case "10145E00":
				handleABSWNexToken(w, r, rd.ToHost, rd.Port, rd.AccessMode)
			case "00003200":
				handleFriendsNexToken(w, r, rd.ToHost, rd.Port, rd.AccessMode)
			case "101D9D00":
				handleMinecraftNexToken(w, r, rd.ToHost, rd.Port, rd.AccessMode)
			default:
				proxyAndCachePID(w, r)
			}
		} else {
			proxyAndCachePID(w, r)
		}
		return
	}

	// Intercept profile responses to capture PID early and queue new users.
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/people/@me/profile") {
		body, status, headers, err := doUpstream(r)
		if err != nil {
			http.Error(w, "upstream error", 502)
			return
		}
		if status == http.StatusOK {
			var p profilePerson
			if xml.Unmarshal(body, &p) == nil && p.PID != 0 {
				ip := realIP(r)
				pidCache.Store(ip, p.PID)
				if hash := authHash(r); hash != "" {
					storePIDInDB(hash, p.PID)
					storeMiiName(p.PID, p.Mii.Name)
					go uploadMiiImages(p.PID, p.Mii.Data)
				}
				if p.PNID != "" {
					db.Exec(`INSERT INTO pnid_cache (pid, pnid)
					         VALUES ($1, $2)
					         ON CONFLICT (pid) DO UPDATE SET pnid = EXCLUDED.pnid, updated_at = NOW()`,
						p.PID, p.PNID)
				}
				log.Printf("profile: captured PID=%d PNID=%q for %s", p.PID, p.PNID, ip)
				go queueNewUser(p.PID)
			}
		}
		writeResponse(w, status, headers, body)
		return
	}

	proxy(w, r, nil)
}

func handleNexToken(w http.ResponseWriter, r *http.Request, host string, port uint16, accessMode string) {
	ip := realIP(r)
	pid := fetchRealPID(r)

	if pid != 0 {
		if checkBanned(pid) {
			log.Printf("WiiUChat: PID=%d is banned, proxying to Pretendo", pid)
			proxyAndCachePID(w, r)
			return
		}
		if !checkUserAccess(pid, "1005A000", accessMode) {
			log.Printf("WiiUChat: PID=%d access denied (mode=%s), queued for review", pid, accessMode)
			queueForReview(pid, "1005A000")
			proxyAndCachePID(w, r)
			return
		}
	}

	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		http.Error(w, "token error", 500)
		return
	}
	sessionToken := fmt.Sprintf("%x", b)

	if _, err := db.Exec(`
		INSERT INTO nex_sessions (token, ip, expires_at)
		VALUES ($1, $2, NOW() + INTERVAL '5 minutes')
	`, sessionToken, ip); err != nil {
		log.Printf("insert session: %v", err)
		http.Error(w, "db error", 500)
		return
	}

	if pid == 0 {
		db.QueryRow(`
			SELECT pid FROM nex_sessions
			WHERE ip = $1 AND pid IS NOT NULL
			ORDER BY expires_at DESC LIMIT 1
		`, ip).Scan(&pid)
	}

	// Pre-populate nex_accounts so PasswordFromPID works when the Wii U connects
	// to the NEX auth server. The nex-go framework calls AccountDetailsByPID before
	// our LoginEx handler runs, so the account must already exist in the DB.
	if pid != 0 {
		if _, err := db.Exec(`
			INSERT INTO nex_accounts (pid, username, nex_password)
			VALUES ($1, $2, $3)
			ON CONFLICT (pid) DO UPDATE SET nex_password = EXCLUDED.nex_password
		`, pid, fmt.Sprintf("%d", pid), sessionToken); err != nil {
			log.Printf("upsert nex_account: %v", err)
		}
	}

	if pid != 0 {
		db.Exec(`INSERT INTO relay_requests (pid, game_server_id) VALUES ($1, $2)`, pid, "1005A000")
	}
	log.Printf("nex_token for %s: PID=%d token=%s…", ip, pid, sessionToken[:8])

	tkn := nexToken{
		Host:        host,
		NexPassword: sessionToken,
		PID:         pid,
		Port:        port,
		Token:       sessionToken,
	}
	body, err := xml.MarshalIndent(tkn, "", "  ")
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

func upsertMK8Account(pid uint32, password string) {
	col := mongoDB.Collection("nexaccounts")
	_, err := col.UpdateOne(
		context.Background(),
		bson.D{{Key: "pid", Value: pid}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "pid", Value: pid},
			{Key: "password", Value: password},
		}}},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		log.Printf("mongo upsert: %v", err)
	}
}

func handleMK8NexToken(w http.ResponseWriter, r *http.Request, host string, port uint16, accessMode string) {
	ip := realIP(r)
	pid := fetchRealPID(r)

	if pid != 0 {
		if checkBanned(pid) {
			log.Printf("MK8: PID=%d is banned, proxying to Pretendo", pid)
			proxyAndCachePID(w, r)
			return
		}
		if !checkUserAccess(pid, "1010EB00", accessMode) {
			log.Printf("MK8: PID=%d access denied (mode=%s), queued for review", pid, accessMode)
			queueForReview(pid, "1010EB00")
			proxyAndCachePID(w, r)
			return
		}
	}

	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		http.Error(w, "token error", 500)
		return
	}
	sessionToken := fmt.Sprintf("%x", b)

	if pid != 0 {
		upsertMK8Account(pid, sessionToken)
		db.Exec(`INSERT INTO relay_requests (pid, game_server_id) VALUES ($1, $2)`, pid, "1010EB00")
	}
	log.Printf("mk8_token for %s: PID=%d token=%s…", ip, pid, sessionToken[:8])

	tkn := nexToken{
		Host:        host,
		NexPassword: sessionToken,
		PID:         pid,
		Port:        port,
		Token:       sessionToken,
	}
	body, err := xml.MarshalIndent(tkn, "", "  ")
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

func upsertWSCAccount(pid uint32, password string) {
	col := mongoDB.Collection("wsc_nexaccounts")
	_, err := col.UpdateOne(
		context.Background(),
		bson.D{{Key: "pid", Value: pid}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "pid", Value: pid},
			{Key: "password", Value: password},
		}}},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		log.Printf("mongo upsert wsc: %v", err)
	}
}

func handleWSCNexToken(w http.ResponseWriter, r *http.Request, host string, port uint16, accessMode string) {
	ip := realIP(r)
	pid := fetchRealPID(r)

	if pid != 0 {
		if checkBanned(pid) {
			log.Printf("WSC: PID=%d is banned, proxying to Pretendo", pid)
			proxyAndCachePID(w, r)
			return
		}
		if !checkUserAccess(pid, "1012F100", accessMode) {
			log.Printf("WSC: PID=%d access denied (mode=%s), queued for review", pid, accessMode)
			queueForReview(pid, "1012F100")
			proxyAndCachePID(w, r)
			return
		}
	}

	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		http.Error(w, "token error", 500)
		return
	}
	sessionToken := fmt.Sprintf("%x", b)

	if pid != 0 {
		upsertWSCAccount(pid, sessionToken)
		db.Exec(`INSERT INTO relay_requests (pid, game_server_id) VALUES ($1, $2)`, pid, "1012F100")
	}
	log.Printf("wsc_token for %s: PID=%d token=%s…", ip, pid, sessionToken[:8])

	tkn := nexToken{
		Host:        host,
		NexPassword: sessionToken,
		PID:         pid,
		Port:        port,
		Token:       sessionToken,
	}
	body, err := xml.MarshalIndent(tkn, "", "  ")
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

func handleFriendsNexToken(w http.ResponseWriter, r *http.Request, host string, port uint16, accessMode string) {
	ip := realIP(r)

	// Forward to Pretendo to obtain the stable nex_password for this PNID.
	// The Wii U Friends module caches this password across boots, so it must match
	// whatever Pretendo gave this console when the user first set up their account.
	// We rewrite host/port to point at our server but keep the original nex_password.
	upBody, upStatus, _, upErr := doUpstream(r)
	var pretendoPID uint32
	var nexPassword string
	var sessionToken string
	if upErr == nil && upStatus == http.StatusOK && len(upBody) > 0 {
		var upstream nexToken
		if xmlErr := xml.Unmarshal(upBody, &upstream); xmlErr == nil && upstream.NexPassword != "" {
			nexPassword = upstream.NexPassword
			sessionToken = upstream.Token
			pretendoPID = upstream.PID
			// Launch PRUDP friends sync in background using Pretendo's real server address.
			if upstream.Host != "" && upstream.Port != 0 {
				go fetchFriendsPRUDP(pretendoPID, nexPassword, upstream.Host, upstream.Port)
			}
			log.Printf("friends_token: got Pretendo pw for PID=%d from %s", pretendoPID, ip)
		} else {
			log.Printf("friends_token: upstream parse failed (err=%v pw=%q) for %s", xmlErr, upstream.NexPassword, ip)
		}
	} else {
		log.Printf("friends_token: upstream failed (err=%v status=%d) for %s — using fallback", upErr, upStatus, ip)
	}

	// Fallback: generate a stable local password if Pretendo is unreachable or unknown.
	if nexPassword == "" {
		b := make([]byte, 16)
		rand.Read(b)
		sessionToken = fmt.Sprintf("%x", b)
		nexPassword = sessionToken
	}
	if sessionToken == "" {
		b := make([]byte, 16)
		rand.Read(b)
		sessionToken = fmt.Sprintf("%x", b)
	}

	// Store the session token for method-2 auth validation.
	db.Exec(`INSERT INTO nex_sessions (token, ip, expires_at) VALUES ($1, $2, NOW() + INTERVAL '5 minutes')`,
		sessionToken, ip)

	// Pre-seed the PID cache so fetchRealPID avoids a redundant Pretendo round-trip.
	if pretendoPID != 0 {
		pidCache.Store(ip, pretendoPID)
	}
	pid := fetchRealPID(r)
	if pid == 0 {
		pid = pretendoPID
	}
	if pid == 0 {
		db.QueryRow(`SELECT pid FROM nex_sessions WHERE ip = $1 AND pid IS NOT NULL ORDER BY expires_at DESC LIMIT 1`, ip).Scan(&pid)
	}

	if pid != 0 {
		pidCache.Store(ip, pid)
		if hash := authHash(r); hash != "" {
			storePIDInDB(hash, pid)
		}
		// Always update the password with whatever Pretendo says (it's stable on their side).
		// For the fallback case, use COALESCE so we don't overwrite an existing local password.
		var coalesceNexPw string
		if nexPassword != sessionToken {
			// Pretendo password: always authoritative, always write it.
			coalesceNexPw = nexPassword
		} else {
			// Fallback: only write if no existing password.
			var existing string
			db.QueryRow(`SELECT friends_nex_password FROM nex_accounts WHERE pid = $1 AND friends_nex_password IS NOT NULL`, pid).Scan(&existing)
			if existing != "" {
				nexPassword = existing
			}
			coalesceNexPw = nexPassword
		}
		if _, err := db.Exec(`
			INSERT INTO nex_accounts (pid, username, nex_password, friends_nex_password)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (pid) DO UPDATE SET
				nex_password = EXCLUDED.nex_password,
				friends_nex_password = EXCLUDED.friends_nex_password
		`, pid, fmt.Sprintf("%d", pid), sessionToken, coalesceNexPw); err != nil {
			log.Printf("upsert friends nex_account: %v", err)
		}
		db.Exec(`INSERT INTO relay_requests (pid, game_server_id) VALUES ($1, $2)`, pid, "00003200")
		go fetchAndStoreFriends(pid, r.Header)
	}

	pwPreview := nexPassword
	if len(pwPreview) > 8 {
		pwPreview = pwPreview[:8] + "…"
	}
	log.Printf("friends_token for %s: PID=%d token=%s… nexpw=%s", ip, pid, sessionToken[:8], pwPreview)

	tkn := nexToken{
		Host:        host,
		NexPassword: nexPassword,
		PID:         pid,
		Port:        port,
		Token:       sessionToken,
	}
	body, err := xml.MarshalIndent(tkn, "", "  ")
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

func handleABSWNexToken(w http.ResponseWriter, r *http.Request, host string, port uint16, accessMode string) {
	ip := realIP(r)
	pid := fetchRealPID(r)

	if pid != 0 {
		if checkBanned(pid) {
			log.Printf("ABSW: PID=%d is banned, proxying to Pretendo", pid)
			proxyAndCachePID(w, r)
			return
		}
		if !checkUserAccess(pid, "10145E00", accessMode) {
			log.Printf("ABSW: PID=%d access denied (mode=%s), queued for review", pid, accessMode)
			queueForReview(pid, "10145E00")
			proxyAndCachePID(w, r)
			return
		}
	}

	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		http.Error(w, "token error", 500)
		return
	}
	sessionToken := fmt.Sprintf("%x", b)

	if _, err := db.Exec(`
		INSERT INTO nex_sessions (token, ip, expires_at)
		VALUES ($1, $2, NOW() + INTERVAL '5 minutes')
	`, sessionToken, ip); err != nil {
		log.Printf("insert absw session: %v", err)
		http.Error(w, "db error", 500)
		return
	}

	if pid == 0 {
		db.QueryRow(`
			SELECT pid FROM nex_sessions
			WHERE ip = $1 AND pid IS NOT NULL
			ORDER BY expires_at DESC LIMIT 1
		`, ip).Scan(&pid)
	}

	if pid != 0 {
		pidCache.Store(ip, pid)
		if hash := authHash(r); hash != "" {
			storePIDInDB(hash, pid)
		}
		if _, err := db.Exec(`
			INSERT INTO nex_accounts (pid, username, nex_password)
			VALUES ($1, $2, $3)
			ON CONFLICT (pid) DO UPDATE SET nex_password = EXCLUDED.nex_password
		`, pid, fmt.Sprintf("%d", pid), sessionToken); err != nil {
			log.Printf("upsert absw nex_account: %v", err)
		}
		db.Exec(`INSERT INTO relay_requests (pid, game_server_id) VALUES ($1, $2)`, pid, "10145E00")
	}
	log.Printf("absw_token for %s: PID=%d token=%s…", ip, pid, sessionToken[:8])

	tkn := nexToken{
		Host:        host,
		NexPassword: sessionToken,
		PID:         pid,
		Port:        port,
		Token:       sessionToken,
	}
	body, err := xml.MarshalIndent(tkn, "", "  ")
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

func upsertMinecraftAccount(pretendoPID uint32, token string) {
	col := wscMongoDB.Collection("minecraft_nexaccounts")
	username := fmt.Sprintf("%d", pretendoPID)
	_, err := col.UpdateOne(
		context.Background(),
		bson.D{{Key: "username", Value: username}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "username", Value: username},
			{Key: "pid", Value: int32(pretendoPID)},
			{Key: "nex_password", Value: token},
		}}},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		log.Printf("upsert minecraft account: %v", err)
	}
}

func handleMinecraftNexToken(w http.ResponseWriter, r *http.Request, host string, port uint16, accessMode string) {
	ip := realIP(r)
	pid := fetchRealPID(r)

	if pid != 0 && checkBanned(pid) {
		log.Printf("Minecraft: PID=%d is banned, proxying to Pretendo", pid)
		proxyAndCachePID(w, r)
		return
	}

	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		http.Error(w, "token error", 500)
		return
	}
	token := fmt.Sprintf("%x", b)

	if pid != 0 {
		upsertMinecraftAccount(pid, token)
		db.Exec(`INSERT INTO relay_requests (pid, game_server_id) VALUES ($1, $2)`, pid, "101D9D00")
	}
	log.Printf("mc_token for %s: PID=%d token=%s…", ip, pid, token[:8])

	tkn := nexToken{
		Host:        host,
		NexPassword: token,
		PID:         pid,
		Port:        port,
		Token:       token,
	}
	body, err := xml.MarshalIndent(tkn, "", "  ")
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

// proxyAndCachePID forwards any non-WiiU-Chat nex_token request to Pretendo and
// caches the returned PID so handleNexToken can use it for the same client IP.
func proxyAndCachePID(w http.ResponseWriter, r *http.Request) {
	body, statusCode, headers, err := doUpstream(r)
	if err == nil && statusCode == http.StatusOK && len(body) > 0 {
		var tkn nexToken
		if xmlErr := xml.Unmarshal(body, &tkn); xmlErr == nil && tkn.PID != 0 {
			ip := realIP(r)
			pidCache.Store(ip, tkn.PID)
			if hash := authHash(r); hash != "" {
				storePIDInDB(hash, tkn.PID)
			}
			gameID := r.URL.Query().Get("game_server_id")
			log.Printf("cached PID=%d for %s (game %s)", tkn.PID, ip, gameID)
			db.Exec(`INSERT INTO relay_requests (pid, game_server_id) VALUES ($1, $2)`, tkn.PID, gameID)
		}
	}
	if err != nil {
		log.Printf("upstream error: %v", err)
		http.Error(w, "upstream error", 502)
		return
	}
	writeResponse(w, statusCode, headers, body)
}

func authHash(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ""
	}
	h := sha256.Sum256([]byte(auth))
	return hex.EncodeToString(h[:])
}

func lookupPIDFromDB(hash string) uint32 {
	var pid uint32
	db.QueryRow(`SELECT pid FROM pid_cache WHERE auth_hash = $1`, hash).Scan(&pid)
	return pid
}

func storePIDInDB(hash string, pid uint32) {
	db.Exec(`INSERT INTO pid_cache (auth_hash, pid)
	         VALUES ($1, $2)
	         ON CONFLICT (auth_hash) DO UPDATE SET pid = EXCLUDED.pid, cached_at = NOW()`,
		hash, pid)
}

var miiNameCache sync.Map // pid (uint32) → mii name (string)

func storeMiiName(pid uint32, name string) {
	if name == "" {
		return
	}
	miiNameCache.Store(pid, name)
	db.Exec(`INSERT INTO mii_names (pid, mii_name)
	         VALUES ($1, $2)
	         ON CONFLICT (pid) DO UPDATE SET mii_name = EXCLUDED.mii_name`,
		pid, name)
}

var (
	s3Endpoint  = os.Getenv("S3_ENDPOINT")
	s3AccessKey = os.Getenv("S3_ACCESS_KEY")
	s3SecretKey = os.Getenv("S3_SECRET_KEY")
	s3Bucket    = os.Getenv("S3_BUCKET")
	s3Region    = os.Getenv("S3_REGION")
)

var miiExpressions = map[string]string{
	"normal_face.png":         "normal",
	"smile_open_mouth.png":    "smile",
	"wink_left.png":           "wink_left",
	"surprise_open_mouth.png": "surprise_open_mouth",
	"frustrated.png":          "frustrated",
	"sorrow.png":              "sorrow",
}

// uploadMiiImages renders each expression in miiExpressions from the user's raw
// FFLStoreData and uploads them to S3. Pretendo's own CDN only ever serves a single
// static "standard" face render (no per-expression endpoint exists there), so all
// expressions are rendered via mii-unsecure.ariankordi.net — the same public render
// backend the Discord /mii command uses — passing the expression names as its
// `expression` query param (confirmed against its own render form: values like
// smile_open_mouth/wink_left/surprise_open_mouth/frustrated/sorrow match exactly).
// Skips if the images are already present in S3 to avoid redundant re-rendering.
func uploadMiiImages(pid uint32, miiDataB64 string) {
	if s3Endpoint == "" || s3AccessKey == "" || miiDataB64 == "" {
		return
	}
	s3c, err := minio.New(s3Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(s3AccessKey, s3SecretKey, ""),
		Region: s3Region,
		Secure: true,
	})
	if err != nil {
		log.Printf("uploadMiiImages: s3 init: %v", err)
		return
	}
	checkKey := fmt.Sprintf("mii/%d/normal_face.png", pid)
	if info, statErr := s3c.StatObject(context.Background(), s3Bucket, checkKey, minio.StatObjectOptions{}); statErr == nil {
		if time.Since(info.LastModified) < 3*24*time.Hour {
			return // already uploaded and fresh
		}
	}

	miiBytes, err := base64.StdEncoding.DecodeString(miiDataB64)
	if err != nil {
		log.Printf("uploadMiiImages: decode mii data pid=%d: %v", pid, err)
		return
	}
	renderDataParam := base64.RawURLEncoding.EncodeToString(miiBytes)

	for filename, expression := range miiExpressions {
		renderURL := fmt.Sprintf(
			"https://mii-unsecure.ariankordi.net/miis/image.png?data=%s&width=128&type=face&expression=%s&api_id=1",
			renderDataParam, expression,
		)
		resp, err := http.Get(renderURL)
		if err != nil {
			log.Printf("uploadMiiImages: render fetch pid=%d expression=%s: %v", pid, expression, err)
			continue
		}
		if resp.StatusCode != 200 {
			log.Printf("uploadMiiImages: render fetch pid=%d expression=%s status=%d", pid, expression, resp.StatusCode)
			resp.Body.Close()
			continue
		}
		pngData, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			log.Printf("uploadMiiImages: read render pid=%d expression=%s: %v", pid, expression, readErr)
			continue
		}

		key := fmt.Sprintf("mii/%d/%s", pid, filename)
		_, err = s3c.PutObject(
			context.Background(), s3Bucket, key,
			bytes.NewReader(pngData), int64(len(pngData)),
			minio.PutObjectOptions{
				ContentType:  "image/png",
				UserMetadata: map[string]string{"x-amz-acl": "public-read"},
			},
		)
		if err != nil {
			log.Printf("uploadMiiImages: s3 put %s: %v", key, err)
		} else {
			log.Printf("uploadMiiImages: uploaded %s for PID %d", key, pid)
		}
	}
}

// fetchRealPID returns the Pretendo PID for the requesting client.
// Check order: in-memory cache → DB (keyed by auth token hash) → Pretendo pivot.
// MK8 (1010EB00) is intentionally excluded from the pivot list so it can be
// redirected to our own server without being consumed here.
func realIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip == "" {
		return r.RemoteAddr
	}
	return ip
}

func fetchRealPID(r *http.Request) uint32 {
	ip := realIP(r)

	if v, ok := pidCache.Load(ip); ok {
		pid := v.(uint32)
		log.Printf("fetchRealPID: memory cache hit PID=%d for %s", pid, ip)
		return pid
	}

	hash := authHash(r)
	if hash != "" {
		if pid := lookupPIDFromDB(hash); pid != 0 {
			pidCache.Store(ip, pid)
			log.Printf("fetchRealPID: db cache hit PID=%d for %s", pid, ip)
			return pid
		}
	}

	for _, gameID := range []string{"1018DB00", "10176A00"} {
		uri := "/v1/api/provider/nex_token/@me?game_server_id=" + gameID
		synth, _ := http.NewRequest("GET", uri, nil)
		synth.RequestURI = uri
		synth.Header = r.Header.Clone()

		body, statusCode, _, err := doUpstream(synth)
		if err != nil || statusCode != http.StatusOK || len(body) == 0 {
			continue
		}
		var tkn nexToken
		if err := xml.Unmarshal(body, &tkn); err != nil || tkn.PID == 0 {
			continue
		}
		pidCache.Store(ip, tkn.PID)
		if hash != "" {
			storePIDInDB(hash, tkn.PID)
		}
		log.Printf("fetchRealPID: PID=%d via game %s (stored in db)", tkn.PID, gameID)
		return tkn.PID
	}

	// Fallback: directly fetch the profile using the Wii U's auth credentials
	profReq, _ := http.NewRequest("GET", "/v1/api/people/@me/profile", nil)
	profReq.RequestURI = "/v1/api/people/@me/profile"
	profReq.Header = r.Header.Clone()
	body, statusCode, _, err := doUpstream(profReq)
	if err == nil && statusCode == http.StatusOK {
		var p profilePerson
		if xml.Unmarshal(body, &p) == nil && p.PID != 0 {
			pidCache.Store(ip, p.PID)
			if hash != "" {
				storePIDInDB(hash, p.PID)
				storeMiiName(p.PID, p.Mii.Name)
					go uploadMiiImages(p.PID, p.Mii.Data)
			}
			log.Printf("fetchRealPID: PID=%d via profile fetch (stored)", p.PID)
			return p.PID
		}
	}

	log.Printf("fetchRealPID: could not determine PID for %s", ip)
	return 0
}

func proxy(w http.ResponseWriter, r *http.Request, body []byte) {
	b, statusCode, headers, err := doUpstream(r)
	if err != nil {
		log.Printf("upstream error: %v", err)
		http.Error(w, "upstream error", 502)
		return
	}
	log.Printf("upstream %d (%d bytes)", statusCode, len(b))
	if statusCode != http.StatusOK {
		log.Printf("upstream body: %s", b)
	}
	if body != nil {
		b = body
	}
	writeResponse(w, statusCode, headers, b)
}

// wiiUTLSConfig mimics the Wii U TLS fingerprint so Cloudflare accepts the connection.
var wiiUTLSConfig = &tls.Config{
	MinVersion: tls.VersionTLS10,
	MaxVersion: tls.VersionTLS11,
	CipherSuites: []uint16{
		tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
		tls.TLS_RSA_WITH_AES_256_CBC_SHA,
		tls.TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA,
		tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA,
		tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
		tls.TLS_RSA_WITH_AES_128_CBC_SHA,
		tls.TLS_RSA_WITH_RC4_128_SHA,
	},
	NextProtos: []string{"http/1.1"},
}

// threeDSTLSConfig mimics a real 3DS's TLS fingerprint, captured from an actual
// console's ClientHello (empty SNI, versions=[TLS1.1,TLS1.0], ciphers=[57 53 51
// 47 22 10 5 4] - i.e. RSA/DHE-RSA key exchange only, no ECDHE at all, unlike
// wiiUTLSConfig). Go's crypto/tls has no DHE (non-ephemeral-EC) cipher suites,
// so only the plain-RSA subset of that list is reproducible here, but that's
// still meaningfully different from wiiUTLSConfig's ECDHE-heavy profile.
// Used for doUpstream calls whose original request came from a 3DS, not a Wii
// U - proxying through with the wrong platform's fingerprint means Pretendo's
// real server sees a 3DS-shaped HTTP request arrive over a Wii-U-shaped TLS
// handshake, a combination a real direct 3DS connection would never produce,
// which is suspected to cause the intermittent 022-2932 login failures.
var threeDSTLSConfig = &tls.Config{
	MinVersion: tls.VersionTLS10,
	MaxVersion: tls.VersionTLS11,
	CipherSuites: []uint16{
		tls.TLS_RSA_WITH_AES_256_CBC_SHA,
		tls.TLS_RSA_WITH_AES_128_CBC_SHA,
		tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA,
		tls.TLS_RSA_WITH_RC4_128_SHA,
	},
	NextProtos: []string{"http/1.1"},
}

// realConsoleDeviceModels: genuine Nintendo hardware model codes sent in
// X-Nintendo-Device-Model (CTR=old 3DS, SPR=3DS XL, KTR=New 3DS, FTR=New 3DS
// XL, WUP=Wii U). Confirmed via a real captured request/response comparison
// (see feedback_azahar_device_model memory) that Azahar sends "RED" here -
// not a real Nintendo code - and that this exact request/response pair
// deterministically succeeds or fails 1:1 with which of these two values is
// present, nothing else differing between the captures.
var realConsoleDeviceModels = map[string]bool{
	"CTR": true, "SPR": true, "KTR": true, "FTR": true, "WUP": true,
}

// upstreamTLSConfig picks the outbound TLS fingerprint for a proxied request
// to Pretendo's real account server based on X-Nintendo-Device-Model. A real,
// direct console-to-Pretendo connection's TLS handshake and its claimed
// device model are always consistent (real hardware fingerprint + real model
// code, or a plain app/emulator fingerprint + an emulator's own model
// string). Since we sit in the middle and re-originate the connection
// ourselves, forcing a console-shaped fingerprint onto a request that
// honestly identifies as non-hardware (like Azahar's "RED") creates exactly
// the mismatch a real direct connection would never produce - suspected
// cause of the 022-2932 failures unique to that device model. For anything
// that isn't a real, known console model, fall back to Go's own default TLS
// behavior (nil CipherSuites/version pins) rather than spoofing any specific
// hardware at all.
func upstreamTLSConfig(headers http.Header) *tls.Config {
	model := headers.Get("X-Nintendo-Device-Model")
	if !realConsoleDeviceModels[model] {
		return &tls.Config{}
	}
	if model == "WUP" {
		return wiiUTLSConfig
	}
	return threeDSTLSConfig
}

// headers that must not be forwarded to the upstream
var stripUpstream = map[string]bool{
	"Content-Length":    true,
	"Transfer-Encoding": true,
	"Connection":        true,
	"Keep-Alive":        true,
	"Te":                true,
	"Trailer":           true,
	"Upgrade":           true,
	"Host":              true,
}

func doUpstreamGetPath(path string, headers http.Header) ([]byte, int, error) {
	rawConn, err := net.DialTimeout("tcp", "account.pretendo.cc:443", 15*time.Second)
	if err != nil {
		return nil, 0, err
	}
	cfg := upstreamTLSConfig(headers).Clone()
	cfg.ServerName = "account.pretendo.cc"
	tlsConn := tls.Client(rawConn, cfg)
	defer tlsConn.Close()
	tlsConn.SetDeadline(time.Now().Add(15 * time.Second))

	bw := bufio.NewWriter(tlsConn)
	fmt.Fprintf(bw, "GET %s HTTP/1.1\r\n", path)
	fmt.Fprintf(bw, "Host: account.pretendo.cc\r\n")
	fmt.Fprintf(bw, "Connection: close\r\n")
	for k, vs := range headers {
		if stripUpstream[k] {
			continue
		}
		for _, v := range vs {
			fmt.Fprintf(bw, "%s: %s\r\n", k, v)
		}
	}
	fmt.Fprintf(bw, "\r\n")
	if err := bw.Flush(); err != nil {
		return nil, 0, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), nil)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return body, resp.StatusCode, err
}

func periodicFriendSync() {
	rows, err := db.Query(`
		SELECT pid, friends_nex_password, last_auth_host, last_auth_port
		FROM nex_accounts
		WHERE friends_nex_password IS NOT NULL
		  AND last_auth_host IS NOT NULL
		  AND last_auth_port IS NOT NULL`)
	if err != nil {
		log.Printf("periodicFriendSync: query failed: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var pid uint32
		var nexPassword, authHost string
		var authPort uint16
		if err := rows.Scan(&pid, &nexPassword, &authHost, &authPort); err != nil {
			continue
		}
		go fetchFriendsPRUDP(pid, nexPassword, authHost, authPort)
	}
}

func fetchFriendsPRUDP(ownerPID uint32, nexPassword, authHost string, authPort uint16) {
	script := "/nico-pretendo-bridge/account-proxy/fetch_friends.py"
	dbURI := os.Getenv("PN_WUC_POSTGRES_URI")
	cmd := exec.Command("python3", script,
		"--pid", fmt.Sprintf("%d", ownerPID),
		"--nex-password", nexPassword,
		"--auth-host", authHost,
		"--auth-port", fmt.Sprintf("%d", authPort),
		"--db-uri", dbURI,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("fetchFriendsPRUDP PID=%d: %v\n%s", ownerPID, err, out)
	} else {
		log.Printf("fetchFriendsPRUDP PID=%d: ok\n%s", ownerPID, out)
	}
}

type friendsXMLPersons struct {
	Persons []profilePerson `xml:"person"`
}

func fetchAndStoreFriends(ownerPID uint32, headers http.Header) {
	body, status, err := doUpstreamGetPath("/v1/api/people/@me/friends", headers)
	if err != nil || status != http.StatusOK {
		log.Printf("friends fetch PID=%d: status=%d err=%v body=%s", ownerPID, status, err, body)
		return
	}
	var persons friendsXMLPersons
	if err := xml.Unmarshal(body, &persons); err != nil {
		log.Printf("friends parse PID=%d: %v body=%s", ownerPID, err, body)
		return
	}
	for _, p := range persons.Persons {
		var miiData []byte
		if p.Mii.Data != "" {
			miiData, _ = base64.StdEncoding.DecodeString(p.Mii.Data)
		}
		db.Exec(`
			INSERT INTO pretendo_friends (owner_pid, friend_pid, friend_nnid, mii_name, mii_data, updated_at)
			VALUES ($1, $2, $3, $4, $5, NOW())
			ON CONFLICT (owner_pid, friend_pid) DO UPDATE SET
				friend_nnid = EXCLUDED.friend_nnid,
				mii_name    = EXCLUDED.mii_name,
				mii_data    = EXCLUDED.mii_data,
				updated_at  = NOW()
		`, ownerPID, p.PID, p.PNID, p.Mii.Name, miiData)
	}
	log.Printf("friends stored PID=%d: %d friends", ownerPID, len(persons.Persons))
}

func doUpstream(r *http.Request) ([]byte, int, http.Header, error) {
	var reqBody []byte
	if r.Body != nil && r.Body != http.NoBody {
		var err error
		reqBody, err = io.ReadAll(r.Body)
		if err != nil {
			return nil, 0, nil, err
		}
	}

	// Dial raw TLS so we can write headers with non-RFC-7230 names unmodified.
	rawConn, err := net.DialTimeout("tcp", "account.pretendo.cc:443", 15*time.Second)
	if err != nil {
		return nil, 0, nil, err
	}
	cfg := upstreamTLSConfig(r.Header).Clone()
	cfg.ServerName = "account.pretendo.cc"
	tlsConn := tls.Client(rawConn, cfg)
	defer tlsConn.Close()
	tlsConn.SetDeadline(time.Now().Add(15 * time.Second))

	bw := bufio.NewWriter(tlsConn)
	fmt.Fprintf(bw, "%s %s HTTP/1.1\r\n", r.Method, r.RequestURI)
	fmt.Fprintf(bw, "Host: account.pretendo.cc\r\n")
	fmt.Fprintf(bw, "Connection: close\r\n")
	for k, vs := range r.Header {
		if stripUpstream[k] {
			continue
		}
		for _, v := range vs {
			fmt.Fprintf(bw, "%s: %s\r\n", k, v)
		}
	}
	fmt.Fprintf(bw, "Content-Length: %d\r\n", len(reqBody))
	fmt.Fprintf(bw, "\r\n")
	bw.Write(reqBody)
	if err := bw.Flush(); err != nil {
		return nil, 0, nil, err
	}

	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), nil)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return body, resp.StatusCode, resp.Header, err
}


var hopByHop = map[string]bool{
	"Transfer-Encoding":   true,
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Upgrade":             true,
}

func writeResponse(w http.ResponseWriter, status int, headers http.Header, body []byte) {
	for k, vs := range headers {
		if hopByHop[k] {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w.WriteHeader(status)
	w.Write(body)
}

// ── Internal auth endpoint (localhost only) ────────────────────
// Used by the gRPC stub to authenticate Pretendo credentials via
// doUpstream, which uses wiiUTLSConfig to pass Cloudflare's check.

type internalAuthResp struct {
	PID   uint32 `json:"pid"`
	PNID  string `json:"pnid"`
	Error string `json:"error,omitempty"`
}

func wiiUHeaders(deviceID, serial, deviceCert string) http.Header {
	h := http.Header{}
	h.Set("X-Nintendo-Client-Id", "a2efa818a34fa16b8afbc8a74eba3eda")
	h.Set("X-Nintendo-Client-Secret", "c91cdb5658bd4954ade78533a339cf9a")
	h.Set("X-Nintendo-Platform-Id", "1")
	h.Set("X-Nintendo-Device-Type", "2")
	h.Set("X-Nintendo-System-Version", "0260")
	h.Set("X-Nintendo-Environment", "L1")
	h.Set("X-Nintendo-Region", "4")
	h.Set("X-Nintendo-Country", "DE")
	h.Set("X-Nintendo-Fpd-Version", "0000")
	h.Set("Accept", "*/*")
	if deviceID != "" {
		h.Set("X-Nintendo-Device-Id", deviceID)
	}
	if serial != "" {
		h.Set("X-Nintendo-Serial-Number", serial)
	}
	if deviceCert != "" {
		h.Set("X-Nintendo-Device-Cert", deviceCert)
	}
	return h
}

// pretendoOAuthToken authenticates against Pretendo using the stored Wii U hash + device cert.
// Returns the short-lived OAuth access token on success.
func pretendoOAuthToken(userID, deviceID, serial, deviceCert, pwHash string) (string, error) {
	body := fmt.Sprintf(
		"grant_type=password&user_id=%s&password=%s&password_type=hash",
		url.QueryEscape(userID), url.QueryEscape(pwHash),
	)
	req, _ := http.NewRequest("POST", "/v1/api/oauth20/access_token/generate", strings.NewReader(body))
	req.RequestURI = "/v1/api/oauth20/access_token/generate"
	req.Header = wiiUHeaders(deviceID, serial, deviceCert)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, status, _, err := doUpstream(req)
	if err != nil {
		return "", fmt.Errorf("upstream: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("pretendo %d: %s", status, resp)
	}
	type oauthXML struct {
		XMLName     xml.Name `xml:"OAuth20"`
		AccessToken struct {
			Token string `xml:"token"`
		} `xml:"access_token"`
	}
	var oa oauthXML
	if err := xml.Unmarshal(resp, &oa); err != nil || oa.AccessToken.Token == "" {
		return "", fmt.Errorf("parse token response")
	}
	return oa.AccessToken.Token, nil
}

func pretendoFetchProfile(deviceID, serial, deviceCert, token string) (profilePerson, error) {
	req, _ := http.NewRequest("GET", "/v1/api/people/@me/profile", nil)
	req.RequestURI = "/v1/api/people/@me/profile"
	req.Header = wiiUHeaders(deviceID, serial, deviceCert)
	req.Header.Set("Authorization", "Bearer "+token)
	body, _, _, err := doUpstream(req)
	if err != nil {
		return profilePerson{}, fmt.Errorf("profile fetch: %w", err)
	}
	os.WriteFile("/tmp/profile_raw.xml", body, 0644)
	var prof profilePerson
	if err := xml.Unmarshal(body, &prof); err != nil || prof.PID == 0 {
		return profilePerson{}, fmt.Errorf("parse profile")
	}
	log.Printf("profile parsed: PID=%d MiiName=%q ImageURL=%s", prof.PID, prof.Mii.Name, prof.Mii.ImageURL)
	return prof, nil
}

// initMiiImages runs once at startup to upload Mii images for all registered users.
// Paced at one user/minute — even a 2s delay between users still hit Pretendo's OAuth
// rate limit (confirmed 2026-08-20: 186 devices, 126-135 hit 429 Too Many Requests
// across two attempts). Runs fully in the background (go initMiiImages() in main()),
// so a 186-device backfill taking ~3h doesn't block or slow anything else down.
func initMiiImages() {
	rows, err := db.Query(`SELECT username, device_id, serial, device_cert, pw_hash FROM wii_devices WHERE device_cert != '' AND pw_hash != ''`)
	if err != nil {
		log.Printf("initMiiImages: db query: %v", err)
		return
	}
	defer rows.Close()
	first := true
	for rows.Next() {
		if !first {
			time.Sleep(1 * time.Minute)
		}
		first = false
		var username, deviceID, serial, deviceCert, pwHash string
		if err := rows.Scan(&username, &deviceID, &serial, &deviceCert, &pwHash); err != nil {
			continue
		}
		token, err := pretendoOAuthToken(username, deviceID, serial, deviceCert, pwHash)
		if err != nil {
			log.Printf("initMiiImages: oauth %s: %v", username, err)
			continue
		}
		prof, err := pretendoFetchProfile(deviceID, serial, deviceCert, token)
		if err != nil {
			log.Printf("initMiiImages: profile %s: %v", username, err)
			continue
		}
		storeMiiName(prof.PID, prof.Mii.Name)
		uploadMiiImages(prof.PID, prof.Mii.Data)
		log.Printf("initMiiImages: done for %s (PID=%d)", username, prof.PID)
	}
}

func handleInternalFriends(w http.ResponseWriter, r *http.Request) {
	pidStr := strings.TrimPrefix(r.URL.Path, "/internal/friends/")
	if pidStr == "" {
		http.Error(w, "missing pid", http.StatusBadRequest)
		return
	}
	pid, err := strconv.ParseUint(pidStr, 10, 64)
	if err != nil {
		http.Error(w, "bad pid", http.StatusBadRequest)
		return
	}
	rows, err := db.Query(`SELECT friend_pid, friend_nnid FROM pretendo_friends WHERE owner_pid = $1`, pid)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type friendEntry struct {
		PID  uint64 `json:"pid"`
		NNID string `json:"nnid"`
	}
	var out []friendEntry
	for rows.Next() {
		var e friendEntry
		if err := rows.Scan(&e.PID, &e.NNID); err == nil {
			out = append(out, e)
		}
	}
	if out == nil {
		out = []friendEntry{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

type lookupResult struct {
	PID     uint64 `json:"pid"`
	PNID    string `json:"pnid"`
	MiiName string `json:"mii_name"`
}

func handleInternalLookup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fail := func(msg string, code int) {
		w.WriteHeader(code)
		fmt.Fprintf(w, `{"error":%q}`, msg)
	}

	targetStr := strings.TrimPrefix(r.URL.Path, "/internal/lookup/")
	targetPID, err := strconv.ParseUint(targetStr, 10, 64)
	if err != nil || targetPID == 0 {
		fail("bad pid", http.StatusBadRequest)
		return
	}

	// 1. nex_accounts (skip if username is the numeric PID — bridge users have PID as username)
	var pnid string
	db.QueryRow(`SELECT username FROM nex_accounts WHERE pid = $1 AND username IS NOT NULL`, targetPID).Scan(&pnid)
	if pnid == strconv.FormatUint(targetPID, 10) {
		pnid = ""
	}

	// 2. pnid_cache
	if pnid == "" {
		db.QueryRow(`SELECT pnid FROM pnid_cache WHERE pid = $1`, targetPID).Scan(&pnid)
	}

	// 3. pretendo_friends nnid column
	if pnid == "" {
		db.QueryRow(`SELECT friend_nnid FROM pretendo_friends WHERE friend_pid = $1 AND friend_nnid != '' LIMIT 1`, targetPID).Scan(&pnid)
		if pnid != "" {
			db.Exec(`INSERT INTO pnid_cache (pid, pnid) VALUES ($1, $2) ON CONFLICT (pid) DO UPDATE SET pnid = EXCLUDED.pnid, updated_at = NOW()`, targetPID, pnid)
		}
	}

	if pnid != "" {
		var miiName string
		db.QueryRow(`SELECT mii_name FROM mii_names WHERE pid = $1`, targetPID).Scan(&miiName)
		json.NewEncoder(w).Encode(lookupResult{PID: targetPID, PNID: pnid, MiiName: miiName})
		return
	}

	// 4. NEX GetBasicInfo via Pretendo — pick caller credentials.
	callerStr := r.URL.Query().Get("caller")
	callerPID, _ := strconv.ParseUint(callerStr, 10, 64)

	var nexPassword, authHost string
	var authPort uint16
	if callerPID != 0 {
		db.QueryRow(`SELECT friends_nex_password, last_auth_host, last_auth_port FROM nex_accounts WHERE pid = $1 AND friends_nex_password IS NOT NULL AND last_auth_host IS NOT NULL`, callerPID).Scan(&nexPassword, &authHost, &authPort)
	}
	if nexPassword == "" {
		db.QueryRow(`SELECT pid, friends_nex_password, last_auth_host, last_auth_port FROM nex_accounts WHERE friends_nex_password IS NOT NULL AND last_auth_host IS NOT NULL ORDER BY RANDOM() LIMIT 1`).Scan(&callerPID, &nexPassword, &authHost, &authPort)
	}
	if nexPassword == "" {
		fail("no NEX credentials available", http.StatusNotFound)
		return
	}

	script := "/nico-pretendo-bridge/account-proxy/lookup_pnid.py"
	dbURI := os.Getenv("PN_WUC_POSTGRES_URI")
	out, err := exec.Command("python3", script,
		"--target-pid", fmt.Sprintf("%d", targetPID),
		"--caller-pid", fmt.Sprintf("%d", callerPID),
		"--nex-password", nexPassword,
		"--auth-host", authHost,
		"--auth-port", fmt.Sprintf("%d", authPort),
		"--db-uri", dbURI,
	).Output()
	if err != nil {
		log.Printf("lookup_pnid PID=%d: %v", targetPID, err)
		fail("NEX lookup failed", http.StatusNotFound)
		return
	}

	w.Write(out)
}

// keepAliveProcs tracks long-running fetch_friends.py --keep-alive processes (pid → *exec.Cmd).
var keepAliveProcs sync.Map

func killKeepAlive(pid uint64) {
	if v, ok := keepAliveProcs.LoadAndDelete(pid); ok {
		if cmd := v.(*exec.Cmd); cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}
}

func launchKeepAlive(pid uint32, nexPassword, authHost string, authPort uint16) {
	killKeepAlive(uint64(pid))
	script := "/nico-pretendo-bridge/account-proxy/fetch_friends.py"
	cmd := exec.Command("python3", script,
		"--pid", fmt.Sprintf("%d", pid),
		"--nex-password", nexPassword,
		"--auth-host", authHost,
		"--auth-port", fmt.Sprintf("%d", authPort),
		"--db-uri", os.Getenv("PN_WUC_POSTGRES_URI"),
		"--keep-alive",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Printf("launchKeepAlive PID=%d: %v", pid, err)
		return
	}
	keepAliveProcs.Store(uint64(pid), cmd)
	go func() {
		_ = cmd.Wait()
		keepAliveProcs.CompareAndDelete(uint64(pid), cmd)
		log.Printf("keepAlive PID=%d: process exited", pid)
	}()
}

func handlePresenceStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	pidStr := strings.TrimPrefix(r.URL.Path, "/internal/presence/start/")
	pid, err := strconv.ParseUint(pidStr, 10, 64)
	if err != nil || pid == 0 {
		http.Error(w, "bad pid", http.StatusBadRequest)
		return
	}
	var nexPassword, authHost string
	var portInt int
	if err := db.QueryRow(`SELECT friends_nex_password, last_auth_host, last_auth_port
		FROM nex_accounts WHERE pid = $1 AND friends_nex_password IS NOT NULL AND last_auth_host IS NOT NULL`, pid).
		Scan(&nexPassword, &authHost, &portInt); err != nil {
		http.Error(w, "no credentials for pid", http.StatusNotFound)
		return
	}
	go launchKeepAlive(uint32(pid), nexPassword, authHost, uint16(portInt))
	w.WriteHeader(http.StatusAccepted)
}

func handlePresenceCommand(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	pidStr := strings.TrimPrefix(r.URL.Path, "/internal/presence/command/")
	pid, err := strconv.ParseUint(pidStr, 10, 64)
	if err != nil || pid == 0 {
		http.Error(w, "bad pid", http.StatusBadRequest)
		return
	}
	socketPath := fmt.Sprintf("/tmp/pretendo-presence-%d.sock", pid)
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		http.Error(w, "no keep-alive socket", http.StatusServiceUnavailable)
		return
	}
	defer conn.Close()
	body, _ := io.ReadAll(r.Body)
	conn.Write(append(bytes.TrimRight(body, "\n"), '\n'))
	resp, _ := io.ReadAll(conn)
	w.Header().Set("Content-Type", "application/json")
	w.Write(resp)
}

func handlePresenceStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	pidStr := strings.TrimPrefix(r.URL.Path, "/internal/presence/stop/")
	pid, err := strconv.ParseUint(pidStr, 10, 64)
	if err != nil || pid == 0 {
		http.Error(w, "bad pid", http.StatusBadRequest)
		return
	}
	killKeepAlive(pid)
	// Run a one-shot sync to flush pending commands (Pretendo marks us offline via TCP drop).
	var nexPassword, authHost string
	var portInt int
	if err := db.QueryRow(`SELECT friends_nex_password, last_auth_host, last_auth_port
		FROM nex_accounts WHERE pid = $1 AND friends_nex_password IS NOT NULL AND last_auth_host IS NOT NULL`, pid).
		Scan(&nexPassword, &authHost, &portInt); err == nil {
		go fetchFriendsPRUDP(uint32(pid), nexPassword, authHost, uint16(portInt))
	}
	w.WriteHeader(http.StatusOK)
}

func handleInternalSync(w http.ResponseWriter, r *http.Request) {
	pidStr := strings.TrimPrefix(r.URL.Path, "/internal/sync/")
	pid, err := strconv.ParseUint(pidStr, 10, 64)
	if err != nil || pid == 0 {
		http.Error(w, "bad pid", http.StatusBadRequest)
		return
	}

	var nexPassword, authHost string
	var authPort uint16
	var portInt int
	err = db.QueryRow(`SELECT friends_nex_password, last_auth_host, last_auth_port
		FROM nex_accounts WHERE pid = $1 AND friends_nex_password IS NOT NULL AND last_auth_host IS NOT NULL`, pid).
		Scan(&nexPassword, &authHost, &portInt)
	if err != nil {
		http.Error(w, "no credentials for pid", http.StatusNotFound)
		return
	}
	authPort = uint16(portInt)

	go fetchFriendsPRUDP(uint32(pid), nexPassword, authHost, authPort)
	w.WriteHeader(http.StatusAccepted)
}

func handleInternalAuth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fail := func(msg string, code int) {
		w.WriteHeader(code)
		fmt.Fprintf(w, `{"error":%q}`, msg)
	}

	if r.Method != http.MethodPost {
		fail("method not allowed", 405)
		return
	}
	r.ParseForm()
	userID := r.FormValue("user_id")
	password := r.FormValue("password")
	if userID == "" || password == "" {
		fail("user_id and password required", 400)
		return
	}

	var deviceID, serial, deviceCert, pwHash, webPwHash string
	db.QueryRow(`SELECT device_id, serial, device_cert, pw_hash, web_password_hash FROM wii_devices WHERE username = $1`, userID).
		Scan(&deviceID, &serial, &deviceCert, &pwHash, &webPwHash)
	if deviceCert == "" || pwHash == "" {
		log.Printf("internal/auth: no Wii U credentials for %q", userID)
		fail("no device registered — connect via Wii U first", 401)
		return
	}
	if webPwHash == "" {
		log.Printf("internal/auth: no web password set for %q", userID)
		fail("no web password set for this account", 401)
		return
	}

	ip := realIP(r)

	entered := sha256.Sum256([]byte(password))
	if hex.EncodeToString(entered[:]) != webPwHash {
		log.Printf("internal/auth: wrong web password for %q", userID)
		var pid uint32
		db.QueryRow(`SELECT pid FROM pnid_cache WHERE pnid = $1`, userID).Scan(&pid)
		db.Exec(`INSERT INTO web_logins (pid, ip, success) VALUES ($1, $2, FALSE)`, pid, ip)
		fail("invalid username or password", 401)
		return
	}

	// Password matched locally - derive the PID from cache instead of asking Pretendo.
	// The only thing this endpoint's caller (grpc-stubs' apiLogin) reads from our response
	// is pid/pnid (access_token is discarded), so there's nothing Pretendo-specific left to
	// fetch here. This matters because the whole point of web_password_hash is to grant
	// Juxt access independent of Pretendo account state - a banned-from-Pretendo user (who
	// may get unbanned later but still wants Juxt access now) would otherwise pass the
	// local check above and then get rejected anyway by a real Pretendo OAuth call.
	var pid uint32
	db.QueryRow(`SELECT pid FROM pnid_cache WHERE pnid = $1`, userID).Scan(&pid)
	if pid == 0 {
		log.Printf("internal/auth: no cached PID for %q, cannot authenticate locally", userID)
		fail("authentication failed", 401)
		return
	}

	db.Exec(`INSERT INTO web_logins (pid, ip, success) VALUES ($1, $2, TRUE)`, pid, ip)
	log.Printf("internal/auth: authenticated %s (PID %d) from %s (local, Pretendo skipped)", userID, pid, ip)
	fmt.Fprintf(w, `{"pid":%d,"pnid":%q,"access_token":""}`, pid, userID)
}

// ── Cached Pretendo token for internal use ─────────────────────

type cachedTokenState struct {
	token    string
	exp      time.Time
	deviceID string
	serial   string
	cert     string
}

var (
	cachedToken   cachedTokenState
	cachedTokenMu sync.Mutex
)

func ensureCachedToken() (cachedTokenState, error) {
	cachedTokenMu.Lock()
	defer cachedTokenMu.Unlock()
	if cachedToken.token != "" && time.Now().Before(cachedToken.exp) {
		return cachedToken, nil
	}
	var userID, deviceID, serial, deviceCert, pwHash string
	err := db.QueryRow(`
		SELECT username, device_id, serial, device_cert, pw_hash
		FROM wii_devices
		WHERE device_cert != '' AND pw_hash != ''
		ORDER BY RANDOM() LIMIT 1
	`).Scan(&userID, &deviceID, &serial, &deviceCert, &pwHash)
	if err != nil {
		return cachedTokenState{}, fmt.Errorf("no device with credentials: %w", err)
	}
	token, err := pretendoOAuthToken(userID, deviceID, serial, deviceCert, pwHash)
	if err != nil {
		return cachedTokenState{}, fmt.Errorf("oauth for %s: %w", userID, err)
	}
	cachedToken = cachedTokenState{
		token:    token,
		exp:      time.Now().Add(50 * time.Minute),
		deviceID: deviceID,
		serial:   serial,
		cert:     deviceCert,
	}
	log.Printf("internal/mii: cached new Pretendo token via %s", userID)
	return cachedToken, nil
}

// fetchPretendoProfile fetches /v1/api/people/@me/profile using the given credentials.
func fetchPretendoProfile(deviceID, serial, deviceCert, pwHash, userID string) (profilePerson, string, error) {
	token, err := pretendoOAuthToken(userID, deviceID, serial, deviceCert, pwHash)
	if err != nil {
		return profilePerson{}, "", fmt.Errorf("oauth: %w", err)
	}
	h := wiiUHeaders(deviceID, serial, deviceCert)
	h.Set("Authorization", "Bearer "+token)
	body, status, err := doUpstreamGetPath("/v1/api/people/@me/profile", h)
	if err != nil {
		return profilePerson{}, "", fmt.Errorf("upstream: %w", err)
	}
	if status != 200 {
		return profilePerson{}, "", fmt.Errorf("pretendo %d: %s", status, body)
	}
	var person profilePerson
	if err := xml.Unmarshal(body, &person); err != nil || person.Mii.Data == "" {
		return profilePerson{}, "", fmt.Errorf("parse or no mii data")
	}
	return person, token, nil
}

func handleInternalMii(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	pnid := r.URL.Query().Get("pnid")
	if pnid == "" {
		http.Error(w, `{"error":"pnid required"}`, 400)
		return
	}

	// 1. Local user with Pretendo device credentials — authenticate as themselves.
	var deviceID, serial, deviceCert, pwHash string
	db.QueryRow(`SELECT device_id, serial, device_cert, pw_hash FROM wii_devices WHERE username = $1 AND device_cert != '' AND pw_hash != ''`, pnid).
		Scan(&deviceID, &serial, &deviceCert, &pwHash)
	if deviceCert != "" {
		prof, _, err := fetchPretendoProfile(deviceID, serial, deviceCert, pwHash, pnid)
		if err != nil {
			log.Printf("internal/mii: self-auth failed for %s: %v", pnid, err)
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), 502)
			return
		}
		// Store in mii_cache so the bot hits it directly next time.
		if miiBytes, err2 := base64.StdEncoding.DecodeString(prof.Mii.Data); err2 == nil {
			db.Exec(`INSERT INTO mii_cache (pid, pnid, mii_name, mii_data, updated_at)
				VALUES ($1, $2, $3, $4, NOW())
				ON CONFLICT (pid) DO UPDATE SET pnid=EXCLUDED.pnid, mii_name=EXCLUDED.mii_name, mii_data=EXCLUDED.mii_data, updated_at=NOW()`,
				prof.PID, prof.PNID, prof.Mii.Name, miiBytes)
		}
		fmt.Fprintf(w, `{"name":%q,"data":%q}`, prof.Mii.Name, prof.Mii.Data)
		return
	}

	// 2. Check mii_cache (fresh = updated within 24 h).
	var cachedPID uint64
	var cachedName string
	var cachedData []byte
	err := db.QueryRow(`SELECT pid, mii_name, mii_data FROM mii_cache
		WHERE pnid = $1 AND updated_at > NOW() - INTERVAL '15 minutes'`, pnid).
		Scan(&cachedPID, &cachedName, &cachedData)
	if err == nil && len(cachedData) > 0 {
		log.Printf("internal/mii: cache hit for pnid=%s pid=%d", pnid, cachedPID)
		fmt.Fprintf(w, `{"name":%q,"data":%q}`, cachedName, base64.StdEncoding.EncodeToString(cachedData))
		return
	}

	// 3. Look up PID from any local table: pnid_cache, pretendo_friends.
	var targetPID uint64
	db.QueryRow(`SELECT pid FROM pnid_cache WHERE pnid = $1`, pnid).Scan(&targetPID)
	if targetPID == 0 {
		db.QueryRow(`SELECT friend_pid FROM pretendo_friends WHERE friend_nnid = $1 LIMIT 1`, pnid).Scan(&targetPID)
	}

	// 3.5. PID still unknown — Pretendo has no REST endpoint that exposes PNID→PID,
	// and add_friend_by_name is NotImplemented on their server. If targetPID==0 we
	// fall through and return an error below after NEX cred lookup.

	// 4. Use a random local user's NEX credentials to call fetch_mii.py via PRUDP.
	var callerPID uint64
	var callerNexPw, authHost string
	var authPort int
	db.QueryRow(`SELECT pid, friends_nex_password, last_auth_host, last_auth_port
		FROM nex_accounts
		WHERE friends_nex_password IS NOT NULL AND last_auth_host IS NOT NULL
		ORDER BY RANDOM() LIMIT 1`).Scan(&callerPID, &callerNexPw, &authHost, &authPort)
	if callerPID == 0 {
		http.Error(w, `{"error":"no NEX credentials available"}`, 502)
		return
	}

	script := "/nico-pretendo-bridge/account-proxy/fetch_mii.py"
	var args []string
	if targetPID != 0 {
		args = []string{"python3", script,
			"--target-pid", fmt.Sprintf("%d", targetPID),
			"--caller-pid", fmt.Sprintf("%d", callerPID),
			"--nex-password", callerNexPw,
			"--auth-host", authHost,
			"--auth-port", fmt.Sprintf("%d", authPort),
		}
	} else {
		log.Printf("internal/mii: no PID for pnid=%s — using add_friend_by_name PNID mode", pnid)
		args = []string{"python3", script,
			"--target-pnid", pnid,
			"--caller-pid", fmt.Sprintf("%d", callerPID),
			"--nex-password", callerNexPw,
			"--auth-host", authHost,
			"--auth-port", fmt.Sprintf("%d", authPort),
		}
	}
	cmd := exec.Command(args[0], args[1:]...)
	var fetchStderr bytes.Buffer
	cmd.Stderr = &fetchStderr
	out, execErr := cmd.Output()
	if execErr != nil {
		log.Printf("internal/mii: fetch_mii.py failed pnid=%s pid=%d: %v — stderr: %s", pnid, targetPID, execErr, fetchStderr.String())
		http.Error(w, `{"error":"PRUDP fetch failed"}`, 502)
		return
	}

	var result struct {
		PNID    string `json:"pnid"`
		MiiName string `json:"mii_name"`
		MiiData string `json:"mii_data"` // hex
	}
	if jsonErr := json.Unmarshal(out, &result); jsonErr != nil || result.MiiData == "" {
		log.Printf("internal/mii: bad fetch_mii.py output for pid=%d: %v %s", targetPID, jsonErr, out)
		http.Error(w, `{"error":"no mii data returned"}`, 404)
		return
	}

	miiBytes, _ := hex.DecodeString(result.MiiData)
	// Store in mii_cache.
	db.Exec(`INSERT INTO mii_cache (pid, pnid, mii_name, mii_data, updated_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (pid) DO UPDATE SET pnid=EXCLUDED.pnid, mii_name=EXCLUDED.mii_name, mii_data=EXCLUDED.mii_data, updated_at=NOW()`,
		targetPID, result.PNID, result.MiiName, miiBytes)
	// Also cache PNID if we got one.
	if result.PNID != "" {
		db.Exec(`INSERT INTO pnid_cache (pid, pnid) VALUES ($1, $2)
			ON CONFLICT (pid) DO UPDATE SET pnid=EXCLUDED.pnid, updated_at=NOW()`,
			targetPID, result.PNID)
	}
	log.Printf("internal/mii: PRUDP fetched mii for pnid=%s pid=%d (%d bytes)", pnid, targetPID, len(miiBytes))

	miiB64 := base64.StdEncoding.EncodeToString(miiBytes)
	fmt.Fprintf(w, `{"name":%q,"data":%q}`, result.MiiName, miiB64)
}

// handleWebStatus returns whether a web password is set and last 10 login attempts for a PID.
func handleWebStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	pidStr := r.URL.Query().Get("pid")
	pid, err := strconv.ParseUint(pidStr, 10, 32)
	if err != nil {
		http.Error(w, `{"error":"invalid pid"}`, 400)
		return
	}
	var webPwHash string
	db.QueryRow(`SELECT w.web_password_hash FROM wii_devices w JOIN pnid_cache p ON p.pnid = w.username WHERE p.pid = $1`, pid).Scan(&webPwHash)
	hasPassword := webPwHash != ""

	type loginEntry struct {
		IP       string    `json:"ip"`
		LoggedAt time.Time `json:"logged_at"`
		Success  bool      `json:"success"`
	}
	var logins []loginEntry
	rows, err := db.Query(`SELECT ip, logged_at, success FROM web_logins WHERE pid = $1 ORDER BY logged_at DESC LIMIT 10`, pid)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var e loginEntry
			rows.Scan(&e.IP, &e.LoggedAt, &e.Success)
			logins = append(logins, e)
		}
	}
	if logins == nil {
		logins = []loginEntry{}
	}
	type statusResp struct {
		HasPassword bool         `json:"has_password"`
		Logins      []loginEntry `json:"logins"`
	}
	json.NewEncoder(w).Encode(statusResp{HasPassword: hasPassword, Logins: logins})
}

// handleWebSetPassword sets a web password for a PID, called from juxt-ui console portal.
func handleWebSetPassword(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, 405)
		return
	}
	r.ParseForm()
	pidStr := r.FormValue("pid")
	password := r.FormValue("password")
	pid, err := strconv.ParseUint(pidStr, 10, 32)
	if err != nil || password == "" {
		http.Error(w, `{"error":"pid and password required"}`, 400)
		return
	}
	if len(password) < 8 {
		http.Error(w, `{"error":"password must be at least 8 characters"}`, 400)
		return
	}
	h := sha256.Sum256([]byte(password))
	res, err := db.Exec(`UPDATE wii_devices SET web_password_hash = $1 WHERE username = (SELECT pnid FROM pnid_cache WHERE pid = $2)`, hex.EncodeToString(h[:]), pid)
	if err != nil {
		log.Printf("web/set-password: db error for PID %d: %v", pid, err)
		http.Error(w, `{"error":"db error"}`, 500)
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		http.Error(w, `{"error":"no registered device found for this account"}`, 404)
		return
	}
	log.Printf("web/set-password: password set for PID %d", pid)
	fmt.Fprintf(w, `{"ok":true}`)
}

// cdnPrefixes are paths that should be proxied directly to S3 (not juxt-ui).
// The Wii U can't reach the S3 host directly due to TLS CA trust issues.
var cdnPrefixes = []string{"/mii/", "/paintings/", "/screenshots/", "/icons/", "/headers/"}

// wiiUCiphers are the CBC suites the Wii U understands (no GCM, no ChaCha). Used both
// for the inbound OLV proxy TLS config (startOLVProxy) and, via wiiUHTTPClient, for
// outbound requests we make pretending to be a Wii U — Pretendo's edge blocks BOSS
// requests by TLS fingerprint, not headers (confirmed 2026-08-20: forwarding a real
// console's exact User-Agent still got "only meant to be accessed by a Wii U or 3DS",
// but restricting our own outbound handshake to this cipher/version range is what
// actually matters to whatever's doing the fingerprinting).
var wiiUCiphers = []uint16{
	tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
	tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
	tls.TLS_RSA_WITH_AES_128_CBC_SHA,
	tls.TLS_RSA_WITH_AES_256_CBC_SHA,
	tls.TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA,
	tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA,
}

// wiiUHTTPClient makes outbound requests with a TLS ClientHello that looks like a Wii U
// (TLS 1.0-1.2, CBC-only ciphers) rather than Go's default modern/AEAD profile.
// Timeout matters here specifically because this client is also used to reverse-proxy
// arbitrary unrecognized *.pretendo.cc/*.nicochristmann.net hosts (see
// handleGenericPretendoProxy/handleGenericNicochristmannProxy) - an unresponsive real
// Pretendo endpoint (confirmed: onl-npns.app.pretendo.cc just hangs, no response at
// all) would otherwise hold the console's request open indefinitely instead of
// failing fast.
var wiiUHTTPClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion:   tls.VersionTLS10,
			MaxVersion:   tls.VersionTLS12,
			CipherSuites: wiiUCiphers,
		},
	},
}

// realNintendoHTTPClient is wiiUHTTPClient's config plus InsecureSkipVerify: real
// Nintendo BOSS servers (npts.app.nintendo.net) still serve legacy certs with only a
// CommonName and no SANs, which Go's crypto/x509 rejects outright since ~Go 1.15
// ("certificate relies on legacy Common Name field, use SANs instead") - curl has no
// such check and connects fine. We're intentionally querying Nintendo's own real,
// known infrastructure read-only here, so skipping verification is acceptable.
var realNintendoHTTPClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS10,
			MaxVersion:         tls.VersionTLS12,
			CipherSuites:       wiiUCiphers,
			InsecureSkipVerify: true,
		},
	},
}

// wiiuTaskSheetTitleIDRe matches the outer <TaskSheet><TitleId> real Nintendo returns.
var wiiuTaskSheetTitleIDRe = regexp.MustCompile(`<TitleId>([0-9a-fA-F]{16})</TitleId>`)

// realTaskSheetTitleIDCache caches the outer <TaskSheet><TitleId> real Nintendo
// returns per bossAppId - confirmed live 2026-08-24 this is fixed per-bossAppId
// (i.e. per-console/region) and does NOT depend on any request attribute we
// control (UA firmware-region letter, c=/l= query params all had no effect) -
// it's whatever region Nintendo registered that specific console/bossAppId
// under. Different real consoles genuinely differ here (confirmed: a EUR
// console's bossAppId got 000500101004d200, a NA one got ...d100) - this used
// to be hardcoded to a single default, which was silently wrong for any
// console not matching that one guess. Cached forever per bossAppId since
// this is a fixed identity, not something that changes over time.
var realTaskSheetTitleIDCache sync.Map

// lookupRealWiiUTaskSheetTitleID queries the real Nintendo BOSS server for the
// outer TaskSheet TitleId a given bossAppId is really registered under, so we
// echo back the same value the console already expects instead of a guessed
// default. Best-effort: on any failure (network, parse, timeout) ok is false
// and the caller should fall back to its own default - sysmsg1/2/3 are
// stateless/unauthenticated tasks (confirmed live: real Nintendo answers them
// with no device identity needed), so this lookup is safe and typically fast,
// but must never block or break serving our own content if it fails.
func lookupRealWiiUTaskSheetTitleID(bossAppID, taskID string) (string, bool) {
	if v, ok := realTaskSheetTitleIDCache.Load(bossAppID); ok {
		return v.(string), true
	}
	url := fmt.Sprintf("https://npts.app.nintendo.net/p01/tasksheet/1/%s/%s?c=US&l=en", bossAppID, taskID)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("User-Agent", "PBOSU-4.0/00000000-00000000-0000000000000000/5.5.6U")
	req.Header.Set("Accept", "*/*")
	resp, err := realNintendoHTTPClient.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", false
	}
	m := wiiuTaskSheetTitleIDRe.FindSubmatch(body)
	if m == nil {
		return "", false
	}
	titleID := string(m[1])
	realTaskSheetTitleIDCache.Store(bossAppID, titleID)
	return titleID, true
}

// bossCaptureDir holds raw request/response logs from handleBossCapture, for offline
// decryption with a dumped BOSS key once we've seen what real Wara Wara Plaza traffic
// looks like. TEMPORARY — see handleBossCapture doc comment.
const bossCaptureDir = "/nico-pretendo-bridge/boss-capture"

// hppCaptureDir holds raw request logs from handleHPPCapture. TEMPORARY — first
// pass at bringing up HPP (Swapdoodle and friends): the Nimbus socket:U patch
// redirects any hpp-<gameid>-l1.n.app.nintendowifi.net DNS lookup to
// hpp-relay.nicochristmann.net, but leaves the actual HTTP Host header/TLS SNI
// (and the whole HPP wire format) untouched, so we don't yet know what a real
// request looks like. This just logs full requests until we've seen one.
const hppCaptureDir = "/nico-pretendo-bridge/hpp-capture"

// olv3dsCaptureDir: full request/response captures for real Nintendo 3DS
// traffic through handleOLV. TEMPORARY — added to diagnose an ARM11 data
// abort in the 3DS's olv process, which happened after the console
// successfully received a real response for the first time (previously
// blocked by an empty-SNI nginx routing gap). Scoped to 3DS User-Agents
// only so it doesn't capture the much higher volume of normal Wii U
// traffic. Remove once resolved.
const olv3dsCaptureDir = "/nico-pretendo-bridge/olv-capture-3ds"

func logOLV3DSExchange(r *http.Request, reqBody []byte, status int, respHeader http.Header, respBody []byte) {
	os.MkdirAll(olv3dsCaptureDir, 0755)
	ts := time.Now().Format("20060102-150405.000")
	safePath := strings.ReplaceAll(strings.Trim(r.URL.Path, "/"), "/", "_")
	base := fmt.Sprintf("%s/%s_%s", olv3dsCaptureDir, ts, safePath)
	reqLog := fmt.Sprintf("%s %s\nQuery: %s\nHeaders: %v\nBody (hex): %x\n",
		r.Method, r.URL.Path, r.URL.RawQuery, r.Header, reqBody)
	os.WriteFile(base+".request.txt", []byte(reqLog), 0644)
	respLog := fmt.Sprintf("Status: %d\nHeaders: %v\nBody (hex): %x\nBody (raw): %s\n",
		status, respHeader, respBody, respBody)
	os.WriteFile(base+".response.txt", []byte(respLog), 0644)
}

// logBossRequest saves the full incoming request (method, path, query string,
// headers, body) to bossCaptureDir, matching the format handleBossCapture's
// generic proxy path already uses. The synthetic handlers (handleWSCSpotPass*)
// never captured this before, only a one-line summary - added to see the
// query string real Nintendo captures show (e.g. ?c=US&l=en) and full headers
// for a given real request, since WSC's actual request shape has repeatedly
// turned out to matter (bossAppId varying, real vs. assumed field values).
func logBossRequest(r *http.Request) {
	reqBody, _ := io.ReadAll(r.Body)
	r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(reqBody))

	os.MkdirAll(bossCaptureDir, 0755)
	ts := time.Now().Format("20060102-150405.000")
	safePath := strings.ReplaceAll(strings.Trim(r.URL.Path, "/"), "/", "_")
	base := fmt.Sprintf("%s/%s_%s", bossCaptureDir, ts, safePath)

	reqLog := fmt.Sprintf("%s %s\nQuery: %s\nHeaders: %v\nBody (hex): %x\n",
		r.Method, r.URL.Path, r.URL.RawQuery, r.Header, reqBody)
	os.WriteFile(base+".request.txt", []byte(reqLog), 0644)
}

// handleBossCapture is a TEMPORARY diagnostic proxy for reverse-engineering the Wara
// Wara Plaza BOSS content format (see feedback_wsc_ping_timeout-style memory once this
// lands — not yet saved). Inkay-NicoChristmann v3.0.0-9 redirects the client's nppl/npts
// BOSS lookups to boss.nicochristmann.net instead of the real nppl.app.pretendo.cc /
// npts.app.pretendo.cc. This handler transparently forwards to the REAL Pretendo BOSS
// servers (so nothing about the console's plaza experience changes) while logging the
// full raw request and response to bossCaptureDir. The response's <TaskSheet><File> Url
// still points at the real npdi.cdn.pretendo.cc (untouched, unpatched), so the actual
// encrypted file download isn't captured here — only the policylist + tasksheet legs.
// Remove this (and the matching Inkay patch + nginx stream map entry) once the plaza
// content format has been reverse-engineered from the captured samples.
// wscSpotPassTasks: WSC's own SpotPass tasks (sp1_ans, sp1_rnk) that Pretendo's
// real BOSS server has never implemented (404 on the task itself, not just a
// missing file) — this is different from olvinfo-style "task exists but is
// empty". We inject a valid, empty, open TaskSheet ourselves so the console
// treats it as "no new content" instead of "task unavailable". Matched by
// taskID alone - bossAppId is per-registration, not a fixed constant (the
// same sp1_rnk task has been seen with multiple different bossAppId values).
var wscSpotPassTasks = map[string]bool{
	"sp1_ans": true,
	"sp1_rnk": true,
}

// handleWSCSpotPassTasksheet serves a synthetic empty TaskSheet for WSC's own
// SpotPass tasks at the task level (no filename in the URL). Returns true if
// it handled the request.
func handleWSCSpotPassTasksheet(w http.ResponseWriter, r *http.Request) bool {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// /p01/tasksheet/:id/:bossAppId/:taskId  (exactly 5 parts, no filename)
	if len(parts) != 5 || parts[0] != "p01" || parts[1] != "tasksheet" {
		return false
	}
	taskID := parts[4]
	// bossAppId is per-registration, not a fixed constant - confirmed 2026-08-21:
	// the same sp1_rnk task has been seen with both "4m8Xme1wKgzwslTJ" and
	// "pO72Hi5uqf5yuNd8" (the latter matching the real Nintendo capture too).
	// Match on taskID alone.
	if !wscSpotPassTasks[taskID] {
		return false
	}
	logBossRequest(r)
	xml := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<TaskSheet>
  <TitleId>%s</TitleId>
  <TaskId>%s</TaskId>
  <ServiceStatus>open</ServiceStatus>
  <Files>
  </Files>
</TaskSheet>
`, wscTitleIDForCountry(r.URL.Query().Get("c")), taskID)
	log.Printf("BOSS capture: %s %s -> injected empty TaskSheet (Pretendo doesn't implement WSC SpotPass)", r.Method, r.URL.Path)
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	// nn::boss's HTTP client caps simultaneous connections per host
	// (TaskResultCode HTTP_ERROR_CONN_HOST_MAX) - several BOSS requests land
	// in quick succession during boot (policylist, task-level checks,
	// tasksheet, file download), so signal a clean close on each response
	// rather than leaving it to default keep-alive, which was likely
	// exhausting that per-host connection limit.
	w.Header().Set("Connection", "close")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(xml))
	return true
}

// wscEURCountryCodes: ISO country codes covered by Wii Sports Club's EUR-region
// release (Nintendo's PAL SKU covers all of Europe plus Australia/NZ/South
// Africa). Anything not in this set falls back to the USA TitleId.
var wscEURCountryCodes = map[string]bool{
	"GB": true, "DE": true, "FR": true, "IT": true, "ES": true, "NL": true,
	"BE": true, "AT": true, "CH": true, "PT": true, "IE": true, "SE": true,
	"NO": true, "DK": true, "FI": true, "PL": true, "RU": true, "GR": true,
	"CZ": true, "HU": true, "SK": true, "RO": true, "BG": true, "HR": true,
	"SI": true, "LT": true, "LV": true, "EE": true, "LU": true, "MT": true,
	"CY": true, "ZA": true, "AU": true, "NZ": true,
}

// wscTitleIDForCountry returns WSC's real, region-specific TitleId - confirmed
// 2026-08-22 from a real captured TaskSheet for Nico's own bossAppId
// (0005000010144e00, country=DE), which differs from the USA TitleId
// (0005000010144d00, confirmed via requests/responses that succeeded for a
// real US/CA console) only in that one byte. Serving the wrong region's
// TitleId to a console running the other region's actual game is a very
// plausible reason nn::boss would silently abort the download after
// receiving an otherwise-valid TaskSheet (see project_wsc_boss_version_diff
// memory - this supersedes the earlier firmware-version theory as the
// primary suspect).
func wscTitleIDForCountry(country string) string {
	if wscEURCountryCodes[strings.ToUpper(country)] {
		return "0005000010144e00"
	}
	return "0005000010144d00"
}

// wscSpotPassFileNames: known per-task filenames the console requests directly
// (bypassing the task-level listing entirely — confirmed 2026-08-21 from real
// console captures, it never hits handleWSCSpotPassTasksheet for WSC at all).
var wscSpotPassFileNames = map[string]string{
	"sp1_rnk": "rankingdata.dat",
}

var (
	bossKeysOnce sync.Once
	bossAESKey   []byte
	bossHMACKey  []byte
	bossKeysErr  error
)

func loadBossKeys() ([]byte, []byte, error) {
	bossKeysOnce.Do(func() {
		data, err := os.ReadFile("/home/nico/boss_keys.bin")
		if err != nil {
			bossKeysErr = err
			return
		}
		if len(data) != 96 {
			bossKeysErr = fmt.Errorf("boss_keys.bin: expected 96 bytes, got %d", len(data))
			return
		}
		bossAESKey = data[0:16]
		bossHMACKey = data[32:96]
	})
	return bossAESKey, bossHMACKey, bossKeysErr
}

// encryptBossFile implements the WiiU BOSS file format from PretendoNetwork/boss-crypto's
// encryptWiiU: a 32-byte header (magic "boss", version, flags, 12-byte random IV) followed
// by AES-128-CTR(HMAC-SHA256(content) || content), IV padded to 16 bytes with a big-endian
// counter starting at 1.
func encryptBossFile(content []byte) ([]byte, error) {
	aesKey, hmacKey, err := loadBossKeys()
	if err != nil {
		return nil, err
	}

	mac := hmac.New(sha256.New, hmacKey)
	mac.Write(content)
	plaintext := append(mac.Sum(nil), content...)

	iv := make([]byte, 12)
	if _, err := rand.Read(iv); err != nil {
		return nil, err
	}
	ctrIV := append(append([]byte{}, iv...), 0x00, 0x00, 0x00, 0x01)

	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, err
	}
	stream := cipher.NewCTR(block, ctrIV)
	encrypted := make([]byte, len(plaintext))
	stream.XORKeyStream(encrypted, plaintext)

	header := make([]byte, 0x20)
	copy(header[0:4], "boss")
	binary.BigEndian.PutUint32(header[4:8], 0x20001)
	binary.BigEndian.PutUint16(header[8:10], 1)
	binary.BigEndian.PutUint16(header[10:12], 2)
	copy(header[12:24], iv)

	return append(header, encrypted...), nil
}

// rankingDataTemplate is the real Wii Sports Club sp1_rnk/rankingdata.dat
// content, captured from Nintendo's still-live BOSS CDN and HMAC-verified to
// decrypt correctly with our own boss_keys.bin (confirming those are the
// genuine Nintendo keys). Used as a structural template: generateRankingData
// reuses its bytes verbatim except for each populated rank slot's first
// sub-record's embedded Mii name, which gets patched in for our own
// top-ranked players. The histogram values, the rest of the per-sub-record
// fields, and the exact meaning of "3 sub-records per slot" are not fully
// reverse-engineered yet (see plans/atomic-marinating-fog.md) so are
// preserved unmodified rather than guessed.
//
//go:embed assets/rankingdata_template.bin
var rankingDataTemplate []byte

// rankingSlotMarker is the 4-byte marker found right after every sub-record's
// 2-byte little-endian record ID in the real captured rankingdata.dat.
var rankingSlotMarker = []byte{0x1e, 0x00, 0x49, 0x06}

const (
	rankingSubRecordSize = 336
	rankingNameOffset    = 0xfa // relative to sub-record start, confirmed via real capture
	rankingNameMaxRunes  = 10   // matches the Mii nickname's own max length

	// rankingMiiStructOffset/Size: byte-level analysis of the real captured
	// template (2026-08-23) identified a complete, standard 96-byte Mii
	// structure at this offset within every sub-record - rankingNameOffset
	// (0xfa) is exactly this struct's own well-known nickname field offset
	// (0x1a), confirming the boundary. Also exactly matches the format and
	// size of our own user_settings.mii_data column (verified byte-for-byte
	// against Nico's own stored Mii: name/creator name decode correctly at
	// their expected offsets 0x1a/0x48). This means a real player's own
	// mii_data can be copied in wholesale - name, face, creator name, and its
	// own trailing CRC16 all still self-consistent - rather than patching
	// just the name text into Nintendo's original captured Mii.
	rankingMiiStructOffset = rankingNameOffset - 0x1a // 0xe0
	rankingMiiStructSize   = 96

	// rankingScoreDataOffset/Size: verified across 4 different slots in the
	// real captured template (2026-08-23) - every one has 4 little-endian
	// uint32 aggregate counters at 0x20 (e.g. games-played/total-score style
	// values, wildly different per slot: 7785/0/538/4 vs 46829/0/405349/1613)
	// immediately followed by a 24-value uint32 histogram at 0x30 that forms
	// a clean bell curve in every slot checked - unambiguously a real
	// player's own stat distribution, not structural/length data. Zeroed for
	// every slot regardless of whether that slot got one of our own players'
	// Mii (2026-08-23: "we also have to zero out their scores") - a blank
	// stat is an honest "no data", not a stale Nintendo player's real
	// numbers attributed to whichever name/Mii is now shown.
	rankingScoreDataOffset = 0x20
	rankingScoreDataSize   = 0x70 // (0x90 - 0x20): 4 aggregate u32s + 24 histogram u32s
)

// rankingTemplateSlots returns, for each rank slot found in the template
// (a run of 3 consecutive sub-records), the file offset of its first
// sub-record.
func rankingTemplateSlots() [][3]int {
	var starts []int
	i := 0
	for {
		idx := bytes.Index(rankingDataTemplate[i:], rankingSlotMarker)
		if idx == -1 {
			break
		}
		abs := i + idx
		starts = append(starts, abs-2) // back up over the 2-byte record ID
		i = abs + 1
	}
	var slots [][3]int
	for i := 0; i+2 < len(starts); i += 3 {
		slots = append(slots, [3]int{starts[i], starts[i+1], starts[i+2]})
	}
	return slots
}

// patchMiiData overwrites the full 96-byte Mii structure in a cloned
// sub-record with a real player's own mii_data (see rankingMiiStructOffset) -
// replaces Nintendo's originally captured Mii entirely (name, face,
// creator name, CRC) rather than just retexting its name field. No-op if
// miiData isn't exactly 96 bytes (caller has none stored for that player).
func patchMiiData(subRecord []byte, miiData []byte) {
	if len(miiData) != rankingMiiStructSize {
		return
	}
	copy(subRecord[rankingMiiStructOffset:rankingMiiStructOffset+rankingMiiStructSize], miiData)
}

func bsonToUint32(v interface{}) uint32 {
	switch n := v.(type) {
	case int32:
		return uint32(n)
	case int64:
		return uint32(n)
	case float64:
		return uint32(n)
	}
	return 0
}

// topRankedPlayersFiltered queries wsc-secure's ranking_scores collection
// (shared Mongo database, wscMongoDB — see wsc-secure/db.go's
// dbInsertRankingScore) for the best score per player among rows matching
// the given predicate, sorted descending. groups[0] is the club code and
// groups[1] the region (2=US/3=EU) - see plans/atomic-marinating-fog.md for
// how this was confirmed (wsc-secure's own code comment mislabels groups[0]
// as a sport code).
func topRankedPlayersFiltered(limit int, matches func(clubCode, region uint32) bool) ([]uint32, error) {
	col := wscMongoDB.Collection("ranking_scores")
	cur, err := col.Find(context.Background(), bson.D{})
	if err != nil {
		return nil, err
	}
	defer cur.Close(context.Background())

	best := map[uint32]uint32{}
	seen := map[uint32]bool{}
	for cur.Next(context.Background()) {
		var doc bson.M
		if err := cur.Decode(&doc); err != nil {
			continue
		}
		groups, _ := doc["groups"].(bson.A)
		if len(groups) < 2 {
			continue
		}
		if !matches(bsonToUint32(groups[0]), bsonToUint32(groups[1])) {
			continue
		}
		pid := bsonToUint32(doc["pid"])
		score := bsonToUint32(doc["score"])
		if pid == 0 {
			continue
		}
		if !seen[pid] || score > best[pid] {
			best[pid] = score
			seen[pid] = true
		}
	}

	pids := make([]uint32, 0, len(best))
	for pid := range best {
		pids = append(pids, pid)
	}
	sort.Slice(pids, func(i, j int) bool { return best[pids[i]] > best[pids[j]] })
	if len(pids) > limit {
		pids = pids[:limit]
	}
	return pids, nil
}

// topRankedPlayers is the exact club+region match, used directly by tests.
func topRankedPlayers(clubCode, region uint32, limit int) ([]uint32, error) {
	return topRankedPlayersFiltered(limit, func(c, r uint32) bool { return c == clubCode && r == region })
}

// topRankedPlayersWithFallback tries an exact club+region match first (the
// real intent), then widens to the whole region, then to every real player
// regardless of region, returning the first non-empty result. Most of our
// ~200 seeded clubs have never had a real player upload a score for that
// exact club, so without this fallback generateRankingData would leave the
// entire response as Nintendo's original captured data whenever a request
// didn't happen to match one of the handful of clubs someone has actually
// played from (2026-08-23: "then thats still a nintendo leftover in
// spotpass data").
func topRankedPlayersWithFallback(clubCode, region uint32, limit int) ([]uint32, error) {
	pids, err := topRankedPlayersFiltered(limit, func(c, r uint32) bool { return c == clubCode && r == region })
	if err != nil {
		return nil, err
	}
	if len(pids) > 0 {
		return pids, nil
	}
	pids, err = topRankedPlayersFiltered(limit, func(_, r uint32) bool { return r == region })
	if err != nil {
		return nil, err
	}
	if len(pids) > 0 {
		return pids, nil
	}
	return topRankedPlayersFiltered(limit, func(_, _ uint32) bool { return true })
}

// miiNameForPID looks up a player's Mii nickname the same way
// handleInternalLookup does.
func miiNameForPID(pid uint32) string {
	var name string
	db.QueryRow(`SELECT mii_name FROM mii_names WHERE pid = $1`, pid).Scan(&name)
	return name
}

// miiDataForPID returns the player's real 96-byte Mii structure (the same
// format embedded in rankingDataTemplate's slots - see patchMiiData), or nil
// if they have none stored yet.
func miiDataForPID(pid uint32) []byte {
	var data []byte
	db.QueryRow(`SELECT mii_data FROM user_settings WHERE pid = $1`, pid).Scan(&data)
	if len(data) != rankingMiiStructSize {
		return nil
	}
	return data
}

// clubAndRegionForRequest determines the requesting console's own club/region
// from its most recent ranking_scores upload, resolving the PID the same way
// the rest of this file does (fetchRealPID, backed by pidCache which is
// usually already warm from the console's own NEX login moments earlier).
func clubAndRegionForRequest(r *http.Request) (uint32, uint32) {
	pid := fetchRealPID(r)
	if pid == 0 {
		return 0, 0
	}
	col := wscMongoDB.Collection("ranking_scores")
	opts := options.FindOne().SetSort(bson.D{{Key: "created_at", Value: -1}})
	var doc bson.M
	if err := col.FindOne(context.Background(), bson.D{{Key: "pid", Value: pid}}, opts).Decode(&doc); err != nil {
		return 0, 0
	}
	groups, _ := doc["groups"].(bson.A)
	if len(groups) < 2 {
		return 0, 0
	}
	return bsonToUint32(groups[0]), bsonToUint32(groups[1])
}

// generateRankingData builds a real-format WSC rankingdata.dat for the given
// club/region, using rankingDataTemplate as a structural base and patching in
// our own top-ranked players' Mii names.
//
// This was briefly disabled (serving the template verbatim) after the
// community reported the generated file was "dead wrong" and a re-check
// found rankingTemplateSlots' marker misses at least one real record
// boundary (a ~2272-byte gap at content offset 6080 that is NOT all zero and
// looks like a further sub-record with a slightly different header) - so our
// understanding of the full record layout is still incomplete. Re-enabled
// because the actual, better-evidenced cause of every TaskRunNBDL failure
// tonight (including with the template served byte-for-byte unmodified) is
// most likely Pretendo's own policylist server sending a malformed
// <UpdateTime> (invalid seconds values like :85, :94 - see
// fixPolicylistUpdateTime), which would block BOSS from running any task
// regardless of content. Patching is scoped to the one region we've fully
// verified against real data: the complete 96-byte Mii structure at
// rankingMiiStructOffset (see patchMiiData) - confirmed byte-for-byte to
// match our own user_settings.mii_data format - rather than anything
// touching the rest of the not-fully-understood record internals.
func generateRankingData(clubCode, region uint32) ([]byte, error) {
	slots := rankingTemplateSlots()
	if len(slots) == 0 {
		return nil, fmt.Errorf("rankingdata template: no slots found")
	}

	// Falls back to region-wide, then any real player, rather than the exact
	// club+region match alone - most of our ~200 seeded clubs have never had
	// a real score uploaded for that exact club, and without this fallback
	// every one of those requests left the whole response as Nintendo's
	// original captured data (2026-08-23: "then thats still a nintendo
	// leftover in spotpass data").
	players, err := topRankedPlayersWithFallback(clubCode, region, len(slots))
	if err != nil {
		return nil, err
	}

	out := make([]byte, len(rankingDataTemplate))
	copy(out, rankingDataTemplate)

	// Every slot gets one of our own players' complete real Mii (name, face,
	// creator name - see patchMiiData), cycling through the (usually much
	// shorter) real list rather than leaving the template's original Nintendo
	// player untouched past len(players) - otherwise most slots kept showing
	// a real Nintendo player's captured Mii, which is what we're explicitly
	// trying to not do (2026-08-23: "i want it to be ours only" / "can we
	// only include data from our server?" / "can we remove all nintendo
	// data?"). If we truly have zero real players anywhere (fresh deploy, no
	// scores uploaded yet) or a specific player has no mii_data row synced
	// yet, the slot is zeroed out instead of left as Nintendo's Mii - blank
	// is an honest "no data", not someone else's real identity.
	zeroMiiData := make([]byte, rankingMiiStructSize)
	zeroScoreData := make([]byte, rankingScoreDataSize)
	for i := range slots {
		miiData := zeroMiiData
		if len(players) > 0 {
			if real := miiDataForPID(players[i%len(players)]); real != nil {
				miiData = real
			}
		}
		firstSubStart := slots[i][0]
		sub := out[firstSubStart : firstSubStart+rankingSubRecordSize]
		patchMiiData(sub, miiData)
		copy(sub[rankingScoreDataOffset:rankingScoreDataOffset+rankingScoreDataSize], zeroScoreData)
	}

	return out, nil
}

// wscSpotPassDataCache maps the MD5 hash referenced in a TaskSheet's <Url> to
// the exact encrypted bytes that hash was computed from, so
// handleWSCSpotPassData serves the same content handleWSCSpotPassFile just
// promised — previously each handler called encryptBossFile independently
// with its own random IV, so the hash and the served bytes never matched.
var wscSpotPassDataCache sync.Map

// realNintendoDataID queries Nintendo's still-live real BOSS tasksheet server
// for the DataId it assigns a given bossAppId/task/file combination. nn::boss
// appears to track a per-bossAppId expected DataId in the console's own local
// state (confirmed: Nico's real bossAppId "4m8Xme1wKgzwslTJ" resolves to a
// real, different DataId/TitleId on Nintendo's server than a fresh/unrelated
// bossAppId does) - serving a hardcoded placeholder regardless of bossAppId
// may be why real, previously-Nintendo-registered consoles fail where a
// fresh/never-used bossAppId doesn't. Falls back to false if the real server
// can't be reached (e.g. WAF), so the caller can use a placeholder instead.
func realNintendoDataID(bossAppID, taskID, fileName, rawQuery string) (string, bool) {
	target := fmt.Sprintf("https://npts.app.nintendo.net/p01/tasksheet/1/%s/%s/%s", bossAppID, taskID, fileName)
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	resp, err := realNintendoHTTPClient.Get(target)
	if err != nil {
		log.Printf("BOSS capture: real Nintendo DataId lookup failed: %v", err)
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("BOSS capture: real Nintendo DataId lookup status=%d", resp.StatusCode)
		return "", false
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", false
	}
	var sheet struct {
		Files struct {
			File struct {
				DataId string `xml:"DataId"`
			} `xml:"File"`
		} `xml:"Files"`
	}
	if err := xml.Unmarshal(body, &sheet); err != nil {
		log.Printf("BOSS capture: real Nintendo DataId parse failed: %v", err)
		return "", false
	}
	if sheet.Files.File.DataId == "" {
		return "", false
	}
	return sheet.Files.File.DataId, true
}

// handleWSCSpotPassFile serves a real-format single-File TaskSheet for WSC's
// own SpotPass file lookups (e.g. sp1_rnk/rankingdata.dat) — the actual
// request shape the console uses, bypassing the task-level listing entirely.
// The Url points back at our own /p01/data/wsc/... path (boss.nicochristmann.net),
// since npdi.cdn.pretendo.cc isn't redirected and wouldn't have our file.
func handleWSCSpotPassFile(w http.ResponseWriter, r *http.Request) bool {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// /p01/tasksheet/:id/:bossAppId/:taskId/:fileName  (exactly 6 parts)
	if len(parts) != 6 || parts[0] != "p01" || parts[1] != "tasksheet" {
		return false
	}
	bossAppID, taskID, fileName := parts[3], parts[4], parts[5]
	// bossAppId is per-registration, not a fixed constant - see
	// handleWSCSpotPassTasksheet's comment. Match on taskID/fileName alone
	// and echo back whatever bossAppId the console actually used.
	if wscSpotPassFileNames[taskID] != fileName {
		return false
	}

	logBossRequest(r)

	clubCode, region := clubAndRegionForRequest(r)
	content, err := generateRankingData(clubCode, region)
	if err != nil {
		log.Printf("BOSS capture: generateRankingData failed, falling back to empty: %v", err)
		content = []byte{}
	}

	encrypted, err := encryptBossFile(content)
	if err != nil {
		log.Printf("BOSS capture: WSC spotpass file encrypt failed: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return true
	}
	hash := fmt.Sprintf("%x", md5.Sum(encrypted))
	wscSpotPassDataCache.Store(hash, encrypted)

	dataID := "1"
	if real, ok := realNintendoDataID(bossAppID, taskID, fileName, r.URL.RawQuery); ok {
		dataID = real
		log.Printf("BOSS capture: using real Nintendo DataId=%s for bossAppId=%s", dataID, bossAppID)
	}

	xmlBody := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<TaskSheet>
  <TitleId>%s</TitleId>
  <TaskId>%s</TaskId>
  <ServiceStatus>open</ServiceStatus>
  <Files>
    <File>
      <Filename>%s</Filename>
      <DataId>%s</DataId>
      <Type>AppData</Type>
      <Url>https://boss.nicochristmann.net/p01/data/wsc/%s/%s/%s</Url>
      <Size>%d</Size>
      <Notify>
        <New></New>
        <LED>false</LED>
      </Notify>
    </File>
  </Files>
</TaskSheet>
`, wscTitleIDForCountry(r.URL.Query().Get("c")), taskID, fileName, dataID, taskID, fileName, hash, len(encrypted))

	log.Printf("BOSS capture: %s %s -> generated real-format TaskSheet + %s (%d bytes encrypted, bossAppId=%s club=%d region=%d)", r.Method, r.URL.Path, fileName, len(encrypted), bossAppID, clubCode, region)
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Connection", "close") // see handleWSCSpotPassTasksheet's comment
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(xmlBody))
	return true
}

// handleWSCSpotPassData serves the actual encrypted rankingdata.dat bytes
// referenced by handleWSCSpotPassFile's injected <Url>.
func handleWSCSpotPassData(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/p01/data/wsc/") {
		return false
	}
	logBossRequest(r)
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	hash := parts[len(parts)-1]

	var encrypted []byte
	if v, ok := wscSpotPassDataCache.Load(hash); ok {
		encrypted = v.([]byte)
	} else {
		// No matching TaskSheet fetch happened first (shouldn't normally
		// occur) - regenerate with no club/region filter rather than fail.
		content, err := generateRankingData(0, 0)
		if err != nil {
			content = []byte{}
		}
		var encErr error
		encrypted, encErr = encryptBossFile(content)
		if encErr != nil {
			log.Printf("BOSS capture: WSC spotpass data encrypt failed: %v", encErr)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return true
		}
	}

	log.Printf("BOSS capture: %s %s -> served %d bytes", r.Method, r.URL.Path, len(encrypted))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Connection", "close") // see handleWSCSpotPassTasksheet's comment
	w.WriteHeader(http.StatusOK)
	w.Write(encrypted)
	return true
}

// wiiuSysMsgTasks: the Wii U Home Menu's own always-polled, generic system
// announcement tasks (TitleId 000500101004d100) - independent of any specific
// game, unlike WSC's own tasks above. Real format confirmed 2026-08-24 by
// decrypting a genuine 2023 Nintendo eShop-closure message recovered from
// PretendoNetwork/BOSS's own seed data with our own boss_keys.bin (valid
// HMAC). Content is admin-composed via relay-admin's wiiu_system_messages
// table (see /inkay/admin/spotpass-wiiu/), not synthesized here.
var wiiuSysMsgTasks = map[string]bool{
	"sysmsg1": true,
	"sysmsg2": true,
}

var wiiuSysMsgDataCache sync.Map

type wiiuSysMsgRow struct {
	id           int
	subject      string
	body         string
	titleID      string
	highPriority bool
	region       string
}

func activeWiiUSysMsgs() []wiiuSysMsgRow {
	rows, err := db.Query(`SELECT id, subject, body, title_id, high_priority, region FROM wiiu_system_messages WHERE active = true ORDER BY id ASC`)
	if err != nil {
		log.Printf("wiiu sysmsg: query failed: %v", err)
		return nil
	}
	defer rows.Close()
	var out []wiiuSysMsgRow
	for rows.Next() {
		var m wiiuSysMsgRow
		if err := rows.Scan(&m.id, &m.subject, &m.body, &m.titleID, &m.highPriority, &m.region); err == nil {
			out = append(out, m)
		}
	}
	return out
}

// wiiuHomeMenuTitleIDs are the known Home Menu self-referential TitleId
// variants (JPN/USA/EUR) - used to detect a message that's still at the DB
// column's generic placeholder default so its region can be corrected per
// requesting console, vs. one an admin deliberately set to reference a
// specific game (which must NOT be touched).
var wiiuHomeMenuTitleIDs = map[string]bool{
	"000500101004d000": true,
	"000500101004d100": true,
	"000500101004d200": true,
}

// wiiuRegionFromTaskSheetTitleID derives a region code from the outer
// TaskSheet TitleId real Nintendo returns for a given bossAppId - confirmed
// live 2026-08-24 that this differs by console region (a EUR console got
// ...d200, a NA one got ...d100) and is fixed per-bossAppId regardless of
// any request attribute. Only the Wii U USA/EUR split is confirmed; JPN
// (...d000) is an unconfirmed guess by the same pattern. Returns "" (unknown)
// for anything else, which callers treat as "don't filter out" rather than
// silently hiding messages because of an unrecognized/new TitleId variant.
func wiiuRegionFromTaskSheetTitleID(titleID string) string {
	switch {
	case strings.HasSuffix(titleID, "d000"):
		return "JPN"
	case strings.HasSuffix(titleID, "d100"):
		return "USA"
	case strings.HasSuffix(titleID, "d200"):
		return "EUR"
	default:
		return ""
	}
}

// regionMatches reports whether a message's configured region (empty = all
// regions) should be shown to a console detected as consoleRegion (empty =
// unknown/undetected, which matches everything rather than hiding messages
// on a failed lookup).
func regionMatches(messageRegion, consoleRegion string) bool {
	return messageRegion == "" || consoleRegion == "" || messageRegion == consoleRegion
}

// buildWiiUMessageXML builds the real <Message> inner XML format - see
// reference_wiiu_sysmsg_format memory / the genuine decrypted example this
// mirrors field-for-field.
//
// Links must NOT be left empty even when LinkType is NONE - the one genuine
// decrypted example we have (the real 2023 eShop-closure notice) also has
// LinkType NONE, but its <Links> still carries a populated <WoodLink>/
// <OliveLink> pair. Every real message we sent before this fix left <Links>
// completely empty, which is the one field that structurally diverged from
// every known genuine example - a plausible cause of the Notifications app
// hanging while loading the message list (built into an internal link
// record per-message regardless of LinkType, rather than only when a link
// is actually followed). Confirmed live 2026-08-25 that the app hangs on
// load for a message sent with these fields empty.
func buildWiiUMessageXML(m wiiuSysMsgRow) []byte {
	highPriority := "false"
	if m.highPriority {
		highPriority = "true"
	}
	xmlBody := fmt.Sprintf(`<Message>
  <MajorVersion>1</MajorVersion>
  <MinorVersion>1</MinorVersion>
  <MessageId>%d</MessageId>
  <UpdateTime>%s</UpdateTime>
  <Language></Language>
  <HighPriority>%s</HighPriority>
  <TitleId>%s</TitleId>
  <Subject>%s</Subject>
  <Body>%s</Body>
  <LinkType>NONE</LinkType>
  <Links>
    <WoodLink>
      <Parameter>launcher_type=info&amp;scene=top&amp;src_title_id=%s&amp;version=1.0.0</Parameter>
    </WoodLink>
    <OliveLink>
      <Parameter>%s,FFFFFFFF</Parameter>
    </OliveLink>
  </Links>
  <OptoutType>NEVER</OptoutType>
  <ForceNotify>false</ForceNotify>
</Message>
`, m.id, time.Now().UTC().Format("2006-01-02T15:04:05-0700"), highPriority, m.titleID, xmlEscape(m.subject), xmlEscape(m.body), m.titleID, m.titleID)
	return []byte(xmlBody)
}

func xmlEscape(s string) string {
	var buf bytes.Buffer
	xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

func gzipBytes(content []byte) ([]byte, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(content); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// isWiiUBossUserAgent distinguishes Wii U from 3DS BOSS requests. Confirmed
// from real captured headers: Wii U's boss sysmodule sends a User-Agent
// prefixed "PBOSU-", 3DS's sends "PBOS-" (no U). Needed because sysmsg1/
// sysmsg2 are task names shared by both platforms (see
// handle3DSSysMsgTasksheet) - without this, the Wii U handler (checked
// first) intercepts real 3DS requests too, since it previously matched on
// task name alone.
func isWiiUBossUserAgent(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("User-Agent"), "PBOSU-")
}

// handleWiiUSysMsgTasksheet serves real-format TaskSheets for sysmsg1/sysmsg2
// backed by relay-admin's wiiu_system_messages table. Handles both the
// task-level request (lists every active message) and the file-level request
// (a single matching <File> entry), mirroring handleWSCSpotPassTasksheet/
// handleWSCSpotPassFile's two-tier pattern.
func handleWiiUSysMsgTasksheet(w http.ResponseWriter, r *http.Request) bool {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 5 || len(parts) > 6 || parts[0] != "p01" || parts[1] != "tasksheet" {
		return false
	}
	// * sysmsg1/sysmsg2 are shared task names between Wii U and 3DS (see
	// * handle3DSSysMsgTasksheet) - confirmed live 2026-08-24 that without a
	// * platform check here, a real 3DS request got served the Wii U table's
	// * content instead, since this handler is checked first and previously
	// * matched on task name alone. Wii U's BOSS user agent is "PBOSU-...",
	// * 3DS's is "PBOS-..." (no U) - reliable since it's baked into each
	// * platform's own boss sysmodule, unlike bossAppId which can vary.
	if !isWiiUBossUserAgent(r) {
		return false
	}
	taskID := parts[4]
	if !wiiuSysMsgTasks[taskID] {
		return false
	}
	logBossRequest(r)

	var requestedFile string
	if len(parts) == 6 {
		requestedFile = parts[5]
	}

	msgs := activeWiiUSysMsgs()
	var fileEntries strings.Builder
	bossAppID := parts[3]
	// * titleID here is the OUTER <TaskSheet><TitleId> - this identifies the
	// * requesting console/bossAppId itself (region-specific, confirmed fixed
	// * per-bossAppId regardless of request content - see
	// * lookupRealWiiUTaskSheetTitleID's comment), NOT what each message is
	// * about. Distinct from each message's own m.titleID, which is the INNER
	// * <Message><TitleId> (e.g. "this announcement concerns Wii Sports
	// * Club") and correctly varies per message - previously these two were
	// * conflated by reassigning this same variable from m.titleID in the
	// * loop below, which used one console's real region value as a stand-in
	// * for a completely different field.
	titleID := "000500101004d100"
	if real, ok := lookupRealWiiUTaskSheetTitleID(bossAppID, taskID); ok {
		titleID = real
	}
	consoleRegion := wiiuRegionFromTaskSheetTitleID(titleID)
	filtered := msgs[:0]
	for _, m := range msgs {
		if regionMatches(m.region, consoleRegion) {
			filtered = append(filtered, m)
		}
	}
	msgs = filtered

	// * Filename must be a contiguous sequence (1, 2, 3, ...) with no gaps -
	// * confirmed live 2026-08-25 that a real Wii U badly malfunctioned
	// * (required a full SpotPass reset) after receiving a region-filtered
	// * TaskSheet whose filenames (previously the DB row's own raw serial id)
	// * had a gap where another region's message had been filtered out
	// * (00000006, 00000008 - missing 00000007). Renumbering by position in
	// * the already-filtered, id-ordered list keeps filenames stable across
	// * the tasksheet-then-specific-file two-step fetch (same query, same
	// * ORDER BY, so the same position maps to the same message) while never
	// * producing a gap regardless of which messages a given console's region
	// * excludes.
	for i, m := range msgs {
		filename := fmt.Sprintf("%08x", i+1)
		if requestedFile != "" && requestedFile != filename {
			continue
		}

		// * The genuine 2023 eShop-closure example's inner <Message><TitleId>
		// * is Home Menu's own self-referential id, matching the outer
		// * TaskSheet exactly - and that's region-specific (confirmed live
		// * 2026-08-25: a EUR console's own Home Menu id is ...d200, a NA
		// * one's is ...d100, not interchangeable). A message left at the
		// * DB column's generic Home-Menu-placeholder default would silently
		// * embed the WRONG region's self-id for any console whose region
		// * doesn't match that hardcoded default - only override the known
		// * generic placeholders here, not a deliberately custom title_id
		// * (e.g. referencing a specific game like Wii Sports Club), which
		// * an admin may have set intentionally and which isn't a self-id at
		// * all.
		if wiiuHomeMenuTitleIDs[m.titleID] {
			m.titleID = titleID
		}

		innerXML := buildWiiUMessageXML(m)
		compressed, err := gzipBytes(innerXML)
		if err != nil {
			log.Printf("wiiu sysmsg: gzip failed for message %d: %v", m.id, err)
			continue
		}
		encrypted, err := encryptBossFile(compressed)
		if err != nil {
			log.Printf("wiiu sysmsg: encrypt failed for message %d: %v", m.id, err)
			continue
		}
		hash := fmt.Sprintf("%x", md5.Sum(encrypted))
		wiiuSysMsgDataCache.Store(hash, encrypted)

		// * DataId must be a large, ever-increasing value, not just the DB
		// * row's own small serial id - confirmed live 2026-08-24: a real
		// * console fetched a real-format TaskSheet with DataId=1 twice
		// * (30+ seconds apart) but never followed up with the actual data
		// * fetch. Real Nintendo DataIds were a global incrementing counter
		// * already in the tens of thousands (e.g. 56957 in the genuine
		// * 2023 example this format is modeled on) - a client that tracks
		// * "highest DataId already processed for this task" would treat a
		// * low DataId like 1 as already-seen/stale and silently skip it.
		// * Unix-seconds is always far above any historical real value and
		// * always increasing, so this can never look stale.
		dataID := time.Now().Unix()*1000 + int64(m.id)

		fileEntries.WriteString(fmt.Sprintf(`    <File>
      <Filename>%s</Filename>
      <DataId>%d</DataId>
      <Type>Message</Type>
      <Url>https://boss.nicochristmann.net/p01/data/sysmsg/%s</Url>
      <Size>%d</Size>
      <Notify>
        <New>app,account</New>
        <LED>false</LED>
      </Notify>
    </File>
`, filename, dataID, hash, len(encrypted)))
	}

	xmlBody := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<TaskSheet>
  <TitleId>%s</TitleId>
  <TaskId>%s</TaskId>
  <ServiceStatus>open</ServiceStatus>
  <Files>
%s  </Files>
</TaskSheet>
`, titleID, taskID, fileEntries.String())

	log.Printf("BOSS capture: %s %s -> WiiU sysmsg TaskSheet (%d active message(s))", r.Method, r.URL.Path, len(msgs))
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Connection", "close")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(xmlBody))
	return true
}

// handleWiiUSysMsgData serves the actual encrypted system-message bytes
// referenced by handleWiiUSysMsgTasksheet's injected <Url>.
func handleWiiUSysMsgData(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/p01/data/sysmsg/") {
		return false
	}
	logBossRequest(r)
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	hash := parts[len(parts)-1]

	v, ok := wiiuSysMsgDataCache.Load(hash)
	if !ok {
		http.NotFound(w, r)
		return true
	}
	encrypted := v.([]byte)

	log.Printf("BOSS capture: %s %s -> served %d bytes", r.Method, r.URL.Path, len(encrypted))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Connection", "close")
	w.WriteHeader(http.StatusOK)
	w.Write(encrypted)
	return true
}

// n3dsSysMsgTasks: the 3DS's own equivalent generic system-message channel,
// independent of any specific game - confirmed live 2026-08-24 by querying
// Pretendo's real BOSS server directly with bossAppIds recovered from
// PretendoNetwork/BOSS's own list-known-boss-apps.ts registry (real, open,
// currently-empty TaskSheets for both). Unlike Wii U (2 tasks, 1 TitleId),
// 3DS has a THIRD task (sysmsg3) and TWO known TitleId/bossAppId pairs
// (likely region variants, both under the 0004003... 3DS system-applet
// category) - see reference_wiiu_sysmsg_format memory. The inner <Message>
// format itself has not been independently confirmed for 3DS specifically
// (no genuine decrypted 3DS example exists, unlike Wii U's) - reusing the
// same schema is a reasonable bet given it's the same shared BOSS/system-
// message infrastructure, but treat it as unverified until tested live.
var n3dsSysMsgTasks = map[string]bool{
	"sysmsg1": true,
	"sysmsg2": true,
	"sysmsg3": true,
}

// n3dsHomeMenuTitleIDs are the known real 3DS sysmsg bossAppId TitleIds
// (see lookupRealWiiUTaskSheetTitleID) - see wiiuHomeMenuTitleIDs' comment,
// same purpose for the 3DS side.
var n3dsHomeMenuTitleIDs = map[string]bool{
	"000400300000a102": true,
	"000400300000b102": true,
}

func active3DSSysMsgs() []wiiuSysMsgRow {
	rows, err := db.Query(`SELECT id, subject, body, title_id, high_priority, region FROM n3ds_system_messages WHERE active = true ORDER BY id ASC`)
	if err != nil {
		log.Printf("3ds sysmsg: query failed: %v", err)
		return nil
	}
	defer rows.Close()
	var out []wiiuSysMsgRow
	for rows.Next() {
		var m wiiuSysMsgRow
		if err := rows.Scan(&m.id, &m.subject, &m.body, &m.titleID, &m.highPriority, &m.region); err == nil {
			out = append(out, m)
		}
	}
	return out
}

// handle3DSSysMsgTasksheet mirrors handleWiiUSysMsgTasksheet exactly, backed
// by n3ds_system_messages instead. Shares wiiuSysMsgDataCache/
// handleWiiUSysMsgData for the actual content bytes - the cache is keyed by
// content hash, so there's no cross-platform collision risk.
func handle3DSSysMsgTasksheet(w http.ResponseWriter, r *http.Request) bool {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 5 || len(parts) > 6 || parts[0] != "p01" || parts[1] != "tasksheet" {
		return false
	}
	// * See handleWiiUSysMsgTasksheet's comment - sysmsg1/sysmsg2 are shared
	// * task names between platforms, must gate on User-Agent.
	if isWiiUBossUserAgent(r) {
		return false
	}
	taskID := parts[4]
	if !n3dsSysMsgTasks[taskID] {
		return false
	}
	logBossRequest(r)

	var requestedFile string
	if len(parts) == 6 {
		requestedFile = parts[5]
	}

	msgs := active3DSSysMsgs()
	var fileEntries strings.Builder
	bossAppID := parts[3]
	// * See handleWiiUSysMsgTasksheet's comment - outer TaskSheet TitleId is
	// * the requesting bossAppId's own identity, distinct from each message's
	// * inner m.titleID.
	titleID := "000400300000a102"
	if real, ok := lookupRealWiiUTaskSheetTitleID(bossAppID, taskID); ok {
		titleID = real
	}
	// * 3DS TitleIds don't follow the Wii U d000/d100/d200 region-suffix
	// * pattern (confirmed real ones end in a102/b102 instead), so this
	// * always returns "" (unknown) for 3DS today - regionMatches treats
	// * unknown as "show everything", which is the safe default until a 3DS
	// * region scheme is actually confirmed. Reused rather than duplicated so
	// * both platforms share one place to update if that changes.
	consoleRegion := wiiuRegionFromTaskSheetTitleID(titleID)
	filtered := msgs[:0]
	for _, m := range msgs {
		if regionMatches(m.region, consoleRegion) {
			filtered = append(filtered, m)
		}
	}
	msgs = filtered

	// * See handleWiiUSysMsgTasksheet's comment - filenames must be a
	// * contiguous sequence with no gaps, renumbered by position in the
	// * already-filtered list rather than the DB row's own raw id.
	for i, m := range msgs {
		filename := fmt.Sprintf("%08x", i+1)
		if requestedFile != "" && requestedFile != filename {
			continue
		}

		// * See handleWiiUSysMsgTasksheet's comment - correct a message still
		// * at the generic placeholder default to the actual requesting
		// * console's own real self-id, without touching a deliberately
		// * custom title_id.
		if n3dsHomeMenuTitleIDs[m.titleID] {
			m.titleID = titleID
		}

		innerXML := buildWiiUMessageXML(m)
		compressed, err := gzipBytes(innerXML)
		if err != nil {
			log.Printf("3ds sysmsg: gzip failed for message %d: %v", m.id, err)
			continue
		}
		encrypted, err := encryptBossFile(compressed)
		if err != nil {
			log.Printf("3ds sysmsg: encrypt failed for message %d: %v", m.id, err)
			continue
		}
		hash := fmt.Sprintf("%x", md5.Sum(encrypted))
		wiiuSysMsgDataCache.Store(hash, encrypted)

		dataID := time.Now().Unix()*1000 + int64(m.id)

		fileEntries.WriteString(fmt.Sprintf(`    <File>
      <Filename>%s</Filename>
      <DataId>%d</DataId>
      <Type>Message</Type>
      <Url>https://boss.nicochristmann.net/p01/data/sysmsg/%s</Url>
      <Size>%d</Size>
      <Notify>
        <New>app,account</New>
        <LED>false</LED>
      </Notify>
    </File>
`, filename, dataID, hash, len(encrypted)))
	}

	xmlBody := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<TaskSheet>
  <TitleId>%s</TitleId>
  <TaskId>%s</TaskId>
  <ServiceStatus>open</ServiceStatus>
  <Files>
%s  </Files>
</TaskSheet>
`, titleID, taskID, fileEntries.String())

	log.Printf("BOSS capture: %s %s -> 3DS sysmsg TaskSheet (%d active message(s))", r.Method, r.URL.Path, len(msgs))
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Connection", "close")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(xmlBody))
	return true
}

// handleHPPCapture logs the full raw request (method, path, headers - including
// Host, which is what tells us whether the real per-game hostname survives the
// DNS-only redirect as expected - and body) to hppCaptureDir. TEMPORARY, see
// hppCaptureDir's doc comment. Always responds 200 with an empty body; we don't
// yet know the real HPP wire format so there's nothing meaningful to serve.
func handleHPPCapture(w http.ResponseWriter, r *http.Request) {
	reqBody, _ := io.ReadAll(r.Body)

	os.MkdirAll(hppCaptureDir, 0755)
	ts := time.Now().Format("20060102-150405.000")
	safePath := strings.ReplaceAll(strings.Trim(r.URL.Path, "/"), "/", "_")
	if safePath == "" {
		safePath = "root"
	}
	base := fmt.Sprintf("%s/%s_%s", hppCaptureDir, ts, safePath)

	reqLog := fmt.Sprintf("From: %s\n%s %s\nHost: %s\nQuery: %s\nHeaders: %v\nBody (hex): %x\nBody (raw): %q\n",
		realIP(r), r.Method, r.URL.Path, r.Host, r.URL.RawQuery, r.Header, reqBody, reqBody)
	os.WriteFile(base+".request.txt", []byte(reqLog), 0644)

	log.Printf("HPP capture: %s %s Host=%s from %s -> logged %d byte body", r.Method, r.URL.Path, r.Host, realIP(r), len(reqBody))
	w.WriteHeader(http.StatusOK)
}

// connTestPage is the exact real conntest.nintendowifi.net/test.html content (from
// ToadKing/nintendo_dwc_emulator, which preserved the real Nintendo response) - the
// system periodically (every ~5 minutes) fetches this to decide whether internet
// connectivity is available at all, independent of and before any game-specific
// online feature. http:C's generic substring redirect already turns
// "conntest.nintendowifi.net" into "conntest.nicochristmann.net" for us, but
// nothing served real content there yet, so it fell through to the generic
// pretendo.cc reversal proxy and (apparently) timed out - which the system reads as
// "no internet", blocking online features and retrying every 5 minutes.
const connTestPage = `<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Transitional//EN" "http://www.w3.org/TR/xhtml1/DTD/xhtml1-transitional.dtd"><html><head><title>HTML Page</title></head><body bgcolor="#FFFFFF">This is test.html page</body></html>`

func handleConnTest(w http.ResponseWriter, r *http.Request) {
	log.Printf("conntest: %s %s from %s", r.Method, r.URL.Path, realIP(r))
	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("X-Organization", "Nintendo")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(connTestPage))
}

// nintendoBase64Decode/Encode implement Nintendo's custom base64 alphabet used on all
// NASC ac/ fields: '+' -> '.', '/' -> '-', '=' -> '*'.
func nintendoBase64Decode(s string) ([]byte, error) {
	s = strings.NewReplacer(".", "+", "-", "/", "*", "=").Replace(s)
	return base64.StdEncoding.DecodeString(s)
}

func nintendoBase64Encode(b []byte) string {
	s := base64.StdEncoding.EncodeToString(b)
	return strings.NewReplacer("+", ".", "/", "-", "=", "*").Replace(s)
}

// ctrLFCSPubModulus is Nintendo's own hardcoded RSA-2048 public key (modulus,
// exponent 65537) used to sign every 3DS's LocalFriendCodeSeed_B - the basis of
// the "fcdcert" field every NASC request carries. This is Nintendo's own public
// key, not a secret - it's baked into every 3DS's friends module and has been
// extracted/republished across the homebrew community for years (this exact
// constant matches PretendoNetwork/account's nintendo-certificate.ts).
var ctrLFCSPubModulus = new(big.Int).SetBytes([]byte{
	0x00, 0xA3, 0x75, 0x9A, 0x35, 0x46, 0xCF, 0xA7, 0xFE, 0x30, 0xEC, 0x55,
	0xA1, 0xB6, 0x4E, 0x08, 0xE9, 0x44, 0x9D, 0x0C, 0x72, 0xFC, 0xD1, 0x91,
	0xFD, 0x61, 0x0A, 0x28, 0x89, 0x75, 0xBC, 0xE6, 0xA9, 0xB2, 0x15, 0x56,
	0xE9, 0xC7, 0x67, 0x02, 0x55, 0xAD, 0xFC, 0x3C, 0xEE, 0x5E, 0xDB, 0x78,
	0x25, 0x9A, 0x4B, 0x22, 0x1B, 0x71, 0xE7, 0xE9, 0x51, 0x5B, 0x2A, 0x67,
	0x93, 0xB2, 0x18, 0x68, 0xCE, 0x5E, 0x5E, 0x12, 0xFF, 0xD8, 0x68, 0x06,
	0xAF, 0x31, 0x8D, 0x56, 0xF9, 0x54, 0x99, 0x02, 0x34, 0x6A, 0x17, 0xE7,
	0x83, 0x74, 0x96, 0xA0, 0x5A, 0xAF, 0x6E, 0xFD, 0xE6, 0xBE, 0xD6, 0x86,
	0xAA, 0xFD, 0x7A, 0x65, 0xA8, 0xEB, 0xE1, 0x1C, 0x98, 0x3A, 0x15, 0xC1,
	0x7A, 0xB5, 0x40, 0xC2, 0x3D, 0x9B, 0x7C, 0xFD, 0xD4, 0x63, 0xC5, 0xE6,
	0xDE, 0xB7, 0x78, 0x24, 0xC6, 0x29, 0x47, 0x33, 0x35, 0xB2, 0xE9, 0x37,
	0xE0, 0x54, 0xEE, 0x9F, 0xA5, 0x3D, 0xD7, 0x93, 0xCA, 0x3E, 0xAE, 0x4D,
	0xB6, 0x0F, 0x5A, 0x11, 0xE7, 0x0C, 0xDF, 0xBA, 0x03, 0xB2, 0x1E, 0x2B,
	0x31, 0xB6, 0x59, 0x06, 0xDB, 0x5F, 0x94, 0x0B, 0xF7, 0x6E, 0x74, 0xCA,
	0xD4, 0xAB, 0x55, 0xD9, 0x40, 0x05, 0x8F, 0x10, 0xFE, 0x06, 0x05, 0x0C,
	0x81, 0xBB, 0x42, 0x21, 0x90, 0xBA, 0x4F, 0x5C, 0x53, 0x82, 0xE1, 0xE1,
	0x0F, 0xBC, 0x94, 0x9F, 0x60, 0x69, 0x5D, 0x13, 0x03, 0xAA, 0xE2, 0xE0,
	0xC1, 0x08, 0x42, 0x4C, 0x20, 0x0B, 0x9B, 0xAA, 0x55, 0x2D, 0x55, 0x27,
	0x6E, 0x24, 0xE5, 0xD6, 0x04, 0x57, 0x58, 0x8F, 0xF7, 0x5F, 0x0C, 0xEC,
	0x81, 0x9F, 0x6D, 0x2D, 0x28, 0xF3, 0x10, 0x55, 0xF8, 0x3B, 0x76, 0x62,
	0xD4, 0xE4, 0xA6, 0x93, 0x69, 0xB5, 0xDA, 0x6B, 0x40, 0x23, 0xAF, 0x07,
	0xEB, 0x9C, 0xBF, 0xA9, 0xC9,
})

var ctrLFCSPubKey = &rsa.PublicKey{N: ctrLFCSPubModulus, E: 65537}

// validateFcdcert checks a NASC request's fcdcert field (base64, Nintendo's
// alphabet) against Nintendo's own LFCS public key. A genuine fcdcert is 0x110
// bytes: a 0x100-byte RSA-2048 PKCS1v15/SHA256 signature over the trailing
// 0x10-byte body. This proves the request came from a real 3DS (fcdcert is
// derived from a per-console secret only Nintendo's own signing process could
// have produced), independent of and instead of knowing that console's actual
// NEX/HPP account password - see feedback_wildcard_cert_ordering.md's sibling
// note on why HPP's own password-signature check can't be satisfied without it.
func validateFcdcert(fcdcertField string) bool {
	raw, err := nintendoBase64Decode(fcdcertField)
	if err != nil || len(raw) != 0x110 {
		return false
	}
	signature := raw[:0x100]
	body := raw[0x100:]
	hashed := sha256.Sum256(body)
	return rsa.VerifyPKCS1v15(ctrLFCSPubKey, crypto.SHA256, hashed[:], signature) == nil
}

// swapdoodleGameServerID is Swapdoodle's NASC game_server_id (%08X format),
// confirmed via live capture 2026-08-23 from the HPP hostname
// hpp-001a2c00-l1.n.app.nintendowifi.net.
const swapdoodleGameServerID = "001a2c00"

// swapdoodleLocator is where the dedicated non-SNI TLS listener for the swapdoodle
// HPP server (see nginx.conf's stream block) is publicly reachable. NASC's locator
// field is a raw ip:port, so this must be the server's actual public IP, not a
// hostname.
const swapdoodleLocator = "45.157.178.35:9013"

// handleNASC intercepts nasc.nicochristmann.net (patches/friends/src/nasc_url.s
// points here instead of Pretendo's real NASC). Only Swapdoodle's own
// game_server_id gets a real override, pointing at our own swapdoodle server;
// every other NASC request (3DS Friends' own bootstrap, any other title) is passed
// through to the real nasc.pretendo.cc unchanged via handleGenericOwnDomainProxy,
// so nothing else changes behavior.
func handleNASC(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	form, err := url.ParseQuery(string(bodyBytes))
	if err != nil {
		handleGenericOwnDomainProxy(w, r, ".nicochristmann.net")
		return
	}

	actionRaw, _ := nintendoBase64Decode(form.Get("action"))
	gameIDRaw, _ := nintendoBase64Decode(form.Get("gameid"))
	action := string(actionRaw)
	gameID := string(gameIDRaw)

	if action != "LOGIN" || gameID != swapdoodleGameServerID {
		handleGenericOwnDomainProxy(w, r, ".nicochristmann.net")
		return
	}

	if !validateFcdcert(form.Get("fcdcert")) {
		log.Printf("NASC: Swapdoodle LOGIN from %s rejected - invalid fcdcert", realIP(r))
		handleGenericOwnDomainProxy(w, r, ".nicochristmann.net")
		return
	}

	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		http.Error(w, "token error", http.StatusInternalServerError)
		return
	}

	// Built manually rather than via url.Values.Encode(), which would percent-encode
	// the literal '*' characters Nintendo's base64 alphabet uses as padding - the
	// real captured NASC response (from nasc.pretendo.cc) has those unescaped
	// (e.g. "locator=...MA**&retry=MA**"), so ours must match that exactly.
	respBody := fmt.Sprintf("locator=%s&retry=%s&returncd=%s&token=%s&datetime=%s",
		nintendoBase64Encode([]byte(swapdoodleLocator)),
		nintendoBase64Encode([]byte("0")),
		nintendoBase64Encode([]byte("001")),
		nintendoBase64Encode(tokenBytes),
		nintendoBase64Encode([]byte(time.Now().UTC().Format("20060102150405"))),
	)

	log.Printf("NASC: Swapdoodle LOGIN from %s -> locator=%s", realIP(r), swapdoodleLocator)
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(respBody))
}

// swapdoodleHPPBackend is where the actual swapdoodle Go server (PN_SD_HPP_SERVER_PORT)
// listens - plain HTTP, TLS/SNI is already terminated by the time a request reaches
// here via the normal account-proxy OLV listener.
const swapdoodleHPPBackend = "http://127.0.0.1:9010"

// handleSwapdoodleHPP proxies Swapdoodle's actual HPP game-data connection
// (hpp-001a2c00-l1.n.app.nicoch.net, produced by http:C's generic substring
// redirect - see patches/http/src/main.s) straight to our own swapdoodle server,
// instead of letting it fall into the generic *.nicoch.net -> *.pretendo.cc
// reversal proxy (which just times out, since Pretendo doesn't host this).
func handleSwapdoodleHPP(w http.ResponseWriter, r *http.Request) {
	// TEMPORARY - dumping the raw HPP request/response bodies (multipart
	// forms carrying an embedded NEX packet - see the PretendoNetwork
	// swapdoodle PR #1 thread for the extraction technique) so real note
	// upload/DataStore calls from the actual 3DS can be inspected offline,
	// same capture pattern as handleBossCapture/handleNpdlCDN.
	reqBody, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(reqBody))
	os.MkdirAll(bossCaptureDir, 0755)
	ts := time.Now().Format("20060102-150405.000")
	base := fmt.Sprintf("%s/hpp_%s", bossCaptureDir, ts)
	reqLog := fmt.Sprintf("%s %s from %s\nHeaders: %v\n", r.Method, r.URL.Path, realIP(r), r.Header)
	os.WriteFile(base+".request.txt", []byte(reqLog), 0644)
	os.WriteFile(base+".request.bin", reqBody, 0644)

	fullTarget := swapdoodleHPPBackend + r.URL.Path
	if r.URL.RawQuery != "" {
		fullTarget += "?" + r.URL.RawQuery
	}
	proxyReq, err := http.NewRequest(r.Method, fullTarget, bytes.NewReader(reqBody))
	if err != nil {
		http.Error(w, "proxy error", http.StatusInternalServerError)
		return
	}
	proxyReq.Header = r.Header.Clone()

	resp, err := http.DefaultClient.Do(proxyReq)
	if err != nil {
		log.Printf("swapdoodle HPP: upstream error for %s: %v", fullTarget, err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	respLog := fmt.Sprintf("Status: %d\nHeaders: %v\n", resp.StatusCode, resp.Header)
	os.WriteFile(base+".response.txt", []byte(respLog), 0644)
	os.WriteFile(base+".response.bin", respBody, 0644)

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
	log.Printf("swapdoodle HPP: %s %s from %s -> %d (captured to %s)", r.Method, r.URL.Path, realIP(r), resp.StatusCode, base)
}

// parseSimpleMultipart extracts form fields and the "file" field's raw bytes
// from a multipart/form-data body by manual byte scanning, rather than Go's
// stricter mime/multipart.Reader - see handleSwapdoodleS3RelayUpload's doc
// comment for why. Only handles the flat "Content-Disposition: form-data;
// name=\"...\"" shape (no nested/quoted-filename parts) the presigned-POST
// upload actually uses.
func parseSimpleMultipart(body []byte, contentType string) (map[string]string, []byte, error) {
	const boundaryParam = "boundary="
	idx := strings.Index(contentType, boundaryParam)
	if idx < 0 {
		return nil, nil, fmt.Errorf("no boundary in Content-Type %q", contentType)
	}
	boundary := contentType[idx+len(boundaryParam):]
	if semi := strings.IndexByte(boundary, ';'); semi >= 0 {
		boundary = boundary[:semi]
	}
	boundary = strings.Trim(strings.TrimSpace(boundary), `"`)
	delim := []byte("--" + boundary)

	fields := make(map[string]string)
	var fileBytes []byte

	parts := bytes.Split(body, delim)
	for _, part := range parts {
		part = bytes.TrimPrefix(part, []byte("\r\n"))
		headerEnd := bytes.Index(part, []byte("\r\n\r\n"))
		if headerEnd < 0 {
			continue
		}
		header := string(part[:headerEnd])
		value := part[headerEnd+4:]
		value = bytes.TrimSuffix(value, []byte("\r\n"))

		nameIdx := strings.Index(header, `name="`)
		if nameIdx < 0 {
			continue
		}
		nameIdx += len(`name="`)
		nameEnd := strings.IndexByte(header[nameIdx:], '"')
		if nameEnd < 0 {
			continue
		}
		name := header[nameIdx : nameIdx+nameEnd]

		if name == "file" {
			fileBytes = value
		} else {
			fields[name] = string(value)
		}
	}

	return fields, fileBytes, nil
}

var (
	swapdoodleS3ClientOnce sync.Once
	swapdoodleS3Client     *minio.Client
	swapdoodleS3ClientErr  error
)

// swapdoodleS3Client lazily builds a MinIO client for swapdoodle's own S3
// bucket, credentials loaded from swapdoodle/.env at startup (see main()).
// Only needed by handleSwapdoodleS3Relay's POST path - see its doc comment.
func swapdoodleS3() (*minio.Client, error) {
	swapdoodleS3ClientOnce.Do(func() {
		swapdoodleS3Client, swapdoodleS3ClientErr = minio.New(os.Getenv("PN_SD_CONFIG_S3_ENDPOINT"), &minio.Options{
			Creds:  credentials.NewStaticV4(os.Getenv("PN_SD_CONFIG_S3_ACCESS_KEY"), os.Getenv("PN_SD_CONFIG_S3_ACCESS_SECRET"), ""),
			Secure: true,
		})
	})
	return swapdoodleS3Client, swapdoodleS3ClientErr
}

// handleSwapdoodleS3Relay handles /s3relay/<real-host>/<real-path> requests
// produced by globals.relayThroughHPPHost in the swapdoodle submodule. The
// 3DS trusts zero root CAs per-title until the title explicitly adds one
// (confirmed via 3dbrew's SSL Services page) - Swapdoodle's original compiled
// code never added trust for Exoscale's cert chain (GandiCert -> DigiCert
// Global Root G2), so a direct HTTPS connection from the console to
// sos-de-fra-1.exo.io silently fails at the TLS layer. Confirmed live
// 2026-08-24: multiple real upload attempts returned success from
// CompletePostObjectV1 (which never actually validates against S3) while the
// object was verifiably never created in the bucket at all.
//
// The console DOES already trust hpp-<gameserverid>-l1.n.app.nicoch.net for
// every other HPP call, so instead of needing a 3DS-side cert-trust patch,
// swapdoodle-server now hands out presigned S3 URLs rewritten to point back
// at this same host/path.
//
// GET (notification/note downloads) is a plain reverse-proxy - relaying the
// raw bytes works fine there (verified live), since the SigV4 query
// signature only covers the Host header, which is preserved via realHost.
//
// POST (uploads) can NOT be a raw byte relay, though - confirmed live
// 2026-08-24 that Exoscale's presigned-POST/multipart endpoint returns
// "IncompleteBody: Content length does not match received size" specifically
// for the 3DS's real note content, reproduced deterministically in complete
// isolation (a standalone Go process replaying the exact captured bytes,
// no account-proxy/nginx/3DS involved) - ruling out our transport, header
// handling, and the policy/signature. Replacing only the file bytes with
// plain 'A' padding of the identical total length succeeds instantly, and a
// plain PutObject of the exact real bytes also succeeds instantly - so this
// is a real parsing edge case in Exoscale's own POST-policy endpoint for
// this specific binary content, not something wrong with the data or with
// us. Fix: parse the incoming multipart form ourselves and re-issue the
// upload as a normal PutObject (which reliably handles the same bytes),
// then synthesize the 204 response a real presigned-POST success would give.
func handleSwapdoodleS3Relay(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/s3relay/")
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		http.Error(w, "bad s3relay path", http.StatusBadRequest)
		return
	}
	realHost := rest[:slash]
	realPath := rest[slash:]

	if r.Method == http.MethodPost && strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		handleSwapdoodleS3RelayUpload(w, r)
		return
	}

	fullTarget := "https://" + realHost + realPath
	if r.URL.RawQuery != "" {
		fullTarget += "?" + r.URL.RawQuery
	}

	reqBody, _ := io.ReadAll(r.Body)

	os.MkdirAll(bossCaptureDir, 0755)
	ts := time.Now().Format("20060102-150405.000")
	base := fmt.Sprintf("%s/s3relay_%s", bossCaptureDir, ts)
	reqLog := fmt.Sprintf("%s %s -> %s\nHeaders: %v\nBody length: %d\n", r.Method, r.URL.Path, fullTarget, r.Header, len(reqBody))
	os.WriteFile(base+".request.txt", []byte(reqLog), 0644)
	os.WriteFile(base+".request.bin", reqBody, 0644)

	proxyReq, err := http.NewRequest(r.Method, fullTarget, bytes.NewReader(reqBody))
	if err != nil {
		http.Error(w, "proxy error", http.StatusInternalServerError)
		return
	}
	proxyReq.Header = r.Header.Clone()
	proxyReq.Header.Del("Content-Length")
	proxyReq.ContentLength = int64(len(reqBody))
	proxyReq.Host = realHost

	resp, err := http.DefaultClient.Do(proxyReq)
	if err != nil {
		log.Printf("swapdoodle S3 relay: upstream error for %s: %v", fullTarget, err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	respLog := fmt.Sprintf("Status: %d\nHeaders: %v\n", resp.StatusCode, resp.Header)
	os.WriteFile(base+".response.txt", []byte(respLog), 0644)
	os.WriteFile(base+".response.bin", respBody, 0644)

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
	log.Printf("swapdoodle S3 relay: %s %s -> %s status=%d bytes=%d (captured to %s)", r.Method, r.URL.Path, fullTarget, resp.StatusCode, len(respBody), base)
}

// handleSwapdoodleS3RelayUpload parses the presigned-POST multipart form the
// 3DS sends (the same fields real S3 POST policies use: bucket, key, policy,
// x-amz-*, file) and re-uploads the file via a direct PutObject instead of
// forwarding the raw multipart body - see handleSwapdoodleS3Relay's doc
// comment for why. Policy/signature fields are intentionally not
// re-validated here (this only runs for our own already-authenticated
// swapdoodle HPP path, not exposed generally), matching the note in
// CompletePostObjectV1 that the official servers don't validate against S3
// either.
func handleSwapdoodleS3RelayUpload(w http.ResponseWriter, r *http.Request) {
	ts := time.Now().Format("20060102-150405.000")
	base := fmt.Sprintf("%s/s3relayupload_%s", bossCaptureDir, ts)
	os.MkdirAll(bossCaptureDir, 0755)

	reqBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}
	// * Raw pre-parse capture, so a suspected parseSimpleMultipart bug can be
	// * diffed against the exact wire bytes - previously only the post-parse
	// * fileBytes were saved (as .file.bin), which is no help for telling
	// * whether THIS function altered the content vs. it already looking that
	// * way on the wire.
	os.WriteFile(base+".request.bin", reqBody, 0644)

	// * Manual byte-level field extraction instead of Go's mime/multipart
	// * reader - confirmed live 2026-08-24 that Go's stricter RFC 2046
	// * parser rejects the 3DS's own (older, non-standard) multipart
	// * encoding outright ("no such file"), even though the exact same
	// * bytes forward and parse fine everywhere else we've checked
	// * (Exoscale's own endpoint gets far enough to reject only the file
	// * part specifically, and simple byte-scanning - the same approach
	// * used throughout this investigation - extracts every field cleanly).
	fields, fileBytes, err := parseSimpleMultipart(reqBody, r.Header.Get("Content-Type"))
	if err != nil {
		log.Printf("swapdoodle S3 relay upload: multipart parse error: %v", err)
		http.Error(w, "bad multipart form", http.StatusBadRequest)
		return
	}

	bucket := fields["bucket"]
	key := fields["key"]
	if bucket == "" || key == "" || fileBytes == nil {
		log.Printf("swapdoodle S3 relay upload: missing bucket/key/file (bucket=%q key=%q hasFile=%v)", bucket, key, fileBytes != nil)
		http.Error(w, "missing bucket/key/file field", http.StatusBadRequest)
		return
	}
	os.WriteFile(base+".file.bin", fileBytes, 0644)
	os.WriteFile(base+".meta.txt", []byte(fmt.Sprintf("bucket=%s key=%s size=%d\n", bucket, key, len(fileBytes))), 0644)

	client, err := swapdoodleS3()
	if err != nil {
		log.Printf("swapdoodle S3 relay upload: client init error: %v", err)
		http.Error(w, "s3 init error", http.StatusInternalServerError)
		return
	}

	_, err = client.PutObject(context.Background(), bucket, key, bytes.NewReader(fileBytes), int64(len(fileBytes)), minio.PutObjectOptions{})
	if err != nil {
		log.Printf("swapdoodle S3 relay upload: PutObject error for %s/%s: %v", bucket, key, err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	// * A real presigned-POST upload returns 204 No Content on success.
	w.WriteHeader(http.StatusNoContent)
	log.Printf("swapdoodle S3 relay upload: PUT %s/%s (%d bytes) -> 204 (captured to %s)", bucket, key, len(fileBytes), base)
}

// pidFromBossUserAgent extracts the requesting console's own PID from a BOSS
// User-Agent - confirmed real format
// "PBOS-8.0/<devicehex>-<pidhex>/<firmware>-<region>/<n>/<n>" embeds it as
// the hex half after the '-' in the first path segment (e.g.
// "0000005b426dcb78" -> 1114491768, confirmed live 2026-08-24 against a
// known real PID).
func pidFromBossUserAgent(r *http.Request) (int64, bool) {
	ua := r.Header.Get("User-Agent")
	slash := strings.Index(ua, "/")
	if slash < 0 {
		return 0, false
	}
	rest := ua[slash+1:]
	if nextSlash := strings.Index(rest, "/"); nextSlash >= 0 {
		rest = rest[:nextSlash]
	}
	dash := strings.LastIndex(rest, "-")
	if dash < 0 {
		return 0, false
	}
	pid, err := strconv.ParseInt(rest[dash+1:], 16, 64)
	if err != nil {
		return 0, false
	}
	return pid, true
}

// pendingSwapdoodleNote looks up the oldest real, fully-uploaded note
// waiting for pid that our own RNG_EC1 stub hasn't already pushed to this
// recipient yet. Deliberately does NOT look at datastore.notifications.read -
// that column belongs to the real GetNewArrivedNotifications(V1) HPP calls
// (manual "check for notes", and whatever real fetch follows a BOSS wakeup),
// and marking it from here would lie to that code about whether the
// recipient's console ever actually processed the note through the genuine
// protocol. Confirmed live 2026-08-26/27: once this stub set read=true
// itself, a later manual check found nothing "new" for a note the console
// had in fact never real-fetched - manual download looked broken because we
// had already told the DB it was done. Own bookkeeping lives in
// public.boss_ring_delivered (see markSwapdoodleBossServed), a table private
// to this file with no relation to the vendor's datastore schema.
func pendingSwapdoodleNote(pid int64) (dataID int64, ok bool) {
	if swapdoodleDB == nil {
		return 0, false
	}
	err := swapdoodleDB.QueryRow(`
		SELECT o.data_id
		FROM datastore.notifications n
		JOIN datastore.objects o ON o.data_id = n.data_id
		LEFT JOIN public.boss_ring_delivered d ON d.data_id = n.data_id AND d.recipient_id = n.recipient_id
		WHERE n.recipient_id = $1 AND o.upload_completed = true AND d.data_id IS NULL
		ORDER BY o.creation_date ASC
		LIMIT 1`, pid).Scan(&dataID)
	if err != nil {
		return 0, false
	}
	return dataID, true
}

// markSwapdoodleBossServed records that our RNG_EC1 stub has already pushed
// dataID's raw bytes to pid via the BOSS Ring, so pendingSwapdoodleNote won't
// offer it again. This is intentionally separate from
// datastore.notifications.read - see pendingSwapdoodleNote's doc comment for
// why conflating the two broke manual note checks.
func markSwapdoodleBossServed(dataID, pid int64) {
	if swapdoodleDB == nil {
		return
	}
	if _, err := swapdoodleDB.Exec(`
		INSERT INTO public.boss_ring_delivered (data_id, recipient_id)
		VALUES ($1, $2)
		ON CONFLICT (data_id, recipient_id) DO NOTHING`, dataID, pid); err != nil {
		log.Printf("npdl CDN: failed to record BOSS ring delivery (dataID=%d, PID=%d): %v", dataID, pid, err)
	}
}

// handleNpdlCDN intercepts npdl.cdn.pretendo.cc (a real Pretendo domain, overridden
// infra there doesn't know about Swapdoodle). Swapdoodle refuses to upload notes
// unless it can download a "dstsetting" file from BOSS; originally a plain 304
// satisfied it even with an empty save (community-documented behavior), though
// see the placeholder-200 experiment below this comment. Everything else is
// proxied through to the real npdl.cdn.pretendo.cc unchanged, so other titles'
// BOSS content keeps working.
//
// The SpotPass "Ring" note-delivery check (System Settings -> manual SpotPass
// check, or automatic background check) additionally requests three more
// files under the same bossAppId, confirmed via live capture 2026-08-24 (error
// 004-7004/004-7008 on the console when these 404, which they always have,
// since Pretendo doesn't implement any of this either):
//   - RNG_MD1/dstdatList.bin - destination/metadata list
//   - RNG_NT1/<lang>/nt1, RNG_NT2/<lang>/nt2 - localized notification templates
// All three send If-Modified-Since, exactly like dstsetting, and were
// answered the same synthetic-304 way until the 2026-08-27 placeholder-200
// experiment below. RNG_EC1/<n>.dlp is a plain unconditional GET with no such
// header - this is the one BOSS actually
// checks to decide whether to wake the console up and go fetch a note via
// the real DataStore/HPP flow (see project_swapdoodle_spotpass_working
// memory).
//
// Per Nintendo's own BOSS Programming Manual ("8. DataStoreDL Tasks"), files
// like this are the generic NBDL/RawDL layer (developer content, identical
// for every recipient) and are architecturally NOT meant to carry per-user
// payloads - the real account-to-account transfer is supposed to happen
// entirely through NEX DataStore's own check-token protocol, i.e. exactly
// the HPP calls (GetNewArrivedNotificationsV1, PrepareGetObjectV1) that
// already power the working manual "check for notes" path. So RNG_EC1 only
// needs to look different/new to the console to make it bother triggering
// that real flow - it does NOT need to contain the actual note bytes.
// Serving the pending data_id as a small text marker (not the real object)
// as of 2026-08-27 - previously this served the real object bytes directly,
// which worked but wasn't necessary and is not how any of this is
// documented to behave. If background delivery regresses, this experiment
// is the first thing to revert (see feedback_swapdoodle_read_flag_conflation
// memory for the related history).
func handleNpdlCDN(w http.ResponseWriter, r *http.Request) {
	isRingEC1 := strings.Contains(r.URL.Path, "/RNG_EC1/") && strings.HasSuffix(r.URL.Path, ".dlp")
	if strings.Contains(r.URL.Path, "/RNG_") && !isRingEC1 {
		// Experiment as of 2026-08-27 - previously always answered 304 Not
		// Modified here (the "same synthetic-304 trick as dstsetting"
		// mentioned in this function's doc comment). Per the BOSS
		// Programming Manual, these bare-filename-under-a-directory paths
		// are the RawDL/NBDL shape (generic developer content, identical
		// for every player), not DataStoreDL - a real server would have had
		// to serve an actual 200 with real content at least once before a
		// 304 could ever be valid. Since we've never sent real bytes here,
		// the console's own BOSS task bookkeeping may still consider its
		// baseline sync incomplete, which is a plausible reason it falls
		// into a slower validation/retry path before the game's manual
		// "check for notes" flow (which waits on this ring round trip
		// first) proceeds to the real HPP calls - reported live 2026-08-27
		// as a ~10-47s gap between this response and the first real HPP
		// call, on top of official service having been "almost instant".
		// Serving a small real 200 body instead - unconditionally, since we
		// have no genuine content to make conditional on - tests whether
		// giving the console a completed download closes that gap. If it
		// doesn't help, revert to the 304 (see
		// feedback_tls_config_per_connection_no_resumption and
		// feedback_swapdoodle_invalid_dataid_notification for the other,
		// already-confirmed fixes from this same debugging session).
		//
		// Matched by the generic "/RNG_*/" folder convention rather than an
		// enumerated filename list (dstsetting, dstdatList.bin, nt1, nt2) as
		// of 2026-08-28: any RNG_-prefixed folder other than RNG_EC1 is, per
		// the same BOSS manual reasoning, a config/developer-content switch
		// that's safe to satisfy unconditionally - covers ring files we've
		// captured live (RNG_MD1, RNG_NT1, RNG_NT2) plus ones the console
		// requests only from screens we haven't captured yet (e.g. the "LS1"
		// tag seen on the letter-list screen, never observed in
		// boss-capture/ - see feedback_swapdoodle_ls1_unknown_ring_file).
		body := []byte("SD1")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		w.Write(body)
		log.Printf("npdl CDN: %s -> served placeholder 200 (Swapdoodle SpotPass ring file, was synthetic 304)", r.URL.Path)
		return
	}

	if strings.Contains(r.URL.Path, "/RNG_EC1/") && strings.HasSuffix(r.URL.Path, ".dlp") {
		if pid, ok := pidFromBossUserAgent(r); ok {
			if dataID, ok := pendingSwapdoodleNote(pid); ok {
				content := []byte(fmt.Sprintf("NEW:%d", dataID))
				w.Header().Set("Content-Type", "application/octet-stream")
				w.WriteHeader(http.StatusOK)
				w.Write(content)
				markSwapdoodleBossServed(dataID, pid)
				log.Printf("npdl CDN: %s -> served change-signal for pending note dataID=%d for PID=%d", r.URL.Path, dataID, pid)
				return
			}
		}
		log.Printf("npdl CDN: %s -> served local 404 (no pending Swapdoodle note)", r.URL.Path)
		http.NotFound(w, r)
		return
	}

	// TEMPORARY - capturing real Swapdoodle SpotPass note-check traffic
	// (RNG_EC1/RNG_MD1/RNG_NT1/RNG_NT2-style paths) that currently 404s
	// against the real npdl.cdn.pretendo.cc, so we can see the exact
	// request shape (query params, headers) needed to build a synthetic
	// response backed by our own swapdoodle-server. See handleBossCapture's
	// doc comment for the same pattern used for WSC.
	reqBody, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(reqBody))
	os.MkdirAll(bossCaptureDir, 0755)
	ts := time.Now().Format("20060102-150405.000")
	safePath := strings.ReplaceAll(strings.Trim(r.URL.Path, "/"), "/", "_")
	base := fmt.Sprintf("%s/npdl_%s_%s", bossCaptureDir, ts, safePath)
	reqLog := fmt.Sprintf("%s %s\nQuery: %s\nHeaders: %v\nBody (hex): %x\n",
		r.Method, r.URL.Path, r.URL.RawQuery, r.Header, reqBody)
	os.WriteFile(base+".request.txt", []byte(reqLog), 0644)

	fullTarget := "https://npdl.cdn.pretendo.cc" + r.URL.Path
	if r.URL.RawQuery != "" {
		fullTarget += "?" + r.URL.RawQuery
	}
	proxyReq, err := http.NewRequest(r.Method, fullTarget, bytes.NewReader(reqBody))
	if err != nil {
		http.Error(w, "proxy error", http.StatusInternalServerError)
		return
	}
	proxyReq.Header = r.Header.Clone()
	proxyReq.Host = "npdl.cdn.pretendo.cc"

	resp, err := wiiUHTTPClient.Do(proxyReq)
	if err != nil {
		log.Printf("npdl CDN: upstream error for %s: %v", fullTarget, err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	respLog := fmt.Sprintf("Status: %d\nHeaders: %v\nBody length: %d\n", resp.StatusCode, resp.Header, len(respBody))
	os.WriteFile(base+".response.txt", []byte(respLog), 0644)
	os.WriteFile(base+".response.bin", respBody, 0644)

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
	log.Printf("npdl CDN: %s %s -> proxied, status=%d (captured to %s)", r.Method, r.URL.Path, resp.StatusCode, base)
}

func handleBossCapture(w http.ResponseWriter, r *http.Request) {
	// * Serves the Azahar http_hle_replace_rules.txt draft directly from our
	// * own server (not IP-restricted, unlike a presigned Exoscale URL) - kept
	// * around intentionally as the reference copy for the combined real-
	// * hardware + emulator install guide (not yet written).
	if r.URL.Path == "/tmp-download/http_hle_replace_rules.txt" {
		http.ServeFile(w, r, "/nico-pretendo-bridge/http_hle_replace_rules.txt")
		return
	}
	if handleWSCSpotPassTasksheet(w, r) {
		return
	}
	if handleWSCSpotPassFile(w, r) {
		return
	}
	if handleWSCSpotPassData(w, r) {
		return
	}
	if handleWiiUSysMsgTasksheet(w, r) {
		return
	}
	if handle3DSSysMsgTasksheet(w, r) {
		return
	}
	if handleWiiUSysMsgData(w, r) {
		return
	}

	var target string
	var realHost string
	switch {
	case strings.HasPrefix(r.URL.Path, "/p01/policylist/"):
		target = "https://nppl.app.pretendo.cc"
		realHost = "nppl.app.pretendo.cc"
	case strings.HasPrefix(r.URL.Path, "/p01/tasksheet/"):
		target = "https://npts.app.pretendo.cc"
		realHost = "npts.app.pretendo.cc"
	default:
		log.Printf("BOSS capture: unknown path %s", r.URL.Path)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	reqBody, _ := io.ReadAll(r.Body)

	os.MkdirAll(bossCaptureDir, 0755)
	ts := time.Now().Format("20060102-150405.000")
	safePath := strings.ReplaceAll(strings.Trim(r.URL.Path, "/"), "/", "_")
	base := fmt.Sprintf("%s/%s_%s", bossCaptureDir, ts, safePath)

	reqLog := fmt.Sprintf("%s %s\nQuery: %s\nHeaders: %v\nBody (hex): %x\n",
		r.Method, r.URL.Path, r.URL.RawQuery, r.Header, reqBody)
	os.WriteFile(base+".request.txt", []byte(reqLog), 0644)

	fullTarget := target + r.URL.Path
	if r.URL.RawQuery != "" {
		fullTarget += "?" + r.URL.RawQuery
	}
	proxyReq, err := http.NewRequest(r.Method, fullTarget, bytes.NewReader(reqBody))
	if err != nil {
		http.Error(w, "proxy error", http.StatusInternalServerError)
		return
	}
	proxyReq.Header = r.Header.Clone()
	proxyReq.Host = realHost

	resp, err := wiiUHTTPClient.Do(proxyReq)
	if err != nil {
		log.Printf("BOSS capture: upstream error for %s: %v", fullTarget, err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	respLog := fmt.Sprintf("Status: %d\nHeaders: %v\nBody length: %d\n", resp.StatusCode, resp.Header, len(respBody))
	os.WriteFile(base+".response.txt", []byte(respLog), 0644)
	os.WriteFile(base+".response.bin", respBody, 0644)

	respHeader := resp.Header
	// Pretendo's real policylist server has a consistent bug: <UpdateTime>'s
	// seconds field is malformed (seen values like :85, :94, :80 - seconds
	// only go 0-59). WSC's own BOSS client likely rejects the whole
	// PolicyList as invalid when it can't parse this, which would block
	// every subsequent SpotPass task regardless of what content we serve -
	// this may be the real cause behind Task Error Code 1040340 persisting
	// through every fix made to sp1_rnk specifically. Since we can't fix
	// Pretendo's server, correct the timestamp before relaying to the
	// console. Serve uncompressed (drop Content-Encoding) rather than
	// re-encoding brotli - not worth the extra dependency for ~300 bytes.
	if strings.HasPrefix(r.URL.Path, "/p01/policylist/") && resp.StatusCode == http.StatusOK {
		fixed, changed, err := fixPolicylistUpdateTime(respBody, resp.Header.Get("Content-Encoding"))
		if err != nil {
			log.Printf("BOSS capture: policylist fixup failed, relaying as-is: %v", err)
		} else if changed {
			log.Printf("BOSS capture: policylist UpdateTime corrected for %s", r.URL.Path)
			respBody = fixed
			respHeader = resp.Header.Clone()
			respHeader.Del("Content-Encoding")
			respHeader.Set("Content-Length", fmt.Sprintf("%d", len(respBody)))
		}
	}

	log.Printf("BOSS capture: %s %s -> %s status=%d bytes=%d (saved %s)",
		r.Method, r.URL.Path, target, resp.StatusCode, len(respBody), base)

	for k, vs := range respHeader {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Connection", "close") // see handleWSCSpotPassTasksheet's comment
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

// policylistUpdateTimeRe matches PolicyList's <UpdateTime>YYYY-MM-DDTHH:MM:SS+0000</UpdateTime>.
var policylistUpdateTimeRe = regexp.MustCompile(`<UpdateTime>[^<]+</UpdateTime>`)

// fixPolicylistUpdateTime decompresses (if needed) a policylist response body,
// replaces a malformed <UpdateTime> with the current, correctly-formatted
// time, and returns the corrected plaintext body. changed is false (body nil)
// if no UpdateTime tag was found, so the caller can leave the response alone.
func fixPolicylistUpdateTime(body []byte, contentEncoding string) ([]byte, bool, error) {
	plain := body
	if contentEncoding == "br" {
		decoded, err := io.ReadAll(brotli.NewReader(bytes.NewReader(body)))
		if err != nil {
			return nil, false, fmt.Errorf("brotli decode: %w", err)
		}
		plain = decoded
	}

	if !policylistUpdateTimeRe.Match(plain) {
		return nil, false, nil
	}

	replacement := fmt.Sprintf("<UpdateTime>%s+0000</UpdateTime>", time.Now().UTC().Format("2006-01-02T15:04:05"))
	fixed := policylistUpdateTimeRe.ReplaceAll(plain, []byte(replacement))
	return fixed, true, nil
}

// handleGenericPretendoProxy transparently forwards a request to the real Pretendo
// server at the same hostname, unchanged. Fallback for any *.pretendo.cc host we
// haven't explicitly built handling for (e.g. BOSS CDN/content domains other than
// npdl.cdn.pretendo.cc) - rather than adding a one-off cert/route/handler for every
// domain a title happens to touch, unknown pretendo.cc hosts default to "just work
// like real Pretendo" and only get a specific override (like handleNpdlCDN's
// dstsetting fix) when something's actually known to need one.
func handleGenericPretendoProxy(w http.ResponseWriter, r *http.Request) {
	fullTarget := "https://" + r.Host + r.URL.Path
	if r.URL.RawQuery != "" {
		fullTarget += "?" + r.URL.RawQuery
	}
	proxyReq, err := http.NewRequest(r.Method, fullTarget, r.Body)
	if err != nil {
		http.Error(w, "proxy error", http.StatusInternalServerError)
		return
	}
	proxyReq.Header = r.Header.Clone()
	proxyReq.Host = r.Host

	resp, err := wiiUHTTPClient.Do(proxyReq)
	if err != nil {
		log.Printf("generic pretendo.cc proxy: upstream error for %s: %v", fullTarget, err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
	log.Printf("generic pretendo.cc proxy: %s %s -> %d", r.Method, fullTarget, resp.StatusCode)
}

// knownOLVHosts are the nicochristmann.net hostnames that are genuinely Miiverse/OLV
// traffic and should keep going through handleOLV's normal Juxt/miiverse-api
// forwarding below - everything else on our domain falls through to
// handleGenericNicochristmannProxy instead.
var knownOLVHosts = map[string]bool{
	"olv.nicochristmann.net":        true,
	"portal.olv.nicochristmann.net": true,
	"ctr.olv.nicochristmann.net":    true,
	"olv3ds.nicochristmann.net":     true,
}

// handleGenericOwnDomainProxy is the counterpart to handleGenericPretendoProxy, for
// after the Nimbus patches were changed to redirect everything to our own domain(s)
// instead of pretendo.cc directly (so we can selectively intercept specific titles,
// like Swapdoodle's HPP traffic via NASC, without needing Pretendo's cooperation).
// Any host on one of our own domains that we haven't built specific handling for is
// reversed back to the real *.pretendo.cc host Pretendo's own Nimbus fork would have
// produced (both are ultimately substituted from the same handful of real Nintendo
// domains) and proxied through transparently, so nothing not explicitly overridden
// changes behavior - this is purely about being ABLE to intercept, not intercepting
// by default. ownSuffix is whichever of our domains matched (".nicochristmann.net"
// or ".nicoch.net").
func handleGenericOwnDomainProxy(w http.ResponseWriter, r *http.Request, ownSuffix string) {
	realHost := strings.TrimSuffix(r.Host, ownSuffix) + ".pretendo.cc"
	fullTarget := "https://" + realHost + r.URL.Path
	if r.URL.RawQuery != "" {
		fullTarget += "?" + r.URL.RawQuery
	}
	proxyReq, err := http.NewRequest(r.Method, fullTarget, r.Body)
	if err != nil {
		http.Error(w, "proxy error", http.StatusInternalServerError)
		return
	}
	proxyReq.Header = r.Header.Clone()
	proxyReq.Host = realHost

	resp, err := wiiUHTTPClient.Do(proxyReq)
	if err != nil {
		log.Printf("generic %s proxy: upstream error for %s: %v", ownSuffix, fullTarget, err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
	log.Printf("generic %s proxy: %s %s -> %s -> %d", ownSuffix, r.Method, r.Host, fullTarget, resp.StatusCode)
}

// handleOLV forwards OLV discovery/API requests to miiverse-api on port 8080.
func handleOLV(w http.ResponseWriter, r *http.Request) {
	// Anything on a real pretendo.cc domain other than our own known OLV discovery
	// host is not Miiverse traffic at all - fall through to a transparent proxy
	// instead of feeding it into the Juxt/miiverse-api forwarding below.
	// (Re-enabled 2026-08-23: the real cause of the Wii U regression these were
	// briefly blamed for was a getCert() cert-ordering bug - see
	// feedback_wildcard_cert_ordering.md - not this routing logic. Confirmed Wii U
	// works with the cert fix alone; these were never the actual problem.)
	if strings.HasSuffix(r.Host, ".pretendo.cc") && r.Host != "discovery.olv.pretendo.cc" {
		handleGenericPretendoProxy(w, r)
		return
	}
	if strings.HasSuffix(r.Host, ".nicochristmann.net") && !knownOLVHosts[r.Host] {
		handleGenericOwnDomainProxy(w, r, ".nicochristmann.net")
		return
	}
	if strings.HasSuffix(r.Host, ".nicoch.net") {
		handleGenericOwnDomainProxy(w, r, ".nicoch.net")
		return
	}
	// Match Juxt's own detectVersion.ts logic: anything not explicitly a Wii U
	// UA is treated as 'ctr' (3DS) - don't assume a specific 3DS UA substring,
	// since that was never confirmed against a real device (see TLS ClientHello
	// with empty SNI, confirmed to be the 3DS, whose UA didn't match "Nintendo 3DS").
	is3DS := !strings.Contains(r.UserAgent(), "Nintendo WiiU")
	var reqBodyCopy []byte
	if is3DS {
		reqBodyCopy, _ = io.ReadAll(r.Body)
		r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(reqBodyCopy))
	}
	// CDN assets: proxy directly to S3 so the Wii U (which trusts only our cert) can load them.
	for _, prefix := range cdnPrefixes {
		if strings.HasPrefix(r.URL.Path, prefix) {
			s3URL := "https://olv-data.sos-de-fra-1.exo.io" + r.RequestURI
			resp, err := http.Get(s3URL)
			if err != nil {
				http.Error(w, "cdn error", http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
			for k, vs := range resp.Header {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(resp.StatusCode)
			io.Copy(w, resp.Body)
			return
		}
	}
	// API requests go to miiverse-api; everything else to juxt-ui
	port := "8081"
	if strings.HasPrefix(r.URL.Path, "/v1/") {
		port = "8080"
	}
	target := "http://127.0.0.1:" + port + r.RequestURI
	req, err := http.NewRequest(r.Method, target, r.Body)
	if err != nil {
		log.Printf("OLV proxy: %s %s -> build request error: %v", r.Method, r.URL.Path, err)
		http.Error(w, "proxy error", http.StatusInternalServerError)
		return
	}
	req.Header = r.Header.Clone()
	req.Host = "olv.nicochristmann.net"
	if ip := realIP(r); ip != "" {
		req.Header.Set("X-Real-IP", ip)
		req.Header.Set("X-Forwarded-For", ip)
	}
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("OLV proxy: %s %s -> 127.0.0.1:%s upstream error: %v", r.Method, r.URL.Path, port, err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	// olv.nicochristmann.net (host/api_host in the discovery XML, and every
	// hardcoded https://olv.nicochristmann.net/... asset/icon URL Juxt renders
	// into HTML) is shared with Wii U and still signed by the 4096-bit CA,
	// which 3DS's AddRootCA cave doesn't trust (it only fits a 2048-bit CA -
	// see the discovery_string/rootca.der change in the Nimbus fork). A single
	// untrusted preload/image URL is enough for CTR WebKit to abort loading
	// the rest of the page's resources, including the trailing <script> tag -
	// which is why the toolbar/navbar JS never even got requested. Rewrite
	// every occurrence, in both the XML discovery fields and HTML asset URLs,
	// to the 3DS-only, 2048-bit-CA-signed host - without touching what Wii U
	// receives.
	var respBodyCopy []byte
	if is3DS {
		raw, _ := io.ReadAll(resp.Body)
		raw = bytes.ReplaceAll(raw, []byte(">olv.nicochristmann.net<"), []byte(">olv3ds.nicochristmann.net<"))
		raw = bytes.ReplaceAll(raw, []byte("https://olv.nicochristmann.net"), []byte("https://olv3ds.nicochristmann.net"))
		respBodyCopy = raw
	}
	for k, vs := range resp.Header {
		// Node/Express's own "Keep-Alive: timeout=5" is a local implementation
		// detail that Cloudflare strips when proxying Pretendo's real responses -
		// a real console never sees it there. Drop it here too.
		if strings.EqualFold(k, "Keep-Alive") {
			continue
		}
		if is3DS && strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if is3DS {
		w.Header().Set("Content-Length", strconv.Itoa(len(respBodyCopy)))
	}
	// Forcing "Connection: close" here (removed) made every 3DS image/asset
	// request open a brand-new TLS handshake instead of reusing one - the
	// 3DS's ssl:C module can't reliably sustain many concurrent handshakes to
	// the same host, which is what caused the intermittent "unexpected
	// message" alerts (a genuine TLS Alert record from the 3DS itself,
	// confirmed via a raw packet capture) on parallel icon loads. Let
	// upstream's own Connection: keep-alive pass through instead.
	w.WriteHeader(resp.StatusCode)
	if is3DS {
		w.Write(respBodyCopy)
		logOLV3DSExchange(r, reqBodyCopy, resp.StatusCode, resp.Header, respBodyCopy)
		log.Printf("OLV proxy: %s %s -> 127.0.0.1:%s status=%d bytes=%d (3DS, captured)", r.Method, r.URL.Path, port, resp.StatusCode, len(respBodyCopy))
		return
	}
	n, _ := io.Copy(w, resp.Body)
	log.Printf("OLV proxy: %s %s -> 127.0.0.1:%s status=%d bytes=%d", r.Method, r.URL.Path, port, resp.StatusCode, n)
}

// startOLVProxy starts a TLS 1.0+ capable HTTPS proxy on port 7443 for OLV.
// Nginx stream passes olv.nicochristmann.net, discovery.olv.nintendo.net, and
// api.olv.nintendo.net TCP here - the latter is hit directly by nn_olv for
// community/post calls (GET .../v1/communities/*/posts, POST .../v1/posts), not
// just the discovery bootstrap, per real endpoint captures shared on the Pretendo
// forum. Inkay's DNS hook redirects it the same way as discovery.olv.nintendo.net.
func startOLVProxy() {
	baseCerts := "/nico-pretendo-bridge/certs"
	olvCert, err := tls.LoadX509KeyPair(baseCerts+"/olv-nicochristmann-net.crt", baseCerts+"/olv-nicochristmann-net.key")
	if err != nil {
		log.Fatalf("olv cert: %v", err)
	}
	nintendoCert, err := tls.LoadX509KeyPair(baseCerts+"/discovery-olv-nintendo-net.crt", baseCerts+"/discovery-olv-nintendo-net.key")
	if err != nil {
		log.Fatalf("nintendo discovery cert: %v", err)
	}
	nintendoAPICert, err := tls.LoadX509KeyPair(baseCerts+"/api-olv-nintendo-net.crt", baseCerts+"/api-olv-nintendo-net.key")
	if err != nil {
		log.Fatalf("nintendo api cert: %v", err)
	}
	pretendoCert, err := tls.LoadX509KeyPair(baseCerts+"/discovery-olv-pretendo-cc.crt", baseCerts+"/discovery-olv-pretendo-cc.key")
	if err != nil {
		log.Fatalf("pretendo discovery cert: %v", err)
	}
	// TEMPORARY — see handleBossCapture doc comment.
	bossCert, err := tls.LoadX509KeyPair(baseCerts+"/boss-nicochristmann-net.crt", baseCerts+"/boss-nicochristmann-net.key")
	if err != nil {
		log.Fatalf("boss capture cert: %v", err)
	}
	// 3DS-only discovery host, signed by a separate 2048-bit CA sized to fit the
	// Nimbus miiverse patch's fixed 848-byte AddRootCA cave - the shared
	// nicochristmann-nn-ca.crt (4096-bit) overflows that cave by 517 bytes.
	olv3dsCert, err := tls.LoadX509KeyPair(baseCerts+"/3ds/olv3ds-nicochristmann-net.crt", baseCerts+"/3ds/olv3ds-nicochristmann-net.key")
	if err != nil {
		log.Fatalf("3ds olv cert: %v", err)
	}
	// n3ds_host from the discovery response - only ever contacted by 3DS
	// (Wii U doesn't read this field), so safe to re-sign with the 3DS CA too.
	ctrOlvCert, err := tls.LoadX509KeyPair(baseCerts+"/3ds/ctr-olv-nicochristmann-net.crt", baseCerts+"/3ds/ctr-olv-nicochristmann-net.key")
	if err != nil {
		log.Fatalf("3ds ctr.olv cert: %v", err)
	}
	// HPP relay - see hppCaptureDir's doc comment. Cert trust doesn't matter here:
	// the Nimbus ssl:C patch disables root CA verification system-wide.
	hppRelayCert, err := tls.LoadX509KeyPair(baseCerts+"/hpp-relay-nicochristmann-net.crt", baseCerts+"/hpp-relay-nicochristmann-net.key")
	if err != nil {
		log.Fatalf("hpp relay cert: %v", err)
	}
	// npdl.cdn.pretendo.cc - see handleNpdlCDN's doc comment.
	npdlCert, err := tls.LoadX509KeyPair(baseCerts+"/npdl-cdn-pretendo-cc.crt", baseCerts+"/npdl-cdn-pretendo-cc.key")
	if err != nil {
		log.Fatalf("npdl cdn cert: %v", err)
	}
	// Fallback for any *.pretendo.cc host we haven't explicitly cased above - see
	// handleGenericPretendoProxy's doc comment. Cert trust doesn't matter (ssl:C
	// verification is disabled system-wide), this just needs the handshake to complete.
	wildcardPretendoCert, err := tls.LoadX509KeyPair(baseCerts+"/wildcard-pretendo-cc.crt", baseCerts+"/wildcard-pretendo-cc.key")
	if err != nil {
		log.Fatalf("wildcard pretendo cert: %v", err)
	}
	// Fallback for any *.nicochristmann.net host we haven't explicitly cased above -
	// see handleGenericNicochristmannProxy's doc comment.
	wildcardNicoCert, err := tls.LoadX509KeyPair(baseCerts+"/wildcard-nicochristmann-net.crt", baseCerts+"/wildcard-nicochristmann-net.key")
	if err != nil {
		log.Fatalf("wildcard nicochristmann.net cert: %v", err)
	}
	// nicoch.net - http:C's generic substring redirect target (must stay <= 12
	// chars, see patches/http/src/main.s's comment on replacementPretendo).
	// CNAMEd (with all subdomains) to netcup-server.nicochristmann.net.
	wildcardNicochCert, err := tls.LoadX509KeyPair(baseCerts+"/wildcard-nicoch-net.crt", baseCerts+"/wildcard-nicoch-net.key")
	if err != nil {
		log.Fatalf("wildcard nicoch.net cert: %v", err)
	}

	getCert := func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
		switch chi.ServerName {
		case "discovery.olv.nintendo.net":
			return &nintendoCert, nil
		case "api.olv.nintendo.net":
			return &nintendoAPICert, nil
		case "discovery.olv.pretendo.cc":
			return &pretendoCert, nil
		case "boss.nicochristmann.net":
			return &bossCert, nil
		case "olv3ds.nicochristmann.net":
			return &olv3dsCert, nil
		case "ctr.olv.nicochristmann.net":
			return &ctrOlvCert, nil
		case "hpp-relay.nicochristmann.net":
			return &hppRelayCert, nil
		case "npdl.cdn.pretendo.cc":
			return &npdlCert, nil
		case "olv.nicochristmann.net", "portal.olv.nicochristmann.net":
			// Explicit despite being what the default branch would otherwise
			// serve anyway - these two were relying on falling through to
			// "default: return &olvCert" below, until the *.nicochristmann.net
			// wildcard check was added to that same default and started
			// intercepting them first, serving the wrong (wildcard) cert. 3DS
			// never noticed (ssl:C verification is disabled there entirely),
			// but this broke real Wii U Miiverse (confirmed 2026-08-23,
			// 116-1097 on multiple consoles) since Wii U's TLS stack actually
			// validates the cert. Keep this explicit so it can never regress
			// the same way again regardless of what's added to default.
			return &olvCert, nil
		default:
			if strings.HasSuffix(chi.ServerName, ".pretendo.cc") {
				return &wildcardPretendoCert, nil
			}
			if strings.HasSuffix(chi.ServerName, ".nicoch.net") {
				return &wildcardNicochCert, nil
			}
			if strings.HasSuffix(chi.ServerName, ".nicochristmann.net") {
				return &wildcardNicoCert, nil
			}
			return &olvCert, nil
		}
	}


	// is3DSSensitiveHost/stagger3DSHandshake are a 3DS-only gate, deliberately
	// separate from anything Wii U touches: a no-op for every hostname Wii U
	// ever connects to, so Wii U's dispatch/cert/TLS-version behavior is
	// provably unaffected by this. Added 2026-08-27 after nginx's
	// stream-sni.log and account-proxy's own TLS error log showed the real
	// 3DS's IP throwing genuine "tls: unexpected message" alerts, in bursts,
	// exactly matching feedback_3ds_ssl_concurrency's already-documented
	// finding that the 3DS's ssl:C module can't reliably survive multiple
	// concurrent TLS handshakes to us - e.g. a Swap Doodle manual "check for
	// notes" HPP call and a background BOSS ring check
	// (npdl.cdn.pretendo.cc) landing in the same second, confirmed via log
	// timestamps. Rather than touch the shared TLS accept path every Wii U
	// connection also goes through, this only engages for hostnames a Wii U
	// never requests. Uses the package-level staggerHandshakePerIP so the
	// same per-IP throttle state is shared with the ACT listener (port 6666)
	// below - a real 3DS hitting both in close succession (e.g. an ACT token
	// refresh right before a Swap Doodle HPP call) should still be
	// desynchronized against itself even across listeners.
	//
	// Bug found 2026-08-28: this was a complete no-op for real 3DS traffic
	// until now. The real 3DS's ssl:C sends NO SNI at all for these
	// connections (confirmed - see feedback_3ds_ssl_concurrency and the
	// nginx stream map's own `"" -> 127.0.0.1:7443` fallback, originally
	// added for exactly this reason on the Miiverse side), so
	// chi.ServerName is always "" for genuine 3DS HPP/BOSS traffic - which
	// matched none of the named hosts below, meaning the stagger never
	// engaged for the one client it was built for. A live capture right
	// before this fix showed a real 3DS burst (BOSS ring check,
	// notification-URL fetch, 3 HPP calls, an S3 download) all within 23
	// seconds, immediately followed by a 004-7010 - exactly the
	// unprotected-concurrency pattern this gate was supposed to prevent.
	// Empty SNI is safe to treat as 3DS-sensitive: real Wii U's TLS stack
	// actually validates certs (see feedback_wildcard_cert_ordering), so it
	// depends on sending correct SNI to get the right cert back - it has
	// never been observed sending an empty one to this listener.
	is3DSSensitiveHost := func(serverName string) bool {
		if serverName == "" {
			return true
		}
		switch serverName {
		case "olv3ds.nicochristmann.net", "ctr.olv.nicochristmann.net", "npdl.cdn.pretendo.cc", "nasc.nicochristmann.net":
			return true
		}
		return serverName == "hpp-"+swapdoodleGameServerID+"-l1.n.app.nicoch.net"
	}

	// stagger3DSHandshake delays this handshake just enough (capped, bounded
	// per-IP) to avoid starting it at the exact same instant as another
	// in-flight handshake from the same remote IP to a 3DS-sensitive host -
	// cheap insurance against the concurrency limit above. Runs inside
	// GetConfigForClient, i.e. before the rest of the handshake proceeds for
	// this connection, so the delay actually desynchronizes the two
	// handshakes instead of just delaying our response after the fact.
	stagger3DSHandshake := func(chi *tls.ClientHelloInfo) {
		if !is3DSSensitiveHost(chi.ServerName) {
			return
		}
		if chi.Conn != nil {
			staggerHandshakePerIP(chi.Conn.RemoteAddr())
		}
	}

	// Built once and reused for every legacy (Wii U/3DS) connection below -
	// Go's TLS session-ticket resumption state (the STEK used to encrypt/
	// decrypt tickets) lives on the *tls.Config instance itself. Allocating
	// a fresh literal per-connection inside GetConfigForClient (as this used
	// to do) meant every single legacy connection got its own throwaway
	// ticket key, so a resumption ticket issued on one connection could
	// never be decrypted by the "config" handling the next one - forcing a
	// full asymmetric handshake every time even for the same console
	// reconnecting repeatedly within the same second. Confirmed live
	// 2026-08-27: nginx's stream-sni.log showed a real 3DS opening 6 brand
	// new TCP connections to hpp-...-l1.n.app.nicoch.net within ~2 seconds
	// for a single Swap Doodle "check for notes" round (one per HPP/S3-relay
	// call, no reuse) - on the 3DS's much weaker ARM11 CPU, paying full
	// handshake cost 6 times over is a very plausible source of the
	// perceived slowness/004-7010 client timeouts, independent of how fast
	// our own request handling is (measured ~100ms server-side per call,
	// including the real Exoscale S3 round trip). Sharing one Config lets
	// legitimate repeat connections from the same client resume cheaply
	// instead of redoing the full handshake.
	legacyTLSCfg := &tls.Config{
		MinVersion:     tls.VersionTLS10,
		MaxVersion:     tls.VersionTLS12,
		CipherSuites:   wiiUCiphers,
		GetCertificate: getCert,
	}
	tlsCfg := &tls.Config{
		// Default config for modern clients: TLS 1.2+ with standard cipher suites.
		MinVersion:     tls.VersionTLS12,
		GetCertificate: getCert,
		GetConfigForClient: func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			stagger3DSHandshake(chi)
			// TLS 1.3 cipher IDs are 0x1301/1302/1303 — only modern clients send them.
			// The Wii U sends only CBC suites, so if none of these appear, it's a Wii U.
			for _, cs := range chi.CipherSuites {
				if cs == 0x1301 || cs == 0x1302 || cs == 0x1303 {
					return nil, nil // modern client: use base config (TLS 1.2/1.3)
				}
			}
			// Wii U/3DS: cap at TLS 1.2 to suppress the RFC 8446 downgrade sentinel
			// in ServerHello.Random, which their TLS 1.0/1.1 stacks reject.
			return legacyTLSCfg, nil
		},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:7443")
	if err != nil {
		log.Fatalf("olv listen: %v", err)
	}
	// nginx's stream module does raw TCP passthrough for SNI-based routing to
	// this listener (no HTTP layer, so no X-Forwarded-For is possible) - every
	// connection here otherwise looks like it came from nginx itself
	// (127.0.0.1), silently breaking any IP-based lookup (e.g. fetchRealPID)
	// for real client identity. nginx now sends a PROXY protocol header ahead
	// of the TLS bytes (proxy_protocol on; in nginx.conf's stream block) so we
	// can recover the real client IP. Policy is USE (not the library's default
	// REQUIRE) so a direct connection without the header - e.g. local testing -
	// still works instead of hanging on ErrNoProxyProtocol.
	proxyLn := &proxyproto.Listener{
		Listener: ln,
		Policy: func(net.Addr) (proxyproto.Policy, error) {
			return proxyproto.USE, nil
		},
	}
	tlsLn := tls.NewListener(proxyLn, tlsCfg)
	// A plain host switch instead of http.ServeMux: ServeMux silently 301-redirects
	// any request whose path isn't already "clean" (e.g. contains "//") to the
	// cleaned path before our handlers ever run. Real Nintendo NPNS requests
	// genuinely contain a double slash (/api/v//notifications.json) and don't
	// follow redirects, so that "helpful" stdlib behavior broke NPNS outright.
	dispatch := func(w http.ResponseWriter, r *http.Request) {
		switch r.Host {
		case "boss.nicochristmann.net": // TEMPORARY — see handleBossCapture doc comment.
			handleBossCapture(w, r)
		case "hpp-relay.nicochristmann.net": // TEMPORARY — see hppCaptureDir doc comment.
			handleHPPCapture(w, r)
		case "npdl.cdn.pretendo.cc", "npdl.cdn.nicochristmann.net", "npdl.cdn.nicoch.net":
			handleNpdlCDN(w, r)
		case "conntest.nicochristmann.net", "conntest.nicoch.net":
			handleConnTest(w, r)
		case "nasc.nicochristmann.net":
			handleNASC(w, r)
		case "hpp-" + swapdoodleGameServerID + "-l1.n.app.nicoch.net":
			if strings.HasPrefix(r.URL.Path, "/s3relay/") {
				handleSwapdoodleS3Relay(w, r)
			} else {
				handleSwapdoodleHPP(w, r)
			}
		default:
			handleOLV(w, r)
		}
	}
	srv := &http.Server{Handler: http.HandlerFunc(dispatch)}
	log.Printf("OLV proxy listening on 127.0.0.1:7443")
	if err := srv.Serve(tlsLn); err != nil {
		log.Fatalf("olv server: %v", err)
	}
}
