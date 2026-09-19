// Package nexauth runs the NEX authentication servers (WSC, MK8, Badge Arcade)
// on a relay. They are byte-for-byte the same servers the main runs, except
// that a PID's password comes from a local store the main fills (or, on a
// miss, from the main on demand) instead of from Mongo.
//
// It runs in a CHILD PROCESS of relayd, never inside it: nex-go panics inside
// its own goroutines when a socket closes (which nothing can recover) and cannot
// be reconfigured in place, so a NEX bug or a change of the main's Kerberos
// secrets must only ever cost the child, not the relay's HTTPS front.
package nexauth

import (
	"strconv"
	"sync"
	"time"
)

// Store holds the passwords the main has issued, until they expire.
type Store struct {
	Now func() time.Time

	mu sync.Mutex
	m  map[string]storeEntry
}

type storeEntry struct {
	password string
	expires  time.Time
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func key(game string, pid uint32) string { return game + "/" + strconv.FormatUint(uint64(pid), 10) }

// Put stores a password for ttl. A shorter or longer TTL replaces the old one.
func (s *Store) Put(game string, pid uint32, password string, ttl time.Duration) {
	if password == "" || ttl <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]storeEntry{}
	}
	now := s.now()
	if len(s.m) > 4096 { // bounded: sweep expired entries when it grows
		for k, e := range s.m {
			if !now.Before(e.expires) {
				delete(s.m, k)
			}
		}
	}
	s.m[key(game, pid)] = storeEntry{password: password, expires: now.Add(ttl)}
}

// Get returns a password that has not expired.
func (s *Store) Get(game string, pid uint32) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[key(game, pid)]
	if !ok {
		return "", false
	}
	if !s.now().Before(e.expires) {
		delete(s.m, key(game, pid))
		return "", false
	}
	return e.password, true
}

func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}
