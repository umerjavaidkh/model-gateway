// Package memkv is an in-process KVStore.
//
// It makes rate limiting work on a single worker with no infrastructure, which
// is the right default for development and for a single-worker deployment. In a
// fleet it means limits are enforced per worker rather than globally: the
// ceiling becomes limit × workers. That is a real difference and the worker logs
// it at startup rather than letting an operator discover it from a graph.
//
// The Redis store that makes limits fleet-wide fills the same interface.
package memkv

import (
	"context"
	"sync"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// Store is a map with TTLs. Safe for concurrent use.
type Store struct {
	now func() time.Time

	mu      sync.Mutex
	entries map[string]entry
}

type entry struct {
	value     []byte
	expiresAt time.Time
}

// Option configures a Store.
type Option func(*Store)

// WithClock replaces the time source, for tests.
func WithClock(now func() time.Time) Option {
	return func(s *Store) {
		if now != nil {
			s.now = now
		}
	}
}

// New returns an empty store.
func New(opts ...Option) *Store {
	s := &Store{now: time.Now, entries: map[string]entry{}}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Get returns a value if present and unexpired.
func (s *Store) Get(_ context.Context, key string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[key]
	if !ok || s.expired(e) {
		return nil, false, nil
	}
	return e.value, true, nil
}

// Set stores a value with a TTL.
func (s *Store) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.entries[key] = entry{value: value, expiresAt: s.now().Add(ttl)}
	return nil
}

// Incr adds to a counter and returns the total, creating it if absent.
func (s *Store) Incr(_ context.Context, key string, delta int64, ttl time.Duration) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current := int64(0)
	if e, ok := s.entries[key]; ok && !s.expired(e) {
		parsed, err := parseInt(e.value)
		if err != nil {
			return 0, core.Wrap(core.CodeInternal, err, "counter holds a non-numeric value")
		}
		current = parsed
	}

	total := current + delta
	s.entries[key] = entry{value: formatInt(total), expiresAt: s.now().Add(ttl)}
	return total, nil
}

// Delete removes a key.
func (s *Store) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.entries, key)
	return nil
}

// Sweep removes expired entries.
//
// Lazy expiry on read is enough for correctness, but a key never read again
// would be held forever — and rate-limit keys are per window, so the set of
// stale keys grows with time rather than with traffic.
func (s *Store) Sweep() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	for key, e := range s.entries {
		if s.expired(e) {
			delete(s.entries, key)
			removed++
		}
	}
	return removed
}

// Len reports how many entries are held, for tests and metrics.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

func (s *Store) expired(e entry) bool {
	return !e.expiresAt.IsZero() && s.now().After(e.expiresAt)
}
