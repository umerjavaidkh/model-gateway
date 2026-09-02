package gateway_test

import (
	"strings"
	"testing"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// The audit trail records decisions. What is *not* recorded matters as much as
// what is: a chain that grows by one entry per request costs what a chain
// cannot afford, and says nothing a usage record does not already say.

func TestAnOrdinaryRequestIsNotAudited(t *testing.T) {
	p, c := pipelineWithCollector(t)
	if _, err := p.Handle(t.Context(), buildSnapshot(t, snapshotOpts{}),
		request("echo-model", "gw_acme_secret-1")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if audits := c.auditEvents(); len(audits) != 0 {
		t.Fatalf("got %d audit events for an unclassified success, want none: %+v",
			len(audits), audits)
	}
	// The usage record still exists. Audit is a second, narrower trail, not a
	// replacement for the first.
	if len(c.all()) != 1 {
		t.Fatalf("got %d usage events, want one", len(c.all()))
	}
}

func TestARefusalIsAudited(t *testing.T) {
	p, c := pipelineWithCollector(t)
	// A key that authenticates against nothing: the refusal somebody disputes.
	_, err := p.Handle(t.Context(), buildSnapshot(t, snapshotOpts{}),
		request("echo-model", "gw_acme_not-a-real-secret"))
	if err == nil {
		t.Fatal("Handle accepted an unknown key")
	}

	audits := c.auditEvents()
	if len(audits) != 1 {
		t.Fatalf("got %d audit events, want exactly one: %+v", len(audits), audits)
	}
	got := audits[0]
	if got.Outcome != core.CodeUnauthenticated {
		t.Errorf("Outcome = %q, want %q", got.Outcome, core.CodeUnauthenticated)
	}
	if got.Action != string(core.EndpointChatCompletions) {
		t.Errorf("Action = %q", got.Action)
	}
	if got.RequestID != "req-1" {
		t.Errorf("RequestID = %q", got.RequestID)
	}
	// The model asked for, since no deployment ever served it.
	if got.Resource != "echo-model" {
		t.Errorf("Resource = %q, want the model that was requested", got.Resource)
	}
}

func TestAnAuditEventCarriesNoPayload(t *testing.T) {
	// The audit tap sits after redaction. A record that quoted the request
	// would make this table a copy of the data it exists to protect.
	p, c := pipelineWithCollector(t)
	secret := "swordfish-4242-4242-4242-4242"
	req := request("echo-model", "gw_acme_not-a-real-secret")
	req.Body = []byte(`{"model":"echo-model","messages":[{"role":"user","content":"` + secret + `"}]}`)

	if _, err := p.Handle(t.Context(), buildSnapshot(t, snapshotOpts{}), req); err == nil {
		t.Fatal("Handle accepted an unknown key")
	}

	for _, event := range c.auditEvents() {
		for field, value := range map[string]string{
			"Reason": event.Reason, "Resource": event.Resource, "Actor": event.Actor,
		} {
			if strings.Contains(value, secret) {
				t.Errorf("%s carried the payload: %q", field, value)
			}
		}
	}
}

func TestTheEventIDIsStableForOneDecision(t *testing.T) {
	// The stream is at-least-once, so the consumer must be able to recognise a
	// redelivered record as the same one rather than extending the chain twice.
	p, c := pipelineWithCollector(t)
	snap := buildSnapshot(t, snapshotOpts{})
	for range 2 {
		if _, err := p.Handle(t.Context(), snap, request("echo-model", "gw_acme_nope")); err == nil {
			t.Fatal("Handle accepted an unknown key")
		}
	}

	audits := c.auditEvents()
	if len(audits) != 2 {
		t.Fatalf("got %d audit events, want two", len(audits))
	}
	if audits[0].EventID != audits[1].EventID {
		t.Fatalf("same request id and action produced different event ids: %q and %q",
			audits[0].EventID, audits[1].EventID)
	}
	if !strings.HasPrefix(audits[0].EventID, "req-1:") {
		t.Fatalf("EventID = %q, want it derived from the request id", audits[0].EventID)
	}
}

func TestAStreamedRefusalIsAuditedToo(t *testing.T) {
	// The streaming path is a separate exit path, and a decision that goes
	// missing on one of two paths is worse than one that is never recorded:
	// the gap looks like an absence of refusals.
	p, c := pipelineWithCollector(t)
	if _, err := p.HandleStream(t.Context(), buildSnapshot(t, snapshotOpts{}),
		request("echo-model", "gw_acme_wrong")); err == nil {
		t.Fatal("HandleStream accepted an unknown key")
	}

	if audits := c.auditEvents(); len(audits) != 1 {
		t.Fatalf("got %d audit events from the streaming path, want one", len(audits))
	}
}

func TestAccessToClassifiedDataIsAuditedEvenWhenItSucceeds(t *testing.T) {
	// "Who touched the restricted material" cannot be reconstructed from
	// aggregates after the fact, so it has to be written down as it happens.
	p, c := pipelineWithCollector(t)
	snap := buildSnapshot(t, snapshotOpts{
		principal: func(pr *core.Principal) { pr.DefaultClass = core.DataClassRestricted },
	})

	if _, err := p.Handle(t.Context(), snap, request("echo-model", "gw_acme_secret-1")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	audits := c.auditEvents()
	if len(audits) != 1 {
		t.Fatalf("got %d audit events, want one: %+v", len(audits), audits)
	}
	if audits[0].Outcome != "" {
		t.Errorf("Outcome = %q, want empty: the access was allowed", audits[0].Outcome)
	}
	if !strings.Contains(audits[0].Reason, string(core.DataClassRestricted)) {
		t.Errorf("Reason = %q, want it to name the classification", audits[0].Reason)
	}
	// The deployment that served it, now that there is one.
	if audits[0].Resource != "echo-1" {
		t.Errorf("Resource = %q, want the deployment that served it", audits[0].Resource)
	}
}
