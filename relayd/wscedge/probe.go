package wscedge

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/Happynico7504/relaylink"
)

// ProbeEvery is how often each connected player is measured. Same cadence as the main's
// direct ping (wsc-secure pingPlayerConnectivity).
var ProbeEvery = 30 * time.Second

// Pinger runs one ICMP probe and returns its raw output. A variable so tests do not need
// a real network or a ping binary.
var Pinger = func(ip string) string {
	// Our own ICMP first: under the relay's systemd hardening the ping binary is killed by the
	// syscall filter and prints nothing. The binary remains the fallback (IPv6, other OSes).
	if s, ok := icmpSummary(ip, 5, 250*time.Millisecond, 2*time.Second); ok {
		return s
	}
	out, _ := exec.Command("ping", "-c", "5", "-W", "2", ip).CombinedOutput()
	return string(out)
}

// Tracer runs a traceroute toward ip (a variable so tests need no network). Same options as
// the main's: 15 hops max, 1 s per probe, one probe per hop.
var Tracer = func(ip string) string {
	out, _ := exec.Command("traceroute", "-m", "15", "-w", "1", "-q", "1", ip).CombinedOutput()
	return string(out)
}

// TraceCooldown is the least time between two loss-triggered traceroutes toward one player,
// so persistent loss does not run one every cycle.
var TraceCooldown = 5 * time.Minute

// traceAsync runs a traceroute in the background and reports it.
func (e *Edge) traceAsync(pid uint32, ip, reason string) {
	go func() {
		e.backend.Trace(relaylink.WSCTrace{PID: pid, IP: ip, Reason: reason, Output: Tracer(ip)})
	}()
}

// probeLoop measures every connected player from here, the host that terminates their UDP
// session, so the numbers describe the path the player actually uses.
func (e *Edge) probeLoop() {
	for range time.Tick(ProbeEvery) {
		e.probeOnce()
	}
}

// probeOnce measures every connected player, reports it, and chases real loss with the
// heavier traceroute at most once per cooldown per player. "?" (nothing to parse) and 0% are
// not loss.
func (e *Edge) probeOnce() {
	for _, s := range e.probeAll() {
		e.backend.Stats(s)
		if s.Loss != "0%" && s.Loss != "?" && e.traceDue(s.PID) {
			e.traceAsync(s.PID, s.IP, "loss="+s.Loss)
		}
	}
}

// probeAll takes one measurement of every connected player.
func (e *Edge) probeAll() []relaylink.WSCStats {
	type target struct {
		pid uint32
		ip  string
		rtt func() (time.Duration, time.Duration, int)
	}
	e.mu.Lock()
	var targets []target
	for pid, c := range e.clients {
		if addr := c.Address(); addr != nil && addr.IP != nil {
			targets = append(targets, target{pid, addr.IP.String(), c.RTT})
		}
	}
	e.mu.Unlock()

	out := make([]relaylink.WSCStats, len(targets))
	done := make(chan struct{}, len(targets))
	for i, t := range targets {
		go func(i int, t target) {
			loss, avg := parsePingOutput(Pinger(t.ip))
			sm, min, n := t.rtt()
			out[i] = relaylink.WSCStats{
				PID: t.pid, IP: t.ip, Loss: loss, AvgRTT: avg,
				PRUDPRTTMs: float64(sm) / float64(time.Millisecond),
				PRUDPMinMs: float64(min) / float64(time.Millisecond),
				Samples:    n,
			}
			done <- struct{}{}
		}(i, t)
	}
	for range targets {
		<-done
	}
	return out
}

// parsePingOutput pulls the packet-loss percentage and average RTT out of `ping`'s summary
// (the same parser wsc-secure uses): "?" for a field that is not there (100% loss has no rtt
// line).
func parsePingOutput(out string) (lossPct string, avgRTT string) {
	lossPct, avgRTT = "?", "?"
	for _, line := range strings.Split(out, "\n") {
		if idx := strings.Index(line, "% packet loss"); idx > 0 {
			start := idx
			for start > 0 && (line[start-1] == '.' || (line[start-1] >= '0' && line[start-1] <= '9')) {
				start--
			}
			lossPct = line[start:idx] + "%"
		}
		if strings.Contains(line, "min/avg/max") {
			if eq := strings.LastIndex(line, "="); eq >= 0 {
				fields := strings.Split(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line[eq+1:]), "ms")), "/")
				if len(fields) >= 2 {
					avgRTT = fields[1] + "ms"
				}
			}
		}
	}
	return
}

// String renders stats the way the main logs them.
func FormatStats(s relaylink.WSCStats, via string) (ping, rtt string) {
	ping = fmt.Sprintf("PlayerPing: PID=%d ip=%s loss=%s avgRTT=%s via=%s", s.PID, s.IP, s.Loss, s.AvgRTT, via)
	if s.Samples == 0 {
		rtt = fmt.Sprintf("PlayerRTT: PID=%d ip=%s via=%s prudp=? samples=0", s.PID, s.IP, via)
	} else {
		rtt = fmt.Sprintf("PlayerRTT: PID=%d ip=%s via=%s prudp=%.1fms min=%.1fms samples=%d", s.PID, s.IP, via, s.PRUDPRTTMs, s.PRUDPMinMs, s.Samples)
	}
	return
}

// traceDue reports whether a loss-triggered traceroute toward pid is allowed now, and if so
// starts the cooldown.
func (e *Edge) traceDue(pid uint32) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastTrace == nil {
		e.lastTrace = map[uint32]time.Time{}
	}
	if t, ok := e.lastTrace[pid]; ok && time.Since(t) < TraceCooldown {
		return false
	}
	e.lastTrace[pid] = time.Now()
	return true
}
