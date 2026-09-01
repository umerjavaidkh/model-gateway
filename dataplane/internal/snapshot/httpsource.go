package snapshot

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

const (
	// HeaderSnapshotDigest is what the control plane reports the current
	// snapshot's content address as.
	HeaderSnapshotDigest = "X-Snapshot-Digest"

	// maxSnapshotBytes bounds what a worker will read. A control plane that has
	// gone wrong should not be able to make every worker in the fleet allocate
	// until it is killed.
	maxSnapshotBytes = 64 << 20 // 64 MiB

	defaultFetchTimeout = 30 * time.Second
)

// HTTPSource fetches the snapshot from the control plane.
//
// It sends the digest it already holds as an If-None-Match, so an unchanged
// snapshot costs a header exchange rather than a full transfer and decode on
// every worker on every interval. A control plane that does not implement 304
// still works; it just transfers more.
type HTTPSource struct {
	client *http.Client
	url    string
	token  string
}

// HTTPOption configures an HTTPSource.
type HTTPOption func(*HTTPSource)

// WithHTTPClient replaces the client, for tests and custom transports.
func WithHTTPClient(c *http.Client) HTTPOption {
	return func(s *HTTPSource) { s.client = c }
}

// NewHTTPSource returns a source polling the given control-plane URL.
func NewHTTPSource(baseURL, token string, opts ...HTTPOption) (*HTTPSource, error) {
	if baseURL == "" {
		return nil, core.New(core.CodeInvalidRequest, "a control-plane URL is required")
	}
	s := &HTTPSource{
		client: &http.Client{Timeout: defaultFetchTimeout},
		url:    strings.TrimSuffix(baseURL, "/") + "/v1/snapshots/current",
		token:  token,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// Name identifies the source.
func (s *HTTPSource) Name() string { return "control-plane" }

// Fetch retrieves the current snapshot, or reports it unchanged.
func (s *HTTPSource) Fetch(ctx context.Context, knownDigest string) (Fetched, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return Fetched{}, core.Wrap(core.CodeInternal, err, "building the snapshot request")
	}
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	if knownDigest != "" {
		req.Header.Set("If-None-Match", knownDigest)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		// Unavailable rather than an error the caller might treat as fatal: a
		// control plane being unreachable is the normal degraded state, and the
		// worker keeps serving the snapshot it already has.
		return Fetched{}, core.Wrap(core.CodeUnavailable, err, "fetching the snapshot")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotModified {
		return Fetched{Digest: knownDigest, Unchanged: true}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return Fetched{}, core.Newf(core.CodeUnavailable,
			"control plane returned %d fetching the snapshot", resp.StatusCode)
	}

	// A control plane that does not implement 304 still gets the saving, as
	// long as it reports the digest: nothing is decoded when it matches.
	if reported := resp.Header.Get(HeaderSnapshotDigest); reported != "" && reported == knownDigest {
		return Fetched{Digest: knownDigest, Unchanged: true}, nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSnapshotBytes+1))
	if err != nil {
		return Fetched{}, core.Wrap(core.CodeUnavailable, err, "reading the snapshot response")
	}
	if len(body) > maxSnapshotBytes {
		return Fetched{}, core.Newf(core.CodeUnavailable,
			"snapshot exceeds the %d byte limit", maxSnapshotBytes)
	}

	snap, digest, err := decodeAndVerify(body)
	if err != nil {
		return Fetched{}, err
	}
	return Fetched{Snapshot: snap, Digest: digest}, nil
}
