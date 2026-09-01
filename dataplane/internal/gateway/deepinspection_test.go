package gateway_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/gateway"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/pii"
	pb "github.com/umerjavaidkh/model-gateway/dataplane/internal/wire/gatewayv1"
)

// deepInspectionPolicy allows everything and stamps one class, which is the
// shape a real rule takes: deep inspection is opt-in per rule because it costs
// tens of milliseconds, and the class decides what an unavailable tier means.
func deepInspectionPolicy(t *testing.T, class core.DataClass, deep bool) []byte {
	t.Helper()

	raw, err := proto.Marshal(&pb.PolicyBundle{
		Id: "test", Version: 1,
		Rules: []*pb.PolicyRule{{
			Id:             "classify",
			Effect:         pb.PolicyEffect_POLICY_EFFECT_ALLOW,
			DataClass:      string(class),
			DeepInspection: deep,
		}},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return raw
}

// stubDetector stands in for the sidecar. It records whether it was consulted,
// because "the sidecar was never called" and "the sidecar found nothing" look
// identical in the payload and mean very different things.
type stubDetector struct {
	mu      sync.Mutex
	calls   int
	matches []pii.Match
	err     error
}

func (d *stubDetector) Name() string { return "stub" }

func (d *stubDetector) Detect(context.Context, []byte) ([]pii.Match, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	return d.matches, d.err
}

func (d *stubDetector) called() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// capture records the body a deployment actually received, which is the only
// place the protection either happened or did not.
type capture struct {
	mu   sync.Mutex
	body []byte
}

func (c *capture) Name() string               { return "capture" }
func (c *capture) Endpoints() []core.Endpoint { return []core.Endpoint{core.EndpointChatCompletions} }

func (*capture) Probe(context.Context, core.Deployment, core.Credential) error { return nil }

func (c *capture) Invoke(_ context.Context, call *core.ProviderCall) (*core.ProviderResponse, error) {
	c.mu.Lock()
	c.body = append([]byte(nil), call.Body...)
	c.mu.Unlock()
	return &core.ProviderResponse{StatusCode: 200, Body: []byte(`{}`)}, nil
}

func (*capture) Stream(context.Context, *core.ProviderCall) (core.ChunkStream, error) {
	return nil, core.New(core.CodeInternal, "not used in this test")
}

func (c *capture) sent() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.body)
}

func externalOnly() []core.Deployment {
	return []core.Deployment{
		{ID: "ext-1", Key: routeExternal, Provider: "capture",
			TrustTier: core.TrustExternal, Weight: 100},
	}
}

func TestTheSidecarIsConsultedOnlyWhenARuleAsksForIt(t *testing.T) {
	for _, tc := range []struct {
		name      string
		deep      bool
		wantCalls int
	}{
		{"rule asks", true, 1},
		{"rule does not ask", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			detector := &stubDetector{}
			p := buildPipelineWith(t, []core.ProviderPort{&capture{}},
				gateway.WithNERDetector(detector))
			snap := buildSnapshot(t, snapshotOpts{
				deployments: externalOnly(),
				policy:      deepInspectionPolicy(t, core.DataClassInternal, tc.deep),
			})

			if _, err := p.Handle(t.Context(), snap, request("external-model", "gw_acme_secret-1")); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if got := detector.called(); got != tc.wantCalls {
				t.Fatalf("sidecar calls = %d, want %d", got, tc.wantCalls)
			}
		})
	}
}

func TestWhatTheSidecarFindsIsRemovedFromTheOutboundBody(t *testing.T) {
	// The deterministic tier does not know that "Ada Lovelace" is a person, so
	// a redaction here can only have come from the statistical tier.
	const name = "Ada Lovelace"
	body := `{"model":"external-model","messages":[{"content":"ask ` + name + `"}]}`

	// Offsets are located rather than written down: a transform trusts them,
	// and a hardcoded one that drifts silently mangles a different span.
	start := strings.Index(body, name)
	detector := &stubDetector{matches: []pii.Match{{
		Kind: "PERSON", Start: start, End: start + len(name), Value: name,
	}}}

	upstream := &capture{}
	p := buildPipelineWith(t, []core.ProviderPort{upstream},
		gateway.WithNERDetector(detector))
	snap := buildSnapshot(t, snapshotOpts{
		deployments: externalOnly(),
		policy:      deepInspectionPolicy(t, core.DataClassInternal, true),
	})

	req := request("external-model", "gw_acme_secret-1")
	req.Body = []byte(body)

	if _, err := p.Handle(t.Context(), snap, req); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	sent := upstream.sent()
	if strings.Contains(sent, name) {
		t.Fatalf("the name reached the provider: %s", sent)
	}
	if !strings.Contains(sent, "PERSON") {
		t.Fatalf("no placeholder took its place: %s", sent)
	}
	// A transform that replaces the wrong span still removes the name, so the
	// body has to survive as JSON for the redaction to mean anything.
	if !json.Valid([]byte(sent)) {
		t.Fatalf("the redacted body is no longer valid JSON: %s", sent)
	}
}

func TestTheMostSensitiveDataIsRefusedWhenTheTierThatWasAskedForDidNotRun(t *testing.T) {
	// Confidential data is tokenised, which is the level that exists because
	// the values matter. Sending it on deterministic detection alone would be
	// a silent downgrade of exactly the protection the rule asked for.
	for _, tc := range []struct {
		name     string
		detector pii.Detector
	}{
		{"the sidecar failed", &stubDetector{err: core.New(core.CodeUnavailable, "down")}},
		{"no sidecar is configured", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := buildPipelineWith(t, []core.ProviderPort{&capture{}},
				gateway.WithNERDetector(tc.detector))
			snap := buildSnapshot(t, snapshotOpts{
				deployments: externalOnly(),
				policy:      deepInspectionPolicy(t, core.DataClassConfidential, true),
			})

			_, err := p.Handle(t.Context(), snap, request("external-model", "gw_acme_secret-1"))
			if core.CodeOf(err) != core.CodeUnavailable {
				t.Fatalf("err = %v, want unavailable", err)
			}
		})
	}
}

func TestLessSensitiveDataProceedsOnWhatTheDeterministicTierFound(t *testing.T) {
	// Refusing here would turn one sidecar restart into a fleet-wide outage
	// for data whose protection does not depend on the statistical tier.
	detector := &stubDetector{err: core.New(core.CodeUnavailable, "down")}
	p := buildPipelineWith(t, []core.ProviderPort{&capture{}},
		gateway.WithNERDetector(detector))
	snap := buildSnapshot(t, snapshotOpts{
		deployments: externalOnly(),
		policy:      deepInspectionPolicy(t, core.DataClassInternal, true),
	})

	if _, err := p.Handle(t.Context(), snap, request("external-model", "gw_acme_secret-1")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}
