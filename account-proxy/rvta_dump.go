package main

// RvtA dump uploads: the latest Luma crash dump (/luma/dumps/arm11|arm9/*.dmp)
// and, when the player presses L+R+START+SELECT, a memory dump of the running
// application. Dumps can be tens of MB, so they are uploaded in chunks over
// several rounds and resume where they stopped:
//
//	POST https://rvta.nicoch.net/v1/dump
//	X-RvtA-Id, X-RvtA-FC, X-RvtA-Version   as for /v1/upload
//	X-RvtA-Dump-Kind:   "crash" or "memory"
//	X-RvtA-Dump-Name:   file name, e.g. crash_dump_00000012.dmp or mem-0004000000134600-1791700000.bin
//	X-RvtA-Dump-Offset: where this chunk starts
//	X-RvtA-Dump-Total:  size of the whole dump
//	body:               the chunk
//
// Only appending at the current end is accepted. The answer is always
// "ok <bytes stored so far>" (200) or "size <bytes stored so far>" (409, the
// offset did not match), so RvtA can continue from what the server has.

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
)

// Where dumps are stored, one folder per console id (a variable for the tests).
var rvtaDumpDir = "/nico-pretendo-bridge/rvta-dumps"

const (
	rvtaDumpMaxTotal     = 160 << 20 // a N3DS app has at most 124 MB, plus the region table
	rvtaDumpMaxChunk     = 1 << 20
	rvtaDumpConsoleQuota = 1 << 30
)

// Dumps are refused when the disk has less than this left (a variable for the tests).
var rvtaDumpMinFreeDisk int64 = 5 << 30

var (
	rvtaDumpNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,80}$`)
	rvtaDumpKinds       = map[string]bool{"crash": true, "memory": true}
)

func ensureRvtaDumpTable() {
	db.Exec(`CREATE TABLE IF NOT EXISTS rvta_dumps (
		console_id   TEXT NOT NULL,
		name         TEXT NOT NULL,
		kind         TEXT NOT NULL,
		friend_code  BIGINT NOT NULL,
		total        BIGINT NOT NULL,
		received     BIGINT NOT NULL DEFAULT 0,
		started_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		completed_at TIMESTAMPTZ,
		PRIMARY KEY (console_id, name)
	)`)
}

func rvtaDirSize(dir string) int64 {
	var total int64
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && !e.IsDir() {
			total += info.Size()
		}
	}
	return total
}

func rvtaFreeDisk(path string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

func handleRvtaDump(w http.ResponseWriter, r *http.Request) {
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
	kind := r.Header.Get("X-RvtA-Dump-Kind")
	name := r.Header.Get("X-RvtA-Dump-Name")
	if !rvtaDumpKinds[kind] || !rvtaDumpNamePattern.MatchString(name) || name[0] == '.' {
		http.Error(w, "bad dump kind or name", http.StatusBadRequest)
		return
	}
	offset, err1 := strconv.ParseInt(r.Header.Get("X-RvtA-Dump-Offset"), 10, 64)
	total, err2 := strconv.ParseInt(r.Header.Get("X-RvtA-Dump-Total"), 10, 64)
	if err1 != nil || err2 != nil || offset < 0 || total <= 0 || total > rvtaDumpMaxTotal || offset > total {
		http.Error(w, "bad offset or total", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, rvtaDumpMaxChunk+1))
	if err != nil || len(body) > rvtaDumpMaxChunk {
		http.Error(w, "bad chunk", http.StatusBadRequest)
		return
	}
	if offset+int64(len(body)) > total {
		http.Error(w, "chunk past the end", http.StatusBadRequest)
		return
	}

	dir := filepath.Join(rvtaDumpDir, id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		log.Printf("[rvta] dump dir: %v", err)
		http.Error(w, "storage error", http.StatusServiceUnavailable)
		return
	}
	path := filepath.Join(dir, kind+"-"+name)
	var have int64
	if info, err := os.Stat(path); err == nil {
		have = info.Size()
	}
	if offset != have {
		w.WriteHeader(http.StatusConflict)
		fmt.Fprintf(w, "size %d\n", have)
		return
	}
	if offset == 0 && rvtaDirSize(dir)+total > rvtaDumpConsoleQuota {
		http.Error(w, "quota exceeded", http.StatusInsufficientStorage)
		return
	}
	if rvtaFreeDisk(rvtaDumpDir) < rvtaDumpMinFreeDisk {
		log.Printf("[rvta] dump refused: less than 5 GB free")
		http.Error(w, "server storage full", http.StatusServiceUnavailable)
		return
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o640)
	if err == nil {
		_, err = f.Write(body)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		log.Printf("[rvta] dump write: %v", err)
		http.Error(w, "storage error", http.StatusServiceUnavailable)
		return
	}
	have += int64(len(body))

	if db != nil {
		db.Exec(`INSERT INTO rvta_dumps (console_id, name, kind, friend_code, total, received) VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (console_id, name) DO UPDATE SET received = EXCLUDED.received,
		completed_at = CASE WHEN EXCLUDED.received >= rvta_dumps.total THEN NOW() ELSE NULL END`,
			id, kind+"-"+name, kind, fc, total, have)
	}
	if have == total {
		log.Printf("[rvta] %s fc=%d %s dump complete: %s (%d bytes)", id[:8], fc, kind, name, total)
	}
	fmt.Fprintf(w, "ok %d\n", have)
}
