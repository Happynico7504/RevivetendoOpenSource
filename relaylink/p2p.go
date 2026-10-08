package relaylink

// P2P tunnel: a UDP relay for consoles that cannot reach each other directly (carrier-grade
// NAT, or simply a long international path the hole-punch keeps losing).
//
// Every station (console) in a tunnel session gets its own UDP port on the tunnel host: its
// alias. The other consoles are told that the station lives at host:alias, so all their
// packets for it arrive there. A packet from station X arriving at Y's alias is sent on to Y
// FROM X's alias, so Y sees X at exactly the address it was told about and its answers come
// back through the tunnel too. Nothing in the packets is touched.
//
// The tunnel learns where each console really is from what it receives, like TURN: a console
// behind a symmetric NAT reaches the tunnel from a different port than it reached the game
// server from, and each alias it talks to may even see a different port. Packets for a
// console go to the address it last used toward the sending alias, else to the last address
// it used at all, else to the address the game server announced.

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	MethodP2POpen  = "p2p.open"  // hub -> relay call: P2POpen -> P2POpenResult (open, or add stations)
	MethodP2PClose = "p2p.close" // hub -> relay call: P2PClose
)

// P2PStation is one console as the game server knows it.
type P2PStation struct {
	PID  uint32 `json:"pid"`
	IP   string `json:"ip"`
	Port int    `json:"port"`
	Hard string `json:"hard,omitempty"` // why the game server thinks it is hard to reach directly (policy input only)
}

// P2POpen opens a tunnel session, or adds stations to an open one (same Key).
type P2POpen struct {
	Key      string       `json:"key"`
	IP       string       `json:"ip,omitempty"` // the tunnel host's public IPv4 (needed to rewrite in-band addresses)
	Stations []P2PStation `json:"stations"`
}

// P2POpenResult maps each station's PID to its alias port.
type P2POpenResult struct {
	Ports map[uint32]int `json:"ports"`
}

type P2PClose struct {
	Key string `json:"key"`
}

// P2PConfig configures a tunnel host. Zero values pick the defaults.
type P2PConfig struct {
	ListenIP     string        // address the alias sockets bind to (default all)
	PortMin      int           // alias port range (default 61000-61999)
	PortMax      int           //
	OpenGrace    time.Duration // how long a session may wait for its first packet (default 2m)
	IdleTimeout  time.Duration // a session that forwarded nothing for this long is closed (default 90s)
	MaxLifetime  time.Duration // hard cap on a session's age (default 4h)
	MaxSessions  int           // default 200
	MaxStations  int           // per session (default 8)
	TracePackets int           // log the first N forwarded packets of each session in hex (0 = off)
	Logf         func(string, ...any)
}

var (
	ErrP2PFull     = errors.New("p2p: tunnel host is full")
	ErrP2PBadInput = errors.New("p2p: bad station list")
)

// P2PTunnels is a tunnel host: the main and every relay that offers tunnels run one.
type P2PTunnels struct {
	cfg P2PConfig

	mu       sync.Mutex
	sessions map[string]*p2pSession
	ports    map[int]bool
	closed   bool
	now      func() time.Time
}

type p2pSession struct {
	key      string
	ip       net.IP // the tunnel host's public IPv4 as the consoles see it (nil = no in-band rewriting)
	rewrites uint64 // station addresses rewritten in PIA packets
	created  time.Time
	lastFwd  time.Time // last forwarded packet (zero until the first)
	stations map[uint32]*p2pStation
	packets  uint64
	bytes    uint64
	dropped  uint64
}

type p2pStation struct {
	pid    uint32
	ip     net.IP
	port   int // as the game server announced it
	alias  int
	conn   *net.UDPConn
	last   *net.UDPAddr         // last source address seen from this console
	toward map[int]*net.UDPAddr // alias port it sent to -> its source address for that alias
	sent   uint64               // packets this console sent into the tunnel
	got    uint64               // packets the tunnel sent to this console
}

func NewP2PTunnels(cfg P2PConfig) *P2PTunnels {
	if cfg.PortMin <= 0 || cfg.PortMax < cfg.PortMin {
		cfg.PortMin, cfg.PortMax = 61000, 61999
	}
	if cfg.OpenGrace <= 0 {
		cfg.OpenGrace = 2 * time.Minute
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 90 * time.Second
	}
	if cfg.MaxLifetime <= 0 {
		cfg.MaxLifetime = 4 * time.Hour
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = 200
	}
	if cfg.MaxStations <= 0 {
		cfg.MaxStations = 8
	}
	return &P2PTunnels{cfg: cfg, sessions: map[string]*p2pSession{}, ports: map[int]bool{}, now: time.Now}
}

func (t *P2PTunnels) logf(f string, a ...any) {
	if t.cfg.Logf != nil {
		t.cfg.Logf(f, a...)
	}
}

// Open creates the session (or adds the stations it does not have yet) and returns every
// station's alias port. Opening an existing session again is harmless.
func (t *P2PTunnels) Open(key, publicIP string, stations []P2PStation) (map[uint32]int, error) {
	if key == "" || len(stations) == 0 {
		return nil, ErrP2PBadInput
	}
	for _, s := range stations {
		if s.PID == 0 || net.ParseIP(s.IP) == nil {
			return nil, ErrP2PBadInput
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, errors.New("p2p: shut down")
	}
	sess := t.sessions[key]
	created := false
	if sess == nil {
		if len(t.sessions) >= t.cfg.MaxSessions {
			return nil, ErrP2PFull
		}
		sess = &p2pSession{key: key, created: t.now(), stations: map[uint32]*p2pStation{}}
		if ip := net.ParseIP(publicIP).To4(); ip != nil {
			sess.ip = ip
		}
		t.sessions[key] = sess
		created = true
	}
	var added []string
	for _, s := range stations {
		if st, ok := sess.stations[s.PID]; ok {
			// The game server may know a newer address (the console reconnected).
			st.ip, st.port = net.ParseIP(s.IP), s.Port
			continue
		}
		if len(sess.stations) >= t.cfg.MaxStations {
			break
		}
		conn, port, err := t.bindLocked()
		if err != nil {
			if created && len(sess.stations) == 0 {
				delete(t.sessions, key)
			}
			return nil, err
		}
		st := &p2pStation{pid: s.PID, ip: net.ParseIP(s.IP), port: s.Port, alias: port, conn: conn,
			toward: map[int]*net.UDPAddr{}}
		sess.stations[s.PID] = st
		added = append(added, fmt.Sprintf("%d=%s:%d->:%d", s.PID, s.IP, s.Port, port))
		go t.serve(sess, st)
	}
	if len(added) > 0 {
		t.logf("p2p: session %s: %v", key, added)
	}
	out := map[uint32]int{}
	for pid, st := range sess.stations {
		out[pid] = st.alias
	}
	return out, nil
}

// bindLocked opens a UDP socket on a free alias port, trying random ports of the range.
func (t *P2PTunnels) bindLocked() (*net.UDPConn, int, error) {
	n := t.cfg.PortMax - t.cfg.PortMin + 1
	start := rand.Intn(n)
	for i := 0; i < n; i++ {
		port := t.cfg.PortMin + (start+i)%n
		if t.ports[port] {
			continue
		}
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(t.cfg.ListenIP), Port: port})
		if err != nil {
			continue // taken by something else
		}
		t.ports[port] = true
		return conn, port, nil
	}
	return nil, 0, ErrP2PFull
}

// Close ends a session and frees its ports.
func (t *P2PTunnels) Close(key string) {
	t.mu.Lock()
	sess := t.sessions[key]
	if sess != nil {
		t.closeLocked(sess, "closed")
	}
	t.mu.Unlock()
}

func (t *P2PTunnels) closeLocked(sess *p2pSession, why string) {
	delete(t.sessions, sess.key)
	for _, st := range sess.stations {
		st.conn.Close()
		delete(t.ports, st.alias)
	}
	var per []string
	for pid, st := range sess.stations {
		per = append(per, fmt.Sprintf("%d sent %d got %d (last seen at %v)", pid, st.sent, st.got, st.last))
	}
	sort.Strings(per)
	t.logf("p2p: session %s %s after %v: %d packets, %d bytes forwarded, %d dropped, %d in-band addresses rewritten; %s",
		sess.key, why, t.now().Sub(sess.created).Round(time.Second), sess.packets, sess.bytes, sess.dropped, sess.rewrites, strings.Join(per, "; "))
}

// Run closes idle and expired sessions until stop is closed, then shuts everything down.
func (t *P2PTunnels) Run(stop <-chan struct{}) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			t.mu.Lock()
			t.closed = true
			for _, s := range t.sessions {
				t.closeLocked(s, "shut down")
			}
			t.mu.Unlock()
			return
		case <-tick.C:
			t.Sweep()
		}
	}
}

// Sweep closes the sessions that are idle or too old.
func (t *P2PTunnels) Sweep() {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for _, s := range t.sessions {
		switch {
		case now.Sub(s.created) > t.cfg.MaxLifetime:
			t.closeLocked(s, "expired")
		case s.lastFwd.IsZero() && now.Sub(s.created) > t.cfg.OpenGrace:
			t.closeLocked(s, "never used")
		case !s.lastFwd.IsZero() && now.Sub(s.lastFwd) > t.cfg.IdleTimeout:
			t.closeLocked(s, "idle")
		}
	}
}

// P2PSessionStatus is one session as Status reports it.
type P2PSessionStatus struct {
	Key     string            `json:"key"`
	Age     time.Duration     `json:"age"`
	Idle    time.Duration     `json:"idle"`
	Ports   map[uint32]int    `json:"ports"`
	Packets uint64            `json:"packets"`
	Bytes   uint64            `json:"bytes"`
	Dropped uint64            `json:"dropped"`
	Learned map[uint32]string `json:"learned"` // where each console was last heard from
}

func (t *P2PTunnels) Status() []P2PSessionStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	var out []P2PSessionStatus
	for _, s := range t.sessions {
		st := P2PSessionStatus{Key: s.key, Age: now.Sub(s.created), Ports: map[uint32]int{}, Learned: map[uint32]string{},
			Packets: s.packets, Bytes: s.bytes, Dropped: s.dropped}
		if !s.lastFwd.IsZero() {
			st.Idle = now.Sub(s.lastFwd)
		}
		for pid, x := range s.stations {
			st.Ports[pid] = x.alias
			if x.last != nil {
				st.Learned[pid] = x.last.String()
			}
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// serve forwards everything that arrives at one station's alias.
func (t *P2PTunnels) serve(sess *p2pSession, dst *p2pStation) {
	buf := make([]byte, 2048)
	for {
		n, src, err := dst.conn.ReadFromUDP(buf)
		if err != nil {
			return // closed
		}
		t.mu.Lock()
		from := sess.identify(src, dst)
		if from == nil {
			sess.dropped++
			drops := sess.dropped
			t.mu.Unlock()
			if drops <= 5 || drops%100 == 0 {
				t.logf("p2p: session %s: dropped a packet from %v to the alias of %d (not a station of this session; %d dropped so far)", sess.key, src, dst.pid, drops)
			}
			continue
		}
		from.last = src
		from.toward[dst.alias] = src
		to := dst.target(from.alias)
		out := from.conn
		sess.packets++
		sess.bytes += uint64(n)
		sess.lastFwd = t.now()
		from.sent++
		dst.got++
		rewrote := 0
		if sess.ip != nil {
			rewrote = rewritePIA(buf[:n], sess.aliasesLocked(), from.pid, dst.pid)
			sess.rewrites += uint64(rewrote)
		}
		rewrites := sess.rewrites
		trace := t.cfg.TracePackets > 0 && sess.packets <= uint64(t.cfg.TracePackets)
		seq := sess.packets
		t.mu.Unlock()
		if rewrote > 0 && rewrites <= 20 {
			t.logf("p2p: session %s: rewrote %d station address(es) in a PIA packet %d -> %d", sess.key, rewrote, from.pid, dst.pid)
		}
		if trace {
			t.logf("p2p: trace %s #%d %d(%v) -> %d(%v) via :%d->:%d %dB %x", sess.key, seq, from.pid, src, dst.pid, to, dst.alias, from.alias, n, buf[:n])
		}
		out.WriteToUDP(buf[:n], to)
	}
}

// identify finds the station a packet came from. A known address is trusted; otherwise the
// sender's IP must belong to exactly one other station (its NAT picked a new port), or, if
// no IP matches (a NAT pool that changes the public IP per destination), the only other
// station that has not been heard from yet.
func (s *p2pSession) identify(src *net.UDPAddr, dst *p2pStation) *p2pStation {
	for _, st := range s.stations {
		if st == dst {
			continue
		}
		if st.last != nil && st.last.IP.Equal(src.IP) && st.last.Port == src.Port {
			return st
		}
		for _, a := range st.toward {
			if a.IP.Equal(src.IP) && a.Port == src.Port {
				return st
			}
		}
	}
	var byIP, unheard []*p2pStation
	for _, st := range s.stations {
		if st == dst {
			continue
		}
		if st.ip.Equal(src.IP) || (st.last != nil && st.last.IP.Equal(src.IP)) {
			byIP = append(byIP, st)
		}
		if st.last == nil {
			unheard = append(unheard, st)
		}
	}
	if len(byIP) == 1 {
		return byIP[0]
	}
	if len(byIP) == 0 && len(unheard) == 1 {
		return unheard[0]
	}
	return nil
}

// target is where a packet for this station goes when it comes from the given alias.
func (st *p2pStation) target(fromAlias int) *net.UDPAddr {
	if a := st.toward[fromAlias]; a != nil {
		return a
	}
	if st.last != nil {
		return st.last
	}
	return &net.UDPAddr{IP: st.ip, Port: st.port}
}

// ---- in-band station addresses (PIA) ------------------------------------------------------
//
// Getting the consoles to send to the tunnel is not enough for matches of three or more: PIA,
// Nintendo's P2P library, tells every member the others' locations itself (the host's station
// list), and those are the consoles' real public addresses. Two joiners then try to reach each
// other directly - the very thing the tunnel exists to avoid. Captured 2026-10-08: the station
// list is plaintext, one location per member as
//
//	public IPv4 (4) | port (2, big endian) | 00 00 | PID (4, big endian) | connection id (8) | flags...
//
// followed by the same for the private (LAN) address, and every packet ends in a 16-byte
// HMAC-MD5 over the rest, keyed with the matchmake session key - which this network hands out
// empty. So the tunnel can replace a member's public location with its alias here and sign the
// packet again. Packets that do not verify with the empty key are left alone.

var piaMagic = []byte{0x32, 0xab, 0x98, 0x64}

type piaAlias struct {
	ip   [4]byte
	port uint16
}

func (s *p2pSession) aliasesLocked() map[uint32]piaAlias {
	m := make(map[uint32]piaAlias, len(s.stations))
	for pid, st := range s.stations {
		var a piaAlias
		copy(a.ip[:], s.ip)
		a.port = uint16(st.alias)
		m[pid] = a
	}
	return m
}

func piaSigned(pkt []byte) bool {
	if len(pkt) < len(piaMagic)+16 || !bytes.Equal(pkt[:4], piaMagic) {
		return false
	}
	mac := hmac.New(md5.New, nil)
	mac.Write(pkt[:len(pkt)-16])
	return hmac.Equal(mac.Sum(nil), pkt[len(pkt)-16:])
}

func lanAddress(ip []byte) bool {
	return ip[0] == 10 || ip[0] == 127 || ip[0] == 0 || (ip[0] == 172 && ip[1]&0xf0 == 16) || (ip[0] == 192 && ip[1] == 168)
}

// rewritePIA replaces, in place, the public locations of other members (third parties) with
// their aliases and re-signs the packet. It returns how many locations it changed.
//
// The sender's and the receiver's own locations are never changed. A console recognises itself
// in a station list by its real location, and the connection handshake between two consoles
// carries each one's own location next to a value apparently derived from it: rewriting those
// broke 2-player joins that worked without rewriting (2026-10-09, sessions 11009 and 233736).
// The 3+ player mesh only needs the third parties: the host telling joiner B where joiner A is.
func rewritePIA(pkt []byte, aliases map[uint32]piaAlias, keep ...uint32) int {
	if !piaSigned(pkt) {
		return 0
	}
	end := len(pkt) - 16
	n := 0
	for i := 4; i+12 <= end; i++ {
		if pkt[i+6] != 0 || pkt[i+7] != 0 {
			continue
		}
		pid := binary.BigEndian.Uint32(pkt[i+8 : i+12])
		a, ok := aliases[pid]
		if !ok || slices.Contains(keep, pid) || lanAddress(pkt[i:i+4]) {
			continue
		}
		if bytes.Equal(pkt[i:i+4], a.ip[:]) && binary.BigEndian.Uint16(pkt[i+4:i+6]) == a.port {
			i += 11
			continue // already the alias
		}
		copy(pkt[i:i+4], a.ip[:])
		binary.BigEndian.PutUint16(pkt[i+4:i+6], a.port)
		n++
		i += 11
	}
	if n > 0 {
		mac := hmac.New(md5.New, nil)
		mac.Write(pkt[:end])
		copy(pkt[end:], mac.Sum(nil))
	}
	return n
}
