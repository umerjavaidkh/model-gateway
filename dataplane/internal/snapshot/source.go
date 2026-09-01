package snapshot

import (
	"context"
	"os"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/wire"
)

// Fetched is the result of asking a source for the current snapshot.
//
// Unchanged is how a source says "you already have this" without transferring
// or decoding anything. That is what the content digest on each layer is for,
// and it is the difference between a poll costing a header exchange and a poll
// costing a full snapshot decode on every worker every interval.
type Fetched struct {
	Snapshot  *core.Snapshot
	Digest    string
	Unchanged bool
}

// Source produces the current snapshot.
//
// Implementations are the swappable part: a file for development and for
// bootstrap, HTTP against the control plane in a deployment, and later a watch
// stream. All three produce the same *core.Snapshot, so nothing downstream
// changes when one replaces another.
type Source interface {
	// Name identifies the source in logs.
	Name() string
	// Fetch returns the current snapshot, or reports it unchanged since the
	// digest the caller supplies.
	Fetch(ctx context.Context, knownDigest string) (Fetched, error)
}

// FileSource reads a serialized snapshot from disk.
//
// It is the development source and the bootstrap source: a worker can start
// with a snapshot supplied by its deployment even when the control plane is
// unreachable, which is what keeps a restart during a control-plane outage from
// being an outage of its own.
//
// This is read-only configuration handed to the worker, not state the worker
// writes. The data plane still keeps no durable state of its own.
type FileSource struct {
	path string
}

// NewFileSource returns a source reading the given path.
func NewFileSource(path string) *FileSource { return &FileSource{path: path} }

// Name identifies the source.
func (s *FileSource) Name() string { return "file:" + s.path }

// Fetch reads, verifies and decodes the file.
func (s *FileSource) Fetch(_ context.Context, knownDigest string) (Fetched, error) {
	// #nosec G304 -- the path is operator configuration read at startup, never
	// caller input.
	b, err := os.ReadFile(s.path)
	if err != nil {
		return Fetched{}, core.Wrap(core.CodeUnavailable, err, "reading the snapshot file")
	}

	snap, digest, err := decodeAndVerify(b)
	if err != nil {
		return Fetched{}, err
	}
	if digest != "" && digest == knownDigest {
		return Fetched{Digest: digest, Unchanged: true}, nil
	}
	return Fetched{Snapshot: snap, Digest: digest}, nil
}

// LoadFile reads a snapshot from disk in one call.
//
// Kept for the paths that want a snapshot rather than a source: the demo, the
// tests, and the worker's bootstrap before its subscriber starts.
func LoadFile(path string) (*core.Snapshot, error) {
	fetched, err := NewFileSource(path).Fetch(context.Background(), "")
	if err != nil {
		return nil, err
	}
	return fetched.Snapshot, nil
}

// decodeAndVerify parses a serialized snapshot, checks each layer's digest, and
// decodes it into the validated form.
//
// Verification happens before decoding so that corruption is reported as
// corruption, rather than as whatever validation error the corruption happens
// to produce.
func decodeAndVerify(b []byte) (*core.Snapshot, string, error) {
	msg, err := wire.UnmarshalSnapshot(b)
	if err != nil {
		return nil, "", err
	}
	if err := wire.VerifyGlobal(msg.GetGlobalLayer()); err != nil {
		return nil, "", err
	}
	for _, tenant := range msg.GetTenants() {
		if err := wire.VerifyTenant(tenant); err != nil {
			return nil, "", err
		}
	}

	snap, err := wire.DecodeSnapshot(msg)
	if err != nil {
		return nil, "", err
	}
	return snap, msg.GetGlobalLayer().GetVersion().GetDigest(), nil
}
