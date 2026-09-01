package snapshot

import (
	"os"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/wire"
)

// LoadFile reads a serialized snapshot, verifies its layer digests, and decodes
// it into a servable snapshot.
//
// A file is the simplest possible snapshot source and is enough to run and test
// the whole request path without a control plane. The subscriber that watches a
// control plane for new versions arrives later and produces the same
// *core.Snapshot, so nothing downstream changes when it does.
func LoadFile(path string) (*core.Snapshot, error) {
	// #nosec G304 -- the path is operator configuration read at startup, never
	// caller input. A worker that cannot choose its own snapshot file has no
	// way to start.
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, core.Wrap(core.CodeUnavailable, err, "reading the snapshot file")
	}

	msg, err := wire.UnmarshalSnapshot(b)
	if err != nil {
		return nil, err
	}
	// Verify before decoding. A corrupted layer should be reported as corrupted,
	// not as whatever validation error the corruption happens to produce.
	if err := wire.VerifyGlobal(msg.GetGlobalLayer()); err != nil {
		return nil, err
	}
	for _, tenant := range msg.GetTenants() {
		if err := wire.VerifyTenant(tenant); err != nil {
			return nil, err
		}
	}
	return wire.DecodeSnapshot(msg)
}
