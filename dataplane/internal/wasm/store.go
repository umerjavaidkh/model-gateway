package wasm

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// ModuleStore reads WASM modules a component registry has admitted.
//
// A directory rather than a client for some artifact service, because
// distributing files is a solved problem an organisation already has an answer
// to — an init container, a volume, a sync sidecar. What is not solved, and is
// this package's job, is making sure the bytes that arrive are the bytes that
// were admitted.
type ModuleStore struct {
	dir string
}

// NewModuleStore returns a store reading modules from dir.
func NewModuleStore(dir string) (*ModuleStore, error) {
	if dir == "" {
		return nil, core.New(core.CodeInvalidRequest, "a module store needs a directory")
	}
	return &ModuleStore{dir: dir}, nil
}

// Dir reports where the store reads modules from.
func (s *ModuleStore) Dir() string { return s.dir }

// Load reads the module with the given digest and proves it is that module.
//
// Verifying rather than trusting the filename is the entire point. An
// admission record vouches for specific bytes; a worker that compiles whatever
// is at the expected path runs whatever an attacker with write access to a
// volume put there, and the registry's guarantee evaporates without any signal
// that it has.
func (s *ModuleStore) Load(digest string) ([]byte, error) {
	expected, err := parseDigest(digest)
	if err != nil {
		return nil, err
	}

	path := filepath.Join(s.dir, expected+".wasm")
	module, err := os.ReadFile(path) //nolint:gosec // the name is a verified hex digest, not caller input
	if err != nil {
		return nil, core.Wrapf(core.CodeUnavailable, err, "reading module %s", digest)
	}

	actual := sha256.Sum256(module)
	if hex.EncodeToString(actual[:]) != expected {
		return nil, core.Newf(core.CodeInvalidRequest,
			"module at %s hashes to sha256:%s, not the admitted %s",
			path, hex.EncodeToString(actual[:]), digest)
	}
	return module, nil
}

// parseDigest accepts the "sha256:<64 hex>" a manifest carries and returns the
// hex half, having established it is hex — which is what makes it safe to put
// in a path.
func parseDigest(digest string) (string, error) {
	algorithm, hexDigest, found := strings.Cut(digest, ":")
	if !found || algorithm != "sha256" || len(hexDigest) != 64 {
		return "", core.Newf(core.CodeInvalidRequest,
			"module reference %q is not a sha256 digest", digest)
	}
	if _, err := hex.DecodeString(hexDigest); err != nil {
		return "", core.Newf(core.CodeInvalidRequest,
			"module reference %q is not hexadecimal", digest)
	}
	return hexDigest, nil
}
