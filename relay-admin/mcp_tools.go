package main

// MCP tools: everything a player can do on the public dashboard pages, as the
// API key's account. Tools that mirror a JSON API (Badge Arcade editor,
// Revivetendo TV, WSC) call that API's handler in-process via mcpDispatch with
// the key's PID in the request context (mySessionPID honours it), so they go
// through exactly the same validation, ownership checks, bans and rate limits
// as the web pages. Things that need a browser (Discord/Patreon OAuth, the
// password change) stay on the pages, and so does the Discord link code: a
// linked Discord can reset the web password (/reset_web_password), so a key
// must not be able to link one.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const mcpInstructions = "Revivetendo is a revival of Nintendo's Wii U / 3DS online services. These tools act as the player who owns the API key: " +
	"their account, friends and RevivetendoCoins; what they share publicly and who else is online; Wii Sports Club players and sessions; creating Badge Arcade badges, stages and machine layouts " +
	"(staff review submissions before they reach the game); and uploading Revivetendo TV videos and channels (also staff reviewed). " +
	"Images are passed as base64 or data: URLs."

type mcpCall struct {
	pid  int64
	r    *http.Request // the MCP request (its client address feeds the IP rate limits)
	args json.RawMessage
}

type mcpToolResult struct {
	Content           []map[string]any `json:"content"`
	StructuredContent map[string]any   `json:"structuredContent,omitempty"`
	IsError           bool             `json:"isError,omitempty"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`

	call func(c mcpCall) mcpToolResult
}

// --- results ---

func mcpOK(v map[string]any) mcpToolResult {
	text, _ := json.MarshalIndent(v, "", "  ")
	return mcpToolResult{Content: []map[string]any{{"type": "text", "text": string(text)}}, StructuredContent: v}
}

// mcpFail is a tool error the model sees and can react to (not a protocol error).
func mcpFail(format string, a ...any) mcpToolResult {
	return mcpToolResult{Content: []map[string]any{{"type": "text", "text": fmt.Sprintf(format, a...)}}, IsError: true}
}

// mcpArgs decodes the tool arguments into v; ok is false with the error result.
func mcpArgs(c mcpCall, v any) (mcpToolResult, bool) {
	if err := json.Unmarshal(c.args, v); err != nil {
		return mcpFail("invalid arguments: %v", err), false
	}
	return mcpToolResult{}, true
}

// mcpPIDContextKey carries the API key's PID into handlers called by mcpDispatch.
type mcpPIDContextKey struct{}

// mcpDispatch runs one of our own routes in-process as the key's account.
func mcpDispatch(c mcpCall, method, path, contentType string, body []byte) (int, []byte) {
	ctx := context.WithValue(c.r.Context(), mcpPIDContextKey{}, c.pid)
	req := httptest.NewRequestWithContext(ctx, method, path, bytes.NewReader(body))
	req.RemoteAddr = c.r.RemoteAddr
	if f := c.r.Header.Get("X-Forwarded-For"); f != "" {
		req.Header.Set("X-Forwarded-For", f)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("X-Requested-With", "mcp") // the video API's anti-CSRF header
	rec := httptest.NewRecorder()
	http.DefaultServeMux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// mcpAPI calls a JSON API route and turns its answer into a tool result.
// Top-level data:image URLs in the answer become image content blocks.
func mcpAPI(c mcpCall, method, path string, body any) mcpToolResult {
	var raw []byte
	ct := ""
	if body != nil {
		raw, _ = json.Marshal(body)
		ct = "application/json"
	}
	status, out := mcpDispatch(c, method, path, ct, raw)
	return mcpAPIResult(status, out)
}

func mcpAPIResult(status int, out []byte) mcpToolResult {
	var v any
	if err := json.Unmarshal(out, &v); err != nil {
		if status >= 400 {
			return mcpFail("HTTP %d: %s", status, strings.TrimSpace(string(out)))
		}
		return mcpFail("unexpected answer (HTTP %d)", status)
	}
	m, isObj := v.(map[string]any)
	if !isObj {
		m = map[string]any{"items": v}
	}
	if status >= 400 {
		if e, ok := m["error"].(string); ok {
			return mcpFail("%s", e)
		}
		return mcpFail("HTTP %d", status)
	}
	var images []map[string]any
	for k, val := range m {
		s, ok := val.(string)
		if !ok || !strings.HasPrefix(s, "data:image/") {
			continue
		}
		mime, b64, ok := strings.Cut(strings.TrimPrefix(s, "data:"), ";base64,")
		if !ok {
			continue
		}
		images = append(images, map[string]any{"type": "text", "text": k + ":"},
			map[string]any{"type": "image", "data": b64, "mimeType": mime})
		m[k] = "(image content block)"
	}
	res := mcpOK(m)
	res.Content = append(res.Content, images...)
	return res
}

// mcpDecodeBase64 accepts plain base64 or a data: URL.
func mcpDecodeBase64(s string) ([]byte, error) {
	if i := strings.Index(s, ","); strings.HasPrefix(s, "data:") && i >= 0 {
		s = s[i+1:]
	}
	return base64.StdEncoding.DecodeString(strings.TrimSpace(s))
}

// --- schema helpers ---

func mcpSchema(required []string, props map[string]any) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func mcpProp(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}

func mcpEnum(desc string, values ...string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": values}
}

var (
	mcpNoArgs     = mcpSchema(nil, map[string]any{})
	mcpReadOnly   = map[string]any{"readOnlyHint": true, "openWorldHint": false}
	mcpWrites     = map[string]any{"readOnlyHint": false, "destructiveHint": false, "openWorldHint": false}
	mcpDestroys   = map[string]any{"readOnlyHint": false, "destructiveHint": true, "openWorldHint": false}
	mcpIDArg      = mcpSchema([]string{"id"}, map[string]any{"id": mcpProp("integer", "ID from the matching list tool")})
	mcpArtDesc    = "PNG/JPEG/GIF as base64 or a data: URL"
	mcpLayoutDesc = "Machine layout: {template, objects:[{kind: badge|obstacle|attachment, part, x, y, sw, sh, rot, from, friction?, bounce?, strength?, falls?}]}. " +
		"Start from badge_arcade_get_template's layout, keep `from` for objects taken from it and use -1 for added ones; part names and value limits come from badge_arcade_list_parts."
)

// --- tools ---

var mcpTools = []mcpTool{
	// Account
	{
		Name: "get_my_status", Title: "My Revivetendo status",
		Description: "Your Revivetendo account: PNID, Mii name, whether your Wii U is online and what it is playing, linked Discord, and your RevivetendoCoin balance.",
		InputSchema: mcpNoArgs, Annotations: mcpReadOnly, call: mcpGetMyStatus,
	},
	{
		Name: "list_friends", Title: "My friends",
		Description: "Your Wii U friends with their online status, the game they are playing and when they were last online. Online friends come first.",
		InputSchema: mcpNoArgs, Annotations: mcpReadOnly, call: mcpListFriends,
	},
	{
		Name: "get_coin_wallet", Title: "RevivetendoCoin wallet",
		Description: "Your RevivetendoCoin balance, the last 10 wallet changes and whether Patreon is linked. Coins buy Badge Arcade plays and paid Revivetendo TV downloads; a wallet appears the first time you open a coin shop on a 3DS.",
		InputSchema: mcpNoArgs, Annotations: mcpReadOnly, call: mcpGetCoinWallet,
	},
	{
		Name: "set_status_sharing", Title: "Set online status sharing",
		Description: "Choose what your account shares publicly: overlay_online lets the WSC stream overlay show when your Wii U goes online or offline (anyone who knows your PNID can then see it), " +
			"public_list lists your Mii, PNID, online status and current game on the public Who's Online page. Both are off until you turn them on; leave a field out to keep its setting. " +
			"get_my_status shows the current settings.",
		InputSchema: mcpSchema(nil, map[string]any{
			"overlay_online": mcpProp("boolean", "Show your Wii U's online/offline changes on the stream overlay"),
			"public_list":    mcpProp("boolean", "List you on the public Who's Online page"),
		}),
		Annotations: mcpWrites, call: mcpSetStatusSharing,
	},
	{
		Name: "turn_off_discord_rich_presence", Title: "Turn off Discord Rich Presence",
		Description: "Stop showing your Revivetendo game on your Discord profile. Turning it on needs a Discord sign-in, so that is only possible on the Discord Link page.",
		InputSchema: mcpNoArgs, Annotations: mcpDestroys, call: mcpRichPresenceOff,
	},

	// Network
	{
		Name: "get_wsc_status", Title: "Wii Sports Club players and sessions",
		Description: "Who is online in Wii Sports Club right now, the open/running sessions (gatherings) per sport with their players, and recent connection failures.",
		InputSchema: mcpNoArgs, Annotations: mcpReadOnly,
		call: func(c mcpCall) mcpToolResult { return mcpAPI(c, http.MethodGet, "/wsc-public/api/players", nil) },
	},
	{
		Name: "get_whos_online", Title: "Who's Online",
		Description: "The players who chose to appear on the public Who's Online page, online ones first: Mii name, PNID, whether they are online, the game they are playing and when they were last online.",
		InputSchema: mcpNoArgs, Annotations: mcpReadOnly,
		call: func(c mcpCall) mcpToolResult {
			status, out := mcpDispatch(c, http.MethodGet, "/online/api", "", nil)
			if status >= 400 {
				return mcpAPIResult(status, out)
			}
			var users []sharedPresence
			if err := json.Unmarshal(out, &users); err != nil {
				return mcpFail("unexpected answer: %v", err)
			}
			return mcpOK(map[string]any{"users": users})
		},
	},
	{
		Name: "get_network_stats", Title: "Network statistics",
		Description: "Revivetendo-wide numbers: registered accounts, relay requests (total and last 24 h) and active game redirects.",
		InputSchema: mcpNoArgs, Annotations: mcpReadOnly,
		call: func(c mcpCall) mcpToolResult { return mcpAPI(c, http.MethodGet, "/api/stats", nil) },
	},

	// Badge Arcade
	{
		Name: "badge_arcade_list_creations", Title: "My Badge Arcade creations",
		Description: "Your Badge Arcade badges, stages and machine layouts with their review status (draft, submitted, approved, rejected) and staff notes.",
		InputSchema: mcpNoArgs, Annotations: mcpReadOnly,
		call: func(c mcpCall) mcpToolResult {
			return mcpAPI(c, http.MethodGet, badgeEditorBase+"api/creations", nil)
		},
	},
	{
		Name: "badge_arcade_get_creation", Title: "Get a Badge Arcade creation",
		Description: "One of your creations with its full spec and its art as an image.",
		InputSchema: mcpIDArg, Annotations: mcpReadOnly, call: mcpBadgeGet,
	},
	{
		Name: "badge_arcade_save_creation", Title: "Save a Badge Arcade creation",
		Description: "Create (no id) or update (id) a badge, stage or machine layout. Saving an edited rejected creation turns it back into a draft; submitted and approved ones can't be changed. " +
			"badge: art is scaled to 128x128, spec {names: up to 16 language slots (0 ja, 1 en - required, 2 fr, 3 de, 4 it, 5 es, 6 zh, 7 ko, 8 nl, 9 pt, 10 ru, 11 zh-TW; empty or missing = English; two lines max), collision: optional up to 8 polygons of 3-8 [x,y] points in 0-128, null = automatic}. " +
			"stage: a machine backdrop, art covers 400x240, spec {tint: \"#rrggbb\" rail strip colour}, title required. " +
			"crane: a machine layout, spec {layout, stage?: id of one of your stages, badge?: id of one of your badges}, title required, no art (the server renders it). " +
			"Use badge_arcade_preview first to see the result. Max 100 creations.",
		InputSchema: mcpSchema([]string{"kind"}, map[string]any{
			"id":     mcpProp("integer", "Creation to update; omit to create a new one"),
			"kind":   mcpEnum("What to save (ignored on update)", "badge", "stage", "crane"),
			"title":  mcpProp("string", "Up to 60 characters; badges default to their English name"),
			"spec":   mcpProp("object", "Kind-specific spec, see the tool description. For crane: {layout: "+mcpLayoutDesc+", stage, badge}"),
			"art":    mcpProp("string", "Badge or stage art: "+mcpArtDesc),
			"source": mcpProp("string", "Optional original image kept for re-editing on the website: "+mcpArtDesc),
		}),
		Annotations: mcpWrites, call: mcpBadgeSave,
	},
	{
		Name: "badge_arcade_delete_creation", Title: "Delete a Badge Arcade creation",
		Description: "Delete one of your creations (approved ones can't be deleted).",
		InputSchema: mcpIDArg, Annotations: mcpDestroys,
		call: func(c mcpCall) mcpToolResult { return mcpBadgeAction(c, http.MethodDelete, "") },
	},
	{
		Name: "badge_arcade_submit_creation", Title: "Submit a creation for review",
		Description: "Send a draft or rejected creation to staff review (at most 5 waiting at once), or take a submission back with withdraw: true. Approved creations go into the game's daily SpotPass content.",
		InputSchema: mcpSchema([]string{"id"}, map[string]any{
			"id":       mcpProp("integer", "Creation id"),
			"withdraw": mcpProp("boolean", "Take a pending submission back instead"),
		}),
		Annotations: mcpWrites, call: mcpBadgeSubmit,
	},
	{
		Name: "badge_arcade_preview", Title: "Preview Badge Arcade art",
		Description: "Render art exactly as the 3DS will show it, without saving. badge: HOME Menu icons, in-machine texture after ETC1 compression, shadow and collision shapes (plus the automatic ones). stage: top screen and rail strip. layout: the machine with its badges and obstacles.",
		InputSchema: mcpSchema([]string{"kind"}, map[string]any{
			"kind":      mcpEnum("What to preview", "badge", "stage", "layout"),
			"art":       mcpProp("string", "badge/stage art: "+mcpArtDesc),
			"collision": map[string]any{"type": "array", "description": "badge: optional collision polygons, see badge_arcade_save_creation"},
			"tint":      mcpProp("string", "stage: rail strip colour \"#rrggbb\""),
			"layout":    mcpProp("object", "layout: "+mcpLayoutDesc),
			"stage":     mcpProp("integer", "layout: one of your stage creation ids to draw behind it"),
			"badge":     mcpProp("integer", "layout: one of your badge creation ids to draw the badge spots with"),
		}),
		Annotations: mcpReadOnly, call: mcpBadgePreview,
	},
	{
		Name: "badge_arcade_list_templates", Title: "Badge Arcade machine templates",
		Description: "The real machines a custom layout can be based on: name, stage, machine type, badge spots, obstacles and whether it is a difficult machine.",
		InputSchema: mcpNoArgs, Annotations: mcpReadOnly,
		call: func(c mcpCall) mcpToolResult {
			return mcpAPI(c, http.MethodGet, badgeEditorBase+"api/layout/templates", nil)
		},
	},
	{
		Name: "badge_arcade_get_template", Title: "Get a machine template",
		Description: "A template machine's layout (the starting point for a crane creation), stage, icon and joints.",
		InputSchema: mcpSchema([]string{"name"}, map[string]any{"name": mcpProp("string", "Template name from badge_arcade_list_templates")}),
		Annotations: mcpReadOnly,
		call: func(c mcpCall) mcpToolResult {
			var a struct {
				Name string `json:"name"`
			}
			if res, ok := mcpArgs(c, &a); !ok {
				return res
			}
			return mcpAPI(c, http.MethodGet, badgeEditorBase+"api/layout/template?name="+url.QueryEscape(a.Name), nil)
		},
	},
	{
		Name: "badge_arcade_list_parts", Title: "Machine obstacles and attachments",
		Description: "The obstacle and attachment parts a layout can use, and the limits for object counts, positions, scale and badge physics.",
		InputSchema: mcpNoArgs, Annotations: mcpReadOnly,
		call: func(c mcpCall) mcpToolResult {
			return mcpAPI(c, http.MethodGet, badgeEditorBase+"api/layout/obstacles", nil)
		},
	},

	// Revivetendo TV
	{
		Name: "tv_list_videos", Title: "My Revivetendo TV videos",
		Description: "Your Revivetendo TV uploads (status: uploading, processing, failed, submitted, live, rejected; views, likes, comments, download option), your channels and whether you have a coin wallet.",
		InputSchema: mcpNoArgs, Annotations: mcpReadOnly,
		call: func(c mcpCall) mcpToolResult { return mcpAPI(c, http.MethodGet, "/my/videos/api/list", nil) },
	},
	{
		Name: "tv_start_video_upload", Title: "Start a video upload",
		Description: "Start uploading a video (max 300 MB, 10 minutes; converted for the 3DS and Wii U eShop, then reviewed by staff). Returns the upload id and chunk size. " +
			"Then send the file in order, chunk by chunk, with tv_upload_video_chunk - or, if you can make HTTP requests, PUT each raw chunk to <endpoint base>/inkay/my/videos/api/chunk?id=<id>&offset=<bytes sent so far> " +
			"with the same Authorization: Bearer header (curl --data-binary). A 409 answer carries the offset to resume from. At most 3 videos waiting at once, 5 uploads per hour.",
		InputSchema: mcpSchema([]string{"title", "size", "rights"}, map[string]any{
			"title":       mcpProp("string", "1-60 characters"),
			"description": mcpProp("string", "Up to 500 characters"),
			"size":        mcpProp("integer", "Exact file size in bytes"),
			"rights":      mcpProp("boolean", "Confirms you have the right to share this video (required: true)"),
			"stereo":      mcpEnum("3D layout of the source; empty for 2D", "", "sbs", "sbs-r", "tb", "tb-r"),
			"channel":     mcpProp("integer", "catalogId of one of your live channels; omit to let staff choose"),
		}),
		Annotations: mcpWrites, call: mcpTVStart,
	},
	{
		Name: "tv_upload_video_chunk", Title: "Upload a video chunk",
		Description: "Send the next piece of an upload started with tv_start_video_upload, as base64 (at most chunkSize bytes, offset = bytes accepted so far). The last chunk starts the conversion.",
		InputSchema: mcpSchema([]string{"id", "offset", "data"}, map[string]any{
			"id":     mcpProp("integer", "Upload id"),
			"offset": mcpProp("integer", "Bytes already received"),
			"data":   mcpProp("string", "The chunk, base64"),
		}),
		Annotations: mcpWrites, call: mcpTVChunk,
	},
	{
		Name: "tv_delete_video", Title: "Delete a video",
		Description: "Delete one of your videos (also takes a live one off the eShop). Not possible while it is being converted.",
		InputSchema: mcpIDArg, Annotations: mcpDestroys,
		call: func(c mcpCall) mcpToolResult { return mcpTVPostID(c, "/my/videos/api/delete") },
	},
	{
		Name: "tv_set_video_download", Title: "Set a video's download option",
		Description: "Whether 3DS players can download your video through the eShop: off, free, or paid (1-500 RevivetendoCoins credited to your wallet; needs a wallet).",
		InputSchema: mcpSchema([]string{"id", "mode"}, map[string]any{
			"id":    mcpProp("integer", "Video id"),
			"mode":  mcpEnum("Download option", "off", "free", "paid"),
			"price": mcpProp("integer", "Coins, for paid"),
		}),
		Annotations: mcpWrites,
		call: func(c mcpCall) mcpToolResult {
			var a struct {
				ID    int64  `json:"id"`
				Mode  string `json:"mode"`
				Price int    `json:"price"`
			}
			if res, ok := mcpArgs(c, &a); !ok {
				return res
			}
			return mcpAPI(c, http.MethodPost, "/my/videos/api/download", a)
		},
	},
	{
		Name: "tv_submit_channel", Title: "Submit a Revivetendo TV channel",
		Description: "Propose your own channel (staff review it; at most 3 per account). Once live you can pick it for your uploads. Images are cropped to the console sizes: icon 102x76, banner 400x168; max 4 MB each.",
		InputSchema: mcpSchema([]string{"name", "icon", "banner", "rights"}, map[string]any{
			"name":        mcpProp("string", "1-60 characters"),
			"description": mcpProp("string", "Up to 500 characters"),
			"icon":        mcpProp("string", "JPG/PNG icon: base64 or a data: URL"),
			"banner":      mcpProp("string", "JPG/PNG banner: base64 or a data: URL"),
			"rights":      mcpProp("boolean", "Confirms you have the right to use these images (required: true)"),
		}),
		Annotations: mcpWrites, call: mcpTVSubmitChannel,
	},
	{
		Name: "tv_delete_channel", Title: "Delete a channel",
		Description: "Delete one of your channel submissions (a live channel is also removed from the eShop).",
		InputSchema: mcpIDArg, Annotations: mcpDestroys,
		call: func(c mcpCall) mcpToolResult { return mcpTVPostID(c, "/my/videos/api/channel-delete") },
	},
}

var mcpToolsByName = map[string]mcpTool{}

func init() {
	for _, t := range mcpTools {
		mcpToolsByName[t.Name] = t
	}
}

// --- account ---

func mcpGame(online bool, titleID int64, serverHex string) string {
	if !online {
		return ""
	}
	if name := resolveGameName(titleID, serverHex); name != "" {
		return name
	}
	if titleID != 0 {
		return fmt.Sprintf("%016X", uint64(titleID))
	}
	return ""
}

func mcpGetMyStatus(c mcpCall) mcpToolResult {
	d := loadMyStatus(c.pid)
	out := map[string]any{
		"pid":            d.PID,
		"pnid":           d.PNID,
		"mii_name":       d.MiiName,
		"discord_linked": d.DiscordLinked,
		"has_wii_u":      d.ShowWiiU,
	}
	if d.ShowWiiU {
		out["online"] = d.IsOnline
		if g := mcpGame(d.IsOnline, d.TitleID, d.GameServerHex); g != "" {
			out["game"] = g
		}
	}
	if d.ShowWallet {
		out["revivetendo_coins"] = d.WalletBalance
	}
	overlay, list := presenceSharing(c.pid)
	out["sharing"] = map[string]any{"overlay_online": overlay, "public_list": list}
	return mcpOK(out)
}

func mcpSetStatusSharing(c mcpCall) mcpToolResult {
	var a struct {
		OverlayOnline *bool `json:"overlay_online"`
		PublicList    *bool `json:"public_list"`
	}
	if res, ok := mcpArgs(c, &a); !ok {
		return res
	}
	if a.OverlayOnline == nil && a.PublicList == nil {
		return mcpFail("set overlay_online, public_list or both")
	}
	overlay, list := presenceSharing(c.pid)
	if a.OverlayOnline != nil {
		overlay = *a.OverlayOnline
	}
	if a.PublicList != nil {
		list = *a.PublicList
	}
	if err := setPresenceSharing(c.pid, overlay, list); err != nil {
		return mcpFail("could not save the sharing settings, try again later")
	}
	return mcpOK(map[string]any{"overlay_online": overlay, "public_list": list})
}

type mcpFriend struct {
	PID        int64   `json:"pid"`
	PNID       string  `json:"pnid,omitempty"`
	MiiName    string  `json:"mii_name,omitempty"`
	Online     bool    `json:"online"`
	Game       string  `json:"game,omitempty"`
	LastOnline *string `json:"last_online,omitempty"`
}

func mcpListFriends(c mcpCall) mcpToolResult {
	myKickFriendSync(c.pid)
	d := loadMyStatus(c.pid)
	friends := make([]mcpFriend, 0, len(d.Friends))
	for _, f := range d.Friends {
		mf := mcpFriend{PID: f.PID, PNID: f.PNID, MiiName: f.MiiName, Online: f.IsOnline,
			Game: mcpGame(f.IsOnline, f.TitleID, f.GameServerHex)}
		if f.LastOnline.Valid {
			t := f.LastOnline.Time.UTC().Format(time.RFC3339)
			mf.LastOnline = &t
		}
		friends = append(friends, mf)
	}
	return mcpOK(map[string]any{"friends": friends})
}

func mcpGetCoinWallet(c mcpCall) mcpToolResult {
	out := map[string]any{"has_wallet": false, "patreon_linked": false}
	var balance int
	if db.QueryRow(`SELECT balance FROM coin_wallets WHERE pid = $1`, c.pid).Scan(&balance) == nil {
		out["has_wallet"] = true
		out["balance"] = balance
	}
	var patreonName string
	if db.QueryRow(`SELECT full_name FROM patreon_links WHERE pid = $1`, c.pid).Scan(&patreonName) == nil {
		out["patreon_linked"] = true
	}
	type change struct {
		Delta        int    `json:"delta"`
		BalanceAfter int    `json:"balance_after"`
		Reason       string `json:"reason"`
		At           string `json:"at"`
	}
	history := []change{}
	if rows, err := db.Query(`SELECT delta, balance_after, reason, created_at FROM coin_ledger WHERE pid = $1 ORDER BY id DESC LIMIT 10`, c.pid); err == nil {
		defer rows.Close()
		for rows.Next() {
			var ch change
			var at time.Time
			if rows.Scan(&ch.Delta, &ch.BalanceAfter, &ch.Reason, &at) == nil {
				ch.At = at.UTC().Format(time.RFC3339)
				history = append(history, ch)
			}
		}
	}
	out["history"] = history
	return mcpOK(out)
}

func mcpRichPresenceOff(c mcpCall) mcpToolResult {
	if !discordPresenceEnabled(c.pid) {
		return mcpOK(map[string]any{"rich_presence_on": false, "changed": false})
	}
	if status, out := mcpDispatch(c, http.MethodPost, "/my/discord/presence/unlink", "", nil); status >= 400 {
		return mcpFail("HTTP %d: %s", status, strings.TrimSpace(string(out)))
	}
	return mcpOK(map[string]any{"rich_presence_on": discordPresenceEnabled(c.pid), "changed": true})
}

// --- Badge Arcade ---

func mcpBadgeAction(c mcpCall, method, sub string) mcpToolResult {
	var a struct {
		ID int64 `json:"id"`
	}
	if res, ok := mcpArgs(c, &a); !ok {
		return res
	}
	path := badgeEditorBase + "api/creations/" + strconv.FormatInt(a.ID, 10)
	if sub != "" {
		return mcpAPI(c, method, path+"/"+sub, struct{}{})
	}
	return mcpAPI(c, method, path, nil)
}

func mcpBadgeGet(c mcpCall) mcpToolResult {
	res := mcpBadgeAction(c, http.MethodGet, "")
	// The original upload can be megabytes; the art is what matters here.
	if !res.IsError {
		delete(res.StructuredContent, "source")
		var kept []map[string]any
		for i := 0; i < len(res.Content); i++ {
			if res.Content[i]["text"] == "source:" && i+1 < len(res.Content) {
				i++
				continue
			}
			kept = append(kept, res.Content[i])
		}
		text, _ := json.MarshalIndent(res.StructuredContent, "", "  ")
		kept[0] = map[string]any{"type": "text", "text": string(text)}
		res.Content = kept
	}
	return res
}

func mcpBadgeSave(c mcpCall) mcpToolResult {
	var a struct {
		ID int64 `json:"id"`
		badgeEditorSaveRequest
	}
	if res, ok := mcpArgs(c, &a); !ok {
		return res
	}
	if len(a.Spec) == 0 {
		a.Spec = json.RawMessage("{}")
	}
	if a.ID != 0 {
		return mcpAPI(c, http.MethodPut, badgeEditorBase+"api/creations/"+strconv.FormatInt(a.ID, 10), a.badgeEditorSaveRequest)
	}
	return mcpAPI(c, http.MethodPost, badgeEditorBase+"api/creations", a.badgeEditorSaveRequest)
}

func mcpBadgeSubmit(c mcpCall) mcpToolResult {
	var a struct {
		Withdraw bool `json:"withdraw"`
	}
	if res, ok := mcpArgs(c, &a); !ok {
		return res
	}
	if a.Withdraw {
		return mcpBadgeAction(c, http.MethodPost, "withdraw")
	}
	return mcpBadgeAction(c, http.MethodPost, "submit")
}

func mcpBadgePreview(c mcpCall) mcpToolResult {
	var a struct {
		Kind      string          `json:"kind"`
		Art       string          `json:"art"`
		Collision json.RawMessage `json:"collision"`
		Tint      string          `json:"tint"`
		Layout    json.RawMessage `json:"layout"`
		Stage     int64           `json:"stage"`
		Badge     int64           `json:"badge"`
	}
	if res, ok := mcpArgs(c, &a); !ok {
		return res
	}
	switch a.Kind {
	case "badge":
		return mcpAPI(c, http.MethodPost, badgeEditorBase+"api/preview", map[string]any{"art": a.Art, "collision": a.Collision})
	case "stage":
		return mcpAPI(c, http.MethodPost, badgeEditorBase+"api/preview-stage", map[string]any{"art": a.Art, "tint": a.Tint})
	case "layout":
		return mcpAPI(c, http.MethodPost, badgeEditorBase+"api/preview-layout", map[string]any{"layout": a.Layout, "stage": a.Stage, "badge": a.Badge})
	}
	return mcpFail("kind must be badge, stage or layout")
}

// --- Revivetendo TV ---

func mcpTVStart(c mcpCall) mcpToolResult {
	var a struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Size        int64  `json:"size"`
		Rights      bool   `json:"rights"`
		Stereo      string `json:"stereo"`
		Channel     int64  `json:"channel"`
	}
	if res, ok := mcpArgs(c, &a); !ok {
		return res
	}
	res := mcpAPI(c, http.MethodPost, "/my/videos/api/start", a)
	if !res.IsError {
		res.StructuredContent["chunkPath"] = "/inkay/my/videos/api/chunk"
		text, _ := json.MarshalIndent(res.StructuredContent, "", "  ")
		res.Content[0]["text"] = string(text)
	}
	return res
}

func mcpTVChunk(c mcpCall) mcpToolResult {
	var a struct {
		ID     int64  `json:"id"`
		Offset int64  `json:"offset"`
		Data   string `json:"data"`
	}
	if res, ok := mcpArgs(c, &a); !ok {
		return res
	}
	data, err := mcpDecodeBase64(a.Data)
	if err != nil {
		return mcpFail("data is not valid base64: %v", err)
	}
	status, out := mcpDispatch(c, http.MethodPut,
		fmt.Sprintf("/my/videos/api/chunk?id=%d&offset=%d", a.ID, a.Offset), "application/octet-stream", data)
	if status == http.StatusConflict {
		var v struct {
			Received int64 `json:"received"`
		}
		json.Unmarshal(out, &v)
		return mcpFail("wrong offset: the server has %d bytes, continue from there", v.Received)
	}
	return mcpAPIResult(status, out)
}

func mcpTVPostID(c mcpCall, path string) mcpToolResult {
	var a struct {
		ID int64 `json:"id"`
	}
	if res, ok := mcpArgs(c, &a); !ok {
		return res
	}
	return mcpAPI(c, http.MethodPost, path, a)
}

func mcpTVSubmitChannel(c mcpCall) mcpToolResult {
	var a struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Icon        string `json:"icon"`
		Banner      string `json:"banner"`
		Rights      bool   `json:"rights"`
	}
	if res, ok := mcpArgs(c, &a); !ok {
		return res
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("name", a.Name)
	mw.WriteField("description", a.Description)
	if a.Rights {
		mw.WriteField("rights", "1")
	}
	for kind, b64 := range map[string]string{"icon": a.Icon, "banner": a.Banner} {
		if b64 == "" {
			continue
		}
		img, err := mcpDecodeBase64(b64)
		if err != nil {
			return mcpFail("%s is not valid base64: %v", kind, err)
		}
		fw, _ := mw.CreateFormFile(kind, kind)
		fw.Write(img)
	}
	mw.Close()
	status, out := mcpDispatch(c, http.MethodPost, "/my/videos/api/channel", mw.FormDataContentType(), body.Bytes())
	return mcpAPIResult(status, out)
}
