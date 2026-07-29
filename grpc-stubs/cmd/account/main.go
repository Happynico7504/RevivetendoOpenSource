package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	pb "github.com/PretendoNetwork/grpc-go/account"
	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// juxtAESKeyHex is the AES-256 key used to generate/verify Juxt service tokens.
// Must match account-proxy/main.go juxtAESKeyHex.
const juxtAESKeyHex = "6014b79d9a50f090f4b7342d00d33a53cd97760ee3e50c35ad40ab7e01dbddcc"

// internalAuthURL is the account-proxy's localhost endpoint that routes auth
// through its wiiUTLSConfig to pass Cloudflare's TLS fingerprint check.
const internalAuthURL = "http://127.0.0.1:9191/internal/auth"

const schema = `
CREATE TABLE IF NOT EXISTS nex_accounts (
	pid          INTEGER PRIMARY KEY,
	username     TEXT UNIQUE NOT NULL,
	nex_password TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS nex_sessions (
	token      TEXT PRIMARY KEY,
	ip         TEXT NOT NULL DEFAULT '',
	pid        INTEGER,
	pnid       TEXT NOT NULL DEFAULT '',
	expires_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE IF NOT EXISTS web_users (
	username      TEXT PRIMARY KEY,
	pid           INTEGER NOT NULL,
	password_hash TEXT NOT NULL
);
ALTER TABLE nex_sessions ADD COLUMN IF NOT EXISTS pnid TEXT NOT NULL DEFAULT '';
CREATE TABLE IF NOT EXISTS mii_names (
	pid      INTEGER PRIMARY KEY,
	mii_name TEXT NOT NULL DEFAULT ''
);`

// ── Pretendo auth via account-proxy internal endpoint ─────────

type internalAuthResp struct {
	PID   uint32 `json:"pid"`
	PNID  string `json:"pnid"`
	Error string `json:"error"`
}

// pretendoAuth calls account-proxy's internal HTTP endpoint which routes through
// wiiUTLSConfig, bypassing Cloudflare's TLS fingerprint check on account.pretendo.cc.
func pretendoAuth(username, password, clientIP string) (uint32, string, error) {
	body := url.Values{"user_id": {username}, "password": {password}}
	req, err := http.NewRequest("POST", internalAuthURL, strings.NewReader(body.Encode()))
	if err != nil {
		return 0, "", fmt.Errorf("internal auth request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if clientIP != "" {
		req.Header.Set("X-Forwarded-For", clientIP)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("internal auth request: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	var result internalAuthResp
	if err := json.Unmarshal(respBody, &result); err != nil {
		return 0, "", fmt.Errorf("parse internal auth response: %w", err)
	}
	if result.Error != "" {
		return 0, "", fmt.Errorf("%s", result.Error)
	}
	if result.PID == 0 {
		return 0, "", fmt.Errorf("no PID in response")
	}
	return result.PID, result.PNID, nil
}

// ── account.Account service ────────────────────────────────────

type accountServer struct {
	pb.UnimplementedAccountServer
	db *sql.DB
}

func (s *accountServer) GetNEXPassword(ctx context.Context, req *pb.GetNEXPasswordRequest) (*pb.GetNEXPasswordResponse, error) {
	var password string
	// COALESCE: return friends_nex_password when available (Friends service), else nex_password (WiiU Chat).
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(friends_nex_password, nex_password) FROM nex_accounts WHERE pid = $1`, req.Pid).Scan(&password)
	if err == sql.ErrNoRows {
		return nil, status.Errorf(codes.NotFound, "no account for PID %d", req.Pid)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "db error: %v", err)
	}
	return &pb.GetNEXPasswordResponse{Password: password}, nil
}

func (s *accountServer) GetNEXData(ctx context.Context, req *pb.GetNEXDataRequest) (*pb.GetNEXDataResponse, error) {
	var password, username string
	err := s.db.QueryRowContext(ctx, `SELECT nex_password, username FROM nex_accounts WHERE pid = $1`, req.Pid).Scan(&password, &username)
	if err == sql.ErrNoRows {
		return nil, status.Errorf(codes.NotFound, "no account for PID %d", req.Pid)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "db error: %v", err)
	}
	return &pb.GetNEXDataResponse{
		Pid:               req.Pid,
		Password:          password,
		OwningPid:         req.Pid,
		AccessLevel:       0,
		ServerAccessLevel: "prod",
	}, nil
}

var developerPIDs = map[uint32]bool{
	1435853600: true,
}

func accessLevelForPID(pid uint32) uint32 {
	if developerPIDs[pid] {
		return 3
	}
	return 0
}

func (s *accountServer) GetUserData(ctx context.Context, req *pb.GetUserDataRequest) (*pb.GetUserDataResponse, error) {
	username, miiName, err := s.lookupPID(ctx, req.Pid)
	if err != nil {
		return nil, err
	}
	resp := &pb.GetUserDataResponse{
		Pid:               req.Pid,
		Username:          username,
		AccessLevel:       accessLevelForPID(req.Pid),
		ServerAccessLevel: "prod",
	}
	if miiName != "" {
		resp.Mii = &pb.Mii{Name: miiName}
	}
	return resp, nil
}

// lookupPID resolves a PID to (username, miiName) using a four-level chain:
//  1. nex_accounts   — users who have logged in to our server
//  2. pnid_cache     — populated by account-proxy from Pretendo profile responses
//  3. pretendo_friends — NNIDs captured during friend-list syncs (written back to pnid_cache)
//  4. NEX GetBasicInfo — live call to Pretendo via account-proxy internal endpoint,
//     authenticated as the caller identified by x-caller-pid gRPC metadata
func (s *accountServer) lookupPID(ctx context.Context, pid uint32) (username, miiName string, err error) {
	// 1. nex_accounts — but skip when username is the PID as a decimal string (NEX protocol convention)
	dbErr := s.db.QueryRowContext(ctx, `SELECT username FROM nex_accounts WHERE pid = $1`, pid).Scan(&username)
	if dbErr != nil && dbErr != sql.ErrNoRows {
		return "", "", status.Errorf(codes.Internal, "db error: %v", dbErr)
	}
	if username == strconv.FormatUint(uint64(pid), 10) {
		username = ""
	}

	// 2. pnid_cache
	if username == "" {
		dbErr = s.db.QueryRowContext(ctx, `SELECT pnid FROM pnid_cache WHERE pid = $1`, pid).Scan(&username)
		if dbErr != nil && dbErr != sql.ErrNoRows {
			return "", "", status.Errorf(codes.Internal, "db error: %v", dbErr)
		}
	}

	// 3. pretendo_friends — friend_nnid captured during sync
	if username == "" {
		dbErr = s.db.QueryRowContext(ctx,
			`SELECT friend_nnid FROM pretendo_friends WHERE friend_pid = $1 AND friend_nnid != '' LIMIT 1`, pid,
		).Scan(&username)
		if dbErr != nil && dbErr != sql.ErrNoRows {
			return "", "", status.Errorf(codes.Internal, "db error: %v", dbErr)
		}
		if username != "" {
			s.db.ExecContext(ctx,
				`INSERT INTO pnid_cache (pid, pnid) VALUES ($1, $2)
				 ON CONFLICT (pid) DO UPDATE SET pnid = EXCLUDED.pnid, updated_at = NOW()`,
				pid, username)
		}
	}

	// 4. NEX GetBasicInfo via account-proxy internal endpoint
	if username == "" {
		callerPID := callerPIDFromCtx(ctx)
		lookupURL := fmt.Sprintf("http://127.0.0.1:9191/internal/lookup/%d", pid)
		if callerPID != 0 {
			lookupURL += fmt.Sprintf("?caller=%d", callerPID)
		}
		if resp, httpErr := http.Get(lookupURL); httpErr == nil {
			defer resp.Body.Close()
			var result struct {
				PNID    string `json:"pnid"`
				MiiName string `json:"mii_name"`
			}
			if json.NewDecoder(resp.Body).Decode(&result) == nil && result.PNID != "" {
				username = result.PNID
				miiName = result.MiiName
				return username, miiName, nil
			}
		}
	}

	if username == "" {
		return "", "", status.Errorf(codes.NotFound, "no account for PID %d", pid)
	}

	// Mii name: mii_names table first, then pretendo_friends as fallback.
	_ = s.db.QueryRowContext(ctx, `SELECT mii_name FROM mii_names WHERE pid = $1`, pid).Scan(&miiName)
	if miiName == "" {
		_ = s.db.QueryRowContext(ctx,
			`SELECT mii_name FROM pretendo_friends WHERE friend_pid = $1 AND mii_name != '' LIMIT 1`, pid,
		).Scan(&miiName)
	}

	return username, miiName, nil
}

// callerPIDFromCtx extracts the x-caller-pid value from incoming gRPC metadata.
func callerPIDFromCtx(ctx context.Context) uint32 {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return 0
	}
	vals := md.Get("x-caller-pid")
	if len(vals) == 0 {
		return 0
	}
	v, _ := strconv.ParseUint(vals[0], 10, 32)
	return uint32(v)
}

// ── ExchangeIndependentServiceTokenForUserData ────────────────
// Decrypts our AES-CBC Juxt service token and returns account data.
// Registered only on account.v2.AccountService (v1 AccountDefinition lacks this method).

// Proto message structs for the exchange method — manually defined to avoid
// needing generated v2 Go bindings. Field tags match the .proto definitions exactly.

type ExchangeServiceTokenRequest struct {
	ClientIds []string `protobuf:"bytes,1,rep,name=client_ids,json=clientIds,proto3"`
	Token     string   `protobuf:"bytes,2,opt,name=token,proto3"`
}

func (x *ExchangeServiceTokenRequest) Reset()         { *x = ExchangeServiceTokenRequest{} }
func (x *ExchangeServiceTokenRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*ExchangeServiceTokenRequest) ProtoMessage()    {}

type ProtoTimestamp struct {
	Seconds int64 `protobuf:"varint,1,opt,name=seconds,proto3"`
	Nanos   int32 `protobuf:"varint,2,opt,name=nanos,proto3"`
}

func (x *ProtoTimestamp) Reset()         { *x = ProtoTimestamp{} }
func (x *ProtoTimestamp) String() string { return fmt.Sprintf("%+v", *x) }
func (*ProtoTimestamp) ProtoMessage()    {}

// TokenInfo (account.v2.TokenInfo) — fields 1,2,3,6 are the ones we populate.
type TokenInfoMsg struct {
	SystemType int32           `protobuf:"varint,1,opt,name=system_type,json=systemType,proto3,enum=account.v2.SystemType"`
	TokenType  int32           `protobuf:"varint,2,opt,name=token_type,json=tokenType,proto3,enum=account.v2.TokenType"`
	Pid        uint64          `protobuf:"varint,3,opt,name=pid,proto3"`
	IssueTime  *ProtoTimestamp `protobuf:"bytes,6,opt,name=issue_time,json=issueTime,proto3"`
}

func (x *TokenInfoMsg) Reset()         { *x = TokenInfoMsg{} }
func (x *TokenInfoMsg) String() string { return fmt.Sprintf("%+v", *x) }
func (*TokenInfoMsg) ProtoMessage()    {}

// GetPNIDResponse (account.v2.GetPNIDResponse) — minimal fields Juxt needs.
type PNIDResponse struct {
	Deleted           bool     `protobuf:"varint,1,opt,name=deleted,proto3"`
	Pid               uint32   `protobuf:"varint,2,opt,name=pid,proto3"`
	Username          string   `protobuf:"bytes,3,opt,name=username,proto3"`
	AccessLevel       int32    `protobuf:"varint,4,opt,name=access_level,json=accessLevel,proto3"`
	ServerAccessLevel string   `protobuf:"bytes,5,opt,name=server_access_level,json=serverAccessLevel,proto3"`
	Mii               *pb.Mii `protobuf:"bytes,6,opt,name=mii,proto3"`
}

func (x *PNIDResponse) Reset()         { *x = PNIDResponse{} }
func (x *PNIDResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*PNIDResponse) ProtoMessage()    {}

// ExchangeServiceTokenResponse (account.v2.ExchangeIndependentServiceTokenForUserDataResponse)
// field 1 = pnid, field 3 = tokenInfo.
type ExchangeServiceTokenResponse struct {
	Pnid      *PNIDResponse `protobuf:"bytes,1,opt,name=pnid,proto3"`
	TokenInfo *TokenInfoMsg `protobuf:"bytes,3,opt,name=token_info,json=tokenInfo,proto3"`
}

func (x *ExchangeServiceTokenResponse) Reset()         { *x = ExchangeServiceTokenResponse{} }
func (x *ExchangeServiceTokenResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*ExchangeServiceTokenResponse) ProtoMessage()    {}

func decryptJuxtToken(tokenB64 string) (systemType byte, pid uint32, issueTimeMs uint64, err error) {
	raw, err := base64.StdEncoding.DecodeString(tokenB64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("base64 decode: %w", err)
	}
	if len(raw) < 4+aes.BlockSize {
		return 0, 0, 0, fmt.Errorf("token too short")
	}
	expectedCRC := binary.BigEndian.Uint32(raw[:4])
	encrypted := raw[4:]

	key, err := hex.DecodeString(juxtAESKeyHex)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("bad AES key constant: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("aes cipher: %w", err)
	}
	if len(encrypted)%aes.BlockSize != 0 {
		return 0, 0, 0, fmt.Errorf("encrypted length not block-aligned")
	}
	plaintext := make([]byte, len(encrypted))
	cipher.NewCBCDecrypter(block, make([]byte, aes.BlockSize)).CryptBlocks(plaintext, encrypted)

	// PKCS7 unpad
	padLen := int(plaintext[len(plaintext)-1])
	if padLen == 0 || padLen > aes.BlockSize {
		return 0, 0, 0, fmt.Errorf("invalid PKCS7 pad byte %d", padLen)
	}
	plaintext = plaintext[:len(plaintext)-padLen]

	// Verify CRC32 against original plaintext (before unpadding isn't possible here;
	// the checksum in account-proxy was computed over the unpadded plaintext).
	if crc32.ChecksumIEEE(plaintext) != expectedCRC {
		return 0, 0, 0, fmt.Errorf("CRC32 mismatch")
	}
	if len(plaintext) < 14 {
		return 0, 0, 0, fmt.Errorf("plaintext too short (%d bytes)", len(plaintext))
	}

	// Layout: system_type[0], token_type[1], pid[2:6] LE, issue_time_ms[6:14] LE
	systemType = plaintext[0]
	pid = binary.LittleEndian.Uint32(plaintext[2:6])
	issueTimeMs = binary.LittleEndian.Uint64(plaintext[6:14])
	return systemType, pid, issueTimeMs, nil
}

func (s *accountServer) ExchangeIndependentServiceTokenForUserData(ctx context.Context, req *ExchangeServiceTokenRequest) (*ExchangeServiceTokenResponse, error) {
	systemType, pid, issueMs, err := decryptJuxtToken(req.Token)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid service token: %v", err)
	}

	// Only WUP (1) and CTR (2) tokens are accepted.
	if systemType != 1 && systemType != 2 {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported system_type %d", systemType)
	}

	// Reject tokens older than 24 hours.
	issued := time.UnixMilli(int64(issueMs))
	if time.Since(issued) > 24*time.Hour {
		return nil, status.Errorf(codes.Unauthenticated, "service token expired")
	}

	// Resolve the PNID via the same 4-level chain used by GetUserData.
	username, miiName, err := s.lookupPID(ctx, pid)
	if err != nil {
		return nil, err
	}

	// Load FFLStoreData from user_settings so Juxt can attach it to posts.
	var miiData []byte
	s.db.QueryRowContext(ctx, `SELECT mii_data FROM user_settings WHERE pid = $1`, pid).Scan(&miiData)
	miiDataB64 := base64.StdEncoding.EncodeToString(miiData)

	issueTimeProto := &ProtoTimestamp{
		Seconds: issued.Unix(),
		Nanos:   int32(issued.Nanosecond()),
	}

	return &ExchangeServiceTokenResponse{
		Pnid: &PNIDResponse{
			Pid:               pid,
			Username:          username,
			AccessLevel:       int32(accessLevelForPID(pid)),
			ServerAccessLevel: "prod",
			Mii:               &pb.Mii{Name: miiName, Data: miiDataB64},
		},
		TokenInfo: &TokenInfoMsg{
			SystemType: int32(systemType),
			TokenType:  4, // TOKEN_TYPE_INDEPENDENT_SERVICE
			Pid:        uint64(pid),
			IssueTime:  issueTimeProto,
		},
	}, nil
}

// ── api.API proto message stubs ────────────────────────────────

type LoginRequest struct {
	GrantType string `protobuf:"bytes,1,opt,name=grant_type,json=grantType,proto3"`
	Username  string `protobuf:"bytes,2,opt,name=username,proto3"`
	Password  string `protobuf:"bytes,3,opt,name=password,proto3"`
}

func (x *LoginRequest) Reset()         { *x = LoginRequest{} }
func (x *LoginRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*LoginRequest) ProtoMessage()    {}

type LoginResponse struct {
	ExpiresIn    uint32 `protobuf:"varint,1,opt,name=expires_in,json=expiresIn,proto3"`
	TokenType    string `protobuf:"bytes,2,opt,name=token_type,json=tokenType,proto3"`
	AccessToken  string `protobuf:"bytes,3,opt,name=access_token,json=accessToken,proto3"`
	RefreshToken string `protobuf:"bytes,4,opt,name=refresh_token,json=refreshToken,proto3"`
}

func (x *LoginResponse) Reset()         { *x = LoginResponse{} }
func (x *LoginResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*LoginResponse) ProtoMessage()    {}

type Empty struct{}

func (x *Empty) Reset()         {}
func (x *Empty) String() string { return "" }
func (*Empty) ProtoMessage()    {}

type ApiGetUserDataResponse struct {
	Pid               uint32 `protobuf:"varint,4,opt,name=pid,proto3"`
	Username          string `protobuf:"bytes,5,opt,name=username,proto3"`
	AccessLevel       int32  `protobuf:"varint,6,opt,name=access_level,json=accessLevel,proto3"`
	ServerAccessLevel string `protobuf:"bytes,7,opt,name=server_access_level,json=serverAccessLevel,proto3"`
}

func (x *ApiGetUserDataResponse) Reset()         { *x = ApiGetUserDataResponse{} }
func (x *ApiGetUserDataResponse) String() string { return fmt.Sprintf("%+v", *x) }
func (*ApiGetUserDataResponse) ProtoMessage()    {}

// ── api.API server ─────────────────────────────────────────────

type APIServiceServer interface {
	apiLogin(ctx context.Context, req *LoginRequest) (*LoginResponse, error)
	apiGetUserData(ctx context.Context, req *Empty) (*ApiGetUserDataResponse, error)
}

type apiServer struct {
	db *sql.DB
}

func hashPwd(p string) string {
	h := sha256.Sum256([]byte(p))
	return hex.EncodeToString(h[:])
}

func (s *apiServer) apiLogin(ctx context.Context, req *LoginRequest) (*LoginResponse, error) {
	clientIP := ""
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if ips := md["x-real-ip"]; len(ips) > 0 {
			clientIP = ips[0]
		}
	}
	pid, pnid, err := pretendoAuth(req.Username, req.Password, clientIP)
	if err != nil {
		log.Printf("web login failed for %q: %v", req.Username, err)
		return nil, status.Error(codes.Unauthenticated, "invalid username or password")
	}

	buf := make([]byte, 32)
	rand.Read(buf)
	token := hex.EncodeToString(buf)

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO nex_sessions (token, ip, pid, pnid, expires_at)
		 VALUES ($1, '', $2, $3, NOW() + INTERVAL '24 hours')`,
		token, pid, pnid,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "session error: %v", err)
	}
	log.Printf("web login: %s (PID %d)", pnid, pid)
	return &LoginResponse{ExpiresIn: 86400, TokenType: "Bearer", AccessToken: token, RefreshToken: token}, nil
}

func (s *apiServer) apiGetUserData(ctx context.Context, _ *Empty) (*ApiGetUserDataResponse, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "no metadata")
	}
	tokens := md["x-token"]
	if len(tokens) == 0 {
		return nil, status.Error(codes.Unauthenticated, "missing X-Token")
	}

	var pid uint32
	var pnid string
	err := s.db.QueryRowContext(ctx,
		`SELECT pid, pnid FROM nex_sessions WHERE token = $1 AND expires_at > NOW()`,
		tokens[0],
	).Scan(&pid, &pnid)
	if err == sql.ErrNoRows {
		return nil, status.Error(codes.Unauthenticated, "invalid or expired token")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "db error: %v", err)
	}
	return &ApiGetUserDataResponse{Pid: pid, Username: pnid, AccessLevel: int32(accessLevelForPID(pid)), ServerAccessLevel: "prod"}, nil
}

// ── Register api.API manually ──────────────────────────────────

func registerAPIService(s *grpc.Server, srv APIServiceServer) {
	desc := grpc.ServiceDesc{
		ServiceName: "api.API",
		HandlerType: (*APIServiceServer)(nil),
		Methods: []grpc.MethodDesc{
			{
				MethodName: "Login",
				Handler: func(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
					in := &LoginRequest{}
					if err := dec(in); err != nil {
						return nil, err
					}
					handler := func(ctx context.Context, req interface{}) (interface{}, error) {
						return srv.(APIServiceServer).apiLogin(ctx, req.(*LoginRequest))
					}
					if interceptor == nil {
						return handler(ctx, in)
					}
					return interceptor(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/api.API/Login"}, handler)
				},
			},
			{
				MethodName: "GetUserData",
				Handler: func(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
					in := &Empty{}
					if err := dec(in); err != nil {
						return nil, err
					}
					handler := func(ctx context.Context, req interface{}) (interface{}, error) {
						return srv.(APIServiceServer).apiGetUserData(ctx, req.(*Empty))
					}
					if interceptor == nil {
						return handler(ctx, in)
					}
					return interceptor(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/api.API/GetUserData"}, handler)
				},
			},
		},
		Streams:  []grpc.StreamDesc{},
		Metadata: "api/api_service.proto",
	}
	s.RegisterService(&desc, srv)
}

// ── api.v2.ApiService alias ────────────────────────────────────
// miiverse-api (post Juxtaposition upgrade) calls api.v2.ApiService.
// Wire format is identical to api.API; reuse the same handlers.

type apiV2Iface interface{}

func registerAPIServiceV2(s *grpc.Server, srv APIServiceServer) {
	desc := grpc.ServiceDesc{
		ServiceName: "api.v2.ApiService",
		HandlerType: (*apiV2Iface)(nil),
		Methods: []grpc.MethodDesc{
			{
				MethodName: "Login",
				Handler: func(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
					in := &LoginRequest{}
					if err := dec(in); err != nil {
						return nil, err
					}
					handler := func(ctx context.Context, req interface{}) (interface{}, error) {
						return srv.(APIServiceServer).apiLogin(ctx, req.(*LoginRequest))
					}
					if interceptor == nil {
						return handler(ctx, in)
					}
					return interceptor(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/api.v2.ApiService/Login"}, handler)
				},
			},
			{
				MethodName: "GetUserData",
				Handler: func(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
					in := &Empty{}
					if err := dec(in); err != nil {
						return nil, err
					}
					handler := func(ctx context.Context, req interface{}) (interface{}, error) {
						return srv.(APIServiceServer).apiGetUserData(ctx, req.(*Empty))
					}
					if interceptor == nil {
						return handler(ctx, in)
					}
					return interceptor(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/api.v2.ApiService/GetUserData"}, handler)
				},
			},
		},
		Streams:  []grpc.StreamDesc{},
		Metadata: "api/v2/api_service.proto",
	}
	s.RegisterService(&desc, srv)
}

// ── account.v2.AccountService alias ───────────────────────────
// Friends server (nex-protocols-common-go v2) calls /account.v2.AccountService/*.
// Wire format (protobuf field numbers) is identical to v1, so we can reuse the
// same handlers under the v2 service name.

type accountV2Iface interface{}

func registerAccountServiceV2(s *grpc.Server, srv *accountServer) {
	desc := grpc.ServiceDesc{
		ServiceName: "account.v2.AccountService",
		HandlerType: (*accountV2Iface)(nil),
		Methods: []grpc.MethodDesc{
			{
				MethodName: "GetNEXPassword",
				Handler: func(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
					in := new(pb.GetNEXPasswordRequest)
					if err := dec(in); err != nil {
						return nil, err
					}
					handler := func(ctx context.Context, req interface{}) (interface{}, error) {
						return srv.(*accountServer).GetNEXPassword(ctx, req.(*pb.GetNEXPasswordRequest))
					}
					if interceptor == nil {
						return handler(ctx, in)
					}
					return interceptor(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/account.v2.AccountService/GetNEXPassword"}, handler)
				},
			},
			{
				MethodName: "GetNEXData",
				Handler: func(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
					in := new(pb.GetNEXDataRequest)
					if err := dec(in); err != nil {
						return nil, err
					}
					handler := func(ctx context.Context, req interface{}) (interface{}, error) {
						return srv.(*accountServer).GetNEXData(ctx, req.(*pb.GetNEXDataRequest))
					}
					if interceptor == nil {
						return handler(ctx, in)
					}
					return interceptor(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/account.v2.AccountService/GetNEXData"}, handler)
				},
			},
			{
				MethodName: "GetUserData",
				Handler: func(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
					in := new(pb.GetUserDataRequest)
					if err := dec(in); err != nil {
						return nil, err
					}
					handler := func(ctx context.Context, req interface{}) (interface{}, error) {
						return srv.(*accountServer).GetUserData(ctx, req.(*pb.GetUserDataRequest))
					}
					if interceptor == nil {
						return handler(ctx, in)
					}
					return interceptor(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/account.v2.AccountService/GetUserData"}, handler)
				},
			},
			{
				MethodName: "ExchangeIndependentServiceTokenForUserData",
				Handler: func(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
					in := new(ExchangeServiceTokenRequest)
					if err := dec(in); err != nil {
						return nil, err
					}
					handler := func(ctx context.Context, req interface{}) (interface{}, error) {
						return srv.(*accountServer).ExchangeIndependentServiceTokenForUserData(ctx, req.(*ExchangeServiceTokenRequest))
					}
					if interceptor == nil {
						return handler(ctx, in)
					}
					return interceptor(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/account.v2.AccountService/ExchangeIndependentServiceTokenForUserData"}, handler)
				},
			},
		},
		Streams:  []grpc.StreamDesc{},
		Metadata: "account/v2/account_service.proto",
	}
	s.RegisterService(&desc, srv)
}

// ── Auth interceptor ───────────────────────────────────────────

func apiKeyInterceptor(apiKey string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if info.FullMethod == "/api.API/Login" || info.FullMethod == "/api.API/GetUserData" ||
			info.FullMethod == "/api.v2.ApiService/Login" || info.FullMethod == "/api.v2.ApiService/GetUserData" {
			return handler(ctx, req)
		}
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok || len(md["x-api-key"]) == 0 || md["x-api-key"][0] != apiKey {
			return nil, status.Error(codes.Unauthenticated, "invalid API key")
		}
		return handler(ctx, req)
	}
}

// ── main ───────────────────────────────────────────────────────

func main() {
	godotenv.Load("../.env", "../../wiiu-chat-secure/.env")

	postgresURI := os.Getenv("PN_WUC_POSTGRES_URI")
	if postgresURI == "" {
		log.Fatal("PN_WUC_POSTGRES_URI not set")
	}
	apiKey := os.Getenv("PN_WUC_ACCOUNT_GRPC_API_KEY")
	if apiKey == "" {
		log.Fatal("PN_WUC_ACCOUNT_GRPC_API_KEY not set")
	}

	db, err := sql.Open("postgres", postgresURI)
	if err != nil {
		log.Fatalf("failed to open db: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(schema); err != nil {
		log.Fatalf("failed to create schema: %v", err)
	}

	port := os.Getenv("PN_WUC_ACCOUNT_GRPC_PORT")
	if port == "" {
		port = "9001"
	}
	lis, err := net.Listen("tcp", ":"+port)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	s := grpc.NewServer(grpc.UnaryInterceptor(apiKeyInterceptor(apiKey)))
	acctSrv := &accountServer{db: db}
	pb.RegisterAccountServer(s, acctSrv)
	registerAccountServiceV2(s, acctSrv)
	apiSrv := &apiServer{db: db}
	registerAPIService(s, apiSrv)
	registerAPIServiceV2(s, apiSrv)

	log.Printf("account gRPC listening on :%s", port)
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
