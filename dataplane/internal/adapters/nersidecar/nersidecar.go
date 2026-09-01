// Package nersidecar detects names, locations and organisations by asking a
// sidecar over a Unix socket.
//
// A socket rather than a port, because the sidecar's entire input is
// unredacted personal data and it must not be reachable from anywhere but the
// worker beside it. A localhost port is reachable by every other container in
// the pod, and by anything outside it after one misconfiguration.
//
// A sidecar rather than a library because the models are Python, carry their
// own CVE surface, want to scale independently of the request path, and need a
// per-language recogniser registry — an English model misses Arabic entities
// almost entirely, and misses them silently.
package nersidecar

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/pii"
)

// DefaultTimeout bounds one call.
//
// A statistical pass is tens of milliseconds, so this is generous enough for a
// slow one and short enough that a hung sidecar cannot hold a request. It is
// the caller's deadline that matters, and this only caps the worst case.
const DefaultTimeout = 250 * time.Millisecond

// Detector asks the sidecar. Safe for concurrent use.
type Detector struct {
	client *http.Client
	// url is a placeholder host; the transport dials the socket regardless of
	// what is written here, and net/http still requires a syntactically valid
	// URL.
	url string
}

// Option configures a Detector.
type Option func(*Detector)

// WithTimeout bounds a single call.
func WithTimeout(d time.Duration) Option {
	return func(det *Detector) {
		if d > 0 {
			det.client.Timeout = d
		}
	}
}

// New returns a detector talking to the socket at path.
func New(socketPath string, opts ...Option) (*Detector, error) {
	if socketPath == "" {
		return nil, core.New(core.CodeInternal, "a NER sidecar needs a socket path")
	}

	dialer := &net.Dialer{}
	detector := &Detector{
		client: &http.Client{
			Timeout: DefaultTimeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dialer.DialContext(ctx, "unix", socketPath)
				},
				// The sidecar is one process on the same host, so a handful of
				// connections is plenty and pooling them avoids a socket
				// handshake per request.
				MaxIdleConns:        8,
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		url: "http://pii-ner",
	}
	for _, opt := range opts {
		opt(detector)
	}
	return detector, nil
}

// Name identifies the tier.
func (*Detector) Name() string { return "ner-sidecar" }

type detectRequest struct {
	Text     string `json:"text"`
	Language string `json:"language,omitempty"`
}

type detectedEntity struct {
	Kind  string  `json:"kind"`
	Start int     `json:"start"`
	End   int     `json:"end"`
	Value string  `json:"value"`
	Score float64 `json:"score"`
}

type detectResponse struct {
	Entities  []detectedEntity `json:"entities"`
	Backend   string           `json:"backend"`
	Truncated bool             `json:"truncated"`
}

// Detect asks the sidecar what it finds.
func (d *Detector) Detect(ctx context.Context, payload []byte) ([]pii.Match, error) {
	body, err := json.Marshal(detectRequest{Text: string(payload)})
	if err != nil {
		return nil, core.Wrap(core.CodeInternal, err, "encoding the detection request")
	}

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, d.url+"/v1/detect", bytes.NewReader(body))
	if err != nil {
		return nil, core.Wrap(core.CodeInternal, err, "building the detection request")
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, core.Wrap(core.CodeUnavailable, err, "calling the NER sidecar")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, core.Newf(core.CodeUnavailable, "NER sidecar returned %d", resp.StatusCode)
	}

	var decoded detectResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, core.Wrap(core.CodeUnavailable, err, "decoding the detection response")
	}

	return toMatches(payload, decoded.Entities), nil
}

// Ping checks the sidecar is answering, for startup.
//
// A misconfigured socket path would otherwise present as every classified
// request failing its detection — which, depending on the strategy, either
// refuses traffic or quietly sends less-protected payloads.
func (d *Detector) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.url+"/healthz", nil)
	if err != nil {
		return core.Wrap(core.CodeInternal, err, "building the health request")
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return core.Wrap(core.CodeUnavailable, err, "reaching the NER sidecar")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return core.Newf(core.CodeUnavailable, "NER sidecar health returned %d", resp.StatusCode)
	}
	return nil
}

// toMatches converts entities to matches, discarding any whose offsets do not
// line up with the payload.
//
// The sidecar reports byte offsets, and a mismatch means the two sides
// disagree about the encoding. Substituting on a bad offset would corrupt the
// payload at a position nobody chose, which is worse than missing the entity —
// so a match that does not verify is dropped rather than trusted.
func toMatches(payload []byte, entities []detectedEntity) []pii.Match {
	matches := make([]pii.Match, 0, len(entities))
	for _, entity := range entities {
		if entity.Start < 0 || entity.End > len(payload) || entity.Start >= entity.End {
			continue
		}
		if string(payload[entity.Start:entity.End]) != entity.Value {
			continue
		}
		matches = append(matches, pii.Match{
			Kind:  pii.Kind(entity.Kind),
			Start: entity.Start,
			End:   entity.End,
			Value: entity.Value,
		})
	}
	return matches
}
