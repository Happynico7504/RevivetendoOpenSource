package relayhub

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Happynico7504/relaylink"
)

// Cache lifetimes the main grants to relays. The main decides these; relays
// obey. Invalidations (see InvalidationLog) cut them short when data changes.
const (
	ttlIdentity   = 30 * 24 * 3600 // PID <-> PNID mappings never change
	ttlNegative   = 60             // "unknown" answers: retry soon, a mapping may appear
	ttlRedirects  = 300
	ttlBanCheck   = 300
	maxPNIDLength = 64
)

// Tags. Whatever changes the underlying data must Append the matching tag.
func TagPID(pid uint64) string   { return "pid:" + strconv.FormatUint(pid, 10) }
func TagPNID(pnid string) string { return "pnid:" + strings.ToLower(pnid) }
func TagRedirects() string       { return "config:redirects" }
func TagBans() string            { return "bans" }

// Hub is the main-side request router for relay calls.
type Hub struct {
	Src   Source
	Log   *InvalidationLog
	Certs *CertStore    // optional: certificate sync
	Fwd   *Forwarder    // optional: request forwarding
	Rel   *ReleaseStore // optional: over-the-air relayd updates
	Now   func() time.Time
}

func (h *Hub) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func fail(status int) *relaylink.Response { return &relaylink.Response{Status: status} }

// Dispatch handles one verified relay request. All relays may call all of
// these; every endpoint is read-only, and nothing here can reach secrets.
func (h *Hub) Dispatch(ctx context.Context, req *relaylink.Request) *relaylink.Response {
	u, err := url.Parse(req.Path)
	if err != nil {
		return fail(http.StatusBadRequest)
	}
	p := u.Path
	if p == relaylink.ForwardPath {
		if req.Method != http.MethodPost || h.Fwd == nil {
			return fail(http.StatusMethodNotAllowed)
		}
		fr, ok := decodeForward(req.Body)
		if !ok {
			return fail(http.StatusBadRequest)
		}
		return h.Fwd.Do(ctx, fr)
	}
	if req.Method != http.MethodGet {
		return fail(http.StatusMethodNotAllowed)
	}
	switch {
	case p == "/relay/v1/ping":
		return relaylink.JSON(200, map[string]any{"now": h.now().Unix(), "relay": req.RelayID}, 0)

	case p == relaylink.InvalidationsPath:
		after, _ := strconv.ParseInt(u.Query().Get("after"), 10, 64)
		return relaylink.JSON(200, h.Log.Batch(after, u.Query().Get("epoch")), 0)

	case strings.HasPrefix(p, "/relay/v1/identity/pid/"):
		pid, err := strconv.ParseUint(strings.TrimPrefix(p, "/relay/v1/identity/pid/"), 10, 64)
		if err != nil || pid == 0 {
			return fail(http.StatusBadRequest)
		}
		pnid, err := h.Src.PNIDForPID(ctx, pid)
		if errors.Is(err, ErrNotFound) {
			return &relaylink.Response{Status: 404, TTL: ttlNegative, Tags: []string{TagPID(pid)}}
		}
		if err != nil {
			return fail(http.StatusInternalServerError)
		}
		return relaylink.JSON(200, map[string]any{"pid": pid, "pnid": pnid}, ttlIdentity, TagPID(pid), TagPNID(pnid))

	case strings.HasPrefix(p, "/relay/v1/identity/pnid/"):
		pnid := strings.TrimPrefix(p, "/relay/v1/identity/pnid/")
		if pnid == "" || len(pnid) > maxPNIDLength || strings.ContainsAny(pnid, "/?#\x00") {
			return fail(http.StatusBadRequest)
		}
		pid, err := h.Src.PIDForPNID(ctx, pnid)
		if errors.Is(err, ErrNotFound) {
			return &relaylink.Response{Status: 404, TTL: ttlNegative, Tags: []string{TagPNID(pnid)}}
		}
		if err != nil {
			return fail(http.StatusInternalServerError)
		}
		return relaylink.JSON(200, map[string]any{"pid": pid, "pnid": pnid}, ttlIdentity, TagPID(pid), TagPNID(pnid))

	case p == relaylink.CertManifestPath:
		if h.Certs == nil {
			return fail(http.StatusNotFound)
		}
		m, err := h.Certs.Manifest()
		if err != nil {
			return fail(http.StatusInternalServerError)
		}
		return relaylink.JSON(200, m, 0) // TTL 0: keys must never enter a relay cache

	case strings.HasPrefix(p, relaylink.CertPairPrefix):
		if h.Certs == nil {
			return fail(http.StatusNotFound)
		}
		pair, ok := h.Certs.Pair(strings.TrimPrefix(p, relaylink.CertPairPrefix))
		if !ok {
			return fail(http.StatusNotFound)
		}
		return relaylink.JSON(200, pair, 0)

	case p == relaylink.UpdateManifestPath:
		if h.Rel == nil {
			return fail(http.StatusNotFound)
		}
		m, err := h.Rel.Manifest(u.Query().Get("os"), u.Query().Get("arch"))
		if err != nil {
			return fail(http.StatusNotFound)
		}
		return relaylink.JSON(200, m, 0)

	case p == relaylink.UpdateChunkPath:
		if h.Rel == nil {
			return fail(http.StatusNotFound)
		}
		ver, e1 := strconv.ParseUint(u.Query().Get("version"), 10, 64)
		off, e2 := strconv.ParseInt(u.Query().Get("offset"), 10, 64)
		if e1 != nil || e2 != nil {
			return fail(http.StatusBadRequest)
		}
		c, err := h.Rel.Chunk(u.Query().Get("os"), u.Query().Get("arch"), ver, off)
		if err != nil {
			return fail(http.StatusNotFound)
		}
		return relaylink.JSON(200, c, 0)

	case p == "/relay/v1/config/redirects":
		rs, err := h.Src.Redirects(ctx)
		if err != nil {
			return fail(http.StatusInternalServerError)
		}
		return relaylink.JSON(200, rs, ttlRedirects, TagRedirects())

	case strings.HasPrefix(p, "/relay/v1/bans/"):
		pid, err := strconv.ParseUint(strings.TrimPrefix(p, "/relay/v1/bans/"), 10, 64)
		if err != nil || pid == 0 {
			return fail(http.StatusBadRequest)
		}
		banned, err := h.Src.IsBanned(ctx, pid)
		if err != nil {
			return fail(http.StatusInternalServerError)
		}
		return relaylink.JSON(200, map[string]any{"pid": pid, "banned": banned}, ttlBanCheck, TagBans(), TagPID(pid))
	}
	return fail(http.StatusNotFound)
}
