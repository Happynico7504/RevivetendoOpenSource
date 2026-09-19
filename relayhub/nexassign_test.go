package relayhub

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Happynico7504/relaylink"
)

type fakeGeo map[string][2]string

func (f fakeGeo) Lookup(ip net.IP) (string, string) { v := f[ip.String()]; return v[0], v[1] }

const (
	ipUS = "198.51.100.1" // a US client
	ipDE = "198.51.100.2" // a German client
	ipJP = "198.51.100.3" // a Japanese client
	ipBR = "198.51.100.4" // a Brazilian client (continent SA)
)

var testGeo = fakeGeo{ipUS: {"US", "NA"}, ipDE: {"DE", "EU"}, ipJP: {"JP", "AS"}, ipBR: {"BR", "SA"}}

func testGames() map[string]relaylink.NexGame {
	g := relaylink.NexGameDefaults()
	out := map[string]relaylink.NexGame{}
	for _, n := range []string{"wsc", "mk8"} {
		x := g[n]
		x.SecureHost, x.SecurePort, x.KerberosPassword = "45.157.178.35", "60015", "kerb-"+n
		out[n] = x
	}
	return out
}

type nexRig struct {
	st       *streamStack
	assigner *NexAssigner
	reg      *MemRegistry
	relays   map[string]*fakeNexRelay
}

type fakeNexRelay struct {
	conn *relaylink.StreamConn
	mu   sync.Mutex
	puts []relaylink.NexCredPut
	fail bool
}

func addRelay(t *testing.T, reg *MemRegistry, id, region, host string) {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := reg.Add(context.Background(), &Relay{ID: id, Region: region, Host: host, PublicKey: pub, Enabled: true}); err != nil {
		t.Fatal(err)
	}
}

func newNexRig(t *testing.T) *nexRig {
	t.Helper()
	st := newStreamStack(t, relaylink.StreamOptions{})
	reg := NewMemRegistry()
	addRelay(t, reg, "us-1", "na", "203.0.113.5")
	addRelay(t, reg, "jp-1", "jp", "203.0.113.20")
	a := &NexAssigner{Streams: st.hub, Registry: reg, Geo: testGeo, Games: testGames,
		RTT: func(id string) time.Duration { return map[string]time.Duration{"us-2": 50 * time.Millisecond}[id] }}
	a.Register()
	return &nexRig{st: st, assigner: a, reg: reg, relays: map[string]*fakeNexRelay{}}
}

// connectRelay connects as relay `id`, announces `games` like relayd does, and
// records every credential pushed to it.
func (r *nexRig) connectRelay(t *testing.T, id string, games ...string) *fakeNexRelay {
	t.Helper()
	fr := &fakeNexRelay{}
	c := r.st.asRelay(id)
	conn, err := c.DialStream(context.Background(), r.st.addr, relaylink.StreamHandlers{
		Call: func(_ context.Context, _ *relaylink.StreamConn, method string, body []byte) ([]byte, error) {
			if method != relaylink.MethodCredPut {
				return nil, errors.New("unknown")
			}
			fr.mu.Lock()
			defer fr.mu.Unlock()
			if fr.fail {
				return nil, errors.New("child not running")
			}
			var p relaylink.NexCredPut
			json.Unmarshal(body, &p)
			fr.puts = append(fr.puts, p)
			return []byte("ok"), nil
		},
	}, r.st.opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	fr.conn = conn
	waitFor(t, "relay "+id+" registered", func() bool {
		for _, s := range r.st.hub.Status() {
			if s.ID == id {
				return true
			}
		}
		return false
	})
	body, _ := json.Marshal(relaylink.NexHelloRequest{Games: games})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := conn.Call(ctx, relaylink.MethodNexHello, body); err != nil {
		t.Fatal(err)
	}
	r.relays[id] = fr
	return fr
}

func (f *fakeNexRelay) putCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.puts) }

func assign(r *nexRig, game string, pid uint32, ip string) (*AssignResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return r.assigner.Assign(ctx, AssignRequest{Game: game, PID: pid, Password: "tok", ClientIP: ip})
}

func TestRegionForClient(t *testing.T) {
	for _, c := range []struct{ country, continent, want string }{
		{"US", "NA", "na"}, {"CA", "NA", "na"}, {"MX", "NA", "na"},
		{"BR", "SA", "na"},                                         // continent rule
		{"JP", "AS", "jp"}, {"KR", "AS", "jp"}, {"TW", "AS", "jp"}, // country rule beats the asia continent rule
		{"TH", "AS", "asia"}, {"AU", "OC", "asia"},
		{"DE", "EU", "eu"}, {"ZA", "AF", "eu"},
		{"", "", ""}, {"AQ", "AN", ""},
	} {
		if got := RegionForClient(c.country, c.continent); got != c.want {
			t.Errorf("%s/%s -> %q, want %q", c.country, c.continent, got, c.want)
		}
	}
}

func TestAssignPushesTheCredentialThenReturnsTheRelay(t *testing.T) {
	r := newNexRig(t)
	us := r.connectRelay(t, "us-1", "wsc", "mk8")
	res, err := assign(r, "wsc", 1435853600, ipUS)
	if err != nil {
		t.Fatal(err)
	}
	if res.Relay != "us-1" || res.Host != "203.0.113.5" || res.Port != 60014 {
		t.Fatalf("result: %+v", res)
	}
	if us.putCount() != 1 {
		t.Fatalf("the relay received %d credentials, want 1", us.putCount())
	}
	p := us.puts[0]
	if p.Game != "wsc" || p.PID != 1435853600 || p.Password != "tok" || p.TTLSeconds != int(AssignTTL/time.Second) {
		t.Fatalf("pushed credential: %+v", p)
	}
	// Another game on the same relay gets that game's port.
	if res, err := assign(r, "mk8", 7, ipBR); err != nil || res.Port != 60002 {
		t.Fatalf("mk8 / Brazil (continent rule): %+v %v", res, err)
	}
}

func TestAssignFallsBackToTheMainWheneverInDoubt(t *testing.T) {
	r := newNexRig(t)
	us := r.connectRelay(t, "us-1", "wsc") // hosts wsc only
	for name, fn := range map[string]func() error{
		"client in Germany (the main serves it)": func() error { _, e := assign(r, "wsc", 1, ipDE); return e },
		"client in Japan, no JP relay connected": func() error { _, e := assign(r, "wsc", 1, ipJP); return e },
		"game the relay does not host":           func() error { _, e := assign(r, "mk8", 1, ipUS); return e },
		"unknown game":                           func() error { _, e := assign(r, "zelda", 1, ipUS); return e },
		"pid zero":                               func() error { _, e := assign(r, "wsc", 0, ipUS); return e },
		"garbage client ip":                      func() error { _, e := assign(r, "wsc", 1, "not-an-ip"); return e },
		"unknown client ip (no geo data)":        func() error { _, e := assign(r, "wsc", 1, "192.0.2.77"); return e },
	} {
		if fn() == nil {
			t.Errorf("%s: an assignment was made", name)
		}
	}
	if us.putCount() != 0 {
		t.Fatalf("credentials were pushed although no assignment was made (%d)", us.putCount())
	}
	// A registered relay that is not connected, or was disabled, is never chosen.
	r.reg.SetEnabled(context.Background(), "us-1", false)
	if _, err := assign(r, "wsc", 1, ipUS); err == nil {
		t.Fatal("a disabled relay was chosen")
	}
	r.reg.SetEnabled(context.Background(), "us-1", true)
	us.conn.Close()
	waitFor(t, "the disconnect to register", func() bool { return len(r.st.hub.Status()) == 0 })
	if _, err := assign(r, "wsc", 1, ipUS); err == nil {
		t.Fatal("a disconnected relay was chosen")
	}
}

func TestAssignRequiresTheRelaysAcknowledgement(t *testing.T) {
	r := newNexRig(t)
	us := r.connectRelay(t, "us-1", "wsc")
	us.mu.Lock()
	us.fail = true // its nexauth child is down
	us.mu.Unlock()
	if _, err := assign(r, "wsc", 55, ipUS); err == nil {
		t.Fatal("the console was sent to a relay that could not store its credential")
	}
	// And that failed attempt gave the relay no right to read the credential later.
	if _, err := r.assigner.handleCredGet(context.Background(), "us-1", mustJSON(relaylink.NexCredGet{Game: "wsc", PID: 55})); err == nil {
		t.Fatal("cred.get served a credential that was never assigned")
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func TestCredGetOnlyForConsolesSentToThatRelay(t *testing.T) {
	r := newNexRig(t)
	r.connectRelay(t, "us-1", "wsc")
	r.connectRelay(t, "jp-1", "wsc")
	if _, err := assign(r, "wsc", 100, ipUS); err != nil {
		t.Fatal(err)
	}
	get := func(relay, game string, pid uint32) (string, error) {
		out, err := r.assigner.handleCredGet(context.Background(), relay, mustJSON(relaylink.NexCredGet{Game: game, PID: pid}))
		if err != nil {
			return "", err
		}
		var a relaylink.NexCredAnswer
		json.Unmarshal(out, &a)
		return a.Password, nil
	}
	if pw, err := get("us-1", "wsc", 100); err != nil || pw != "tok" {
		t.Fatalf("the assigned relay was refused: %q %v", pw, err)
	}
	for name, fn := range map[string]func() error{
		"another relay":    func() error { _, e := get("jp-1", "wsc", 100); return e },
		"another game":     func() error { _, e := get("us-1", "mk8", 100); return e },
		"another console":  func() error { _, e := get("us-1", "wsc", 101); return e },
		"an unknown relay": func() error { _, e := get("evil-9", "wsc", 100); return e },
	} {
		if fn() == nil {
			t.Errorf("cred.get served a credential to %s", name)
		}
	}
	// The right expires: after AssignTTL a relay can no longer read it.
	now := time.Now()
	r.assigner.Now = func() time.Time { return now.Add(AssignTTL + time.Second) }
	if _, err := get("us-1", "wsc", 100); err == nil {
		t.Fatal("an expired assignment still served the credential")
	}
}

func TestAssignPrefersTheFastestRelayInTheRegion(t *testing.T) {
	r := newNexRig(t)
	addRelay(t, r.reg, "us-2", "na", "203.0.113.6")
	r.connectRelay(t, "us-1", "wsc")
	r.connectRelay(t, "us-2", "wsc")
	// us-1 reports 0 (no measurement yet) so it sorts first; make it explicit:
	r.assigner.RTT = func(id string) time.Duration {
		return map[string]time.Duration{"us-1": 90 * time.Millisecond, "us-2": 40 * time.Millisecond}[id]
	}
	res, err := assign(r, "wsc", 9, ipUS)
	if err != nil || res.Relay != "us-2" || res.Host != "203.0.113.6" {
		t.Fatalf("expected the faster relay: %+v %v", res, err)
	}
}

func TestHelloReturnsOnlyRequestedConfiguredGames(t *testing.T) {
	r := newNexRig(t)
	us := r.connectRelay(t, "us-1") // announce nothing yet
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := us.conn.Call(ctx, relaylink.MethodNexHello, mustJSON(relaylink.NexHelloRequest{Games: []string{"mk8", "wsc", "zelda", "badge-arcade"}}))
	if err != nil {
		t.Fatal(err)
	}
	var resp relaylink.NexHelloResponse
	json.Unmarshal(out, &resp)
	if len(resp.Games) != 2 || resp.Games[0].Name != "mk8" || resp.Games[1].Name != "wsc" {
		t.Fatalf("games: %+v", resp.Games)
	}
	if resp.Games[1].KerberosPassword != "kerb-wsc" || resp.Games[1].SecureHost != "45.157.178.35" || resp.Games[1].SecurePort != "60015" {
		t.Fatalf("configuration incomplete: %+v", resp.Games[1])
	}
	if _, err := us.conn.Call(ctx, relaylink.MethodNexHello, []byte("garbage")); err == nil {
		t.Fatal("garbage hello accepted")
	}
}

func TestLoadNexGames(t *testing.T) {
	root := t.TempDir()
	write := func(dir, content string) {
		os.MkdirAll(filepath.Join(root, dir), 0o755)
		os.WriteFile(filepath.Join(root, dir, ".env"), []byte(content), 0o600)
	}
	write("wsc-authentication", "MONGO_URI=x\nSECURE_SERVER_LOCATION=45.157.178.35\nSECURE_SERVER_PORT=60015\n# c\nKERBEROS_PASSWORD=ignored\n")
	write("mk8-authentication", "SECURE_SERVER_LOCATION=\"1.2.3.4\"\nSECURE_SERVER_PORT=60003\n")
	write("badge-arcade-authentication", "SECURE_SERVER_LOCATION=host.example\nSECURE_SERVER_PORT=60019\n")
	env := map[string]string{"WSC_KERBEROS_PASSWORD": "wk", "MK8_KERBEROS_PASSWORD": "mk"} // no BA password
	games := LoadNexGames(root, func(k string) string { return env[k] })
	if len(games) != 2 {
		t.Fatalf("games: %v", games)
	}
	if g := games["wsc"]; g.SecureHost != "45.157.178.35" || g.SecurePort != "60015" || g.KerberosPassword != "wk" || g.Port != 60014 {
		t.Fatalf("wsc: %+v", g)
	}
	if g := games["mk8"]; g.SecureHost != "1.2.3.4" || g.KerberosPassword != "mk" {
		t.Fatalf("mk8 (quoted value): %+v", g)
	}
	if _, ok := games["badge-arcade"]; ok {
		t.Fatal("a game without a Kerberos password was offered to relays")
	}
	if len(LoadNexGames(t.TempDir(), func(string) string { return "x" })) != 0 {
		t.Fatal("games offered without an .env")
	}
}

func TestForcedPlayersGoToARelayWhateverTheirRegion(t *testing.T) {
	r := newNexRig(t)
	us := r.connectRelay(t, "us-1", "wsc")
	force := map[uint32]bool{}
	r.assigner.ForcePIDs = func() map[uint32]bool { return force }

	// Not forced: a German console is served by the main, as before.
	if _, err := assign(r, "wsc", 1435853600, ipDE); err == nil {
		t.Fatal("a German console was assigned without being forced")
	}
	// Forced by PID: assigned to the relay, and only that PID.
	force[1435853600] = true
	res, err := assign(r, "wsc", 1435853600, ipDE)
	if err != nil || res.Relay != "us-1" || res.Port != 60014 {
		t.Fatalf("forced German console: %+v %v", res, err)
	}
	if us.putCount() != 1 {
		t.Fatalf("relay got %d credentials, want 1", us.putCount())
	}
	if _, err := assign(r, "wsc", 42, ipDE); err == nil {
		t.Fatal("forcing one PID assigned another")
	}
	// Forced players need no usable client IP at all.
	if _, err := assign(r, "wsc", 1435853600, ""); err != nil {
		t.Fatalf("forced player with no client IP: %v", err)
	}
	// "Everyone".
	force[ForceAll] = true
	if res, err := assign(r, "wsc", 42, ipDE); err != nil || res.Relay != "us-1" {
		t.Fatalf("force-all: %+v %v", res, err)
	}
}

func TestForcingNeverPicksARelayThatDoesNotHostTheGameOrIsDown(t *testing.T) {
	r := newNexRig(t)
	r.assigner.ForcePIDs = func() map[uint32]bool { return map[uint32]bool{ForceAll: true} }
	r.connectRelay(t, "us-1", "mk8") // hosts mk8 only
	if _, err := assign(r, "wsc", 5, ipDE); err == nil {
		t.Fatal("assigned to a relay that does not host the game")
	}
	// jp-1 is registered but not connected: never chosen either.
	if res, err := assign(r, "mk8", 5, ipDE); err != nil || res.Relay != "us-1" {
		t.Fatalf("mk8: %+v %v", res, err)
	}
}

func TestParseForcePIDs(t *testing.T) {
	m := ParseForcePIDs("# my consoles\n1435853600, 1532880379   # second console\n\nabc 0 -5\n")
	if len(m) != 2 || !m[1435853600] || !m[1532880379] || m[ForceAll] {
		t.Fatalf("parsed: %v", m)
	}
	if !ParseForcePIDs("*")[ForceAll] || !ParseForcePIDs("1, *")[ForceAll] {
		t.Fatal(`"*" not recognised`)
	}
	if len(ParseForcePIDs("")) != 0 || len(ParseForcePIDs("# only a comment")) != 0 {
		t.Fatal("empty input produced entries")
	}
}

func TestHelloDerivesTheEdgeGameFromWSCAndTheRelaysOwnAddress(t *testing.T) {
	r := newNexRig(t)
	us := r.connectRelay(t, "us-1") // relay us-1 is registered with host 203.0.113.5
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := us.conn.Call(ctx, relaylink.MethodNexHello, mustJSON(relaylink.NexHelloRequest{Games: []string{"wsc", relaylink.WSCEdgeGame}}))
	if err != nil {
		t.Fatal(err)
	}
	var resp relaylink.NexHelloResponse
	json.Unmarshal(out, &resp)
	if len(resp.Games) != 2 || resp.Games[0].Name != "wsc" || resp.Games[1].Name != relaylink.WSCEdgeGame {
		t.Fatalf("games: %+v", resp.Games)
	}
	wsc, edge := resp.Games[0], resp.Games[1]
	// Same secret and protocol settings as WSC (the edge decrypts the tickets the auth issues)...
	if edge.KerberosPassword != wsc.KerberosPassword || edge.AccessKey != wsc.AccessKey || edge.NEXMinor != wsc.NEXMinor || edge.GameServerID != wsc.GameServerID {
		t.Fatalf("edge differs from wsc in what must match: %+v vs %+v", edge, wsc)
	}
	// ...but consoles are sent to the relay itself, and it has its own auth port.
	if edge.SecureHost != "203.0.113.5" || edge.SecurePort != relaylink.WSCEdgeSecurePort || edge.Port == wsc.Port {
		t.Fatalf("edge routing: host %q port %q auth port %d", edge.SecureHost, edge.SecurePort, edge.Port)
	}
	// Without WSC configured on the main there is nothing to derive it from.
	r.assigner.Games = func() map[string]relaylink.NexGame { return map[string]relaylink.NexGame{} }
	out, _ = us.conn.Call(ctx, relaylink.MethodNexHello, mustJSON(relaylink.NexHelloRequest{Games: []string{relaylink.WSCEdgeGame}}))
	json.Unmarshal(out, &resp)
	if len(resp.Games) != 0 {
		t.Fatalf("edge game offered without wsc: %+v", resp.Games)
	}
}

func TestListedConsolesAreSentToTheEdgeAuthAndOthersAreNot(t *testing.T) {
	r := newNexRig(t)
	us := r.connectRelay(t, "us-1", "wsc", relaylink.WSCEdgeGame)
	edgeList := map[uint32]bool{1435853600: true}
	r.assigner.EdgePIDs = func() map[uint32]bool { return edgeList }
	r.assigner.ForcePIDs = func() map[uint32]bool { return map[uint32]bool{ForceAll: true} } // relay for every region

	res, err := assign(r, "wsc", 1435853600, ipDE)
	if err != nil || res.Game != relaylink.WSCEdgeGame || res.Port != 60114 || res.Host != "203.0.113.5" {
		t.Fatalf("listed console: %+v %v", res, err)
	}
	if us.putCount() != 1 || us.puts[0].Game != relaylink.WSCEdgeGame {
		t.Fatalf("the credential was staged for %+v", us.puts)
	}
	// Not listed: ordinary WSC auth on the relay.
	res, err = assign(r, "wsc", 42, ipDE)
	if err != nil || res.Game != "wsc" || res.Port != 60014 {
		t.Fatalf("unlisted console: %+v %v", res, err)
	}
	// Only WSC has an edge: other games are untouched even for a listed PID.
	edgeList[ForceAll] = true
	if res, err := assign(r, "mk8", 7, ipDE); err == nil && res.Game != "mk8" {
		t.Fatalf("mk8 was sent to %q", res.Game)
	}
	// "*" lists everyone.
	if res, err := assign(r, "wsc", 43, ipDE); err != nil || res.Game != relaylink.WSCEdgeGame {
		t.Fatalf("everyone listed: %+v %v", res, err)
	}
}

func TestEdgeListedConsoleFallsBackToPlainAuthWhenNoRelayOffersTheEdge(t *testing.T) {
	r := newNexRig(t)
	r.connectRelay(t, "us-1", "wsc") // hosts wsc but has NOT announced the edge (its edge is down)
	r.assigner.EdgePIDs = func() map[uint32]bool { return map[uint32]bool{ForceAll: true} }
	res, err := assign(r, "wsc", 5, ipUS)
	if err != nil || res.Game != "wsc" || res.Port != 60014 {
		t.Fatalf("no edge offered: %+v %v", res, err)
	}
}

func TestEdgeCredentialIsScopedToTheEdgeGame(t *testing.T) {
	r := newNexRig(t)
	r.connectRelay(t, "us-1", "wsc", relaylink.WSCEdgeGame)
	r.assigner.EdgePIDs = func() map[uint32]bool { return map[uint32]bool{100: true} }
	if res, err := assign(r, "wsc", 100, ipUS); err != nil || res.Game != relaylink.WSCEdgeGame {
		t.Fatalf("%+v %v", res, err)
	}
	get := func(game string) error {
		_, err := r.assigner.handleCredGet(context.Background(), "us-1", mustJSON(relaylink.NexCredGet{Game: game, PID: 100}))
		return err
	}
	if err := get(relaylink.WSCEdgeGame); err != nil {
		t.Fatalf("the edge auth was refused its own credential: %v", err)
	}
	// The plain WSC auth server never received it, so it must not be able to pull it either.
	if err := get("wsc"); err == nil {
		t.Fatal("plain wsc auth could pull a credential staged for the edge")
	}
}

func TestRefusedCredPullsExplainThemselvesWithoutLeakingThePassword(t *testing.T) {
	r := newNexRig(t)
	var lines []string
	var mu sync.Mutex
	r.assigner.Logf = func(f string, a ...any) { mu.Lock(); lines = append(lines, fmt.Sprintf(f, a...)); mu.Unlock() }
	r.connectRelay(t, "us-1", "wsc")
	r.connectRelay(t, "jp-1", "wsc")
	if _, err := assign(r, "wsc", 100, ipUS); err != nil { // assigned to us-1 with password "tok"
		t.Fatal(err)
	}
	pull := func(relay string, pid uint32) {
		r.assigner.handleCredGet(context.Background(), relay, mustJSON(relaylink.NexCredGet{Game: "wsc", PID: pid}))
	}
	pull("us-1", 555) // never assigned (or forgotten by a hub restart)
	pull("jp-1", 100) // assigned to another relay
	now := time.Now()
	r.assigner.Now = func() time.Time { return now.Add(AssignTTL + 5*time.Second) }
	pull("us-1", 100) // expired
	pull("us-1", 100)
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 4 {
		t.Fatalf("%d log lines: %v", len(lines), lines)
	}
	for i, want := range []string{"never assigned", "was assigned to relay us-1", "expired", "expired"} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("line %d = %q, want it to mention %q", i, lines[i], want)
		}
	}
	for _, l := range lines {
		if strings.Contains(l, "tok") {
			t.Fatalf("the credential leaked into a log line: %q", l)
		}
	}
	// A pull that IS allowed logs nothing.
	r.assigner.Now = nil
	before := len(lines)
	mu.Unlock()
	pull("us-1", 100)
	mu.Lock()
	if len(lines) != before {
		t.Fatalf("a granted pull logged: %v", lines[before:])
	}
}

func TestWiiUChatIsOfferedOnlyOnceTheMainSharesItsKerberosPassword(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "wiiu-chat-secure"), 0o755)
	// Wii U Chat's .env uses its own key names, not SECURE_SERVER_LOCATION / SECURE_SERVER_PORT.
	os.WriteFile(filepath.Join(root, "wiiu-chat-secure", ".env"),
		[]byte("PN_WUC_SECURE_SERVER_HOST=45.157.178.35\nPN_WUC_SECURE_SERVER_PORT=60005\nPN_WUC_AUTHENTICATION_SERVER_PORT=60004\n"), 0o600)

	// Before the main exports the shared password (an older bridge start) there is nothing to
	// offer: the process made up its own, which a relay could not use.
	if _, ok := LoadNexGames(root, func(string) string { return "" })["wiiu-chat"]; ok {
		t.Fatal("wiiu-chat offered without a shared Kerberos password")
	}
	games := LoadNexGames(root, func(k string) string {
		if k == "PN_WUC_KERBEROS_PASSWORD" {
			return "shared-secret"
		}
		return ""
	})
	g, ok := games["wiiu-chat"]
	if !ok {
		t.Fatal("wiiu-chat not offered although its password and secure address are known")
	}
	if g.SecureHost != "45.157.178.35" || g.SecurePort != "60005" || g.KerberosPassword != "shared-secret" ||
		g.Port != 60004 || g.AccessKey != "e7a47214" || g.NEXMajor != 3 || g.NEXMinor != 3 || g.NEXPatch != 2 || g.GameServerID != "1005A000" {
		t.Fatalf("wiiu-chat configuration: %+v", g)
	}
}
