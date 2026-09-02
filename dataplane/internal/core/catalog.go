package core

import "slices"

// MicroUSD is money in millionths of a US dollar.
//
// Integer, never float. Per-token prices are small enough that float64 rounding
// shows up in monthly invoices, and a budget system that disagrees with the bill
// is worse than no budget system.
type MicroUSD int64

// DeploymentID names one concrete, reachable model endpoint.
type DeploymentID string

// RoutingKey identifies a servable target as (base model, adapter).
//
// This is the key the routing table is indexed by, from the first commit, even
// though fine-tuning does not land until much later. The reason is economic: a
// per-tenant fine-tune only pencils out if one vLLM base-model deployment serves
// many LoRA adapters. If the routing key were a bare model name, adding adapters
// later would be a migration of the catalog, the snapshot, and every consumer.
//
// AdapterID is empty for a plain base model. The struct is comparable, so it can
// be a map key directly.
type RoutingKey struct {
	BaseModel string
	AdapterID string
}

// IsAdapter reports whether the key names a fine-tuned adapter.
func (k RoutingKey) IsAdapter() bool { return k.AdapterID != "" }

func (k RoutingKey) String() string {
	if k.AdapterID == "" {
		return k.BaseModel
	}
	return k.BaseModel + "+" + k.AdapterID
}

// Capability is a feature a caller may require of a deployment: streaming, tool
// calling, vision, a context-window class. The router filters candidates by it.
type Capability string

// The capabilities a deployment may declare and a caller may require.
const (
	CapabilityStreaming   Capability = "streaming"
	CapabilityToolCalling Capability = "tool_calling"
	CapabilityVision      Capability = "vision"
	CapabilityEmbeddings  Capability = "embeddings"
)

// Cost is what a deployment charges, per thousand tokens of each class.
//
// The classes exist because providers do not bill all input tokens the same:
// cached input is an order of magnitude cheaper, and writing to a cache is
// dearer than a plain read. A single input rate bills a request that was 90%
// cache reads as though it were not, and the provider reported the truth
// exactly once — in a response nobody kept.
type Cost struct {
	InputPer1K  MicroUSD
	OutputPer1K MicroUSD
	// CachedInputPer1K applies to input served from a provider's prompt cache.
	// Zero means unconfigured and falls back to InputPer1K, which over-bills
	// slightly rather than under-billing — the safe direction for a number a
	// customer disputes.
	CachedInputPer1K MicroUSD
	// CacheWritePer1K applies to input written into a provider's cache, which
	// costs more than a plain read.
	CacheWritePer1K MicroUSD
}

// For returns what a call costs at this price, given what it consumed.
//
// Prices are per thousand tokens, so this divides. Integer division truncates,
// which under-bills by less than a millionth of a dollar per request and never
// over-bills — the right direction for a rounding error a customer can see.
func (c Cost) For(usage TokenUsage) MicroUSD {
	cached := c.CachedInputPer1K
	if cached == 0 {
		// Unconfigured, not free. Billing cached tokens at zero because nobody
		// set a rate would silently give away the majority of a cache-heavy
		// workload.
		cached = c.InputPer1K
	}
	write := c.CacheWritePer1K
	if write == 0 {
		write = c.InputPer1K
	}

	total := MicroUSD(usage.Input)*c.InputPer1K +
		MicroUSD(usage.CachedInput)*cached +
		MicroUSD(usage.CacheWrite)*write +
		MicroUSD(usage.Output)*c.OutputPer1K
	return total / 1000
}

// Deployment is one reachable endpoint that can serve a RoutingKey.
//
// A model with three providers behind it is three Deployments sharing one
// RoutingKey. Selection picks among them; execution walks them in order.
type Deployment struct {
	ID       DeploymentID
	Key      RoutingKey
	Provider string // name of the ProviderPort implementation that speaks to it
	Endpoint string
	Region   string

	TrustTier TrustTier

	// CredentialRef points at a secret; it is never the secret. Provider
	// credentials must not enter a snapshot, because a snapshot is replicated to
	// every worker, cached, and versioned. Workers resolve the reference through
	// the control plane's SecretsPort and cache the result with a TTL.
	CredentialRef string

	// Weight is the share of traffic this deployment may take, 0-100. It is
	// uint32 to match the wire format: a narrower domain type would truncate an
	// out-of-range value into a plausible one instead of failing validation.
	// Zero means
	// registered but not serving: the state a new fine-tuned adapter sits in
	// while it takes shadow traffic, before the canary steps begin.
	Weight uint32

	// ShadowPercent is the share of the base model's traffic mirrored here,
	// 0-100. A separate dimension from Weight: an adapter in shadow serves
	// nobody while seeing real requests, and its response is discarded.
	//
	// Sampled rather than all-or-nothing because mirroring doubles inference
	// spend for whatever fraction it covers, and a shadow that costs as much
	// as production is one an operator turns off.
	ShadowPercent uint32

	Cost         Cost
	Capabilities []Capability
}

// Supports reports whether the deployment offers every required capability.
func (d *Deployment) Supports(required ...Capability) bool {
	for _, want := range required {
		if !slices.Contains(d.Capabilities, want) {
			return false
		}
	}
	return true
}

// Serving reports whether the deployment may take live traffic.
func (d *Deployment) Serving() bool { return d.Weight > 0 }

// Shadowing reports whether this deployment should receive mirrored traffic.
//
// An adapter only: shadowing a base model would mirror a request to the very
// deployment that served it.
func (d *Deployment) Shadowing() bool {
	return d.ShadowPercent > 0 && d.Key.IsAdapter()
}

// ModelAlias decouples client code from concrete model IDs: callers ask for
// "fast" or "reasoning" and the snapshot decides what that means today.
// Targets are in preference order.
type ModelAlias struct {
	Name    string
	Targets []RoutingKey
}
