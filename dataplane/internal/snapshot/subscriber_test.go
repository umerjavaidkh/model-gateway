package snapshot_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/snapshot"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/wire"
	pb "github.com/umerjavaidkh/model-gateway/dataplane/internal/wire/gatewayv1"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// scriptedSource returns whatever a test tells it to, so the subscriber's
// behaviour can be examined without a network.
type scriptedSource struct {
	mu        sync.Mutex
	next      snapshot.Fetched
	err       error
	calls     atomic.Int64
	lastKnown atomic.Pointer[string]
}

func (*scriptedSource) Name() string { return "scripted" }

func (s *scriptedSource) Fetch(_ context.Context, known string) (snapshot.Fetched, error) {
	s.calls.Add(1)
	s.lastKnown.Store(&known)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return snapshot.Fetched{}, s.err
	}
	return s.next, nil
}

func (s *scriptedSource) serve(f snapshot.Fetched, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next, s.err = f, err
}

func newSubscriber(t *testing.T, source snapshot.Source) (*snapshot.Subscriber, *snapshot.Holder) {
	t.Helper()
	holder, err := snapshot.New(build(t, 1, 1))
	if err != nil {
		t.Fatalf("snapshot.New: %v", err)
	}
	sub, err := snapshot.NewSubscriber(source, holder,
		snapshot.WithLogger(quiet()),
		snapshot.WithInterval(time.Millisecond),
		snapshot.WithBackoff(time.Millisecond, 2*time.Millisecond),
		// Deterministic scheduling: production jitter spreads the fleet, and a
		// test that inherits it is a test that flakes.
		snapshot.WithJitter(func(d time.Duration) time.Duration { return d }))
	if err != nil {
		t.Fatalf("NewSubscriber: %v", err)
	}
	return sub, holder
}

func TestRefreshAppliesANewSnapshot(t *testing.T) {
	source := &scriptedSource{}
	source.serve(snapshot.Fetched{Snapshot: build(t, 2, 1), Digest: "sha256:two"}, nil)
	sub, holder := newSubscriber(t, source)

	if err := sub.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := holder.Current().GlobalVersion().Number; got != 2 {
		t.Fatalf("holder is on version %d, want 2", got)
	}

	stats := sub.Stats()
	if stats.Applied != 1 || stats.Digest != "sha256:two" {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestTheKnownDigestIsSentBackSoAnUnchangedSnapshotIsFree(t *testing.T) {
	// This is what the content digest is for: an unchanged snapshot costs a
	// header exchange rather than a full transfer and decode on every worker on
	// every interval.
	source := &scriptedSource{}
	source.serve(snapshot.Fetched{Snapshot: build(t, 2, 1), Digest: "sha256:two"}, nil)
	sub, _ := newSubscriber(t, source)

	if err := sub.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	source.serve(snapshot.Fetched{Digest: "sha256:two", Unchanged: true}, nil)
	if err := sub.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if got := *source.lastKnown.Load(); got != "sha256:two" {
		t.Fatalf("the subscriber sent %q as its known digest", got)
	}
	if stats := sub.Stats(); stats.Unchanged != 1 || stats.Applied != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestAFailedFetchLeavesTheServedSnapshotAlone(t *testing.T) {
	// The property the whole design rests on: a control-plane outage degrades
	// to "configuration is frozen", not "traffic stops".
	source := &scriptedSource{}
	source.serve(snapshot.Fetched{}, errors.New("control plane is unreachable"))
	sub, holder := newSubscriber(t, source)

	if err := sub.Refresh(t.Context()); err == nil {
		t.Fatal("expected the failure to surface to the caller")
	}
	if got := holder.Current().GlobalVersion().Number; got != 1 {
		t.Fatalf("holder moved to version %d during an outage", got)
	}
	if stats := sub.Stats(); stats.Failed != 1 || stats.LastError == "" {
		t.Fatalf("stats = %+v, want the failure recorded", stats)
	}
}

func TestAStaleSnapshotIsRejectedAndCountedSeparately(t *testing.T) {
	// The holder refusing a version that moves backwards is it doing its job.
	// Counting it apart from a fetch failure is what lets an operator tell
	// "cannot reach the control plane" from "the control plane is serving
	// something older than we hold".
	source := &scriptedSource{}
	source.serve(snapshot.Fetched{Snapshot: build(t, 5, 1), Digest: "sha256:five"}, nil)
	sub, holder := newSubscriber(t, source)
	if err := sub.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	source.serve(snapshot.Fetched{Snapshot: build(t, 3, 1), Digest: "sha256:three"}, nil)
	if err := sub.Refresh(t.Context()); err == nil {
		t.Fatal("expected the stale snapshot to be rejected")
	}

	if got := holder.Current().GlobalVersion().Number; got != 5 {
		t.Fatalf("a rejected snapshot was applied: version %d", got)
	}
	stats := sub.Stats()
	if stats.Rejected != 1 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want one rejection and no fetch failures", stats)
	}
	// The digest is not advanced, so the next poll asks for the version the
	// worker actually holds rather than the one it refused.
	if stats.Digest != "sha256:five" {
		t.Fatalf("Digest = %q, want the applied one", stats.Digest)
	}
}

func TestTheLoopKeepsGoingAfterAFailure(t *testing.T) {
	// Giving up would leave the worker serving whatever it held when it
	// stopped, with nothing to say so.
	source := &scriptedSource{}
	source.serve(snapshot.Fetched{}, errors.New("down"))
	sub, holder := newSubscriber(t, source)

	sub.Start(t.Context())
	waitFor(t, func() bool { return sub.Stats().Failed >= 2 })

	source.serve(snapshot.Fetched{Snapshot: build(t, 7, 1), Digest: "sha256:seven"}, nil)
	waitFor(t, func() bool { return sub.Stats().Applied >= 1 })
	sub.Stop()

	if got := holder.Current().GlobalVersion().Number; got != 7 {
		t.Fatalf("holder is on version %d, want the recovered 7", got)
	}
}

func TestStopIsIdempotentAndEndsTheLoop(t *testing.T) {
	source := &scriptedSource{}
	source.serve(snapshot.Fetched{Unchanged: true}, nil)
	sub, _ := newSubscriber(t, source)

	sub.Start(t.Context())
	waitFor(t, func() bool { return sub.Stats().Unchanged >= 1 })
	sub.Stop()
	sub.Stop()

	settled := source.calls.Load()
	waitFor(t, func() bool { return true })
	if grew := source.calls.Load() - settled; grew > 1 {
		t.Fatalf("the source was polled %d more times after Stop", grew)
	}
}

func TestCancellingTheContextEndsTheLoop(t *testing.T) {
	source := &scriptedSource{}
	source.serve(snapshot.Fetched{Unchanged: true}, nil)
	sub, _ := newSubscriber(t, source)

	ctx, cancel := context.WithCancel(t.Context())
	sub.Start(ctx)
	waitFor(t, func() bool { return sub.Stats().Unchanged >= 1 })
	cancel()

	// Stop must return rather than block, or shutdown hangs on a worker whose
	// context was already cancelled.
	done := make(chan struct{})
	go func() { sub.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after the context was cancelled")
	}
}

func TestNewSubscriberRejectsMissingDependencies(t *testing.T) {
	holder, _ := snapshot.New(build(t, 1, 1))
	if _, err := snapshot.NewSubscriber(nil, holder); err == nil {
		t.Fatal("a subscriber with no source must be refused")
	}
	if _, err := snapshot.NewSubscriber(&scriptedSource{}, nil); err == nil {
		t.Fatal("a subscriber with no holder must be refused")
	}
}

// --- HTTP source -------------------------------------------------------------

func serialized(t *testing.T, version uint64) ([]byte, string) {
	t.Helper()

	global := wire.EncodeGlobal(core.GlobalSpec{
		Version: core.LayerVersion{Number: version},
		Deployments: []core.Deployment{
			{ID: "d1", Key: core.RoutingKey{BaseModel: "m"}, Provider: "echo",
				TrustTier: core.TrustInternal, Weight: 100},
		},
		TenantPrefixes: map[core.KeyPrefix]core.TenantID{"acme": "acme"},
	})
	tenant := wire.EncodeTenant(core.TenantSpec{Tenant: "acme", Version: core.LayerVersion{Number: 1}})
	if err := wire.SealGlobal(global); err != nil {
		t.Fatalf("SealGlobal: %v", err)
	}
	if err := wire.SealTenant(tenant); err != nil {
		t.Fatalf("SealTenant: %v", err)
	}

	b, err := wire.Marshal(&pb.Snapshot{GlobalLayer: global, Tenants: []*pb.TenantLayer{tenant}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return b, global.GetVersion().GetDigest()
}

func TestHTTPSourceFetchesAndVerifies(t *testing.T) {
	body, digest := serialized(t, 3)
	var gotAuth, gotNoneMatch string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotNoneMatch = r.Header.Get("If-None-Match")
		w.Header().Set(snapshot.HeaderSnapshotDigest, digest)
		_, _ = w.Write(body)
	}))
	defer server.Close()

	source, err := snapshot.NewHTTPSource(server.URL, "admin-token")
	if err != nil {
		t.Fatalf("NewHTTPSource: %v", err)
	}
	fetched, err := source.Fetch(t.Context(), "")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if fetched.Snapshot.GlobalVersion().Number != 3 {
		t.Fatalf("version = %d", fetched.Snapshot.GlobalVersion().Number)
	}
	if gotAuth != "Bearer admin-token" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotNoneMatch != "" {
		t.Fatalf("If-None-Match = %q on a first fetch", gotNoneMatch)
	}
}

func TestHTTPSourceHonoursNotModified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == "sha256:known" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		t.Errorf("If-None-Match = %q, want the known digest", r.Header.Get("If-None-Match"))
	}))
	defer server.Close()

	source, _ := snapshot.NewHTTPSource(server.URL, "")
	fetched, err := source.Fetch(t.Context(), "sha256:known")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !fetched.Unchanged || fetched.Snapshot != nil {
		t.Fatalf("fetched = %+v, want unchanged with no snapshot", fetched)
	}
}

func TestHTTPSourceSkipsDecodeWhenTheDigestMatches(t *testing.T) {
	// A control plane that does not implement 304 still gets the saving, as
	// long as it reports the digest.
	body, digest := serialized(t, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(snapshot.HeaderSnapshotDigest, digest)
		_, _ = w.Write(body)
	}))
	defer server.Close()

	source, _ := snapshot.NewHTTPSource(server.URL, "")
	fetched, err := source.Fetch(t.Context(), digest)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !fetched.Unchanged {
		t.Fatal("a matching digest must short-circuit the decode")
	}
}

func TestHTTPSourceReportsAnUnreachableControlPlaneAsUnavailable(t *testing.T) {
	// Unavailable rather than something a caller might treat as fatal: an
	// unreachable control plane is the normal degraded state.
	source, _ := snapshot.NewHTTPSource("http://127.0.0.1:1", "")
	_, err := source.Fetch(t.Context(), "")
	if !errors.Is(err, core.ErrUnavailable) {
		t.Fatalf("err = %v, want unavailable", err)
	}
}

func TestHTTPSourceRejectsAnErrorStatusAndCorruptBody(t *testing.T) {
	unauthorized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer unauthorized.Close()

	source, _ := snapshot.NewHTTPSource(unauthorized.URL, "")
	if _, err := source.Fetch(t.Context(), ""); err == nil {
		t.Fatal("a 401 must surface")
	}

	corrupt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte{0xff, 0xff, 0xff})
	}))
	defer corrupt.Close()

	source, _ = snapshot.NewHTTPSource(corrupt.URL, "")
	if _, err := source.Fetch(t.Context(), ""); err == nil {
		t.Fatal("an unparseable body must surface")
	}
}

func TestHTTPSourceRejectsATamperedSnapshot(t *testing.T) {
	// Verification happens before decoding, so corruption is reported as
	// corruption rather than as whatever validation error it happens to cause.
	body, digest := serialized(t, 3)
	msg, err := wire.UnmarshalSnapshot(body)
	if err != nil {
		t.Fatalf("UnmarshalSnapshot: %v", err)
	}
	msg.GetGlobalLayer().GetDeployments()[0].Weight = 50
	tampered, err := wire.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(snapshot.HeaderSnapshotDigest, digest)
		_, _ = w.Write(tampered)
	}))
	defer server.Close()

	source, _ := snapshot.NewHTTPSource(server.URL, "")
	if _, err := source.Fetch(t.Context(), "sha256:something-else"); err == nil {
		t.Fatal("a tampered snapshot must fail its digest check")
	}
}

func TestNewHTTPSourceRequiresAURL(t *testing.T) {
	if _, err := snapshot.NewHTTPSource("", "token"); err == nil {
		t.Fatal("a source with no URL must be refused")
	}
}

// waitFor polls a condition rather than sleeping a fixed amount, so the test is
// as fast as the machine allows and does not flake on a slow one.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met within the deadline")
}
