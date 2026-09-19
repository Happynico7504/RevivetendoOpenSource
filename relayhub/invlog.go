package relayhub

import (
	"crypto/rand"
	"encoding/hex"
	"sync"

	"github.com/Happynico7504/relaylink"
)

// InvalidationLog is the ordered stream relays poll to learn which cached
// entries to drop. Sequence numbers are only meaningful within one epoch: a
// hub restart starts a new epoch, and relays that see it flush everything.
type InvalidationLog struct {
	// OnAppend, if set, is called (outside the lock) for every new event so it
	// can be pushed to relays immediately.
	OnAppend func(epoch string, e relaylink.Event)

	mu     sync.Mutex
	epoch  string
	seq    int64
	events []relaylink.Event
	max    int
}

func NewInvalidationLog(max int) *InvalidationLog {
	if max <= 0 {
		max = 10000
	}
	b := make([]byte, 8)
	rand.Read(b)
	return &InvalidationLog{epoch: hex.EncodeToString(b), max: max}
}

// Append records an invalidation of every entry carrying any of tags.
func (l *InvalidationLog) Append(tags []string) int64 {
	if len(tags) == 0 {
		return l.Latest()
	}
	l.mu.Lock()
	l.seq++
	ev := relaylink.Event{Seq: l.seq, Tags: append([]string(nil), tags...)}
	l.events = append(l.events, ev)
	if len(l.events) > l.max {
		l.events = l.events[len(l.events)-l.max:]
	}
	epoch, hook := l.epoch, l.OnAppend
	l.mu.Unlock()
	if hook != nil {
		hook(epoch, ev)
	}
	return ev.Seq
}

func (l *InvalidationLog) Latest() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq
}

// Batch answers a relay that last saw sequence `after` in `epoch`.
func (l *InvalidationLog) Batch(after int64, epoch string) relaylink.InvalidationBatch {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := relaylink.InvalidationBatch{Epoch: l.epoch, Latest: l.seq}
	if epoch != l.epoch || after > l.seq {
		b.Reset = true
		return b
	}
	// The relay needs every event with Seq > after; if the oldest we still
	// hold is newer than after+1, some were trimmed: it must flush.
	if len(l.events) > 0 && after < l.events[0].Seq-1 {
		b.Reset = true
		return b
	}
	for _, e := range l.events {
		if e.Seq > after {
			b.Events = append(b.Events, e)
		}
	}
	return b
}
