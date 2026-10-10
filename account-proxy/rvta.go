package main

// RvtA: the Revivetendo analytics sysmodule on 3DS consoles (Nimbus patches).
// It samples battery, battery temperature, Wi-Fi and shell/sleep state into a
// CSV on the SD card and uploads that file here every 15 minutes while online,
// then clears it. The data exists to tell power/thermal problems apart from
// real software bugs when a crash dump comes in.
//
// Upload: POST https://rvta.nicoch.net/v1/upload
//   X-RvtA-Id:      random per-console id (32 lowercase hex), generated once on
//                   the console; never a serial number or MAC
//   X-RvtA-Version: RvtA version
//   X-RvtA-FC:      friend code (12 digits, no dashes) of the friend account the
//                   log was written under, or 0 when none was loaded. RvtA keeps
//                   one log file per friend code, so a console switching between
//                   its Nintendo and Pretendo friend accounts never mixes them
//   body:           the CSV file, one sample per line (see rvtaColumns)
//
// Every line carries a per-console sequence number that RvtA keeps across
// reboots, so a re-sent file (reply lost after we stored it) adds nothing.
// No IP address is stored. The friend code does identify the player, which is
// what lets a crash report be matched to its samples.

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	rvtaMaxBody  = 1 << 20 // ~15 min of samples is a few KB; a long offline stretch is capped on the console too
	rvtaMaxLines = 20000
)

var rvtaIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Column order of one CSV line, as written by RvtA:
//
//	seq,time,event,battery_pct,battery_mv,charging,adapter,temp_c,net_state,wifi_connected,wifi_strength,
//	wireless_mode,shell_open,title_id,ac_error,frd_result,act_result
//
// time is the console clock in Unix seconds (user-set, so it can be off);
// event is one of sample, boot, sleep, wake, shell_open, shell_close, title
// (another application started) and error (one of the error codes changed).
// battery_mv 0 / temp_c -128 mean the MCU could not be read. net_state is the
// raw network-state byte of the 3DS shared page, wireless_mode the NDM
// exclusive state (0 none, 1 internet, 2 local play, 3/4 StreetPass).
// title_id is the running application (0 = none, e.g. HOME Menu); ac_error,
// frd_result and act_result are the last error codes the Wi-Fi connection,
// Friends and account services recorded.
const rvtaColumns = 17

var rvtaEvents = map[string]bool{"sample": true, "boot": true, "sleep": true, "wake": true, "shell_open": true,
	"shell_close": true, "title": true, "error": true}

type rvtaSample struct {
	Seq           int64
	ConsoleTime   time.Time
	Event         string
	BatteryPct    int
	BatteryMV     int
	Charging      bool
	Adapter       bool
	TempC         int
	NetState      int
	WifiConnected bool
	WifiStrength  int
	WirelessMode  int
	ShellOpen     bool
	TitleID       int64
	ACError       int64
	FRDResult     int64
	ACTResult     int64
}

// parseFriendCode accepts "0" (no friend account) or a 12-digit friend code
// whose checksum matches its principal id: the code is
// (sha1(pid little-endian)[0] >> 1) << 32 | pid.
func parseFriendCode(v string) (int64, bool) {
	v = strings.ReplaceAll(v, "-", "")
	if v == "0" {
		return 0, true
	}
	if len(v) != 12 {
		return 0, false
	}
	fc, err := strconv.ParseInt(v, 10, 64)
	if err != nil || fc <= 0 {
		return 0, false
	}
	pid := uint32(fc)
	var le [4]byte
	binary.LittleEndian.PutUint32(le[:], pid)
	sum := sha1.Sum(le[:])
	if fc>>32 != int64(sum[0]>>1) {
		return 0, false
	}
	return fc, true
}

func ensureRvtaTables() {
	db.Exec(`CREATE TABLE IF NOT EXISTS rvta_consoles (
		console_id TEXT PRIMARY KEY,
		version    TEXT NOT NULL DEFAULT '',
		first_seen TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		last_seen  TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`)
	db.Exec(`CREATE TABLE IF NOT EXISTS rvta_samples (
		console_id     TEXT NOT NULL,
		seq            BIGINT NOT NULL,
		friend_code    BIGINT NOT NULL,
		console_time   TIMESTAMPTZ NOT NULL,
		event          TEXT NOT NULL,
		battery_pct    SMALLINT NOT NULL,
		battery_mv     INT NOT NULL,
		charging       BOOLEAN NOT NULL,
		adapter        BOOLEAN NOT NULL,
		temp_c         SMALLINT NOT NULL,
		net_state      SMALLINT NOT NULL,
		wifi_connected BOOLEAN NOT NULL,
		wifi_strength  SMALLINT NOT NULL,
		wireless_mode  SMALLINT NOT NULL,
		shell_open     BOOLEAN NOT NULL,
		title_id       BIGINT NOT NULL,
		ac_error       BIGINT NOT NULL,
		frd_result     BIGINT NOT NULL,
		act_result     BIGINT NOT NULL,
		received_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (console_id, seq)
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS rvta_samples_time ON rvta_samples (console_id, console_time)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS rvta_samples_fc ON rvta_samples (friend_code, console_time)`)
}

// parseRvtaCSV turns the uploaded file into samples. Malformed lines are
// skipped (and counted) rather than failing the whole upload, because RvtA
// clears its log after a successful upload and a single torn line (power loss
// mid-write) must not block everything else forever.
func parseRvtaCSV(body []byte) (samples []rvtaSample, skipped int) {
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 4096), 4096)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if len(samples)+skipped >= rvtaMaxLines {
			skipped++
			continue
		}
		s, ok := parseRvtaLine(line)
		if !ok {
			skipped++
			continue
		}
		samples = append(samples, s)
	}
	return samples, skipped
}

func parseRvtaLine(line string) (rvtaSample, bool) {
	f := strings.Split(line, ",")
	if len(f) != rvtaColumns {
		return rvtaSample{}, false
	}
	n := make([]int64, len(f))
	for i, v := range f {
		if i == 2 {
			continue
		}
		x, err := strconv.ParseInt(v, 10, 64) // title ids fit: the top bits are always 0x0004
		if err != nil {
			return rvtaSample{}, false
		}
		n[i] = x
	}
	if n[0] < 0 || !rvtaEvents[f[2]] {
		return rvtaSample{}, false
	}
	inRange := func(v, lo, hi int64) bool { return v >= lo && v <= hi }
	bit := func(v int64) bool { return v == 0 || v == 1 }
	// Console clock: 2000-01-01 .. 2100-01-01 (the 3DS RTC cannot go outside that).
	u32 := func(v int64) bool { return inRange(v, 0, 0xFFFFFFFF) }
	if !inRange(n[1], 946684800, 4102444800) || !inRange(n[3], 0, 100) || !inRange(n[4], 0, 10000) ||
		!bit(n[5]) || !bit(n[6]) || !(n[7] == -128 || inRange(n[7], -40, 125)) || !inRange(n[8], 0, 255) ||
		!bit(n[9]) || !inRange(n[10], 0, 3) || !inRange(n[11], 0, 255) || !bit(n[12]) ||
		n[13] < 0 || !u32(n[14]) || !u32(n[15]) || !u32(n[16]) {
		return rvtaSample{}, false
	}
	return rvtaSample{
		Seq:           n[0],
		ConsoleTime:   time.Unix(n[1], 0).UTC(),
		Event:         f[2],
		BatteryPct:    int(n[3]),
		BatteryMV:     int(n[4]),
		Charging:      n[5] == 1,
		Adapter:       n[6] == 1,
		TempC:         int(n[7]),
		NetState:      int(n[8]),
		WifiConnected: n[9] == 1,
		WifiStrength:  int(n[10]),
		WirelessMode:  int(n[11]),
		ShellOpen:     n[12] == 1,
		TitleID:       n[13],
		ACError:       n[14],
		FRDResult:     n[15],
		ACTResult:     n[16],
	}, true
}

func handleRvta(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/upload" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.Header.Get("X-RvtA-Id")
	if !rvtaIDPattern.MatchString(id) {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	fc, ok := parseFriendCode(r.Header.Get("X-RvtA-FC"))
	if !ok {
		http.Error(w, "bad friend code", http.StatusBadRequest)
		return
	}
	version := r.Header.Get("X-RvtA-Version")
	if len(version) > 32 {
		version = version[:32]
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, rvtaMaxBody+1))
	if err != nil {
		http.Error(w, "read failed", http.StatusBadRequest)
		return
	}
	if len(body) > rvtaMaxBody {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	samples, skipped := parseRvtaCSV(body)

	tx, err := db.Begin()
	if err != nil {
		log.Printf("[rvta] db begin: %v", err)
		http.Error(w, "db error", http.StatusServiceUnavailable)
		return
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO rvta_consoles (console_id, version) VALUES ($1, $2)
		ON CONFLICT (console_id) DO UPDATE SET version = EXCLUDED.version, last_seen = NOW()`, id, version); err != nil {
		log.Printf("[rvta] console upsert: %v", err)
		http.Error(w, "db error", http.StatusServiceUnavailable)
		return
	}
	stmt, err := tx.Prepare(`INSERT INTO rvta_samples (console_id, seq, friend_code, console_time, event, battery_pct,
		battery_mv, charging, adapter, temp_c, net_state, wifi_connected, wifi_strength, wireless_mode, shell_open,
		title_id, ac_error, frd_result, act_result)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19) ON CONFLICT (console_id, seq) DO NOTHING`)
	if err != nil {
		log.Printf("[rvta] prepare: %v", err)
		http.Error(w, "db error", http.StatusServiceUnavailable)
		return
	}
	defer stmt.Close()
	stored := 0
	for _, s := range samples {
		res, err := stmt.Exec(id, s.Seq, fc, s.ConsoleTime, s.Event, s.BatteryPct, s.BatteryMV, s.Charging, s.Adapter,
			s.TempC, s.NetState, s.WifiConnected, s.WifiStrength, s.WirelessMode, s.ShellOpen, s.TitleID, s.ACError,
			s.FRDResult, s.ACTResult)
		if err != nil {
			log.Printf("[rvta] insert: %v", err)
			http.Error(w, "db error", http.StatusServiceUnavailable)
			return
		}
		if n, _ := res.RowsAffected(); n > 0 {
			stored++
		}
	}
	if err := tx.Commit(); err != nil {
		log.Printf("[rvta] commit: %v", err)
		http.Error(w, "db error", http.StatusServiceUnavailable)
		return
	}
	log.Printf("[rvta] %s fc=%d v%s: %d stored, %d duplicate, %d skipped", id[:8], fc, version, stored, len(samples)-stored, skipped)
	// RvtA clears its log only on 200 with this body prefix.
	fmt.Fprintf(w, "ok %d\n", stored)
}
