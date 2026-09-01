package secrets

import (
	"context"
	"os"
	"strings"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// EnvScheme is the reference prefix this store answers to.
const EnvScheme = "env:"

// EnvStore reads secrets from environment variables, for references of the
// form "env:OPENAI_API_KEY".
//
// This is the development and single-tenant store. It is not suitable for a
// multi-tenant deployment: every tenant's credential would be visible to the
// whole process, and rotation means a restart. A Vault or KMS store fills the
// same interface without the request path noticing.
type EnvStore struct {
	// lookup is injected so tests do not mutate process environment.
	lookup func(string) (string, bool)
}

// NewEnvStore returns a store backed by the process environment.
func NewEnvStore() *EnvStore { return &EnvStore{lookup: os.LookupEnv} }

// NewEnvStoreWithLookup returns a store backed by a supplied lookup.
func NewEnvStoreWithLookup(lookup func(string) (string, bool)) *EnvStore {
	return &EnvStore{lookup: lookup}
}

// Fetch resolves an "env:NAME" reference.
func (s *EnvStore) Fetch(_ context.Context, ref string) ([]byte, error) {
	name, ok := strings.CutPrefix(ref, EnvScheme)
	if !ok {
		return nil, core.Newf(core.CodeInvalidRequest,
			"credential reference %q is not an %s reference", ref, EnvScheme)
	}
	if name == "" {
		return nil, core.New(core.CodeInvalidRequest, "credential reference names no variable")
	}

	value, found := s.lookup(name)
	if !found || value == "" {
		// The variable name is safe to report: it is operator configuration,
		// not a secret, and a missing credential is otherwise near-impossible
		// to diagnose from a 502.
		return nil, core.Newf(core.CodeUnavailable, "environment variable %s is not set", name)
	}
	return []byte(value), nil
}
