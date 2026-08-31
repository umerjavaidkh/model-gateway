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

const (
	CapabilityStreaming   Capability = "streaming"
	CapabilityToolCalling Capability = "tool_calling"
	CapabilityVision      Capability = "vision"
	CapabilityEmbeddings  Capability = "embeddings"
)

// Cost is the price of a deployment, per thousand tokens.
type Cost struct {
	InputPer1K  MicroUSD
	OutputPer1K MicroUSD
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

	// Weight is the share of traffic this deployment may take, 0-100. Zero means
	// registered but not serving: the state a new fine-tuned adapter sits in
	// while it takes shadow traffic, before the canary steps begin.
	Weight uint8

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

// ModelAlias decouples client code from concrete model IDs: callers ask for
// "fast" or "reasoning" and the snapshot decides what that means today.
// Targets are in preference order.
type ModelAlias struct {
	Name    string
	Targets []RoutingKey
}
