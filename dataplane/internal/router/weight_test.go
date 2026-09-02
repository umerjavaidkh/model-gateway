package router_test

import (
	"slices"
	"testing"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/router"
)

// A canary is the case where "who should serve" and "who is best" disagree: a
// freshly trained adapter is healthy, the same price and in the same region as
// the base model it was trained from, so it scores at least as well — and must
// still take one request in a hundred until an operator says otherwise.

func weighted(id string, weight uint32) core.Deployment {
	d := deployment(id, core.TrustInternal, "alpha")
	d.Weight = weight
	return d
}

// drawsOf returns a draw function walking the given values, so a test can say
// which point of the distribution it is sampling rather than sample until it
// is convinced.
func drawsOf(values ...float64) func() float64 {
	i := 0
	return func() float64 {
		v := values[i%len(values)]
		i++
		return v
	}
}

func headFor(t *testing.T, draw float64, deployments ...core.Deployment) core.DeploymentID {
	t.Helper()

	r := newRouter(t, defaultRegistry(), router.WithDraw(drawsOf(draw)))
	got := selectWith(t, r, snapshotWith(t, deployments...), router.SelectionInput{})
	return got[0].Deployment.ID
}

func TestACanaryTakesItsShareAndNoMore(t *testing.T) {
	// Weight 1 against weight 99: the canary serves the first hundredth of the
	// distribution and the base model serves the rest.
	base, canary := weighted("base", 99), weighted("canary", 1)

	if got := headFor(t, 0.995, base, canary); got != "canary" {
		t.Fatalf("at the top of the distribution the head was %q, want the canary", got)
	}
	for _, draw := range []float64{0.0, 0.5, 0.98} {
		if got := headFor(t, draw, base, canary); got != "base" {
			t.Fatalf("at draw %v the head was %q, want the base model", draw, got)
		}
	}
}

func TestTheSplitFollowsTheWeights(t *testing.T) {
	// Walking the distribution rather than sampling it: a quarter of the draws
	// should land on a deployment carrying a quarter of the weight.
	base, canary := weighted("base", 75), weighted("canary", 25)

	canaries := 0
	for i := range 100 {
		if headFor(t, float64(i)/100, base, canary) == "canary" {
			canaries++
		}
	}

	if canaries != 25 {
		t.Fatalf("the canary took %d of 100 requests, want 25", canaries)
	}
}

func TestEqualWeightsLeaveScoreOrderAlone(t *testing.T) {
	// A weight is a relative share, so candidates carrying the same one are an
	// operator saying "treat these equally" — and equally falls to the score,
	// which is the system's own judgement about health, price and locality.
	// Drawing lots between them would throw that away and make every routing
	// decision unreproducible for nobody's benefit.
	dear, cheap := priced("dear", 3000, 15000, "eu"), priced("cheap", 150, 600, "eu")

	for _, draw := range []float64{0.0, 0.4, 0.99} {
		r := newRouter(t, defaultRegistry(), router.WithDraw(drawsOf(draw)))
		got := selectWith(t, r, snapshotWith(t, dear, cheap),
			router.SelectionInput{Objective: router.ObjectiveCost})

		if got[0].Deployment.ID != "cheap" {
			t.Fatalf("at draw %v the head was %q, want the cheaper deployment", draw, got[0].Deployment.ID)
		}
	}
}

func TestPromotingTheDrawnCandidateKeepsEveryoneElseInOrder(t *testing.T) {
	// The list below the head is still a failover order. Promoting by swapping
	// would put whatever the winner displaced into the winner's old position,
	// scrambling everything under it — so the winner is rotated to the front
	// instead, and this is the assertion that says so.
	snap := snapshotWith(t,
		weighted("a", 50), weighted("b", 1), weighted("c", 30), weighted("d", 19))

	baseline := ids(selectWith(t, newRouter(t, defaultRegistry(),
		router.WithDraw(drawsOf(0))), snap, router.SelectionInput{}))

	// Walk the distribution; every draw must produce the same set, and a tail
	// that is the baseline with the winner removed.
	for i := range 20 {
		got := ids(selectWith(t, newRouter(t, defaultRegistry(),
			router.WithDraw(drawsOf(float64(i)/20))), snap, router.SelectionInput{}))

		if len(got) != len(baseline) {
			t.Fatalf("got %v, want every candidate kept for failover", got)
		}
		want := without(baseline, got[0])
		if !slices.Equal(got[1:], want) {
			t.Fatalf("with %q promoted the tail was %v, want %v", got[0], got[1:], want)
		}
	}
}

func ids(candidates []router.Candidate) []core.DeploymentID {
	out := make([]core.DeploymentID, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate.Deployment.ID)
	}
	return out
}

func without(all []core.DeploymentID, drop core.DeploymentID) []core.DeploymentID {
	out := make([]core.DeploymentID, 0, len(all))
	for _, id := range all {
		if id != drop {
			out = append(out, id)
		}
	}
	return out
}

func TestAnUnhealthyCanaryLosesShareOfTraffic(t *testing.T) {
	// The weight is a number an operator set yesterday; health is what is
	// happening now. A canary failing every request must not keep drawing its
	// full share on the strength of the former — which is what makes the walk
	// through the canary steps survivable without a human watching it.
	base, canary := weighted("base", 60), weighted("canary", 40)
	snap := snapshotWith(t, base, canary)

	// A draw landing inside the canary's share while it is healthy: 40 of the
	// 100 total weight, so anything above 0.6 is the canary's.
	r := newRouter(t, defaultRegistry(), router.WithDraw(drawsOf(0.8)))
	if got := selectWith(t, r, snap, router.SelectionInput{}); got[0].Deployment.ID != "canary" {
		t.Fatalf("a healthy canary at weight 40 did not take a draw of 0.8: %q",
			got[0].Deployment.ID)
	}

	// Now fail it until its health collapses. Its share shrinks with it, so
	// the same draw no longer reaches it.
	sick := newRouter(t, defaultRegistry(),
		router.WithDraw(drawsOf(0.8)), router.WithMaxAttempts(1), router.WithRetryBackoff(0))
	for range 8 {
		one := []router.Candidate{{Deployment: canary}}
		_, _ = sick.Execute(t.Context(), one, time.Time{},
			failingCall(core.New(core.CodeUpstreamError, "down").AsRetryable()))
	}

	if got := selectWith(t, sick, snap, router.SelectionInput{}); got[0].Deployment.ID == "canary" {
		t.Fatal("a canary failing every request kept its full share of traffic")
	}
}

func TestASingleCandidateIsNeverDrawnFor(t *testing.T) {
	if got := headFor(t, 0.99, weighted("only", 7)); got != "only" {
		t.Fatalf("head = %q", got)
	}
}
