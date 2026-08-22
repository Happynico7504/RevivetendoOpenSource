package main

import (
	"bufio"
	"context"
	"bytes"
	_ "embed"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
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
	"unicode/utf16"

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

func main() {
	godotenv.Load("../wiiu-chat-secure/.env")

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
var wiiUHTTPClient = &http.Client{
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
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS10,
			MaxVersion:         tls.VersionTLS12,
			CipherSuites:       wiiUCiphers,
			InsecureSkipVerify: true,
		},
	},
}

// bossCaptureDir holds raw request/response logs from handleBossCapture, for offline
// decryption with a dumped BOSS key once we've seen what real Wara Wara Plaza traffic
// looks like. TEMPORARY — see handleBossCapture doc comment.
const bossCaptureDir = "/nico-pretendo-bridge/boss-capture"

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

// patchMiiName overwrites the fixed 20-byte UTF-16BE name field in a cloned
// sub-record, null-padding/truncating to fit — the real Mii nickname limit
// (10 characters) means this never needs to shift any surrounding bytes.
func patchMiiName(subRecord []byte, name string) {
	runes := []rune(name)
	if len(runes) > rankingNameMaxRunes {
		runes = runes[:rankingNameMaxRunes]
	}
	units := utf16.Encode(runes)
	nameBytes := make([]byte, rankingNameMaxRunes*2)
	for i, u := range units {
		binary.BigEndian.PutUint16(nameBytes[i*2:i*2+2], u)
	}
	copy(subRecord[rankingNameOffset:rankingNameOffset+len(nameBytes)], nameBytes)
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

// topRankedPlayers queries wsc-secure's ranking_scores collection (shared
// Mongo database, wscMongoDB — see wsc-secure/db.go's dbInsertRankingScore)
// for the best score per player in a given club/region, sorted descending.
// groups[0] is the club code and groups[1] the region (2=US/3=EU) - see
// plans/atomic-marinating-fog.md for how this was confirmed (wsc-secure's own
// code comment mislabels groups[0] as a sport code).
func topRankedPlayers(clubCode, region uint32, limit int) ([]uint32, error) {
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
		if bsonToUint32(groups[0]) != clubCode || bsonToUint32(groups[1]) != region {
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

// miiNameForPID looks up a player's Mii nickname the same way
// handleInternalLookup does.
func miiNameForPID(pid uint32) string {
	var name string
	db.QueryRow(`SELECT mii_name FROM mii_names WHERE pid = $1`, pid).Scan(&name)
	return name
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
// regardless of content. Patching stays narrowly scoped to the one 20-byte
// Mii name field we've actually verified (patchMiiName/
// TestPatchMiiNameRoundTrip) rather than anything touching the
// not-fully-understood record internals.
func generateRankingData(clubCode, region uint32) ([]byte, error) {
	slots := rankingTemplateSlots()
	if len(slots) == 0 {
		return nil, fmt.Errorf("rankingdata template: no slots found")
	}

	players, err := topRankedPlayers(clubCode, region, len(slots))
	if err != nil {
		return nil, err
	}

	out := make([]byte, len(rankingDataTemplate))
	copy(out, rankingDataTemplate)

	for i, pid := range players {
		name := miiNameForPID(pid)
		if name == "" {
			continue
		}
		firstSubStart := slots[i][0]
		patchMiiName(out[firstSubStart:firstSubStart+rankingSubRecordSize], name)
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

func handleBossCapture(w http.ResponseWriter, r *http.Request) {
	if handleWSCSpotPassTasksheet(w, r) {
		return
	}
	if handleWSCSpotPassFile(w, r) {
		return
	}
	if handleWSCSpotPassData(w, r) {
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

// handleOLV forwards OLV discovery/API requests to miiverse-api on port 8080.
func handleOLV(w http.ResponseWriter, r *http.Request) {
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
		default:
			return &olvCert, nil
		}
	}


	tlsCfg := &tls.Config{
		// Default config for modern clients: TLS 1.2+ with standard cipher suites.
		MinVersion:     tls.VersionTLS12,
		GetCertificate: getCert,
		GetConfigForClient: func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			// TLS 1.3 cipher IDs are 0x1301/1302/1303 — only modern clients send them.
			// The Wii U sends only CBC suites, so if none of these appear, it's a Wii U.
			for _, cs := range chi.CipherSuites {
				if cs == 0x1301 || cs == 0x1302 || cs == 0x1303 {
					return nil, nil // modern client: use base config (TLS 1.2/1.3)
				}
			}
			// Wii U: cap at TLS 1.2 to suppress the RFC 8446 downgrade sentinel in
			// ServerHello.Random, which the Wii U's TLS 1.0/1.1 stack rejects.
			return &tls.Config{
				MinVersion:     tls.VersionTLS10,
				MaxVersion:     tls.VersionTLS12,
				CipherSuites:   wiiUCiphers,
				GetCertificate: getCert,
			}, nil
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
	mux := http.NewServeMux()
	mux.HandleFunc("boss.nicochristmann.net/", handleBossCapture) // TEMPORARY — see handleBossCapture doc comment.
	mux.HandleFunc("/", handleOLV)
	srv := &http.Server{Handler: mux}
	log.Printf("OLV proxy listening on 127.0.0.1:7443")
	if err := srv.Serve(tlsLn); err != nil {
		log.Fatalf("olv server: %v", err)
	}
}
