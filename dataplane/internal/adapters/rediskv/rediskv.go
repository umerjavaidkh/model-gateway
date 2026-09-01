// Package rediskv implements KVStore over Redis, which is what makes rate
// limits fleet-wide rather than per worker.
//
// Nothing here is durable by design. Redis holds rate-limit windows and, later,
// the PII token vault; both are ephemeral by nature. Losing Redis costs
// accuracy for one window, not correctness — which is why the limiter fails
// open when this store is unreachable rather than refusing traffic.
package rediskv

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// DefaultTimeout bounds a single command. Short, because this sits in the
// request path and the correct response to a slow Redis is to fail open
// quickly, not to hold the request while it recovers.
const DefaultTimeout = 250 * time.Millisecond

// incrWithTTL increments a counter and sets its expiry only if it has none.
//
// The two operations have to be atomic, and the TTL has to be conditional.
// Setting it unconditionally would extend the expiry on every increment, so a
// continuously busy key would never expire and its rate-limit window would
// never roll — meaning the busiest principal, the one the limit exists for,
// would be the one it stopped applying to.
//
// A pipeline is not enough: between INCRBY and a TTL check another client can
// interleave. A script is one round trip and is executed atomically.
var incrWithTTL = redis.NewScript(`
	local total = redis.call('INCRBY', KEYS[1], ARGV[1])
	if redis.call('PTTL', KEYS[1]) < 0 then
		redis.call('PEXPIRE', KEYS[1], ARGV[2])
	end
	return total
`)

// Store is a KVStore backed by Redis. Safe for concurrent use.
type Store struct {
	client  redis.UniversalClient
	timeout time.Duration
}

// Option configures a Store.
type Option func(*Store)

// WithTimeout bounds each command.
func WithTimeout(d time.Duration) Option {
	return func(s *Store) {
		if d > 0 {
			s.timeout = d
		}
	}
}

// New returns a store using an existing client.
func New(client redis.UniversalClient, opts ...Option) (*Store, error) {
	if client == nil {
		return nil, core.New(core.CodeInternal, "a redis store needs a client")
	}
	s := &Store{client: client, timeout: DefaultTimeout}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// Open parses a redis:// URL and returns a store over a pooled client.
func Open(url string, opts ...Option) (*Store, error) {
	options, err := redis.ParseURL(url)
	if err != nil {
		return nil, core.Wrap(core.CodeInvalidRequest, err, "parsing the redis URL")
	}
	return New(redis.NewClient(options), opts...)
}

// Close releases the connection pool.
func (s *Store) Close() error {
	if err := s.client.Close(); err != nil {
		return core.Wrap(core.CodeUnavailable, err, "closing the redis client")
	}
	return nil
}

// Get returns a value if present.
func (s *Store) Get(ctx context.Context, key string) ([]byte, bool, error) {
	ctx, cancel := s.bounded(ctx)
	defer cancel()

	value, err := s.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		// Absent is not an error. Making the caller distinguish a missing key
		// from a broken store by inspecting an error is how a fail-open path
		// ends up failing closed.
		return nil, false, nil
	}
	if err != nil {
		return nil, false, core.Wrap(core.CodeUnavailable, err, "reading from redis")
	}
	return value, true, nil
}

// Set stores a value with a TTL.
func (s *Store) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	ctx, cancel := s.bounded(ctx)
	defer cancel()

	if err := s.client.Set(ctx, key, value, ttl).Err(); err != nil {
		return core.Wrap(core.CodeUnavailable, err, "writing to redis")
	}
	return nil
}

// Incr adds to a counter and returns the total, setting the expiry only when
// the counter is new.
func (s *Store) Incr(
	ctx context.Context, key string, delta int64, ttl time.Duration,
) (int64, error) {
	ctx, cancel := s.bounded(ctx)
	defer cancel()

	total, err := incrWithTTL.Run(ctx, s.client, []string{key}, delta, ttl.Milliseconds()).Int64()
	if err != nil {
		return 0, core.Wrap(core.CodeUnavailable, err, "incrementing in redis")
	}
	return total, nil
}

// Delete removes a key. Deleting something absent is not an error.
func (s *Store) Delete(ctx context.Context, key string) error {
	ctx, cancel := s.bounded(ctx)
	defer cancel()

	if err := s.client.Del(ctx, key).Err(); err != nil {
		return core.Wrap(core.CodeUnavailable, err, "deleting from redis")
	}
	return nil
}

// Ping checks the connection, for a readiness probe at startup.
func (s *Store) Ping(ctx context.Context) error {
	ctx, cancel := s.bounded(ctx)
	defer cancel()

	if err := s.client.Ping(ctx).Err(); err != nil {
		return core.Wrap(core.CodeUnavailable, err, "pinging redis")
	}
	return nil
}

// bounded applies the command timeout, unless the caller's deadline is sooner.
//
// A request that is already nearly out of time should not wait the full budget
// on a rate-limit lookup; failing open early leaves what remains for the
// upstream call, which is the part the caller actually wanted.
func (s *Store) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.timeout)
}
