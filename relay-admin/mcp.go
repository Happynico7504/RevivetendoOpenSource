package main

// MCP (Model Context Protocol) endpoint for players' own AI tools, at
// /inkay/my/mcp. Streamable HTTP transport, stateless: every JSON-RPC request
// is a POST answered with a single application/json response (no SSE stream,
// no Mcp-Session-Id). Auth is the per-account API key from My Account, sent as
// "Authorization: Bearer <key>"; the tools (mcp_tools.go) act only as that
// account.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
)

const (
	mcpKeyPrefixLen  = 12 // "rvt_" + 8 hex chars, shown on My Account
	mcpLatestVersion = "2025-06-18"
	// Room for a base64 video chunk (tv_upload_video_chunk) or channel images.
	mcpMaxBody = 12 << 20
)

var mcpSupportedVersions = map[string]bool{"2025-06-18": true, "2025-03-26": true, "2024-11-05": true}

func mcpHashKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

// resetMCPKey replaces the account's API key and returns the new one; only its
// hash is stored, so this is the only time the full key exists server-side.
func resetMCPKey(pid int64) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	key := "rvt_" + hex.EncodeToString(b)
	_, err := db.Exec(`INSERT INTO mcp_api_keys (pid, key_hash, key_prefix) VALUES ($1, $2, $3)
		ON CONFLICT (pid) DO UPDATE SET key_hash = EXCLUDED.key_hash, key_prefix = EXCLUDED.key_prefix,
			created_at = NOW(), last_used_at = NULL`,
		pid, mcpHashKey(key), key[:mcpKeyPrefixLen])
	return key, err
}

func mcpKeyPrefix(pid int64) string {
	var prefix string
	db.QueryRow(`SELECT key_prefix FROM mcp_api_keys WHERE pid = $1`, pid).Scan(&prefix)
	return prefix
}

// mcpKeyPID returns the account a bearer key belongs to.
func mcpKeyPID(r *http.Request) (int64, bool) {
	auth := r.Header.Get("Authorization")
	if len(auth) < 7 || !strings.EqualFold(auth[:7], "Bearer ") {
		return 0, false
	}
	key := strings.TrimSpace(auth[7:])
	if !strings.HasPrefix(key, "rvt_") {
		return 0, false
	}
	var pid int64
	if err := db.QueryRow(`UPDATE mcp_api_keys SET last_used_at = NOW() WHERE key_hash = $1 RETURNING pid`,
		mcpHashKey(key)).Scan(&pid); err != nil {
		return 0, false
	}
	return pid, true
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

func mcpHandle(c mcpCall, req mcpRequest) (interface{}, *mcpError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		version := mcpLatestVersion
		if mcpSupportedVersions[p.ProtocolVersion] {
			version = p.ProtocolVersion
		}
		return map[string]interface{}{
			"protocolVersion": version,
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":      map[string]interface{}{"name": "revivetendo", "title": "Revivetendo", "version": "2.0.0"},
			"instructions":    mcpInstructions,
		}, nil
	case "ping":
		return map[string]interface{}{}, nil
	case "tools/list":
		return map[string]interface{}{"tools": mcpTools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
			return nil, &mcpError{Code: -32602, Message: "Invalid params"}
		}
		tool, ok := mcpToolsByName[p.Name]
		if !ok {
			return nil, &mcpError{Code: -32602, Message: "Unknown tool: " + p.Name}
		}
		c.args = p.Arguments
		if len(c.args) == 0 || string(c.args) == "null" {
			c.args = json.RawMessage("{}")
		}
		return tool.call(c), nil
	}
	return nil, &mcpError{Code: -32601, Message: "Method not found: " + req.Method}
}

func myMCPHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// No server-initiated SSE stream and no sessions to DELETE.
		w.Header().Set("Allow", "POST")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	pid, ok := mcpKeyPID(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="revivetendo-mcp", error="invalid_token"`)
		writeJSON(w, http.StatusUnauthorized, mcpResponse{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &mcpError{Code: -32001, Message: "Missing or invalid API key - create one on My Account"}})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, mcpMaxBody))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, mcpResponse{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &mcpError{Code: -32700, Message: "Parse error"}})
		return
	}
	var req mcpRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, mcpResponse{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &mcpError{Code: -32700, Message: "Parse error (batches are not supported)"}})
		return
	}
	if req.JSONRPC != "2.0" {
		id := req.ID
		if len(id) == 0 {
			id = json.RawMessage("null")
		}
		writeJSON(w, http.StatusBadRequest, mcpResponse{JSONRPC: "2.0", ID: id,
			Error: &mcpError{Code: -32600, Message: "Invalid Request"}})
		return
	}
	if req.Method == "" || len(req.ID) == 0 || string(req.ID) == "null" {
		// Notifications (notifications/initialized, cancellations, ...) and responses
		// need no answer; this server never sends requests of its own anyway.
		w.WriteHeader(http.StatusAccepted)
		return
	}

	result, rpcErr := mcpHandle(mcpCall{pid: pid, r: r}, req)
	if rpcErr != nil {
		writeJSON(w, http.StatusOK, mcpResponse{JSONRPC: "2.0", ID: req.ID, Error: rpcErr})
		return
	}
	if req.Method == "tools/call" {
		var p struct {
			Name string `json:"name"`
		}
		json.Unmarshal(req.Params, &p)
		log.Printf("[mcp] pid=%d tools/call %s", pid, p.Name)
	}
	writeJSON(w, http.StatusOK, mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: result})
}
