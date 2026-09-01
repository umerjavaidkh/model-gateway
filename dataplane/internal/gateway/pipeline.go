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

	"go.opentelemetry.io/otel/codes"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/guardrails"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/limits"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/router"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/tracing"
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
	limiter     RateLimiter
	router      *router.Router
	guardrails  GuardrailChain
	region      string
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

// RateLimiter admits or refuses a request against the principal's limits.
//
// An interface so the pipeline does not depend on how limiting is implemented,
// and so a test can refuse deterministically without a store or a clock.
type RateLimiter interface {
	Admit(ctx context.Context, p *core.Principal) limits.Decision
	Release(p *core.Principal)
	RecordTokens(ctx context.Context, p *core.Principal, tokens int64)
}

// GuardrailChain runs the inspections bound to a tenant.
//
// An interface so the pipeline does not depend on how they are run, and so a
// test can deny deterministically without a registry.
type GuardrailChain interface {
	Run(ctx context.Context, bindings []core.GuardrailBinding, in *core.GuardrailInput) (guardrails.Outcome, error)
}

// WithGuardrails sets the guardrail chain. Without one, bindings in the
// snapshot are carried but nothing inspects anything — the right default for a
// unit test and the wrong one for a worker, so main always supplies it.
func WithGuardrails(chain GuardrailChain) Option {
	return func(p *Pipeline) {
		if chain != nil {
			p.guardrails = chain
		}
	}
}

// WithRegion tells selection where this worker runs, so a deployment in the
// same region is preferred.
func WithRegion(region string) Option {
	return func(p *Pipeline) { p.region = region }
}

// WithRouter replaces the router. Without one the pipeline builds its own with
// default breaker and retry settings, which is what a test wants and what a
// worker gets unless it configures otherwise.
func WithRouter(rt *router.Router) Option {
	return func(p *Pipeline) {
		if rt != nil {
			p.router = rt
		}
	}
}

// WithLimiter sets the rate limiter. Without one, limits in the snapshot are
// carried but not enforced — which is the right default for a unit test and
// the wrong one for a worker, so main always supplies it.
func WithLimiter(l RateLimiter) Option {
	return func(p *Pipeline) {
		if l != nil {
			p.limiter = l
		}
	}
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
		limiter:     unlimited{},
		guardrails:  inspectNothing{},
		pepper:      pepper,
		now:         time.Now,
	}
	for _, opt := range opts {
		opt(p)
	}

	if p.router == nil {
		rt, err := router.New(providers)
		if err != nil {
			return nil, err
		}
		p.router = rt
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

	principal, err := stage(ctx, "authenticate", func() (core.Principal, error) {
		return p.authenticate(snap, req)
	})
	if err != nil {
		emit(err)
		return result, err
	}
	result.Principal = principal

	if _, err := stage(ctx, "admit", func() (struct{}, error) {
		return struct{}{}, p.admit(ctx, snap, &principal, req)
	}); err != nil {
		emit(err)
		return result, err
	}
	// The slot is held for the whole request, which is what makes a
	// concurrency limit mean anything. Released on every exit path below.
	defer p.limiter.Release(&principal)

	// Before routing, not after: a credential in the payload must be caught
	// before the gateway decides where to send it, and certainly before it
	// sends it. The tier-dependent inspections the design places after routing
	// are the PII chain, which is a separate stage.
	body, err := stage(ctx, "guard", func() ([]byte, error) {
		return p.guard(ctx, snap, &principal, req)
	})
	if err != nil {
		emit(err)
		return result, err
	}
	req.Body = body

	candidates, err := stage(ctx, "route", func() ([]router.Candidate, error) {
		return p.route(snap, &principal, req)
	})
	if err != nil {
		emit(err)
		return result, err
	}
	// Attributed to the first candidate until execution says otherwise, so a
	// failure that never reached a provider is still attributed somewhere.
	deployment = candidates[0].Deployment
	result.Deployment = deployment.ID
	result.Route = deployment.Key

	executed, err := stage(ctx, "adapt", func() (*router.Result, error) {
		return p.adapt(ctx, candidates, req, false)
	})
	if err != nil {
		emit(err)
		return result, err
	}
	// Whichever candidate actually served it, which is not necessarily the
	// first when one was skipped or failed over.
	deployment = executed.Deployment
	result.Deployment = deployment.ID
	result.Route = deployment.Key
	resp := executed.Response

	result.StatusCode = resp.StatusCode
	result.Body = resp.Body
	result.Usage = resp.Usage
	// Recorded after the call, which is the earliest the count exists. A token
	// limit therefore takes effect on the *next* request; that lag is inherent
	// to limiting something that is only measurable afterwards.
	p.limiter.RecordTokens(ctx, &principal, resp.Usage.Total())
	emit(nil)
	return result, nil
}

// stage runs one pipeline stage inside its own span.
//
// The four stages are where the time actually goes; without them a slow
// request shows only a total, and "the gateway was slow" is not a finding. The
// error is recorded on the span as well as returned, because a failed stage is
// what an operator opens the trace to find.
func stage[T any](ctx context.Context, name string, fn func() (T, error)) (T, error) {
	_, span := tracing.Tracer().Start(ctx, "gateway."+name)
	defer span.End()

	result, err := fn()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, string(core.CodeOf(err)))
	}
	return result, err
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
	cost := d.Cost.For(result.Usage)

	_ = p.telemetry.Emit(ctx, core.UsageEvent{
		RequestID:         req.Meta.RequestID,
		Timestamp:         p.now(),
		Tenant:            result.Principal.Tenant,
		KeyID:             result.Principal.KeyID,
		Tier:              snap.Tier(result.Principal.Tenant),
		Deployment:        result.Deployment,
		Route:             result.Route,
		Provider:          d.Provider,
		Stream:            stream,
		InputTokens:       result.Usage.Input,
		CachedInputTokens: result.Usage.CachedInput,
		CacheWriteTokens:  result.Usage.CacheWrite,
		OutputTokens:      result.Usage.Output,
		CostMicroUSD:      cost,
		// Equal to cost until a rate card exists. Separate now because
		// backfilling a price onto usage records that were only ever written
		// with a cost is not possible.
		PriceMicroUSD:   cost,
		LatencyMs:       result.Latency.Milliseconds(),
		TimeToFirstByte: result.TimeToFirstByte,
		Outcome:         outcome,
		SnapshotVersion: snap.GlobalVersion().Number,
		Budgets:         budgetIDs(result.Principal.Budgets),
	})
}

// budgetIDs flattens a principal's budget chain for the usage event.
func budgetIDs(refs []core.BudgetRef) []core.BudgetID {
	if len(refs) == 0 {
		return nil
	}
	ids := make([]core.BudgetID, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.ID)
	}
	return ids
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

	if err := p.admit(ctx, snap, &principal, req); err != nil {
		emit(err)
		return result, err
	}
	// Held for the life of the stream, which is what makes a concurrency limit
	// mean anything for a response that takes a minute. Released by Finish.
	result.releaseLimit = func() { p.limiter.Release(&principal) }

	body, err := p.guard(ctx, snap, &principal, req)
	if err != nil {
		emit(err)
		return result, err
	}
	req.Body = body

	candidates, err := p.route(snap, &principal, req)
	if err != nil {
		emit(err)
		return result, err
	}

	// Retries here cover starting the stream, not continuing one. Once a byte
	// has been relayed the response is committed and failing over would send
	// the caller two different answers concatenated.
	executed, err := p.adapt(ctx, candidates, req, true)
	if err != nil {
		if executed != nil && len(executed.Attempts) > 0 {
			result.Deployment = executed.Attempts[len(executed.Attempts)-1].Deployment
		}
		emit(err)
		return result, err
	}

	deployment := executed.Deployment
	result.deployment = deployment
	result.Deployment = deployment.ID
	result.Route = deployment.Key
	result.Chunks = executed.Stream
	// Finish is deferred to the caller because only the caller knows how the
	// stream ended and what it consumed. Every early-exit path above has
	// already emitted, so exactly one event is produced either way.
	result.finish = func(usage core.TokenUsage, ttfb time.Duration, streamErr error) {
		p.limiter.RecordTokens(ctx, &principal, usage.Total())
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

	deployment   core.Deployment
	finish       func(core.TokenUsage, time.Duration, error)
	releaseLimit func()
	finished     sync.Once
}

// Finish records the completed stream. It is safe to call more than once and
// safe to call on a result that never started streaming, so a deferred call in
// the transport needs no guard.
func (r *StreamResult) Finish(usage core.TokenUsage, ttfb time.Duration, err error) {
	if r == nil {
		return
	}
	r.finished.Do(func() {
		if r.releaseLimit != nil {
			r.releaseLimit()
		}
		if r.finish != nil {
			r.finish(usage, ttfb, err)
		}
	})
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
func (p *Pipeline) admit(
	ctx context.Context, snap *core.Snapshot, principal *core.Principal, req *Request,
) error {
	// Snapshot-only checks first: they are map lookups and cannot fail, so
	// spending a rate-limit permit on a request that a free check would have
	// refused would let a misconfigured caller exhaust its own limit.
	if !principal.Models.Permits(req.Meta.Model) {
		return core.Newf(core.CodeForbidden, "this key may not call %q", req.Meta.Model)
	}
	if budget, denied := snap.DeniedBudget(principal); denied {
		return core.Newf(core.CodeBudgetExhausted, "budget %q is exhausted", budget.ID)
	}

	if decision := p.limiter.Admit(ctx, principal); !decision.Allowed {
		return core.Newf(decision.Reason, "rate limit exceeded; retry in %s",
			decision.RetryAfter.Round(time.Second))
	}
	return nil
}

// unlimited is the default limiter: it enforces nothing.
type unlimited struct{}

// Admit permits everything.
func (unlimited) Admit(context.Context, *core.Principal) limits.Decision {
	return limits.Decision{Allowed: true}
}

// Release does nothing; nothing was acquired.
func (unlimited) Release(*core.Principal) {}

// RecordTokens discards the count.
func (unlimited) RecordTokens(context.Context, *core.Principal, int64) {}

// guard runs the tenant's request-leg guardrails, returning the payload to
// send onward — which a guardrail may have rewritten.
func (p *Pipeline) guard(
	ctx context.Context, snap *core.Snapshot, principal *core.Principal, req *Request,
) ([]byte, error) {
	bindings := snap.Guardrails(principal.Tenant, core.PhaseRequest)
	if len(bindings) == 0 {
		return req.Body, nil
	}

	outcome, err := p.guardrails.Run(ctx, bindings, &core.GuardrailInput{
		Phase:   core.PhaseRequest,
		Meta:    req.Meta,
		Class:   principal.DefaultClass,
		Payload: req.Body,
	})
	if err != nil {
		return nil, err
	}
	if outcome.Denied {
		// The guardrail's own message is deliberately not relayed. Telling a
		// caller exactly which pattern their payload tripped is telling them
		// how to avoid it next time.
		return nil, core.Newf(core.CodeGuardrailDenied,
			"request refused by the %s guardrail", outcome.Reason)
	}
	return outcome.Payload, nil
}

// inspectNothing is the default chain: no guardrail runs.
type inspectNothing struct{}

// Run allows everything unchanged.
func (inspectNothing) Run(
	_ context.Context, _ []core.GuardrailBinding, in *core.GuardrailInput,
) (guardrails.Outcome, error) {
	return guardrails.Outcome{Payload: in.Payload}, nil
}

// route selects an ordered list of deployments that may serve the request.
//
// The minimum trust tier is computed once here and applied to the whole list,
// not per attempt. That ordering is the point: the reference design transforms
// payloads after routing, so a fallback that changed tier would send a payload
// prepared for one destination to another. Filtering the list makes that
// impossible by construction rather than by care in the execution loop.
func (p *Pipeline) route(
	snap *core.Snapshot, principal *core.Principal, req *Request,
) ([]router.Candidate, error) {
	minTier := snap.MinTrustTier(principal.Tenant)
	if principal.MinTrustTier > minTier {
		minTier = principal.MinTrustTier
	}

	return p.router.Select(router.SelectionInput{
		Snapshot:     snap,
		Tenant:       principal.Tenant,
		Model:        req.Meta.Model,
		Endpoint:     req.Meta.Endpoint,
		MinTrustTier: minTier,
		Region:       p.region,
		// Balanced until an objective can be expressed per tenant or per
		// request. Most traffic wants a working answer at a sensible price,
		// and a default that has to be overridden to be reasonable is the
		// wrong default.
		Objective:       router.ObjectiveBalanced,
		ReferenceTokens: int64(req.Meta.PayloadBytes / 4),
	})
}

// adapt executes the candidate list, returning whichever deployment served it.
// The streaming flag comes from which method called this, not from
// req.Meta.Stream. HandleStream is the streaming path; reading the caller's
// metadata instead would let a transport that set it wrong get a completed
// body where it expected a cursor, and discover it as a nil dereference.
func (p *Pipeline) adapt(
	ctx context.Context, candidates []router.Candidate, req *Request, stream bool,
) (*router.Result, error) {
	return p.router.Execute(ctx, candidates, req.Meta.Deadline,
		func(ctx context.Context, provider core.ProviderPort, d core.Deployment) (
			*core.ProviderResponse, core.ChunkStream, error,
		) {
			// Resolved per attempt rather than once, because each candidate
			// may be a different provider with a different credential.
			credential, err := p.credentials.Resolve(ctx, d.CredentialRef)
			if err != nil {
				return nil, nil, err
			}
			call := &core.ProviderCall{
				Deployment: d, Meta: req.Meta, Body: req.Body, Credential: credential,
			}
			if stream {
				chunks, err := provider.Stream(ctx, call)
				return nil, chunks, err
			}
			response, err := provider.Invoke(ctx, call)
			return response, nil, err
		})
}
