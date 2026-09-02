package core

import (
	"fmt"
	"maps"
	"time"
)

// A snapshot is the immutable, versioned artifact the control plane compiles and
// every worker serves requests from. It is the central idea in this design: the
// data plane holds no durable state and reads no database, so a control-plane
// outage degrades to "configuration is frozen" while traffic keeps flowing.
//
// # Why it is layered
//
// The reference plan describes one monolithic snapshot containing every tenant's
// configuration. That does not survive contact with scale. Plugin bindings,
// aliases and budgets are per-tenant and change constantly, so a single artifact
// means one tenant editing a budget rebuilds and reships every other tenant's
// configuration to every worker. At a few dozen tenants this is invisible; at a
// few thousand it is the system's dominant cost.
//
// So a snapshot is composed from two kinds of layer:
//
//   - a GlobalLayer — the model catalog, deployments, default aliases and
//     default plugin bindings. Large, and changes rarely.
//   - one TenantLayer per tenant — principals, alias overrides, budget state and
//     per-tenant plugin bindings. Small, and changes constantly.
//
// Composition is a pointer join, so replacing one tenant layer costs a map copy
// of pointers rather than a rebuild of the catalog. Lookups resolve tenant-first
// and fall back to global, which is what makes "tenant A gets Presidio, tenant B
// gets regex-only, tenant C gets none, same binary" a data question.
//
// Getting this shape wrong is not a refactor later: the snapshot is the contract
// between the control plane, the wire format, and every worker, so its identity
// has to be right in the first commit.

// LayerVersion identifies one build of one layer. Number is monotonic per layer
// and orders versions; Digest content-addresses the layer so that a worker can
// skip a re-fetch, and so that two workers claiming the same version can be
// proven to hold the same bytes.
type LayerVersion struct {
	Number uint64
	Digest string
}

func (v LayerVersion) String() string { return fmt.Sprintf("%d@%s", v.Number, v.Digest) }

// PluginBinding says which registry component fills a port, at which version.
// Config is carried by reference: component configuration can be large and is
// fetched once, while the binding itself is compared on every snapshot swap.
type PluginBinding struct {
	Port      PortName
	Component string
	Version   string
	ConfigRef string
}

// ---------------------------------------------------------------------------
// Specs — the plain input a layer is built from
// ---------------------------------------------------------------------------

// GlobalSpec is the unvalidated input to NewGlobalLayer. It is a plain struct so
// that a builder, a test, or a decoded wire message can all produce one without
// depending on the layer's internal indexes.
type GlobalSpec struct {
	Version     LayerVersion
	BuiltAt     time.Time
	Deployments []Deployment
	Aliases     []ModelAlias
	// TenantPrefixes routes an API key's prefix segment to a tenant layer. This
	// is why the prefix exists: it turns principal lookup into one probe into
	// one tenant's map, instead of a scan across every tenant in the snapshot.
	TenantPrefixes map[KeyPrefix]TenantID
	// DefaultPlugins apply to any tenant that does not override the port.
	DefaultPlugins  []PluginBinding
	PolicyBundleRef string
	// DefaultGuardrails apply to any tenant declaring none of its own.
	DefaultGuardrails []GuardrailBinding
	// DefaultPolicy applies to any tenant with none of its own. Carried as
	// opaque bytes here because core must not know how policy is evaluated —
	// that belongs to the package that evaluates it, and core imports nothing.
	DefaultPolicy []byte
}

// TenantSpec is the unvalidated input to NewTenantLayer.
type TenantSpec struct {
	Tenant  TenantID
	Version LayerVersion
	BuiltAt time.Time
	Tier    string // plan tier, safe to use as a metrics label

	Principals []Principal
	// Keys maps each principal's lookup value to its KeyID. Lookup values are
	// derived from the key secret with a worker-held pepper and are supplied by
	// the control plane, which is the only component that ever sees a key in
	// clear text — at issuance.
	Keys map[KeyLookup]KeyID

	// AliasOverrides shadow global aliases and may add tenant-private ones.
	AliasOverrides []ModelAlias
	Budgets        []BudgetState
	Plugins        []PluginBinding
	// Guardrails for this tenant. Declaring any replaces the fleet defaults
	// entirely rather than merging: merging two lists of things that can refuse
	// traffic produces a set nobody can predict, and "which guardrails am I
	// running" must have a simple answer.
	Guardrails []GuardrailBinding
	// Policy replaces the fleet default rather than merging, for the same
	// reason guardrails do: two ordered rule lists merged produce an order
	// nobody can predict.
	Policy []byte

	// MinTrustTier is the floor for every request from this tenant, before the
	// request's own data classification raises it further. Data residency is
	// expressed here rather than at deployment time.
	MinTrustTier TrustTier
}

// ---------------------------------------------------------------------------
// Layers — validated and indexed
// ---------------------------------------------------------------------------

// GlobalLayer is the tenant-independent half of a snapshot. Its fields are
// unexported: a layer is built once by NewGlobalLayer, which validates it and
// computes its indexes, and is read-only thereafter.
type GlobalLayer struct {
	version LayerVersion
	builtAt time.Time

	deployments     map[DeploymentID]Deployment
	byRoute         map[RoutingKey][]Deployment
	aliases         map[string]ModelAlias
	tenantByPrefix  map[KeyPrefix]TenantID
	plugins         map[PortName]PluginBinding
	guardrails      []GuardrailBinding
	policy          []byte
	policyBundleRef string
}

// NewGlobalLayer validates a spec and builds the read indexes.
//
// Validation happens here, once, rather than at every use. A layer that exists
// is a layer that is coherent, so the request path has no defensive checks in it.
func NewGlobalLayer(spec GlobalSpec) (*GlobalLayer, error) {
	if spec.Version.Number == 0 {
		return nil, New(CodeInvalidRequest, "global layer version must be non-zero")
	}

	g := &GlobalLayer{
		version:         spec.Version,
		builtAt:         spec.BuiltAt,
		deployments:     make(map[DeploymentID]Deployment, len(spec.Deployments)),
		byRoute:         make(map[RoutingKey][]Deployment),
		aliases:         make(map[string]ModelAlias, len(spec.Aliases)),
		tenantByPrefix:  maps.Clone(spec.TenantPrefixes),
		plugins:         make(map[PortName]PluginBinding, len(spec.DefaultPlugins)),
		guardrails:      spec.DefaultGuardrails,
		policy:          spec.DefaultPolicy,
		policyBundleRef: spec.PolicyBundleRef,
	}
	if g.tenantByPrefix == nil {
		g.tenantByPrefix = map[KeyPrefix]TenantID{}
	}

	for _, d := range spec.Deployments {
		if d.ID == "" {
			return nil, New(CodeInvalidRequest, "deployment has an empty id")
		}
		if _, dup := g.deployments[d.ID]; dup {
			return nil, Newf(CodeInvalidRequest, "duplicate deployment id %q", d.ID)
		}
		if d.Key.BaseModel == "" {
			return nil, Newf(CodeInvalidRequest, "deployment %q has no base model", d.ID)
		}
		if !d.TrustTier.Valid() {
			return nil, Newf(CodeInvalidRequest, "deployment %q has an unset trust tier", d.ID)
		}
		if d.Weight > 100 {
			return nil, Newf(CodeInvalidRequest, "deployment %q weight %d exceeds 100", d.ID, d.Weight)
		}
		g.deployments[d.ID] = d
		g.byRoute[d.Key] = append(g.byRoute[d.Key], d)
	}

	for _, a := range spec.Aliases {
		if err := validateAlias(a, g.byRoute); err != nil {
			return nil, err
		}
		g.aliases[a.Name] = a
	}

	for _, p := range spec.DefaultPlugins {
		if _, dup := g.plugins[p.Port]; dup {
			return nil, Newf(CodeInvalidRequest, "two default plugins bound to port %q", p.Port)
		}
		g.plugins[p.Port] = p
	}
	return g, nil
}

func validateAlias(a ModelAlias, routes map[RoutingKey][]Deployment) error {
	if a.Name == "" {
		return New(CodeInvalidRequest, "alias has an empty name")
	}
	if len(a.Targets) == 0 {
		return Newf(CodeInvalidRequest, "alias %q has no targets", a.Name)
	}
	for _, t := range a.Targets {
		if len(routes[t]) == 0 {
			return Newf(CodeInvalidRequest, "alias %q targets %q, which has no deployment", a.Name, t)
		}
	}
	return nil
}

// Version reports the layer's version.
func (g *GlobalLayer) Version() LayerVersion { return g.version }

// TenantLayer is one tenant's slice of a snapshot.
type TenantLayer struct {
	tenant  TenantID
	version LayerVersion
	builtAt time.Time
	tier    string

	principals   map[KeyID]Principal
	keys         map[KeyLookup]KeyID
	aliases      map[string]ModelAlias
	budgets      map[BudgetID]BudgetState
	plugins      map[PortName]PluginBinding
	guardrails   []GuardrailBinding
	policy       []byte
	minTrustTier TrustTier
}

// NewTenantLayer validates a spec and builds the read indexes.
func NewTenantLayer(spec TenantSpec) (*TenantLayer, error) {
	if spec.Tenant == "" {
		return nil, New(CodeInvalidRequest, "tenant layer has no tenant id")
	}
	if spec.Version.Number == 0 {
		return nil, Newf(CodeInvalidRequest, "tenant %q layer version must be non-zero", spec.Tenant)
	}

	t := &TenantLayer{
		tenant:       spec.Tenant,
		version:      spec.Version,
		builtAt:      spec.BuiltAt,
		tier:         spec.Tier,
		principals:   make(map[KeyID]Principal, len(spec.Principals)),
		keys:         maps.Clone(spec.Keys),
		aliases:      make(map[string]ModelAlias, len(spec.AliasOverrides)),
		budgets:      make(map[BudgetID]BudgetState, len(spec.Budgets)),
		plugins:      make(map[PortName]PluginBinding, len(spec.Plugins)),
		guardrails:   spec.Guardrails,
		policy:       spec.Policy,
		minTrustTier: spec.MinTrustTier,
	}
	if t.keys == nil {
		t.keys = map[KeyLookup]KeyID{}
	}

	for _, b := range spec.Budgets {
		if b.ID == "" {
			return nil, Newf(CodeInvalidRequest, "tenant %q has a budget with no id", spec.Tenant)
		}
		if b.HeadroomBasisPoints > basisPointsPerUnit {
			return nil, Newf(CodeInvalidRequest, "budget %q headroom exceeds 100%%", b.ID)
		}
		t.budgets[b.ID] = b
	}

	for _, p := range spec.Principals {
		if p.KeyID == "" {
			return nil, Newf(CodeInvalidRequest, "tenant %q has a principal with no key id", spec.Tenant)
		}
		if p.Tenant != spec.Tenant {
			return nil, Newf(CodeInvalidRequest, "principal %q belongs to tenant %q, not %q", p.KeyID, p.Tenant, spec.Tenant)
		}
		// A dangling budget reference would make admission silently skip a limit
		// that an operator believes is enforced. Catch it at build time.
		for _, ref := range p.Budgets {
			if _, ok := t.budgets[ref.ID]; !ok {
				return nil, Newf(CodeInvalidRequest, "principal %q references unknown budget %q", p.KeyID, ref.ID)
			}
		}
		t.principals[p.KeyID] = p
	}

	for _, keyID := range t.keys {
		if _, ok := t.principals[keyID]; !ok {
			return nil, Newf(CodeInvalidRequest, "tenant %q maps a key to unknown principal %q", spec.Tenant, keyID)
		}
	}

	for _, a := range spec.AliasOverrides {
		if a.Name == "" || len(a.Targets) == 0 {
			return nil, Newf(CodeInvalidRequest, "tenant %q has a malformed alias override", spec.Tenant)
		}
		t.aliases[a.Name] = a
	}

	for _, p := range spec.Plugins {
		if _, dup := t.plugins[p.Port]; dup {
			return nil, Newf(CodeInvalidRequest, "tenant %q binds two plugins to port %q", spec.Tenant, p.Port)
		}
		t.plugins[p.Port] = p
	}
	return t, nil
}

// Tenant reports which tenant the layer belongs to.
func (t *TenantLayer) Tenant() TenantID { return t.tenant }

// Version reports the layer's version.
func (t *TenantLayer) Version() LayerVersion { return t.version }

// ---------------------------------------------------------------------------
// Snapshot — the composed view
// ---------------------------------------------------------------------------

// Snapshot is the composed, immutable configuration one request is served from.
//
// It is a pointer join over layers, never a merged copy. Swapping a layer
// produces a new Snapshot that shares every layer it did not change, which is
// what keeps a tenant-level edit cheap.
type Snapshot struct {
	global  *GlobalLayer
	tenants map[TenantID]*TenantLayer
}

// Compose joins a global layer with a set of tenant layers.
//
// It rejects a tenant layer with no key prefix routed to it, because such a
// tenant's keys could never be resolved: the snapshot would build cleanly and
// then authenticate nobody. Failing the build is far easier to diagnose than a
// tenant reporting that all their keys stopped working.
func Compose(global *GlobalLayer, tenants ...*TenantLayer) (*Snapshot, error) {
	if global == nil {
		return nil, New(CodeInvalidRequest, "snapshot needs a global layer")
	}

	byTenant := make(map[TenantID]*TenantLayer, len(tenants))
	for _, t := range tenants {
		if t == nil {
			return nil, New(CodeInvalidRequest, "snapshot contains a nil tenant layer")
		}
		if _, dup := byTenant[t.tenant]; dup {
			return nil, Newf(CodeInvalidRequest, "two layers for tenant %q", t.tenant)
		}
		byTenant[t.tenant] = t
	}

	routed := make(map[TenantID]bool, len(byTenant))
	for _, tenant := range global.tenantByPrefix {
		routed[tenant] = true
	}
	for tenant := range byTenant {
		if !routed[tenant] {
			return nil, Newf(CodeInvalidRequest, "tenant %q has a layer but no key prefix routes to it", tenant)
		}
	}
	return &Snapshot{global: global, tenants: byTenant}, nil
}

// WithTenantLayer returns a snapshot with one tenant layer replaced or added.
// The global layer and every other tenant layer are shared, not copied.
func (s *Snapshot) WithTenantLayer(layer *TenantLayer) (*Snapshot, error) {
	if layer == nil {
		return nil, New(CodeInvalidRequest, "nil tenant layer")
	}
	next := maps.Clone(s.tenants)
	next[layer.tenant] = layer
	out := &Snapshot{global: s.global, tenants: next}
	return out, nil
}

// WithGlobalLayer returns a snapshot with the global layer replaced and every
// tenant layer carried over. Compose's routing check is re-run, so a global
// layer that drops a live tenant's prefix is rejected rather than silently
// stranding that tenant's keys.
func (s *Snapshot) WithGlobalLayer(global *GlobalLayer) (*Snapshot, error) {
	tenants := make([]*TenantLayer, 0, len(s.tenants))
	for _, t := range s.tenants {
		tenants = append(tenants, t)
	}
	return Compose(global, tenants...)
}

// GlobalVersion reports the version of the global layer.
func (s *Snapshot) GlobalVersion() LayerVersion { return s.global.version }

// TenantVersion reports the version of one tenant's layer.
func (s *Snapshot) TenantVersion(tenant TenantID) (LayerVersion, bool) {
	t, ok := s.tenants[tenant]
	if !ok {
		return LayerVersion{}, false
	}
	return t.version, true
}

// TenantIDs returns every tenant with a layer in this snapshot, in unspecified
// order. It is for control-path work — version comparison, metrics, draining —
// and is not on the request path.
func (s *Snapshot) TenantIDs() []TenantID {
	out := make([]TenantID, 0, len(s.tenants))
	for id := range s.tenants {
		out = append(out, id)
	}
	return out
}

// DeploymentIDs returns every deployment in the catalog, in unspecified order.
// It is for control-path work — probing, metrics — and is not on the request
// path.
func (s *Snapshot) DeploymentIDs() []DeploymentID {
	out := make([]DeploymentID, 0, len(s.global.deployments))
	for id := range s.global.deployments {
		out = append(out, id)
	}
	return out
}

// PolicyBundleRef reports the compiled policy bundle this snapshot was built
// against.
func (s *Snapshot) PolicyBundleRef() string { return s.global.policyBundleRef }

// TenantForPrefix resolves an API key's prefix segment to a tenant.
func (s *Snapshot) TenantForPrefix(prefix KeyPrefix) (TenantID, bool) {
	t, ok := s.global.tenantByPrefix[prefix]
	return t, ok
}

// Principal resolves a key lookup value within a tenant. This is the whole of
// authentication at request time: one map probe for the tenant, one for the key,
// one for the principal.
func (s *Snapshot) Principal(tenant TenantID, lookup KeyLookup) (Principal, bool) {
	layer, ok := s.tenants[tenant]
	if !ok {
		return Principal{}, false
	}
	keyID, ok := layer.keys[lookup]
	if !ok {
		return Principal{}, false
	}
	p, ok := layer.principals[keyID]
	return p, ok
}

// Tier reports a tenant's plan tier, which is the bounded-cardinality label to
// use in metrics in place of the tenant ID.
func (s *Snapshot) Tier(tenant TenantID) string {
	if layer, ok := s.tenants[tenant]; ok {
		return layer.tier
	}
	return "unknown"
}

// MinTrustTier reports a tenant's floor on destination trust. A request's own
// data classification may raise it, never lower it.
func (s *Snapshot) MinTrustTier(tenant TenantID) TrustTier {
	if layer, ok := s.tenants[tenant]; ok {
		return layer.minTrustTier
	}
	return TrustInternal // unknown tenant: the most restrictive answer
}

// ResolveAlias maps a model name to its ordered routing keys, checking the
// tenant's overrides before the global catalog. A name that is not an alias
// resolves to itself as a base model, so callers may pass either.
func (s *Snapshot) ResolveAlias(tenant TenantID, name string) []RoutingKey {
	if layer, ok := s.tenants[tenant]; ok {
		if a, ok := layer.aliases[name]; ok {
			return a.Targets
		}
	}
	if a, ok := s.global.aliases[name]; ok {
		return a.Targets
	}
	return []RoutingKey{{BaseModel: name}}
}

// Deployments returns every deployment registered for a routing key, in
// catalog order.
//
// The returned slice aliases snapshot memory. Callers may read it and must not
// write to it; the router builds its own ordered candidate list.
func (s *Snapshot) Deployments(key RoutingKey) []Deployment {
	return s.global.byRoute[key]
}

// Deployment looks up a single deployment by id.
func (s *Snapshot) Deployment(id DeploymentID) (Deployment, bool) {
	d, ok := s.global.deployments[id]
	return d, ok
}

// PluginBinding reports which component fills a port for a tenant, preferring
// the tenant's own binding over the global default. The second return is false
// when no component is bound, which is a valid state: a tenant with no guardrail
// bound simply has no guardrail.
func (s *Snapshot) PluginBinding(tenant TenantID, port PortName) (PluginBinding, bool) {
	if layer, ok := s.tenants[tenant]; ok {
		if b, ok := layer.plugins[port]; ok {
			return b, true
		}
	}
	b, ok := s.global.plugins[port]
	return b, ok
}

// Guardrails reports which guardrails apply to a tenant, on the given leg.
//
// A tenant declaring any replaces the fleet defaults rather than adding to
// them, so the answer to "what is running" is one list from one place.
//
// The returned slice aliases snapshot memory; callers may read it and must not
// write to it.
func (s *Snapshot) Guardrails(tenant TenantID, phase Phase) []GuardrailBinding {
	bindings := s.global.guardrails
	if layer, ok := s.tenants[tenant]; ok && len(layer.guardrails) > 0 {
		bindings = layer.guardrails
	}

	applicable := make([]GuardrailBinding, 0, len(bindings))
	for _, binding := range bindings {
		if binding.Inspects(phase) {
			applicable = append(applicable, binding)
		}
	}
	return applicable
}

// AllGuardrails returns every guardrail binding in the snapshot: the fleet
// defaults and every tenant's, both phases.
//
// For whatever has to prepare a component before a request reaches it. That
// cannot ask per tenant and per phase, because the first request to need a
// component is the one that would pay for loading it.
//
// The bindings alias snapshot memory; callers may read them and must not write
// to them.
func (s *Snapshot) AllGuardrails() []GuardrailBinding {
	all := make([]GuardrailBinding, 0, len(s.global.guardrails))
	all = append(all, s.global.guardrails...)
	for _, layer := range s.tenants {
		all = append(all, layer.guardrails...)
	}
	return all
}

// Policy returns the compiled policy bundle that applies to a tenant, as the
// bytes the policy package decodes.
//
// core deliberately does not know what is in them. Policy evaluation is one
// package's concern, and giving core a policy type would make the domain model
// depend on how policy happens to be represented today.
//
// The returned slice aliases snapshot memory; callers may read it and must not
// write to it.
func (s *Snapshot) Policy(tenant TenantID) []byte {
	if layer, ok := s.tenants[tenant]; ok && len(layer.policy) > 0 {
		return layer.policy
	}
	return s.global.policy
}

// Budget reports one budget's state within a tenant.
func (s *Snapshot) Budget(tenant TenantID, id BudgetID) (BudgetState, bool) {
	layer, ok := s.tenants[tenant]
	if !ok {
		return BudgetState{}, false
	}
	b, ok := layer.budgets[id]
	return b, ok
}

// DeniedBudget returns the first budget in the principal's chain that must
// reject the request, if any. The chain is precomputed into the principal, so
// this is a short scan over a handful of map probes rather than a graph walk.
func (s *Snapshot) DeniedBudget(p *Principal) (BudgetState, bool) {
	for _, ref := range p.Budgets {
		b, ok := s.Budget(p.Tenant, ref.ID)
		if !ok {
			continue // validated at layer build time; absent means not yet funded
		}
		if b.Denies() {
			return b, true
		}
	}
	return BudgetState{}, false
}
