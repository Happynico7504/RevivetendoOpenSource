package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMCPInitializeNegotiatesVersion(t *testing.T) {
	for client, want := range map[string]string{"2025-03-26": "2025-03-26", "1999-01-01": mcpLatestVersion} {
		res, rpcErr := mcpHandle(mcpCall{pid: 1}, mcpRequest{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "initialize",
			Params: json.RawMessage(`{"protocolVersion":"` + client + `"}`)})
		if rpcErr != nil {
			t.Fatal(rpcErr.Message)
		}
		if got := res.(map[string]interface{})["protocolVersion"]; got != want {
			t.Errorf("client %s: got %v, want %s", client, got, want)
		}
	}
}

func TestMCPToolsListAndUnknown(t *testing.T) {
	res, _ := mcpHandle(mcpCall{pid: 1}, mcpRequest{JSONRPC: "2.0", ID: json.RawMessage("2"), Method: "tools/list"})
	b, _ := json.Marshal(res)
	mustContain(t, string(b), `"get_my_status"`, `"list_friends"`, `"inputSchema"`)
	if _, rpcErr := mcpHandle(mcpCall{pid: 1}, mcpRequest{JSONRPC: "2.0", ID: json.RawMessage("3"), Method: "tools/call",
		Params: json.RawMessage(`{"name":"nope"}`)}); rpcErr == nil || rpcErr.Code != -32602 {
		t.Errorf("unknown tool: got %+v", rpcErr)
	}
	if _, rpcErr := mcpHandle(mcpCall{pid: 1}, mcpRequest{JSONRPC: "2.0", ID: json.RawMessage("4"), Method: "resources/list"}); rpcErr == nil || rpcErr.Code != -32601 {
		t.Errorf("unknown method: got %+v", rpcErr)
	}
}

func TestMCPRejectsGetAndMissingKey(t *testing.T) {
	w := httptest.NewRecorder()
	myMCPHandler(w, httptest.NewRequest("GET", "/my/mcp", nil))
	if w.Code != 405 {
		t.Errorf("GET: got %d", w.Code)
	}
	w = httptest.NewRecorder()
	myMCPHandler(w, httptest.NewRequest("POST", "/my/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)))
	if w.Code != 401 || w.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("no key: got %d", w.Code)
	}
}

func renderAccount(t *testing.T, d myAccountData) string {
	t.Helper()
	var buf bytes.Buffer
	if err := myAccountTmpl.Execute(&buf, d); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestMyAccountAPIKeySection(t *testing.T) {
	none := renderAccount(t, myAccountData{})
	mustContain(t, none, "Change Password", "/inkay/my/mcp", "Generate API Key", "No API key yet")
	mustNotContain(t, none, "Reset API Key")

	have := renderAccount(t, myAccountData{APIKeyPrefix: "rvt_0123abcd"})
	mustContain(t, have, "rvt_0123abcd…", "Reset API Key", "acct.api_confirm")
	mustNotContain(t, have, "will not be shown again")

	fresh := renderAccount(t, myAccountData{APIKeyPrefix: "rvt_0123abcd", NewAPIKey: "rvt_0123abcdFULL", Success: "New API key created.", Key: "acct.api_ok"})
	mustContain(t, fresh, `value="rvt_0123abcdFULL"`, "will not be shown again", "New API key created.")
}

func TestAIToolsPage(t *testing.T) {
	w := httptest.NewRecorder()
	myAIToolsHandler(w, httptest.NewRequest("GET", "/my/ai-tools", nil))
	out := w.Body.String()
	for _, c := range mcpSetupClients {
		mustContain(t, out, `data-client="`+c.ID+`"`, `id="client-`+c.ID+`"`, `data-i18n="`+c.HintKey+`"`)
	}
	mustContain(t, out, "claude mcp add --transport http revivetendo __URL__", "bearer_token_env_var", "${input:revivetendo-key}", "mcp-remote", "httpUrl", "serverUrl")
	mustNotContain(t, out, "ZgotmplZ")
}

func TestMCPStatusSharingTools(t *testing.T) {
	res, _ := mcpHandle(mcpCall{pid: 1}, mcpRequest{JSONRPC: "2.0", ID: json.RawMessage("5"), Method: "tools/list"})
	b, _ := json.Marshal(res)
	mustContain(t, string(b), `"get_whos_online"`, `"set_status_sharing"`, `"overlay_online"`, `"public_list"`)

	got := mcpSetStatusSharing(mcpCall{pid: 1, args: json.RawMessage(`{}`)})
	if !got.IsError {
		t.Errorf("no fields: want an error, got %+v", got)
	}
	got = mcpSetStatusSharing(mcpCall{pid: 1, args: json.RawMessage(`{"public_list":"yes"}`)})
	if !got.IsError {
		t.Errorf("wrong type: want an error, got %+v", got)
	}
}
