package core

// BudgetID names a spend limit defined in the control plane.
type BudgetID string

// BudgetScope says which level of the identity hierarchy a budget attaches to.
//
// Training is its own scope, not a line item under inference. A single
// fine-tuning run can cost more than a month of serving, so it gets its own
// limit and its own approval threshold.
type BudgetScope uint8

// The scopes a budget may attach to. Unset is the zero value and is never
// valid in a built layer.
const (
	BudgetScopeUnset BudgetScope = iota
	BudgetScopeKey
	BudgetScopeApp
	BudgetScopeUser
	BudgetScopeTeam
	BudgetScopeOrg
	BudgetScopeModel
	BudgetScopeTraining
)

// BudgetRef is one link in a principal's budget chain. The chain is precomputed
// into the Principal so that admission is an array scan over a handful of
// integers rather than a graph walk.
//
// A request must satisfy *every* budget in its chain.
type BudgetRef struct {
	ID    BudgetID
	Scope BudgetScope
}

// BudgetState is a budget's limit and its spend as of the snapshot that carries
// it. Budgets are eventually consistent by design: usage events flow to an
// accounting consumer, which folds the result into the next snapshot. Rate
// limits are the mechanism for anything that must be immediate.
type BudgetState struct {
	ID    BudgetID
	Scope BudgetScope

	LimitMicroUSD MicroUSD
	SpentMicroUSD MicroUSD

	// Hard budgets deny on exhaustion. Soft budgets emit a warning header and an
	// event, and let the request through.
	Hard bool

	// HeadroomBasisPoints holds back a slice of a hard limit so that requests
	// already streaming when the budget tips can finish without overshooting.
	// 500 = 5%, which is the documented default. uint32 to match the wire
	// format, for the same reason Deployment.Weight is.
	HeadroomBasisPoints uint32
}

// Available is the spend remaining before the headroom band begins.
func (b BudgetState) Available() MicroUSD {
	reserved := b.LimitMicroUSD * MicroUSD(b.HeadroomBasisPoints) / 10_000
	remaining := b.LimitMicroUSD - reserved - b.SpentMicroUSD
	if remaining < 0 {
		return 0
	}
	return remaining
}

// Denies reports whether this budget must reject a new request. Soft budgets
// never deny; they are observed at admission and reported, not enforced.
func (b BudgetState) Denies() bool { return b.Hard && b.Available() == 0 }
