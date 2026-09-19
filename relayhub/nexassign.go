package relayhub

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oschwald/maxminddb-golang"

	"github.com/Happynico7504/relaylink"
)

// ---- configuration ----------------------------------------------------------------

// nexAuthDirs maps a game to the directory whose .env says where its secure
// server is (SECURE_SERVER_LOCATION / SECURE_SERVER_PORT): the single source of
// truth the auth servers on the main use themselves.
var nexAuthDirs = map[string]string{
	"wsc":          "wsc-authentication",
	"mk8":          "mk8-authentication",
	"badge-arcade": "badge-arcade-authentication",
	"wiiu-chat":    "wiiu-chat-secure", // its auth server is part of the chat process
}

// nexSecureEnvKeys names the .env keys holding a game's secure-server address, for the games
// that do not use the SECURE_SERVER_LOCATION / SECURE_SERVER_PORT pair.
var nexSecureEnvKeys = map[string][2]string{
	"wiiu-chat": {"PN_WUC_SECURE_SERVER_HOST", "PN_WUC_SECURE_SERVER_PORT"},
}

// nexKerberosEnv names the environment variable holding each game's Kerberos
// password (start.sh generates them at every bridge start).
var nexKerberosEnv = map[string]string{
	"wsc": "WSC_KERBEROS_PASSWORD", "mk8": "MK8_KERBEROS_PASSWORD", "badge-arcade": "BA_KERBEROS_PASSWORD",
	// wiiu-chat generates its own unless this is set; start.sh sets it so the relay can share it.
	"wiiu-chat": "PN_WUC_KERBEROS_PASSWORD",
}

func readDotEnv(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.IndexByte(line, '='); i > 0 {
			out[strings.TrimSpace(line[:i])] = strings.Trim(strings.TrimSpace(line[i+1:]), `"'`)
		}
	}
	return out
}

// LoadNexGames builds the complete configuration of every game the main can
// authenticate for a relay. A game whose Kerberos password or secure server
// address is unknown is left out: relays are never told to host something the
// main cannot vouch for.
func LoadNexGames(root string, getenv func(string) string) map[string]relaylink.NexGame {
	out := map[string]relaylink.NexGame{}
	for name, g := range relaylink.NexGameDefaults() {
		if name == relaylink.WSCEdgeGame {
			continue // derived per relay from wsc (see edgeGame), never configured on its own
		}
		env := readDotEnv(filepath.Join(root, nexAuthDirs[name], ".env"))
		hostKey, portKey := "SECURE_SERVER_LOCATION", "SECURE_SERVER_PORT"
		if k, ok := nexSecureEnvKeys[name]; ok {
			hostKey, portKey = k[0], k[1]
		}
		g.SecureHost, g.SecurePort = env[hostKey], env[portKey]
		g.KerberosPassword = getenv(nexKerberosEnv[name])
		if g.SecureHost == "" || g.SecurePort == "" || g.KerberosPassword == "" {
			continue
		}
		out[name] = g
	}
	return out
}

// ---- geography ------------------------------------------------------------------------

// Geo maps a client address to a country and continent code.
type Geo interface {
	Lookup(ip net.IP) (country, continent string)
}

type MMDBGeo struct{ r *maxminddb.Reader }

func OpenMMDBGeo(path string) (*MMDBGeo, error) {
	r, err := maxminddb.Open(path)
	if err != nil {
		return nil, err
	}
	return &MMDBGeo{r: r}, nil
}

func (g *MMDBGeo) Lookup(ip net.IP) (string, string) {
	var rec struct {
		Country struct {
			ISOCode string `maxminddb:"iso_code"`
		} `maxminddb:"country"`
		Continent struct {
			Code string `maxminddb:"code"`
		} `maxminddb:"continent"`
	}
	if g.r.Lookup(ip, &rec) != nil {
		return "", ""
	}
	return rec.Country.ISOCode, rec.Continent.Code
}

// RegionForClient applies the same rules as the DNS server: an exact country
// match first (country-specific regions such as jp beat continent ones), then a
// continent match. "" means the main serves this client.
func RegionForClient(country, continent string) string {
	contains := func(list []string, v string) bool {
		for _, x := range list {
			if x == v {
				return true
			}
		}
		return false
	}
	for _, name := range regionOrder {
		if country != "" && contains(RegionCatalog[name].Countries, country) {
			return name
		}
	}
	for _, name := range regionOrder {
		if continent != "" && contains(RegionCatalog[name].Continents, continent) {
			return name
		}
	}
	return ""
}

// ---- the assigner -------------------------------------------------------------------------

var (
	ErrNoRelay     = errors.New("relayhub: no suitable relay for this client")
	ErrUnknownGame = errors.New("relayhub: unknown or unconfigured game")
)

// AssignTTL is how long a credential pushed to a relay (and the right of that
// relay to pull it) stays valid: enough for a console to connect, no more.
const AssignTTL = 10 * time.Minute

type assignment struct {
	relayID  string
	password string
	expires  time.Time
}

type AssignRequest struct {
	Game     string `json:"game"`
	PID      uint32 `json:"pid"`
	Password string `json:"password"`
	ClientIP string `json:"client_ip"`
}

type AssignResult struct {
	Relay string `json:"relay"`
	Game  string `json:"game,omitempty"` // the auth game actually assigned (wsc-edge for an edge console)
	Host  string `json:"host"`           // IP address the console should use
	Port  int    `json:"port"`
}

// NexAssigner decides which relay (if any) a console should authenticate at,
// gives that relay the credential BEFORE the console is told to go there, and
// answers a relay's later pull only for consoles it sent there.
// ForceAll is the ForcePIDs key that stands for every player.
const ForceAll uint32 = 0

// ParseForcePIDs reads a list of PIDs separated by whitespace or commas, with "#" comments;
// "*" means everyone. Unparseable words are ignored.
func ParseForcePIDs(text string) map[uint32]bool {
	out := map[uint32]bool{}
	for _, line := range strings.Split(text, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		for _, w := range strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\r' }) {
			if w == "*" {
				out[ForceAll] = true
			} else if n, err := strconv.ParseUint(w, 10, 32); err == nil && n != 0 {
				out[uint32(n)] = true
			}
		}
	}
	return out
}

type NexAssigner struct {
	Streams  *StreamHub
	Registry Registry
	Geo      Geo
	Games    func() map[string]relaylink.NexGame // current configuration (secrets included)
	Now      func() time.Time
	// PutTimeout bounds the wait for the relay's acknowledgement (default 600ms).
	PutTimeout time.Duration
	// ResolveIP turns a relay's configured host into an IPv4 address (default: DNS).
	ResolveIP func(host string) (string, error)
	// RTT returns the stream round trip to a relay (default: from the stream hub).
	RTT func(relayID string) time.Duration
	// ForcePIDs, if set, returns players who are assigned to a relay whatever their region
	// (the fastest connected relay that hosts the game); key ForceAll (0) means every
	// player. It mirrors a DNS that forces clients onto a relay, and for testing a relay
	// with one console before the region rules cover its country.
	// Logf receives the hub's own diagnostics (why a relay's credential pull was refused).
	Logf      func(string, ...any)
	ForcePIDs func() map[uint32]bool
	// EdgePIDs, if set, returns the WSC consoles (or ForceAll) whose session should be
	// terminated on the relay by the WSC edge instead of going to the main's secure server.
	EdgePIDs func() map[uint32]bool

	mu       sync.Mutex
	hello    map[string]map[string]bool // relay id -> games it hosts
	assigned map[string]assignment      // "game/pid"
	ipCache  map[string]ipEntry
}

type ipEntry struct {
	ip      string
	expires time.Time
}

func (a *NexAssigner) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func asKey(game string, pid uint32) string {
	b, _ := json.Marshal([]any{game, pid})
	return string(b)
}

// Register hooks the assigner's calls into the stream hub and forgets a relay's
// announced games when it disconnects.
func (a *NexAssigner) Register() {
	a.Streams.HandleMethod(relaylink.MethodNexHello, a.handleHello)
	a.Streams.HandleMethod(relaylink.MethodCredGet, a.handleCredGet)
}

// edgeGame builds the wsc-edge configuration for one relay: its own IPv4 address is the
// secure server. Not available until WSC itself is configured.
func (a *NexAssigner) edgeGame(ctx context.Context, cfg map[string]relaylink.NexGame, relayID string) (relaylink.NexGame, bool) {
	wsc, ok := cfg[relaylink.WSCEdgeBase]
	if !ok {
		return relaylink.NexGame{}, false
	}
	relays, err := a.Registry.List(ctx)
	if err != nil {
		return relaylink.NexGame{}, false
	}
	for _, r := range relays {
		if r.ID == relayID {
			host, err := a.resolve(r.Host)
			if err != nil {
				return relaylink.NexGame{}, false
			}
			return relaylink.EdgeGame(wsc, host), true
		}
	}
	return relaylink.NexGame{}, false
}

func (a *NexAssigner) handleHello(ctx context.Context, relayID string, body []byte) ([]byte, error) {
	var req relaylink.NexHelloRequest
	if json.Unmarshal(body, &req) != nil {
		return nil, errors.New("bad request")
	}
	cfg := a.Games()
	resp := relaylink.NexHelloResponse{}
	hosted := map[string]bool{}
	for _, name := range req.Games {
		if g, ok := cfg[name]; ok {
			resp.Games = append(resp.Games, g)
			hosted[name] = true
		} else if name == relaylink.WSCEdgeGame {
			// Derived per relay: WSC's configuration with the relay itself as secure server.
			if g, ok := a.edgeGame(ctx, cfg, relayID); ok {
				resp.Games = append(resp.Games, g)
				hosted[name] = true
			}
		}
	}
	sort.Slice(resp.Games, func(i, j int) bool { return resp.Games[i].Name < resp.Games[j].Name })
	a.mu.Lock()
	if a.hello == nil {
		a.hello = map[string]map[string]bool{}
	}
	a.hello[relayID] = hosted
	a.mu.Unlock()
	return json.Marshal(resp)
}

func (a *NexAssigner) handleCredGet(_ context.Context, relayID string, body []byte) ([]byte, error) {
	var req relaylink.NexCredGet
	if json.Unmarshal(body, &req) != nil {
		return nil, errors.New("bad request")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	as, ok := a.assigned[asKey(req.Game, req.PID)]
	// A relay may only ever read credentials of consoles the hub sent to IT, and
	// only while the assignment lives. Everything else looks like "unknown" to the relay;
	// the hub's own log says which it was (never the password).
	if !ok || as.relayID != relayID || !a.now().Before(as.expires) {
		if a.Logf != nil {
			switch {
			case !ok:
				a.Logf("nex: cred.get refused: relay %s asked for %s pid=%d, which the hub never assigned (or has forgotten since a restart)", relayID, req.Game, req.PID)
			case as.relayID != relayID:
				a.Logf("nex: cred.get refused: relay %s asked for %s pid=%d, which was assigned to relay %s", relayID, req.Game, req.PID, as.relayID)
			default:
				a.Logf("nex: cred.get refused: relay %s asked for %s pid=%d, whose assignment expired %v ago", relayID, req.Game, req.PID, a.now().Sub(as.expires).Round(time.Second))
			}
		}
		return nil, errors.New("unknown")
	}
	return json.Marshal(relaylink.NexCredAnswer{Password: as.password})
}

func (a *NexAssigner) hostsGame(relayID, game string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hello[relayID][game]
}

func (a *NexAssigner) rtt(id string) time.Duration {
	if a.RTT != nil {
		return a.RTT(id)
	}
	for _, s := range a.Streams.Status() {
		if s.ID == id {
			return s.RTT
		}
	}
	return 0
}

func (a *NexAssigner) resolve(host string) (string, error) {
	if net.ParseIP(host) != nil {
		return host, nil
	}
	a.mu.Lock()
	if e, ok := a.ipCache[host]; ok && a.now().Before(e.expires) {
		a.mu.Unlock()
		return e.ip, nil
	}
	a.mu.Unlock()
	lookup := a.ResolveIP
	if lookup == nil {
		lookup = func(h string) (string, error) {
			ips, err := net.LookupIP(h)
			if err != nil {
				return "", err
			}
			for _, ip := range ips {
				if v4 := ip.To4(); v4 != nil {
					return v4.String(), nil
				}
			}
			return "", errors.New("no IPv4 address")
		}
	}
	ip, err := lookup(host)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	if a.ipCache == nil {
		a.ipCache = map[string]ipEntry{}
	}
	a.ipCache[host] = ipEntry{ip: ip, expires: a.now().Add(5 * time.Minute)}
	a.mu.Unlock()
	return ip, nil
}

// Assign picks the best relay for a client and gives it the credential. Any
// error means "authenticate at the main as usual".
func (a *NexAssigner) Assign(ctx context.Context, req AssignRequest) (*AssignResult, error) {
	games := a.Games()
	game, ok := games[req.Game]
	if !ok || req.PID == 0 || req.Password == "" {
		return nil, ErrUnknownGame
	}
	forced := false
	if a.ForcePIDs != nil {
		m := a.ForcePIDs()
		forced = m[req.PID] || m[ForceAll]
	}
	region := ""
	if !forced {
		ip := net.ParseIP(req.ClientIP)
		if ip == nil || a.Geo == nil {
			return nil, ErrNoRelay
		}
		country, continent := a.Geo.Lookup(ip)
		region = RegionForClient(country, continent)
		if region == "" {
			return nil, ErrNoRelay
		}
	}
	relays, err := a.Registry.List(ctx)
	if err != nil {
		return nil, err
	}
	connected := map[string]bool{}
	for _, s := range a.Streams.Status() {
		connected[s.ID] = true
	}
	// The relays that could serve this console for a given auth game, fastest first.
	pick := func(name string) []*Relay {
		var cands []*Relay
		for _, r := range relays {
			if r.Enabled && (forced || r.Region == region) && connected[r.ID] && a.hostsGame(r.ID, name) {
				cands = append(cands, r)
			}
		}
		sort.Slice(cands, func(i, j int) bool {
			ri, rj := a.rtt(cands[i].ID), a.rtt(cands[j].ID)
			if ri != rj {
				return ri < rj
			}
			return cands[i].ID < cands[j].ID
		})
		return cands
	}
	// A WSC console the operator listed for the edge is sent to a relay that offers the edge
	// variant (its secure server is the relay itself). A relay only offers it while its edge
	// is healthy, so this falls back to plain WSC auth whenever no edge is available.
	name, port := req.Game, game.Port
	var cands []*Relay
	if req.Game == relaylink.WSCEdgeBase && a.EdgePIDs != nil {
		if m := a.EdgePIDs(); m[req.PID] || m[ForceAll] {
			if c := pick(relaylink.WSCEdgeGame); len(c) > 0 {
				cands, name, port = c, relaylink.WSCEdgeGame, relaylink.NexGameDefaults()[relaylink.WSCEdgeGame].Port
			}
		}
	}
	if len(cands) == 0 {
		cands = pick(req.Game)
	}
	if len(cands) == 0 {
		return nil, ErrNoRelay
	}
	best := cands[0]
	host, err := a.resolve(best.Host)
	if err != nil {
		return nil, err
	}

	// The relay must hold the credential before the console is sent there.
	timeout := a.PutTimeout
	if timeout <= 0 {
		timeout = 600 * time.Millisecond
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, _ := json.Marshal(relaylink.NexCredPut{Game: name, PID: req.PID, Password: req.Password, TTLSeconds: int(AssignTTL / time.Second)})
	if _, err := a.Streams.CallRelay(pctx, best.ID, relaylink.MethodCredPut, body); err != nil {
		return nil, err
	}
	a.mu.Lock()
	if a.assigned == nil {
		a.assigned = map[string]assignment{}
	}
	now := a.now()
	if len(a.assigned) > 4096 {
		for k, v := range a.assigned {
			if !now.Before(v.expires) {
				delete(a.assigned, k)
			}
		}
	}
	a.assigned[asKey(name, req.PID)] = assignment{relayID: best.ID, password: req.Password, expires: now.Add(AssignTTL)}
	a.mu.Unlock()
	return &AssignResult{Relay: best.ID, Host: host, Port: port, Game: name}, nil
}
