package main

// P2P tunnels: consoles that cannot reach each other directly (another region, carrier-grade
// NAT) are told that their partners live at a UDP tunnel the hub opened for the gathering, on
// the main or on a regional relay. The tunnel forwards their packets untouched; all this server
// does is hand out the tunnel's addresses instead of the real ones, consistently, in the two
// places a console learns where another one is: GetSessionURLs (joiner -> host) and the
// InitiateProbe it forwards (host -> joiner). The hub decides whether a gathering gets a tunnel
// (~/.relayhub/wsc-p2p-tunnel) - a missing hub or policy file simply means direct P2P as before.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	nex "github.com/PretendoNetwork/nex-go"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type p2pStation struct {
	PID  uint32 `json:"pid"`
	IP   string `json:"ip"`
	Port int    `json:"port"`
	Hard string `json:"hard,omitempty"` // why this console is hard to reach directly ("" = no known reason)
}

type p2pTunnel struct {
	Instance string         `json:"instance"`
	IP       string         `json:"ip"`
	Ports    map[uint32]int `json:"ports"`
	Reason   string         `json:"reason"`
	at       time.Time
}

// p2pTunnels holds the gatherings that have a tunnel (gid -> *p2pTunnel). A gathering asked once
// without getting one stays direct: its consoles already know each other's real addresses.
var p2pTunnels sync.Map

var p2pHTTP = &http.Client{Timeout: 1500 * time.Millisecond}

// p2pPorts: pid -> the public port a console says its P2P traffic uses. Its secure connection's
// port (what this server sees) is a different socket mapping: captured 2026-10-08, two consoles
// played through a tunnel from ports 56553 and 64478 - the ports in their own probe URLs - while
// their server connections used 64110 and 50490. A tunnel that starts out sending to the
// server-connection port is talking to the wrong mapping until the console speaks first.
var p2pPorts sync.Map

// p2pNotePort remembers the port from a station URL the console sent about itself (its own
// public address, not a tunnel alias). It reports whether the remembered port changed.
func p2pNotePort(pid uint32, station, from string) bool {
	u := nex.NewStationURL(station)
	ip := net.ParseIP(u.Address())
	port, err := strconv.Atoi(u.Port())
	if ip == nil || err != nil || port <= 0 || port > 65535 || ip.IsPrivate() || ip.IsLoopback() {
		return false
	}
	isTunnel := false
	p2pTunnels.Range(func(_, v any) bool {
		if v.(*p2pTunnel).IP == u.Address() {
			isTunnel = true
			return false
		}
		return true
	})
	if isTunnel {
		return false
	}
	old, had := p2pPorts.Swap(pid, port)
	if had && old.(int) == port {
		return false
	}
	fmt.Printf("P2PPort: PID=%d says its P2P port is %d (%s)\n", pid, port, from)
	return true
}

func p2pKey(gid uint32) string { return "wsc:" + strconv.FormatUint(uint64(gid), 10) }

// p2pStationFor describes a console as the hub needs it: the address its secure connection
// comes from and the port of its public (type=3) station URL.
func p2pStationFor(pid uint32) (p2pStation, bool) {
	var doc bson.M
	if sessionsCol.FindOne(context.Background(), bson.D{{Key: "pid", Value: pid}}).Decode(&doc) != nil {
		return p2pStation{}, false
	}
	ip, _ := doc["ip"].(string)
	if net.ParseIP(ip) == nil {
		return p2pStation{}, false
	}
	st := p2pStation{PID: pid, IP: ip}
	for _, u := range dbGetPlayerURLs(pid) {
		su := nex.NewStationURL(u)
		if su.Type() == "3" {
			st.Port, _ = strconv.Atoi(su.Port())
			if v, ok := p2pPorts.Load(pid); ok {
				st.Port = v.(int) // the port its P2P traffic uses, as the console itself said
			}
		} else if a := net.ParseIP(su.Address()); a != nil && cgnatRange.Contains(a) {
			st.Hard = "local address " + su.Address() + " is carrier-grade NAT"
		}
	}
	if st.Hard == "" {
		st.Hard = p2pHardReason(pid)
	}
	return st, true
}

// p2pOpen asks the hub for the gathering's tunnel, adding any members it does not have yet.
// existingOnly: only extend a tunnel the gathering already has (never create one) - used once
// its consoles may already have been told each other's real addresses.
func p2pOpen(gid, host uint32, existingOnly bool) *p2pTunnel {
	if gid == 0 {
		return nil
	}
	old, had := p2pTunnels.Load(gid)
	if existingOnly && !had {
		return nil
	}
	members := dbGetGatheringPlayers(gid)
	ordered := []uint32{host}
	for _, pid := range members {
		if pid != host {
			ordered = append(ordered, pid)
		}
	}
	var stations []p2pStation
	for _, pid := range ordered {
		if st, ok := p2pStationFor(pid); ok {
			stations = append(stations, st)
		}
	}
	if len(stations) < 2 && !had {
		return nil
	}
	body, _ := json.Marshal(map[string]any{"key": p2pKey(gid), "stations": stations})
	resp, err := p2pHTTP.Post(edgeHubURL+"/p2p/open", "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Printf("P2PTunnel: gid=%d hub unreachable: %v\n", gid, err)
		if had {
			return old.(*p2pTunnel)
		}
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if had {
			// The hub could not extend it; the consoles already use it, so keep what we know.
			return old.(*p2pTunnel)
		}
		return nil
	}
	var t p2pTunnel
	if json.NewDecoder(resp.Body).Decode(&t) != nil || net.ParseIP(t.IP) == nil {
		return nil
	}
	t.at = time.Now()
	p2pTunnels.Store(gid, &t)
	if !had {
		var hard []string
		for _, s := range stations {
			if s.Hard != "" {
				hard = append(hard, fmt.Sprintf("%d: %s", s.PID, s.Hard))
			}
		}
		fmt.Printf("P2PTunnel: gid=%d via %s %s ports=%v (%s) hard=%v\n", gid, t.Instance, t.IP, t.Ports, t.Reason, hard)
	}
	return &t
}

// p2pAlias rewrites a station URL to the console's alias on the tunnel. The console's real NAT
// type (natm/natf) stays in the URL. Advertising the tunnel as fully open (natf=1) made joiners
// wait for the host to reach them instead of sending first - and the joiner's own router then
// dropped the host's packets, which came from a tunnel it had never sent to (2026-10-09: every
// "silent joiner" so far, e.g. gid 235976). With the real type both sides send, as they do
// without a tunnel; the tunnel forwards whatever arrives.
func p2pAlias(urlStr string, t *p2pTunnel, pid uint32) (string, bool) {
	port := t.Ports[pid]
	if port == 0 {
		return urlStr, false
	}
	u := nex.NewStationURL(urlStr)
	u.SetAddress(t.IP)
	u.SetPort(strconv.Itoa(port))
	return u.EncodeToString(), true
}

// p2pJanitor forgets tunnels of gatherings that are long gone (the tunnel hosts close idle
// tunnels themselves).
func p2pJanitor() {
	for range time.Tick(time.Minute) {
		p2pTunnels.Range(func(k, v any) bool {
			if time.Since(v.(*p2pTunnel).at) > 4*time.Hour {
				p2pTunnels.Delete(k)
			}
			return true
		})
		p2pStarted.Range(func(k, v any) bool {
			if time.Since(v.(time.Time)) > 4*time.Hour {
				p2pStarted.Delete(k)
			}
			return true
		})
	}
}

// ---- consoles that are hard to reach -------------------------------------------------------
//
// A console behind carrier-grade NAT usually cannot be seen as such from here: it only knows its
// home router's LAN address. What can be seen: a local address inside the CGNAT range
// (100.64.0.0/10; the console sits directly on the carrier's network, e.g. a phone hotspot), a
// symmetric NAT it reports itself (natm=2), and its history - consoles whose matches keep failing
// right at the hole-punch, with different partners, get tunnels for a week.

var cgnatRange = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

const (
	p2pFailWindow   = 24 * time.Hour
	p2pFailCount    = 3 // failed joins within the window ...
	p2pFailPartners = 2 // ... with at least this many different partners
	p2pFlagFor      = 7 * 24 * time.Hour
)

type p2pFailure struct {
	At      time.Time `bson:"at"`
	Partner uint32    `bson:"partner"`
	GID     uint32    `bson:"gid"`
}

type p2pHistory struct {
	PID          uint32       `bson:"pid"`
	Failures     []p2pFailure `bson:"failures"`
	FlaggedUntil time.Time    `bson:"flagged_until"`
	Reason       string       `bson:"reason"`
}

var (
	p2pHistMu  sync.Mutex
	p2pHist    = map[uint32]*p2pHistory{}
	p2pHistCol *mongo.Collection
	// p2pStarted: gatherings whose match started or whose traversal succeeded - leaving one of
	// those quickly is not a connection failure.
	p2pStarted sync.Map
)

func p2pLoadHistory(db *mongo.Database) {
	p2pHistCol = db.Collection("p2p_nat_history")
	cur, err := p2pHistCol.Find(context.Background(), bson.D{})
	if err != nil {
		return
	}
	defer cur.Close(context.Background())
	p2pHistMu.Lock()
	defer p2pHistMu.Unlock()
	for cur.Next(context.Background()) {
		var h p2pHistory
		if cur.Decode(&h) == nil && h.PID != 0 {
			p2pHist[h.PID] = &h
		}
	}
}

// p2pHardReason says why a console is known to be hard to reach, or "".
func p2pHardReason(pid uint32) string {
	if v, ok := pidNATm.Load(pid); ok && v.(uint32) == 2 {
		return "symmetric NAT (natm=2)"
	}
	p2pHistMu.Lock()
	defer p2pHistMu.Unlock()
	if h := p2pHist[pid]; h != nil && time.Now().Before(h.FlaggedUntil) {
		return h.Reason
	}
	return ""
}

// p2pRecordFailure notes a join that ended before the consoles connected, for both consoles.
func p2pRecordFailure(gid, joiner, host uint32) {
	if _, ok := p2pStarted.Load(gid); ok || joiner == host || host == 0 {
		return
	}
	now := time.Now()
	for _, pair := range [][2]uint32{{joiner, host}, {host, joiner}} {
		pid, partner := pair[0], pair[1]
		p2pHistMu.Lock()
		h := p2pHist[pid]
		if h == nil {
			h = &p2pHistory{PID: pid}
			p2pHist[pid] = h
		}
		kept := h.Failures[:0]
		for _, f := range h.Failures {
			if now.Sub(f.At) < p2pFailWindow {
				kept = append(kept, f)
			}
		}
		h.Failures = append(kept, p2pFailure{At: now, Partner: partner, GID: gid})
		partners := map[uint32]bool{}
		for _, f := range h.Failures {
			partners[f.Partner] = true
		}
		flagged := false
		if len(h.Failures) >= p2pFailCount && len(partners) >= p2pFailPartners && !now.Before(h.FlaggedUntil) {
			var l []uint32
			for p := range partners {
				l = append(l, p)
			}
			sort.Slice(l, func(i, j int) bool { return l[i] < l[j] })
			h.FlaggedUntil = now.Add(p2pFlagFor)
			h.Reason = fmt.Sprintf("%d failed joins with %d partners %v in 24h", len(h.Failures), len(partners), l)
			flagged = true
		}
		doc := *h
		doc.Failures = append([]p2pFailure(nil), h.Failures...)
		p2pHistMu.Unlock()
		if flagged {
			fmt.Printf("P2PHard: PID=%d now gets tunnels until %s: %s\n", pid, doc.FlaggedUntil.Format(time.RFC3339), doc.Reason)
		}
		if p2pHistCol != nil {
			p2pHistCol.ReplaceOne(context.Background(), bson.D{{Key: "pid", Value: pid}}, doc, options.Replace().SetUpsert(true))
		}
	}
}
