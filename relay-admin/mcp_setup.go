package main

// /my/ai-tools: how to connect popular AI agents to the MCP endpoint (mcp.go).
// Public, no login: the snippets carry placeholders that the page's script
// fills with this site's endpoint and, if the visitor pastes it, their API key
// (kept in the browser only).

import (
	"html/template"
	"net/http"
)

type mcpSetupClient struct {
	ID, Name, HintKey, Hint, Lang, Snippet string
}

const mcpSetupURL = "__URL__"
const mcpSetupKey = "__KEY__"

var mcpSetupClients = []mcpSetupClient{
	{"claude-code", "Claude Code", "ai.claude_code",
		"Run this in a terminal. Add --scope user to use it in every project.", "sh",
		`claude mcp add --transport http revivetendo ` + mcpSetupURL + ` --header "Authorization: Bearer ` + mcpSetupKey + `"`},
	{"claude-desktop", "Claude Desktop", "ai.claude_desktop",
		"Open Settings → Developer → Edit Config, add this to claude_desktop_config.json and restart Claude. Needs Node.js (it runs the mcp-remote bridge).", "json",
		`{
  "mcpServers": {
    "revivetendo": {
      "command": "npx",
      "args": ["-y", "mcp-remote", "` + mcpSetupURL + `", "--header", "Authorization:${REVIVETENDO_AUTH}"],
      "env": { "REVIVETENDO_AUTH": "Bearer ` + mcpSetupKey + `" }
    }
  }
}`},
	{"cursor", "Cursor", "ai.cursor",
		"Add this to ~/.cursor/mcp.json (or .cursor/mcp.json in a project), then check that it is enabled in Cursor's MCP settings.", "json",
		`{
  "mcpServers": {
    "revivetendo": {
      "url": "` + mcpSetupURL + `",
      "headers": { "Authorization": "Bearer ` + mcpSetupKey + `" }
    }
  }
}`},
	{"vscode", "VS Code (Copilot)", "ai.vscode",
		"Add this to .vscode/mcp.json in a workspace, or run \"MCP: Open User Configuration\" to use it everywhere. VS Code asks for your key the first time and stores it securely. Use the tools in Copilot Chat's agent mode.", "json",
		`{
  "inputs": [
    { "type": "promptString", "id": "revivetendo-key", "description": "Revivetendo API key", "password": true }
  ],
  "servers": {
    "revivetendo": {
      "type": "http",
      "url": "` + mcpSetupURL + `",
      "headers": { "Authorization": "Bearer ${input:revivetendo-key}" }
    }
  }
}`},
	{"windsurf", "Windsurf", "ai.windsurf",
		"Add this to ~/.codeium/windsurf/mcp_config.json, then press Refresh in Cascade's MCP panel.", "json",
		`{
  "mcpServers": {
    "revivetendo": {
      "serverUrl": "` + mcpSetupURL + `",
      "headers": { "Authorization": "Bearer ` + mcpSetupKey + `" }
    }
  }
}`},
	{"gemini", "Gemini CLI", "ai.gemini",
		"Add this to ~/.gemini/settings.json (or .gemini/settings.json in a project).", "json",
		`{
  "mcpServers": {
    "revivetendo": {
      "httpUrl": "` + mcpSetupURL + `",
      "headers": { "Authorization": "Bearer ` + mcpSetupKey + `" }
    }
  }
}`},
	{"codex", "Codex CLI", "ai.codex",
		"Add this to ~/.codex/config.toml and set the environment variable before starting Codex.", "toml",
		`[mcp_servers.revivetendo]
url = "` + mcpSetupURL + `"
bearer_token_env_var = "REVIVETENDO_API_KEY"

# in your shell:
# export REVIVETENDO_API_KEY="` + mcpSetupKey + `"`},
	{"other", "Other / curl", "ai.other",
		"Any client with Streamable HTTP support works: POST JSON-RPC to the endpoint with the Authorization header. Test it with curl:", "sh",
		`curl -s ` + mcpSetupURL + ` \
  -H "Authorization: Bearer ` + mcpSetupKey + `" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'`},
}

var mcpSetupTmpl = template.Must(template.New("ai-tools").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
` + i18nScript + `
<meta charset="utf-8">
<title>AI Tools — Revivetendo</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
body{font-family:system-ui,sans-serif;max-width:760px;margin:2rem auto;padding:0 1rem;color:#222;background:#fff}
h1{font-size:1.4rem;margin-bottom:.4rem}
h2{font-size:1.05rem;margin:2rem 0 .5rem}
p{line-height:1.5}
.nav{display:flex;align-items:center;gap:.5rem;margin-bottom:1.5rem;font-size:.875rem;flex-wrap:wrap}
.nav a{color:#2563eb}
.sub{color:#555;margin-top:0}
.box{border:1px solid #e5e7eb;border-radius:8px;padding:1rem;margin:1rem 0;background:#f9fafb}
label{display:block;font-size:.8rem;font-weight:600;color:#555;margin-bottom:.3rem}
input{width:100%;box-sizing:border-box;border:1px solid #d1d5db;border-radius:6px;padding:.5rem .75rem;font:inherit;font-family:ui-monospace,monospace;font-size:.85rem}
input:focus{outline:none;border-color:#6366f1;box-shadow:0 0 0 2px rgba(99,102,241,.15)}
.hint{font-size:.8rem;color:#666;margin:.35rem 0 0}
.tabs{display:flex;flex-wrap:wrap;gap:.35rem;margin:1.2rem 0 0}
.tabs button{border:1px solid #d1d5db;background:#fff;border-radius:999px;padding:.35rem .8rem;font:inherit;font-size:.85rem;cursor:pointer;color:#333}
.tabs button[aria-selected=true]{background:#6366f1;border-color:#6366f1;color:#fff}
.client{margin-top:1rem}
.client h3{font-size:1rem;margin:0 0 .3rem}
.js .client{display:none}.js .client.on{display:block}
pre{position:relative;background:#0f172a;color:#e2e8f0;border-radius:8px;padding:1rem;overflow-x:auto;font-size:.8rem;line-height:1.45;margin:.6rem 0 0}
pre button{position:absolute;top:.5rem;right:.5rem;background:#334155;color:#e2e8f0;border:none;border-radius:5px;padding:.25rem .6rem;font:inherit;font-size:.75rem;cursor:pointer}
pre button:hover{background:#475569}
ul{padding-left:1.2rem;line-height:1.6}
.warn{background:#fffbeb;border:1px solid #fcd34d;color:#78350f;border-radius:8px;padding:.7rem 1rem;font-size:.875rem;margin-top:1.5rem}
</style>
</head>
<body data-i18n-page-title="ai.title">
<div class="nav" data-lang-slot>
  <a href="/inkay/my/" data-i18n="nav.back_my">← Back to My Status</a>
  <span style="color:#d1d5db">|</span>
  <a href="/inkay/my/account#api-key" data-i18n="ai.manage_key">Manage API key</a>
</div>
<h1 data-i18n="ai.h1">Connect AI Tools</h1>
<p class="sub" data-i18n="ai.intro">Revivetendo speaks MCP (Model Context Protocol), so AI assistants can use your account: check your status and friends, see who is playing Wii Sports Club, create Badge Arcade badges, stages and machines, and upload Revivetendo TV videos.</p>

<div class="box">
  <label for="endpoint" data-i18n="acct.api_endpoint">MCP Endpoint</label>
  <input id="endpoint" type="text" readonly value="https://YOUR-DASHBOARD/inkay/my/mcp" onfocus="this.select()">
  <label for="key" style="margin-top:1rem" data-i18n="acct.api_label">API Key</label>
  <input id="key" type="password" autocomplete="off" spellcheck="false" placeholder="rvt_…">
  <p class="hint"><span data-i18n="ai.key_hint">Paste your key to fill it into the instructions below. It stays in this browser tab and is never sent anywhere.</span> <a href="/inkay/my/account#api-key" data-i18n="ai.need_key">No key yet? Create one on My Account.</a></p>
</div>

<div class="tabs" role="tablist" id="tabs">
{{range .}}<button type="button" role="tab" data-client="{{.ID}}" aria-selected="false">{{.Name}}</button>{{end}}
</div>
{{range .}}
<section class="client" id="client-{{.ID}}">
  <h3>{{.Name}}</h3>
  <p class="hint" data-i18n="{{.HintKey}}">{{.Hint}}</p>
  <pre><button type="button" class="copy" data-i18n="ai.copy">Copy</button><code data-template="{{.Snippet}}">{{.Snippet}}</code></pre>
</section>
{{end}}

<h2 data-i18n="ai.try_h2">Things to try</h2>
<ul>
  <li data-i18n="ai.try1">"Which of my friends are online, and what are they playing?"</li>
  <li data-i18n="ai.try2">"Is anyone playing Wii Sports Club bowling right now?"</li>
  <li data-i18n="ai.try3">"Make a Badge Arcade badge from this picture, show me the preview and submit it."</li>
  <li data-i18n="ai.try4">"Upload this video to Revivetendo TV and make it free to download."</li>
</ul>

<div class="warn" data-i18n="ai.security">Your key can create and delete things on your account (Badge Arcade creations, videos, channels). Keep it private, and if it leaks, reset it on My Account. Linking Discord or Patreon and changing your password still need the website.</div>

<script>
(function () {
  document.documentElement.classList.add('js');
  var url = location.origin + '/inkay/my/mcp';
  var endpoint = document.getElementById('endpoint');
  var keyInput = document.getElementById('key');
  endpoint.value = url;
  function fill() {
    var key = keyInput.value.trim() || 'YOUR_API_KEY';
    document.querySelectorAll('code[data-template]').forEach(function (c) {
      c.textContent = c.getAttribute('data-template').split('__URL__').join(url).split('__KEY__').join(key);
    });
  }
  keyInput.addEventListener('input', fill);
  fill();

  var tabs = document.querySelectorAll('#tabs button');
  function show(id) {
    tabs.forEach(function (t) { t.setAttribute('aria-selected', t.dataset.client === id ? 'true' : 'false'); });
    document.querySelectorAll('.client').forEach(function (s) { s.classList.toggle('on', s.id === 'client-' + id); });
    try { localStorage.setItem('revivetendo-ai-client', id); } catch (e) {}
  }
  tabs.forEach(function (t) { t.addEventListener('click', function () { show(t.dataset.client); }); });
  var saved = null;
  try { saved = localStorage.getItem('revivetendo-ai-client'); } catch (e) {}
  var want = location.hash.slice(1) || saved;
  show(want && document.getElementById('client-' + want) ? want : tabs[0].dataset.client);

  document.querySelectorAll('button.copy').forEach(function (b) {
    b.addEventListener('click', function () {
      var text = b.nextElementSibling.textContent;
      var done = function () {
        b.textContent = (window.I18N && I18N.t('ai.copied', 'Copied')) || 'Copied';
        setTimeout(function () { b.textContent = (window.I18N && I18N.t('ai.copy', 'Copy')) || 'Copy'; }, 1500);
      };
      if (navigator.clipboard) navigator.clipboard.writeText(text).then(done, function () {});
    });
  });
})();
</script>
</body>
</html>`))

func myAIToolsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	mcpSetupTmpl.Execute(w, mcpSetupClients)
}
