package relaylink

import (
	"context"
	"encoding/hex"
	"sync"
	"time"
)

// MemoryReplay is an in-process ReplayStore, for tests and single-process use.
// The main should use a Redis-backed store so replays are caught across
// restarts.
type MemoryReplay struct {
	mu   sync.Mutex
	seen map[string]time.Time
	Now  func() time.Time
}

func (m *MemoryReplay) Seen(_ context.Context, relayID string, nonce []byte, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if m.Now != nil {
		now = m.Now()
	}
	if m.seen == nil {
		m.seen = map[string]time.Time{}
	}
	for k, exp := range m.seen {
		if now.After(exp) {
			delete(m.seen, k)
		}
	}
	k := relayID + "/" + hex.EncodeToString(nonce)
	if _, dup := m.seen[k]; dup {
		return true, nil
	}
	m.seen[k] = now.Add(ttl)
	return false, nil
}
