// Package secrets resolves a deployment's credential reference into the secret
// it names.
//
// # Why this is not a snapshot field
//
// A snapshot is replicated to every worker, cached in memory, versioned, and
// content-addressed for debugging. That is the correct design for
// configuration and exactly the wrong place for a credential: it multiplies
// every secret across the fleet and preserves it in artifacts that outlive the
// rotation. So a deployment carries a *reference*, and workers resolve it
// separately.
//
// # Why this is a control-plane port, not a fifth data-plane port
//
// The reference design caps the data-plane extension surface at four ports and
// is right to: each one is a compatibility contract maintained forever. But it
// leaves secrets homeless. Resolution is asynchronous, cached, and off the
// per-request critical path once warm — the same shape as the trainer and eval
// ports — so it belongs in that set instead.
package secrets

import (
	"context"
	"sync"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// Store is the SecretsPort: it turns a reference into a secret.
//
// Implementations are the swappable part — environment variables in
// development, a Kubernetes projected volume, Vault, a cloud KMS. None of them
// appear in the request path's imports.
type Store interface {
	// Fetch returns the secret named by ref. It returns an error carrying
	// CodeUnavailable when the backing store is unreachable, so that callers
	// can distinguish "this secret does not exist" from "ask again later".
	Fetch(ctx context.Context, ref string) ([]byte, error)
}

// DefaultTTL is how long a resolved credential is reused.
//
// Short enough that a rotation takes effect without a redeploy, long enough
// that the store is not in the hot path. Rotation correctness does not depend
// on it: a rotated credential's predecessor stays valid for its overlap window,
// which is longer than this by design.
const DefaultTTL = 5 * time.Minute

// Resolver caches resolved credentials in front of a Store.
//
// The reference plan says workers "fetch by reference at startup". That is
// wrong for a live system: tenants add and rotate credentials while workers are
// running, and a worker's lifetime is measured in days. Lazy resolution with a
// TTL means a new deployment works without a restart and a rotated secret takes
// effect within one TTL.
//
// Safe for concurrent use.
type Resolver struct {
	store Store
	ttl   time.Duration
	now   func() time.Time

	mu     sync.Mutex
	cached map[string]entry
}

type entry struct {
	secret    []byte
	expiresAt time.Time
}

// Option configures a Resolver.
type Option func(*Resolver)

// WithTTL overrides how long a resolved credential is reused.
func WithTTL(ttl time.Duration) Option {
	return func(r *Resolver) { r.ttl = ttl }
}

// WithClock replaces the time source, for tests.
func WithClock(now func() time.Time) Option {
	return func(r *Resolver) { r.now = now }
}

// NewResolver wraps a store with a TTL cache.
func NewResolver(store Store, opts ...Option) (*Resolver, error) {
	if store == nil {
		return nil, core.New(core.CodeInternal, "a credential resolver needs a store")
	}
	r := &Resolver{store: store, ttl: DefaultTTL, now: time.Now, cached: map[string]entry{}}
	for _, opt := range opts {
		opt(r)
	}
	if r.ttl <= 0 {
		return nil, core.New(core.CodeInternal, "credential cache TTL must be positive")
	}
	return r, nil
}

// Resolve returns the credential a deployment names.
//
// An empty reference resolves to an empty credential, which is how self-hosted
// deployments that need no authentication are expressed.
func (r *Resolver) Resolve(ctx context.Context, ref string) (core.Credential, error) {
	if ref == "" {
		return core.Credential{}, nil
	}

	if secret, ok := r.lookup(ref); ok {
		return core.Credential{Ref: ref, Secret: secret}, nil
	}

	// Deliberately not holding the mutex across the fetch. Two workers racing
	// on the same cold reference will both fetch, which costs one redundant
	// call; holding the lock instead would stall every other credential
	// resolution behind one slow store.
	secret, err := r.store.Fetch(ctx, ref)
	if err != nil {
		return core.Credential{}, err
	}

	r.mu.Lock()
	r.cached[ref] = entry{secret: secret, expiresAt: r.now().Add(r.ttl)}
	r.mu.Unlock()

	return core.Credential{Ref: ref, Secret: secret}, nil
}

func (r *Resolver) lookup(ref string) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.cached[ref]
	if !ok || !r.now().Before(e.expiresAt) {
		return nil, false
	}
	return e.secret, true
}

// Invalidate drops a cached credential, so the next Resolve refetches.
//
// The snapshot subscriber calls this when a deployment's credential reference
// changes, which turns a rotation from "effective within a TTL" into
// "effective at the next snapshot".
func (r *Resolver) Invalidate(ref string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cached, ref)
}

// InvalidateAll drops every cached credential.
func (r *Resolver) InvalidateAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	clear(r.cached)
}
