package core

// TrustTier classifies where a request is allowed to be sent. It is the
// mechanism behind data residency and PII handling: both are expressed as a
// routing constraint, not as a deployment afterthought.
//
// Tiers are ordered, and higher means more trusted. Ordering matters because the
// admission stage computes a *minimum* acceptable tier for the request and the
// router then filters candidates to those that meet it.
type TrustTier uint8

const (
	// TrustUnset is the zero value and is never valid in a built snapshot.
	// Having an explicit zero that fails validation means a deployment cannot
	// silently default to the most permissive tier.
	TrustUnset TrustTier = iota
	// TrustExternal is a third-party SaaS provider: OpenAI, Anthropic, Bedrock.
	TrustExternal
	// TrustPrivateCloud is dedicated tenancy under our own contract and keys.
	TrustPrivateCloud
	// TrustInternal is inside our own network: a self-hosted vLLM or TGI pod.
	TrustInternal
)

var trustTierNames = map[TrustTier]string{
	TrustUnset:        "unset",
	TrustExternal:     "external",
	TrustPrivateCloud: "private_cloud",
	TrustInternal:     "internal",
}

func (t TrustTier) String() string {
	if n, ok := trustTierNames[t]; ok {
		return n
	}
	return "unknown"
}

// Valid reports whether t is a tier a snapshot may contain.
func (t TrustTier) Valid() bool {
	return t >= TrustExternal && t <= TrustInternal
}

// AtLeast reports whether t satisfies a minimum trust requirement.
func (t TrustTier) AtLeast(min TrustTier) bool { return t >= min }

// DataClass is the sensitivity stamped on a request by the policy stage. It
// determines the minimum trust tier the request may be routed to and which PII
// strategy applies.
//
// The DataClass-to-TrustTier mapping is deliberately *not* hardcoded here. It is
// tenant policy, it changes without a redeploy, and it lives in the snapshot.
// core supplies the vocabulary; the policy layer supplies the mapping.
type DataClass string

const (
	DataClassPublic       DataClass = "public"
	DataClassInternal     DataClass = "internal"
	DataClassConfidential DataClass = "confidential"
	DataClassRestricted   DataClass = "restricted"
)
