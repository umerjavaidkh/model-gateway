package rediskv_test

import (
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/adapters/rediskv"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/contracts"
)

// The suite runs twice: against an in-process Redis for fast local feedback,
// and against a real server when GATEWAY_TEST_REDIS_URL names one, which CI
// always does and which is the gate.
//
// Same shape as the Postgres arrangement, and the same reasoning: an emulator
// is not the thing being deployed, so it cannot be the only target. What it is
// good for is catching an obvious error in a second rather than in a CI run —
// the Lua script and the PTTL semantics it depends on are exactly the kind of
// thing to get wrong on the first attempt.

func uniquePrefix() string {
	return "test:" + strconv.FormatInt(time.Now().UnixNano(), 36) + ":"
}

func TestSatisfiesKVStoreOnAnEmulator(t *testing.T) {
	contracts.RunKVStoreSuite(t, func(t *testing.T) contracts.KVTarget {
		t.Helper()
		server := miniredis.RunT(t)
		store, err := rediskv.Open("redis://" + server.Addr())
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })

		// The emulator never expires anything on its own; it has to be told.
		// Without this the TTL case fails in a way that looks exactly like a
		// broken script.
		return contracts.KVTarget{
			Store:   store,
			Prefix:  uniquePrefix(),
			Advance: server.FastForward,
		}
	})
}

func TestSatisfiesKVStoreOnARealServer(t *testing.T) {
	url := os.Getenv("GATEWAY_TEST_REDIS_URL")
	if url == "" {
		t.Skip("set GATEWAY_TEST_REDIS_URL to run against a real server")
	}

	contracts.RunKVStoreSuite(t, func(t *testing.T) contracts.KVTarget {
		t.Helper()
		store, err := rediskv.Open(url)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })

		// No Advance: a real server only knows wall time, so the suite sleeps.
		return contracts.KVTarget{Store: store, Prefix: uniquePrefix()}
	})
}

func TestOpenRejectsAMalformedURL(t *testing.T) {
	if _, err := rediskv.Open("not-a-url"); err == nil {
		t.Fatal("a malformed URL must be refused at startup, not at first use")
	}
}

func TestNewRejectsANilClient(t *testing.T) {
	if _, err := rediskv.New(nil); err == nil {
		t.Fatal("a store with no client would fail open on every request while looking configured")
	}
}

func TestPingReportsAnUnreachableServer(t *testing.T) {
	// Port 1 is reserved and nothing listens on it.
	store, err := rediskv.Open("redis://127.0.0.1:1")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.Ping(t.Context()); err == nil {
		t.Fatal("an unreachable server must be reported at startup")
	}
}
