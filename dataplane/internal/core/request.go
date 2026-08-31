package core

import (
	"net/netip"
	"time"
)

// Endpoint is the API surface a request arrived on. Different endpoints have
// different payload shapes and different capability requirements, but they share
// one admission, routing and accounting path.
type Endpoint string

// The API surfaces the gateway serves.
const (
	EndpointChatCompletions Endpoint = "chat_completions"
	EndpointMessages        Endpoint = "messages"
	EndpointEmbeddings      Endpoint = "embeddings"
)

// PriorityClass lets a tenant say that batch traffic may be shed before
// interactive traffic when capacity is short.
type PriorityClass uint8

// The priority classes, in shedding order: batch is shed before interactive.
const (
	PriorityInteractive PriorityClass = iota
	PriorityBatch
)

// RequestMeta is everything about a request that admission, policy and routing
// need — deliberately without the body.
//
// The body stays opaque bytes until a ProviderPort translates it. Defining a
// normalized request schema in core would mean inventing, on day one, a union of
// every provider's payload; that schema would then be a compatibility surface we
// maintain forever, and it would be wrong in ways we cannot yet see. Metadata is
// the part every stage genuinely shares.
type RequestMeta struct {
	// RequestID is generated at ingress and is also the OTel trace ID, returned
	// to the caller in a response header so that a client-side error can be
	// correlated with a server-side trace.
	RequestID string

	Model    string // the alias or concrete model the caller asked for
	Endpoint Endpoint
	Stream   bool

	PayloadBytes int
	SourceIP     netip.Addr
	Region       string
	Priority     PriorityClass

	// IdempotencyKey, when the caller supplies one, makes a retried request
	// safe to deduplicate.
	IdempotencyKey string

	ReceivedAt time.Time
	// Deadline is the budget shared across *all* execution attempts, so that
	// three retries cannot together outlast the client's own timeout.
	Deadline time.Time
}
