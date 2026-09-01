// Package gateway is the request path: authenticate, admit, route, adapt.
//
// It is deliberately transport-free. Nothing here knows about HTTP status
// codes, headers or JSON envelopes — it takes a request and a snapshot and
// returns a result or a core.Error. That is what lets the whole pipeline be
// tested without a server, and what would let a second transport (gRPC, a
// queue consumer) reuse it unchanged.
package gateway

import (
	"context"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// ProviderRegistry resolves a deployment's provider name to an implementation.
//
// An interface rather than a map so that the component registry can replace it
// later with something that loads providers per tenant from the snapshot,
// without the pipeline changing.
type ProviderRegistry interface {
	Provider(name string) (core.ProviderPort, bool)
}

// CredentialResolver turns a deployment's credential reference into a secret.
//
// Credentials are resolved here rather than carried in the snapshot, because a
// snapshot is replicated to every worker, cached and versioned — exactly the
// wrong place for a secret. Implementations are expected to cache with a TTL
// and to be safe for concurrent use.
type CredentialResolver interface {
	Resolve(ctx context.Context, ref string) (core.Credential, error)
}

// Request is one inbound call, transport-independent.
type Request struct {
	Meta core.RequestMeta
	// APIKey is the presented credential, exactly as the caller sent it.
	APIKey string
	Body   []byte
}

// Result is a completed call plus everything accounting and logging need.
//
// The stage outputs are returned even on failure where they are known, because
// a request rejected at admission still has a principal worth logging, and a
// request that failed upstream still has a deployment worth attributing.
type Result struct {
	StatusCode int
	Body       []byte

	Principal  core.Principal
	Deployment core.DeploymentID
	Route      core.RoutingKey
	Usage      core.TokenUsage
	Latency    time.Duration
}

// Pipeline runs the four stages. It is safe for concurrent use and holds no
// per-request state.
type Pipeline struct {
	providers   ProviderRegistry
	credentials CredentialResolver
	// pepper keys the HMAC that turns a presented key secret into the value a
	// snapshot indexes principals by. It never enters a snapshot, so a stolen
	// snapshot yields no usable key.
	pepper []byte
	now    func() time.Time
}

// Option configures a Pipeline.
type Option func(*Pipeline)

// WithClock replaces the time source, for tests that need a fixed now.
func WithClock(now func() time.Time) Option {
	return func(p *Pipeline) { p.now = now }
}

// New builds a Pipeline. Dependencies are arguments rather than constructed
// here, so a test can supply a fake provider and no credential store.
func New(providers ProviderRegistry, credentials CredentialResolver, pepper []byte, opts ...Option) (*Pipeline, error) {
	if providers == nil {
		return nil, core.New(core.CodeInternal, "the pipeline needs a provider registry")
	}
	if credentials == nil {
		return nil, core.New(core.CodeInternal, "the pipeline needs a credential resolver")
	}
	if len(pepper) == 0 {
		// Refusing to start beats starting with an empty pepper: every key
		// lookup would then be computable by anyone holding a snapshot.
		return nil, core.New(core.CodeInternal, "the pipeline needs a non-empty key pepper")
	}

	p := &Pipeline{providers: providers, credentials: credentials, pepper: pepper, now: time.Now}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

// Handle runs one request against one snapshot.
//
// The snapshot is an argument, not a field: the caller leases a version at
// ingress and holds it for the whole request, so every stage here sees the same
// configuration however many swaps happen meanwhile.
func (p *Pipeline) Handle(ctx context.Context, snap *core.Snapshot, req *Request) (*Result, error) {
	started := p.now()
	result := &Result{}

	principal, err := p.authenticate(snap, req)
	if err != nil {
		return result, err
	}
	result.Principal = principal

	if err := p.admit(snap, &principal, req); err != nil {
		return result, err
	}

	deployment, err := p.route(snap, &principal, req)
	if err != nil {
		return result, err
	}
	result.Deployment = deployment.ID
	result.Route = deployment.Key

	resp, err := p.adapt(ctx, deployment, req)
	result.Latency = p.now().Sub(started)
	if err != nil {
		return result, err
	}

	result.StatusCode = resp.StatusCode
	result.Body = resp.Body
	result.Usage = resp.Usage
	return result, nil
}

// authenticate resolves a presented key to a principal in three map probes:
// prefix to tenant, lookup to key id, key id to principal.
func (p *Pipeline) authenticate(snap *core.Snapshot, req *Request) (core.Principal, error) {
	prefix, secret, err := core.ParseAPIKey(req.APIKey)
	if err != nil {
		return core.Principal{}, err
	}

	tenant, ok := snap.TenantForPrefix(prefix)
	if !ok {
		// Deliberately the same error as an unknown key. Distinguishing them
		// would turn the prefix into an oracle for which tenants exist.
		return core.Principal{}, core.New(core.CodeUnauthenticated, "unknown api key")
	}

	principal, ok := snap.Principal(tenant, core.ComputeKeyLookup(p.pepper, secret))
	if !ok {
		return core.Principal{}, core.New(core.CodeUnauthenticated, "unknown api key")
	}
	if err := principal.Validate(p.now()); err != nil {
		return core.Principal{}, err
	}
	return principal, nil
}

// admit applies the checks that can be answered from the snapshot alone.
// Rate limiting needs Redis and arrives with the limiter; budgets are here
// because their state is folded into the snapshot by the accounting consumer.
func (p *Pipeline) admit(snap *core.Snapshot, principal *core.Principal, req *Request) error {
	if !principal.Models.Permits(req.Meta.Model) {
		return core.Newf(core.CodeForbidden, "this key may not call %q", req.Meta.Model)
	}
	if budget, denied := snap.DeniedBudget(principal); denied {
		return core.Newf(core.CodeBudgetExhausted, "budget %q is exhausted", budget.ID)
	}
	return nil
}

// route resolves the requested model to a deployment that may serve it.
//
// The minimum trust tier is computed once, before any candidate is considered,
// and every candidate is filtered against it. That ordering matters: the
// reference design transforms payloads after routing, so if execution could
// fall back from an internal deployment to an external one, an already-redacted
// payload would be wrong — or worse, an unredacted one would leave the network.
// Pinning the tier for the whole candidate set makes that impossible by
// construction rather than by care.
//
// Selection is first-fit here. Cost and latency objectives, health scoring and
// circuit breakers belong to the router and arrive with it; this stage's
// contract — an ordered, tier-filtered candidate list — does not change.
func (p *Pipeline) route(snap *core.Snapshot, principal *core.Principal, req *Request) (core.Deployment, error) {
	minTier := snap.MinTrustTier(principal.Tenant)
	if principal.MinTrustTier > minTier {
		minTier = principal.MinTrustTier
	}

	targets := snap.ResolveAlias(principal.Tenant, req.Meta.Model)

	var sawAny, sawServing bool
	for _, target := range targets {
		for _, d := range snap.Deployments(target) {
			sawAny = true
			if !d.Serving() {
				continue
			}
			sawServing = true
			if !d.TrustTier.AtLeast(minTier) {
				continue
			}
			if _, ok := p.providers.Provider(d.Provider); !ok {
				continue
			}
			return d, nil
		}
	}

	// The three outcomes are distinguished because they mean different things
	// to the caller: a typo, a capacity problem, and a policy refusal.
	switch {
	case !sawAny:
		return core.Deployment{}, core.Newf(core.CodeModelNotFound, "no model named %q", req.Meta.Model)
	case !sawServing:
		return core.Deployment{}, core.Newf(core.CodeNoCandidates, "no deployment of %q is serving traffic", req.Meta.Model)
	default:
		return core.Deployment{}, core.Newf(core.CodeTrustTierDenied,
			"no deployment of %q meets the required trust tier %s", req.Meta.Model, minTier)
	}
}

// adapt makes the upstream call.
func (p *Pipeline) adapt(ctx context.Context, d core.Deployment, req *Request) (*core.ProviderResponse, error) {
	provider, ok := p.providers.Provider(d.Provider)
	if !ok {
		// route already checked this; reaching here means the registry changed
		// underneath us, which is a bug rather than a caller error.
		return nil, core.Newf(core.CodeInternal, "provider %q vanished between routing and execution", d.Provider)
	}

	credential, err := p.credentials.Resolve(ctx, d.CredentialRef)
	if err != nil {
		return nil, err
	}

	return provider.Invoke(ctx, &core.ProviderCall{
		Deployment: d,
		Meta:       req.Meta,
		Body:       req.Body,
		Credential: credential,
	})
}
