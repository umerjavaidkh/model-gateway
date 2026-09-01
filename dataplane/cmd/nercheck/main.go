// Command nercheck asks a running PII NER sidecar about text whose byte and
// character offsets differ, and fails if the answer does not survive the
// client's own verification.
//
// A cross-language check rather than a test, because the thing being checked is
// two processes in two languages agreeing — which is exactly what unit tests on
// either side cannot observe.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/adapters/nersidecar"
	"github.com/umerjavaidkh/model-gateway/dataplane/internal/pii"
)

// The emoji is four bytes, so every offset after it differs from its character
// index. A pure-ASCII payload would pass whichever convention each side used.
const payload = `{"messages":[{"content":"🙂 please email Dr. Ada Lovelace at Contoso Ltd"}]}`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "nercheck:", err)
		os.Exit(1)
	}
}

func run() error {
	socket := os.Getenv("PII_NER_SOCKET")
	if socket == "" {
		return errors.New("PII_NER_SOCKET is not set")
	}

	detector, err := nersidecar.New(socket, nersidecar.WithTimeout(5*time.Second))
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := detector.Ping(ctx); err != nil {
		return fmt.Errorf("the sidecar is not healthy: %w", err)
	}

	matches, err := detector.Detect(ctx, []byte(payload))
	if err != nil {
		return err
	}
	// Empty means either the sidecar found nothing or the client dropped
	// everything it reported. Both are the failure this check exists to catch.
	if len(matches) == 0 {
		return errors.New("no entity survived; the two sides disagree about offsets")
	}

	for _, m := range matches {
		fmt.Printf("    %-12s %q at [%d,%d)\n", m.Kind, m.Value, m.Start, m.End)
	}

	// The offsets have to be usable, not merely reported: the transform
	// substitutes on them, and that is where a wrong one does its damage.
	result := pii.TransformMatches([]byte(payload), matches, pii.StrategyRedact, nil)
	protected := string(result.Payload)
	for _, m := range matches {
		if strings.Contains(protected, m.Value) {
			return fmt.Errorf("%q survived the transform: %s", m.Value, protected)
		}
	}
	fmt.Printf("    redacted: %s\n", protected)
	return nil
}
