package database

import (
	"database/sql"
	"log"
	"os"
	"strconv"
	"time"

	_ "github.com/lib/pq"
)

var Postgres *sql.DB

func Connect() {
	uri := os.Getenv("PN_WUC_POSTGRES_URI")
	if uri == "" {
		log.Fatal("PN_WUC_POSTGRES_URI not set")
	}
	db, err := sql.Open("postgres", uri)
	if err != nil {
		log.Fatalf("friends-nex db: %v", err)
	}
	Postgres = db
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS user_settings (
			pid                  BIGINT PRIMARY KEY,
			show_online_presence BOOLEAN NOT NULL DEFAULT TRUE,
			show_current_title   BOOLEAN NOT NULL DEFAULT TRUE,
			block_friend_requests BOOLEAN NOT NULL DEFAULT FALSE,
			comment_unknown      SMALLINT NOT NULL DEFAULT 0,
			comment_text         TEXT NOT NULL DEFAULT '',
			comment_changed_at   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
		)`); err != nil {
		log.Fatalf("friends-nex: create user_settings: %v", err)
	}

	// Local NNAInfo+presence forwarded from the Wii U, used to represent the user on Pretendo.
	for _, col := range []string{
		`ALTER TABLE user_settings ADD COLUMN IF NOT EXISTS nnid TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE user_settings ADD COLUMN IF NOT EXISTS mii_name TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE user_settings ADD COLUMN IF NOT EXISTS mii_data BYTEA`,
		`ALTER TABLE user_settings ADD COLUMN IF NOT EXISTS is_online BOOLEAN NOT NULL DEFAULT FALSE`,
		`ALTER TABLE user_settings ADD COLUMN IF NOT EXISTS presence_title_id BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE user_settings ADD COLUMN IF NOT EXISTS presence_title_version SMALLINT NOT NULL DEFAULT 0`,
		`ALTER TABLE user_settings ADD COLUMN IF NOT EXISTS presence_game_server_id INT NOT NULL DEFAULT 0`,
		`ALTER TABLE user_settings ADD COLUMN IF NOT EXISTS last_online_at TIMESTAMPTZ`,
		`ALTER TABLE user_settings ADD COLUMN IF NOT EXISTS last_heartbeat_at TIMESTAMPTZ`,
	} {
		db.Exec(col)
	}

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS pending_pretendo_commands (
			id         BIGSERIAL   PRIMARY KEY,
			pid        BIGINT      NOT NULL,
			method     TEXT        NOT NULL,
			args_json  TEXT        NOT NULL DEFAULT '{}',
			created_at TIMESTAMPTZ DEFAULT NOW()
		)`); err != nil {
		log.Fatalf("friends-nex: create pending_pretendo_commands: %v", err)
	}

	db.Exec(`CREATE TABLE IF NOT EXISTS pretendo_friend_requests (
		id              BIGINT NOT NULL DEFAULT 0,
		owner_pid       BIGINT NOT NULL,
		requester_pid   BIGINT NOT NULL,
		nnid            TEXT NOT NULL DEFAULT '',
		mii_name        TEXT NOT NULL DEFAULT '',
		mii_data        BYTEA,
		message         TEXT NOT NULL DEFAULT '',
		unk1            SMALLINT NOT NULL DEFAULT 0,
		unk2            SMALLINT NOT NULL DEFAULT 0,
		unk3            SMALLINT NOT NULL DEFAULT 0,
		str_field       TEXT NOT NULL DEFAULT '',
		title_id        BIGINT NOT NULL DEFAULT 0,
		title_version   SMALLINT NOT NULL DEFAULT 0,
		sent_on         TIMESTAMPTZ,
		expires_on      TIMESTAMPTZ,
		PRIMARY KEY (owner_pid, requester_pid)
	)`)
}

type FriendRequestRow struct {
	ID            uint64
	RequesterPID  uint64
	NNID          string
	MiiName       string
	MiiData       []byte
	Message       string
	Unk1          uint8
	Unk2          uint8
	Unk3          uint8
	StrField      string
	TitleID       uint64
	TitleVersion  uint16
	SentOn        time.Time
	ExpiresOn     time.Time
}

func GetIncomingFriendRequests(ownerPID uint64) []FriendRequestRow {
	rows, err := Postgres.Query(`
		SELECT id, requester_pid, nnid, mii_name, mii_data,
		       message, unk1, unk2, unk3, str_field,
		       title_id, title_version,
		       COALESCE(sent_on, NOW()), COALESCE(expires_on, NOW() + INTERVAL '30 days')
		FROM pretendo_friend_requests WHERE owner_pid = $1`, ownerPID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var result []FriendRequestRow
	for rows.Next() {
		var r FriendRequestRow
		if err := rows.Scan(&r.ID, &r.RequesterPID, &r.NNID, &r.MiiName, &r.MiiData,
			&r.Message, &r.Unk1, &r.Unk2, &r.Unk3, &r.StrField,
			&r.TitleID, &r.TitleVersion, &r.SentOn, &r.ExpiresOn); err == nil {
			result = append(result, r)
		}
	}
	return result
}

func DeleteIncomingFriendRequest(ownerPID, requesterPID uint64) {
	Postgres.Exec(`DELETE FROM pretendo_friend_requests WHERE owner_pid = $1 AND requester_pid = $2`, ownerPID, requesterPID)
}

// GetFriendRequestByID returns the requester PID for the given request ID and owner.
// Returns 0 if not found.
func GetFriendRequestByID(ownerPID uint64, requestID uint64) uint64 {
	var requesterPID uint64
	Postgres.QueryRow(`SELECT requester_pid FROM pretendo_friend_requests WHERE owner_pid = $1 AND id = $2`, ownerPID, requestID).Scan(&requesterPID)
	return requesterPID
}

func DeleteIncomingFriendRequestByID(ownerPID uint64, requestID uint64) {
	Postgres.Exec(`DELETE FROM pretendo_friend_requests WHERE owner_pid = $1 AND id = $2`, ownerPID, requestID)
}

type UserSettings struct {
	ShowOnlinePresence   bool
	ShowCurrentTitle     bool
	BlockFriendRequests  bool
	CommentUnknown       uint8
	CommentText          string
	CommentChangedAt     time.Time
}

func GetUserSettings(pid uint64) UserSettings {
	var s UserSettings
	err := Postgres.QueryRow(`
		SELECT show_online_presence, show_current_title, block_friend_requests,
		       comment_unknown, comment_text, comment_changed_at
		FROM user_settings WHERE pid = $1`, pid).Scan(
		&s.ShowOnlinePresence, &s.ShowCurrentTitle, &s.BlockFriendRequests,
		&s.CommentUnknown, &s.CommentText, &s.CommentChangedAt,
	)
	if err != nil {
		// Return defaults if not found.
		return UserSettings{ShowOnlinePresence: true, ShowCurrentTitle: true, CommentChangedAt: time.Now()}
	}
	return s
}

func SaveUserPreference(pid uint64, showOnlinePresence, showCurrentTitle, blockFriendRequests bool) {
	Postgres.Exec(`
		INSERT INTO user_settings (pid, show_online_presence, show_current_title, block_friend_requests)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (pid) DO UPDATE SET
			show_online_presence  = EXCLUDED.show_online_presence,
			show_current_title    = EXCLUDED.show_current_title,
			block_friend_requests = EXCLUDED.block_friend_requests`,
		pid, showOnlinePresence, showCurrentTitle, blockFriendRequests)
}

func SaveUserComment(pid uint64, unknown uint8, text string, changedAt time.Time) {
	Postgres.Exec(`
		INSERT INTO user_settings (pid, comment_unknown, comment_text, comment_changed_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (pid) DO UPDATE SET
			comment_unknown    = EXCLUDED.comment_unknown,
			comment_text       = EXCLUDED.comment_text,
			comment_changed_at = EXCLUDED.comment_changed_at`,
		pid, unknown, text, changedAt)
}

func GetNEXPassword(pidInt uint64) (string, bool) {
	var pw string
	err := Postgres.QueryRow(`SELECT friends_nex_password FROM nex_accounts WHERE pid = $1 AND friends_nex_password IS NOT NULL`, pidInt).Scan(&pw)
	if err != nil {
		return "", false
	}
	return pw, pw != ""
}

type FriendRow struct {
	FriendPID          uint32
	FriendNNID         string
	MiiName            string
	MiiData            []byte
	IsOnline           bool
	GameServerID       uint32
	TitleID            uint64
	TitleVersion       uint16
	PresenceFlags      uint32
	PresencePID        uint64
	GatheringID        uint32
	PresenceUnk5       uint8
	PresenceUnk6       uint8
	PresenceUnk7       uint8
	BecameFriend       time.Time
	LastOnline         time.Time
	CommentText        string
	CommentUnknown     uint8
	CommentChangedAt   time.Time
}

func GetFriends(ownerPID uint64) []FriendRow {
	rows, err := Postgres.Query(`
		SELECT f.friend_pid,
		       COALESCE(NULLIF(f.friend_nnid,''), pc.pnid, ''),
		       COALESCE(NULLIF(f.mii_name,''), m.mii_name, ''),
		       COALESCE(f.mii_data, ''::bytea),
		       COALESCE(f.is_online, false), COALESCE(f.game_server_id, 0),
		       COALESCE(NULLIF(s.presence_title_id, 0), NULLIF(f.title_id, 0), 0),
		       COALESCE(NULLIF(s.presence_title_version, 0), NULLIF(f.title_version, 0), 0),
		       COALESCE(f.presence_flags, 0), COALESCE(f.presence_pid, 0),
		       COALESCE(f.presence_gathering_id, 0),
		       COALESCE(f.presence_unk5, 3), COALESCE(f.presence_unk6, 3), COALESCE(f.presence_unk7, 3),
		       f.befriended_at, f.last_online,
		       COALESCE(s.comment_text, ''), COALESCE(s.comment_unknown, 0),
		       COALESCE(s.comment_changed_at, NOW())
		FROM pretendo_friends f
		LEFT JOIN user_settings s ON s.pid = f.friend_pid
		LEFT JOIN mii_names m ON m.pid = f.friend_pid
		LEFT JOIN pnid_cache pc ON pc.pid = f.friend_pid
		WHERE f.owner_pid = $1`, ownerPID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var friends []FriendRow
	for rows.Next() {
		var f FriendRow
		var befriendedAt, lastOnlineAt sql.NullTime
		var commentChangedAt sql.NullTime
		if err := rows.Scan(&f.FriendPID, &f.FriendNNID, &f.MiiName, &f.MiiData,
			&f.IsOnline, &f.GameServerID, &f.TitleID, &f.TitleVersion,
			&f.PresenceFlags, &f.PresencePID, &f.GatheringID,
			&f.PresenceUnk5, &f.PresenceUnk6, &f.PresenceUnk7,
			&befriendedAt, &lastOnlineAt,
			&f.CommentText, &f.CommentUnknown, &commentChangedAt); err == nil {
			if commentChangedAt.Valid {
				f.CommentChangedAt = commentChangedAt.Time
			}
			if befriendedAt.Valid {
				f.BecameFriend = befriendedAt.Time
			}
			if lastOnlineAt.Valid {
				f.LastOnline = lastOnlineAt.Time
			}
			friends = append(friends, f)
		}
	}
	return friends
}

// GetBasicInfoForPID resolves NNID, Mii name, and Mii data for a PID using local sources.
// Sources in priority order: nex_accounts/pnid_cache, then pretendo_friends.
func GetBasicInfoForPID(pid uint64) (nnid, miiName string, miiData []byte) {
	// NNID: nex_accounts first (skip numeric PID usernames), then pnid_cache, then pretendo_friends
	Postgres.QueryRow(`SELECT username FROM nex_accounts WHERE pid = $1`, pid).Scan(&nnid)
	if nnid == strconv.FormatUint(pid, 10) {
		nnid = ""
	}
	if nnid == "" {
		Postgres.QueryRow(`SELECT pnid FROM pnid_cache WHERE pid = $1`, pid).Scan(&nnid)
	}
	if nnid == "" {
		Postgres.QueryRow(`SELECT friend_nnid FROM pretendo_friends WHERE friend_pid = $1 AND friend_nnid != '' LIMIT 1`, pid).Scan(&nnid)
	}

	// Mii name: mii_names first, then pretendo_friends
	Postgres.QueryRow(`SELECT mii_name FROM mii_names WHERE pid = $1`, pid).Scan(&miiName)
	if miiName == "" {
		Postgres.QueryRow(`SELECT mii_name FROM pretendo_friends WHERE friend_pid = $1 AND mii_name != '' LIMIT 1`, pid).Scan(&miiName)
	}

	// Mii data: pretendo_friends
	Postgres.QueryRow(`SELECT mii_data FROM pretendo_friends WHERE friend_pid = $1 AND mii_data IS NOT NULL LIMIT 1`, pid).Scan(&miiData)

	return
}

func SaveLocalNNAAndPresence(pid uint64, nnid, miiName string, miiData []byte, isOnline bool, titleID uint64, titleVersion uint16, gameServerID uint32) {
	Postgres.Exec(`
		INSERT INTO user_settings
			(pid, nnid, mii_name, mii_data, is_online,
			 presence_title_id, presence_title_version, presence_game_server_id,
			 last_heartbeat_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
		ON CONFLICT (pid) DO UPDATE SET
			nnid                    = EXCLUDED.nnid,
			mii_name                = EXCLUDED.mii_name,
			mii_data                = EXCLUDED.mii_data,
			is_online               = EXCLUDED.is_online,
			presence_title_id       = EXCLUDED.presence_title_id,
			presence_title_version  = EXCLUDED.presence_title_version,
			presence_game_server_id = EXCLUDED.presence_game_server_id,
			last_heartbeat_at       = NOW()`,
		pid, nnid, miiName, miiData, isOnline, titleID, titleVersion, gameServerID)
}

func SaveLocalPresence(pid uint64, _ bool, titleID uint64, titleVersion uint16, gameServerID uint32) {
	Postgres.Exec(`
		INSERT INTO user_settings
			(pid, presence_title_id, presence_title_version, presence_game_server_id,
			 last_heartbeat_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (pid) DO UPDATE SET
			presence_title_id       = EXCLUDED.presence_title_id,
			presence_title_version  = EXCLUDED.presence_title_version,
			presence_game_server_id = EXCLUDED.presence_game_server_id,
			last_heartbeat_at       = NOW()`,
		pid, titleID, titleVersion, gameServerID)
}

func SaveLocalMii(pid uint64, miiName string, miiData []byte) {
	Postgres.Exec(`
		INSERT INTO user_settings (pid, mii_name, mii_data)
		VALUES ($1, $2, $3)
		ON CONFLICT (pid) DO UPDATE SET
			mii_name = EXCLUDED.mii_name,
			mii_data = EXCLUDED.mii_data`,
		pid, miiName, miiData)
}

func SetOnlineStatus(pid uint64, isOnline bool) {
	if isOnline {
		Postgres.Exec(`INSERT INTO user_settings (pid, is_online, last_heartbeat_at)
			VALUES ($1, TRUE, NOW())
			ON CONFLICT (pid) DO UPDATE SET is_online = TRUE, last_heartbeat_at = NOW()`, pid)
	} else {
		Postgres.Exec(`UPDATE user_settings SET is_online = FALSE, last_online_at = NOW() WHERE pid = $1`, pid)
	}
}

func QueuePretendoCommand(pid uint64, method, argsJSON string) {
	Postgres.Exec(`INSERT INTO pending_pretendo_commands (pid, method, args_json) VALUES ($1, $2, $3)`,
		pid, method, argsJSON)
}

func DeleteLocalFriend(ownerPID, friendPID uint64) {
	Postgres.Exec(`DELETE FROM pretendo_friends WHERE owner_pid = $1 AND friend_pid = $2`, ownerPID, friendPID)
}

func ValidateSessionToken(token string) bool {
	var count int
	err := Postgres.QueryRow(
		`SELECT COUNT(*) FROM nex_sessions WHERE token = $1 AND expires_at > NOW()`,
		token,
	).Scan(&count)
	return err == nil && count > 0
}
