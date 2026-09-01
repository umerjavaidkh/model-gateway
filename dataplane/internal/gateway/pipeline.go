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
	"sync"
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
	// TimeToFirstByte is what a user perceives as latency on a streamed
	// response. It is zero for a non-streaming call, where it would be the
	// same as Latency and so says nothing.
	TimeToFirstByte time.Duration
}

// Pipeline runs the four stages. It is safe for concurrent use and holds no
// per-request state.
type Pipeline struct {
	providers   ProviderRegistry
	credentials CredentialResolver
	telemetry   core.TelemetryPort
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

// WithTelemetry sets where usage events go. Without it events are discarded,
// which is the right default for a unit test and the wrong one for a worker —
// so main always supplies it.
func WithTelemetry(t core.TelemetryPort) Option {
	return func(p *Pipeline) { p.telemetry = t }
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

	p := &Pipeline{
		providers:   providers,
		credentials: credentials,
		telemetry:   discardTelemetry{},
		pepper:      pepper,
		now:         time.Now,
	}
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

	// One usage event per request, on every exit path. A request rejected at
	// admission still consumed a slot worth counting, and a request that failed
	// upstream may already have burned tokens that a provider will bill for.
	// Emitting only on success is how a cost report quietly disagrees with an
	// invoice.
	var deployment core.Deployment
	emit := func(err error) {
		result.Latency = p.now().Sub(started)
		p.emitUsage(ctx, snap, req, result, deployment, false, err)
	}

	principal, err := p.authenticate(snap, req)
	if err != nil {
		emit(err)
		return result, err
	}
	result.Principal = principal

	if err := p.admit(snap, &principal, req); err != nil {
		emit(err)
		return result, err
	}

	deployment, err = p.route(snap, &principal, req)
	if err != nil {
		emit(err)
		return result, err
	}
	result.Deployment = deployment.ID
	result.Route = deployment.Key

	resp, err := p.adapt(ctx, deployment, req)
	if err != nil {
		emit(err)
		return result, err
	}

	result.StatusCode = resp.StatusCode
	result.Body = resp.Body
	result.Usage = resp.Usage
	emit(nil)
	return result, nil
}

// emitUsage builds and sends the record for one request.
//
// Cost is computed here rather than by a consumer because this is the only
// place that holds both the token counts and the deployment's price at the
// snapshot version that served the request. A consumer looking the price up
// later would use whatever the price is then, which silently re-bills history
// when a provider changes its rates.
func (p *Pipeline) emitUsage(ctx context.Context, snap *core.Snapshot, req *Request, result *Result, d core.Deployment, stream bool, err error) {
	outcome := core.CodeOf(err)
	if err == nil {
		outcome = ""
	}

	_ = p.telemetry.Emit(ctx, core.UsageEvent{
		RequestID:       req.Meta.RequestID,
		Timestamp:       p.now(),
		Tenant:          result.Principal.Tenant,
		KeyID:           result.Principal.KeyID,
		Tier:            snap.Tier(result.Principal.Tenant),
		Deployment:      result.Deployment,
		Route:           result.Route,
		Provider:        d.Provider,
		Stream:          stream,
		InputTokens:     result.Usage.Input,
		OutputTokens:    result.Usage.Output,
		CostMicroUSD:    d.Cost.For(result.Usage),
		LatencyMs:       result.Latency.Milliseconds(),
		TimeToFirstByte: result.TimeToFirstByte,
		Outcome:         outcome,
		SnapshotVersion: snap.GlobalVersion().Number,
	})
}

// discardTelemetry is the default: events go nowhere.
type discardTelemetry struct{}

// Name identifies the no-op implementation.
func (discardTelemetry) Name() string { return "discard" }

// Emit throws the events away.
func (discardTelemetry) Emit(context.Context, ...core.Event) error { return nil }

// HandleStream runs the same four stages and returns a cursor over the
// response instead of a completed body.
//
// It is a separate method rather than a flag on Handle because the caller's
// obligations differ: a stream must be closed, and its usage is only known once
// it has been drained. Hiding that behind a bool would make both leaks easy to
// write.
func (p *Pipeline) HandleStream(ctx context.Context, snap *core.Snapshot, req *Request) (*StreamResult, error) {
	started := p.now()
	result := &StreamResult{}

	emit := func(err error) {
		r := &Result{
			Principal:  result.Principal,
			Deployment: result.Deployment,
			Route:      result.Route,
			Latency:    p.now().Sub(started),
		}
		p.emitUsage(ctx, snap, req, r, result.deployment, true, err)
	}

	principal, err := p.authenticate(snap, req)
	if err != nil {
		emit(err)
		return result, err
	}
	result.Principal = principal

	if err := p.admit(snap, &principal, req); err != nil {
		emit(err)
		return result, err
	}

	deployment, err := p.route(snap, &principal, req)
	if err != nil {
		emit(err)
		return result, err
	}
	result.deployment = deployment
	result.Deployment = deployment.ID
	result.Route = deployment.Key

	provider, ok := p.providers.Provider(deployment.Provider)
	if !ok {
		err := core.Newf(core.CodeInternal, "provider %q vanished between routing and execution", deployment.Provider)
		emit(err)
		return result, err
	}
	credential, err := p.credentials.Resolve(ctx, deployment.CredentialRef)
	if err != nil {
		emit(err)
		return result, err
	}

	chunks, err := provider.Stream(ctx, &core.ProviderCall{
		Deployment: deployment,
		Meta:       req.Meta,
		Body:       req.Body,
		Credential: credential,
	})
	if err != nil {
		emit(err)
		return result, err
	}

	result.Chunks = chunks
	// Finish is deferred to the caller because only the caller knows how the
	// stream ended and what it consumed. Every early-exit path above has
	// already emitted, so exactly one event is produced either way.
	result.finish = func(usage core.TokenUsage, ttfb time.Duration, streamErr error) {
		r := &Result{
			Principal:       result.Principal,
			Deployment:      result.Deployment,
			Route:           result.Route,
			Usage:           usage,
			Latency:         p.now().Sub(started),
			TimeToFirstByte: ttfb,
		}
		p.emitUsage(ctx, snap, req, r, deployment, true, streamErr)
	}
	return result, nil
}

// StreamResult is a started stream plus what accounting needs about it.
//
// The caller owns Chunks and must Close it, and must call Finish exactly once
// when the stream ends — that is what produces the usage event, since token
// counts only exist after the stream has been drained.
type StreamResult struct {
	Chunks core.ChunkStream

	Principal  core.Principal
	Deployment core.DeploymentID
	Route      core.RoutingKey

	deployment core.Deployment
	finish     func(core.TokenUsage, time.Duration, error)
	finished   sync.Once
}

// Finish records the completed stream. It is safe to call more than once and
// safe to call on a result that never started streaming, so a deferred call in
// the transport needs no guard.
func (r *StreamResult) Finish(usage core.TokenUsage, ttfb time.Duration, err error) {
	if r == nil || r.finish == nil {
		return
	}
	r.finished.Do(func() { r.finish(usage, ttfb, err) })
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
