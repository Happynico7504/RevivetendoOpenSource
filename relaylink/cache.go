package relaylink

import (
	"sync"
	"time"
)

// Store is where a relay keeps cached responses. MemoryStore is the reference
// implementation; a relay can back this with its local Redis instead.
type Store interface {
	Get(key string) (*Response, bool)
	Set(key string, r *Response, ttl time.Duration)
	DeleteTag(tag string)
	Flush()
}

type memItem struct {
	resp *Response
	exp  time.Time
}

// MemoryStore is an in-process Store with per-entry expiry, a tag index and a
// size cap.
type MemoryStore struct {
	Max int              // max entries; 0 = 10000
	Now func() time.Time // test hook

	mu    sync.Mutex
	items map[string]*memItem
	tags  map[string]map[string]struct{} // tag -> keys
}

func (m *MemoryStore) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *MemoryStore) init() {
	if m.items == nil {
		m.items = map[string]*memItem{}
		m.tags = map[string]map[string]struct{}{}
	}
}

func (m *MemoryStore) removeLocked(key string) {
	it, ok := m.items[key]
	if !ok {
		return
	}
	for _, t := range it.resp.Tags {
		if set := m.tags[t]; set != nil {
			delete(set, key)
			if len(set) == 0 {
				delete(m.tags, t)
			}
		}
	}
	delete(m.items, key)
}

func (m *MemoryStore) Get(key string) (*Response, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	it, ok := m.items[key]
	if !ok {
		return nil, false
	}
	if !m.now().Before(it.exp) {
		m.removeLocked(key)
		return nil, false
	}
	cp := *it.resp
	return &cp, true
}

func (m *MemoryStore) Set(key string, r *Response, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.removeLocked(key)
	max := m.Max
	if max == 0 {
		max = 10000
	}
	if len(m.items) >= max {
		now := m.now()
		for k, it := range m.items { // expired first
			if !now.Before(it.exp) {
				m.removeLocked(k)
			}
		}
		for k := range m.items { // still full: evict arbitrary entries
			if len(m.items) < max {
				break
			}
			m.removeLocked(k)
		}
	}
	cp := *r
	m.items[key] = &memItem{resp: &cp, exp: m.now().Add(ttl)}
	for _, t := range cp.Tags {
		if m.tags[t] == nil {
			m.tags[t] = map[string]struct{}{}
		}
		m.tags[t][key] = struct{}{}
	}
}

func (m *MemoryStore) DeleteTag(tag string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	for key := range m.tags[tag] {
		m.removeLocked(key)
	}
}

func (m *MemoryStore) Flush() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items = map[string]*memItem{}
	m.tags = map[string]map[string]struct{}{}
}

func (m *MemoryStore) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.items)
}
