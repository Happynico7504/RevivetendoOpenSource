package database

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// TriggerPretendoSync asks account-proxy to run a one-shot fetch_friends.py for the given PID.
// Safe to call with `go` — returns quickly regardless of the sync outcome.
func TriggerPretendoSync(pid uint64) {
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:9191/internal/sync/%d", pid), "", nil)
	if err != nil {
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// StartPretendoPresence launches a persistent fetch_friends.py --keep-alive process for the
// given PID, keeping them visible as online on Pretendo for as long as they're connected here.
func StartPretendoPresence(pid uint64) {
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:9191/internal/presence/start/%d", pid), "", nil)
	if err != nil {
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// ForwardPresenceCommand sends a JSON command to the keep-alive process's Unix socket via
// account-proxy. Returns true if the command was delivered successfully.
// Falls back gracefully when no keep-alive is running (503 → caller should use TriggerPretendoSync).
func ForwardPresenceCommand(pid uint64, cmdJSON string) bool {
	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:9191/internal/presence/command/%d", pid),
		"application/json",
		strings.NewReader(cmdJSON),
	)
	if err != nil || resp.StatusCode != http.StatusOK {
		return false
	}
	defer resp.Body.Close()
	var result struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false
	}
	return result.OK
}

// StopPretendoPresence kills the keep-alive process for the given PID (Pretendo detects
// the TCP drop and marks them offline) then runs a final one-shot sync for pending commands.
func StopPretendoPresence(pid uint64) {
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:9191/internal/presence/stop/%d", pid), "", nil)
	if err != nil {
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}
