package main

// Why did a console's connection end? A "Disconnect" from nex-go means the console sent a
// PRUDP DISCONNECT, or started a new connection from the same address; for relay-edge players the
// relay closed it (its journal says why). Either way the console decided - but it decides that
// too when the server's answers do not reach it, so each disconnect line also says what the
// server still had in flight to that console and whether its last call was answered.

import (
	"fmt"
	"sync"
	"time"

	nex "github.com/PretendoNetwork/nex-go"
)

type callNote struct {
	proto    uint8
	method   uint32
	at       time.Time
	answered time.Time
}

var (
	callNotesMu  sync.Mutex
	callNotes    = map[uint32]*callNote{}
	closedByEdge sync.Map // pid -> struct{}: the Disconnect being emitted comes from the edge
)

func noteCall(pid uint32, proto uint8, method uint32) {
	if pid == 0 {
		return
	}
	callNotesMu.Lock()
	callNotes[pid] = &callNote{proto: proto, method: method, at: time.Now()}
	callNotesMu.Unlock()
}

func noteAnswer(pid uint32) {
	callNotesMu.Lock()
	if n := callNotes[pid]; n != nil && n.answered.IsZero() {
		n.answered = time.Now()
	}
	callNotesMu.Unlock()
}

func disconnectDiagnosis(pid uint32, packet *nex.PacketV1) string {
	why := "console sent DISCONNECT"
	if _, ok := closedByEdge.Load(pid); ok {
		why = "the relay edge closed it, see its journal"
	} else if packet.Type() == nex.SynPacket {
		why = "console opened a new connection from the same address"
	}
	count, oldest, retries := packet.Sender().PendingStats()
	inflight := "nothing unacknowledged from us"
	if count > 0 {
		inflight = fmt.Sprintf("%d of our packets unacknowledged, oldest %v, resent up to %dx", count, oldest.Round(time.Millisecond), retries)
	}
	last := "no call seen"
	callNotesMu.Lock()
	if n := callNotes[pid]; n != nil {
		state := "unanswered"
		if !n.answered.IsZero() {
			state = fmt.Sprintf("answered after %v", n.answered.Sub(n.at).Round(time.Millisecond))
		}
		last = fmt.Sprintf("last call proto=%#x method=%#x %v ago, %s", n.proto, n.method, time.Since(n.at).Round(time.Second), state)
		delete(callNotes, pid)
	}
	callNotesMu.Unlock()
	return why + "; " + inflight + "; " + last
}
