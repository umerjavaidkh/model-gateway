package core_test

import (
	"testing"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

func TestBudgetHeadroomIsHeldBack(t *testing.T) {
	// The headroom band exists so in-flight streaming requests can finish
	// without overshooting a hard limit. A budget is "denying" once spend
	// reaches the band, not once it reaches the limit.
	b := core.BudgetState{
		LimitMicroUSD:       1_000_000,
		SpentMicroUSD:       940_000,
		Hard:                true,
		HeadroomBasisPoints: 500, // 5%
	}

	if got, want := b.Available(), core.MicroUSD(10_000); got != want {
		t.Fatalf("Available = %d, want %d", got, want)
	}
	if b.Denies() {
		t.Fatal("a budget with headroom left must not deny")
	}

	b.SpentMicroUSD = 950_000
	if !b.Denies() {
		t.Fatal("a hard budget must deny once spend reaches the headroom band")
	}
}

func TestSoftBudgetNeverDenies(t *testing.T) {
	b := core.BudgetState{LimitMicroUSD: 100, SpentMicroUSD: 10_000, Hard: false}
	if b.Denies() {
		t.Fatal("a soft budget warns, it does not enforce")
	}
}

func TestOverspentBudgetReportsZeroAvailable(t *testing.T) {
	// Available is clamped so that callers can treat it as a headroom figure
	// without guarding against negatives.
	b := core.BudgetState{LimitMicroUSD: 100, SpentMicroUSD: 500, Hard: true}
	if got := b.Available(); got != 0 {
		t.Fatalf("Available = %d, want 0", got)
	}
}
