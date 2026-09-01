// Package config reads process settings from the environment.
package config

import (
	"fmt"
	"strconv"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// Defaults chosen to be safe rather than convenient: a worker that starts with
// a missing setting should fail loudly at boot, not misbehave under load.
const (
	DefaultListenAddr    = ":8080"
	DefaultReadTimeout   = 30 * time.Second
	DefaultWriteTimeout  = 5 * time.Minute // long, because model responses are
	DefaultIdleTimeout   = 120 * time.Second
	DefaultShutdownGrace = 30 * time.Second
	// DefaultTraceSampleRatio records a tenth of the traces this worker starts.
	// Not all of them: a gateway at any real rate would drown a collector.
	// Not fewer: a rare error is worth catching. Deciding which traces are
	// interesting is a collector's tail-sampling job, and this is what feeds it.
	DefaultTraceSampleRatio = 0.1
	// DefaultSnapshotInterval sits inside the plan's 30-second propagation
	// ceiling with room for one failed attempt.
	DefaultSnapshotInterval = 15 * time.Second
	minPepperBytes          = 32
)

// Config is the worker's settings. Every field is validated at load, so nothing
// downstream re-checks.
type Config struct {
	ListenAddr string
	// SnapshotFile is the bootstrap snapshot. Optional once a control plane is
	// configured, but keeping one lets a worker restart during a control-plane
	// outage and serve last-known configuration instead of nothing.
	//
	// It is read-only configuration supplied by the deployment, not state the
	// worker writes: the data plane still keeps nothing durable of its own.
	SnapshotFile string
	// ControlPlaneURL is polled for updates. Without it the worker serves the
	// bootstrap snapshot and never changes.
	ControlPlaneURL string
	// ControlPlaneToken authenticates to the control plane's admin listener.
	ControlPlaneToken string
	// SnapshotInterval is how often the control plane is polled.
	SnapshotInterval time.Duration

	// OTLPEndpoint is the collector traces are exported to. Empty disables
	// tracing: a worker must not fail or stall because no collector was
	// configured.
	OTLPEndpoint string
	// OTLPInsecure sends without TLS, for a collector in the same pod.
	OTLPInsecure bool
	// TraceSampleRatio is the fraction of traces started here that are
	// recorded. A caller's sampling decision is always honoured regardless.
	TraceSampleRatio float64

	// Region is where this worker runs. A deployment in the same region is
	// preferred during selection, which is local-first expressed as a weight
	// rather than a hard rule — the hard version makes a regional outage a
	// total outage.
	Region string

	// RedisURL makes rate limits fleet-wide. Without it limits are enforced
	// per worker, which is a legitimate deployment rather than a failure, so
	// this stays optional and the worker says which mode it is in.
	RedisURL string
	// KeyPepper keys the HMAC that turns a presented API key into the value a
	// snapshot indexes principals by. It must be the same across every worker
	// and the control plane that issued the keys, and it never enters a
	// snapshot.
	KeyPepper []byte

	ReadTimeout   time.Duration
	WriteTimeout  time.Duration
	IdleTimeout   time.Duration
	ShutdownGrace time.Duration
}

// Getenv is the environment lookup, passed in rather than read from os so that
// configuration is testable without mutating process state.
type Getenv func(string) string

// Load builds a Config from an environment lookup.
func Load(getenv Getenv) (Config, error) {
	cfg := Config{
		ListenAddr:        firstNonEmpty(getenv("GATEWAY_LISTEN_ADDR"), DefaultListenAddr),
		SnapshotFile:      getenv("GATEWAY_SNAPSHOT_FILE"),
		ControlPlaneURL:   getenv("GATEWAY_CONTROL_PLANE_URL"),
		ControlPlaneToken: getenv("GATEWAY_CONTROL_PLANE_TOKEN"),
		KeyPepper:         []byte(getenv("GATEWAY_KEY_PEPPER")),
		SnapshotInterval:  DefaultSnapshotInterval,
		OTLPEndpoint:      getenv("GATEWAY_OTLP_ENDPOINT"),
		RedisURL:          getenv("GATEWAY_REDIS_URL"),
		Region:            getenv("GATEWAY_REGION"),
		OTLPInsecure:      getenv("GATEWAY_OTLP_INSECURE") == "true",
		TraceSampleRatio:  DefaultTraceSampleRatio,
		ReadTimeout:       DefaultReadTimeout,
		WriteTimeout:      DefaultWriteTimeout,
		IdleTimeout:       DefaultIdleTimeout,
		ShutdownGrace:     DefaultShutdownGrace,
	}

	if raw := getenv("GATEWAY_TRACE_SAMPLE_RATIO"); raw != "" {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return Config{}, core.Wrap(core.CodeInvalidRequest, err,
				"GATEWAY_TRACE_SAMPLE_RATIO is not a number")
		}
		if parsed <= 0 || parsed > 1 {
			return Config{}, core.Newf(core.CodeInvalidRequest,
				"GATEWAY_TRACE_SAMPLE_RATIO must be in (0, 1], got %v", parsed)
		}
		cfg.TraceSampleRatio = parsed
	}

	if raw := getenv("GATEWAY_SNAPSHOT_INTERVAL"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, core.Wrap(core.CodeInvalidRequest, err,
				"GATEWAY_SNAPSHOT_INTERVAL is not a duration")
		}
		if parsed <= 0 {
			return Config{}, core.New(core.CodeInvalidRequest,
				"GATEWAY_SNAPSHOT_INTERVAL must be positive")
		}
		cfg.SnapshotInterval = parsed
	}

	// A worker with neither has no configuration at all and could only serve
	// errors, so it refuses to start rather than looking healthy.
	if cfg.SnapshotFile == "" && cfg.ControlPlaneURL == "" {
		return Config{}, core.New(core.CodeInvalidRequest,
			"one of GATEWAY_SNAPSHOT_FILE or GATEWAY_CONTROL_PLANE_URL is required")
	}
	// A short pepper is worse than an obviously missing one, because it looks
	// configured. 32 bytes is the HMAC-SHA256 block-appropriate minimum.
	if len(cfg.KeyPepper) < minPepperBytes {
		return Config{}, core.Newf(core.CodeInvalidRequest,
			"GATEWAY_KEY_PEPPER must be at least %d bytes, got %d", minPepperBytes, len(cfg.KeyPepper))
	}
	return cfg, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// String redacts the pepper, so a config dump in a log is not a credential
// leak.
func (c Config) String() string {
	return fmt.Sprintf(
		"listen=%s snapshot=%s control_plane=%s interval=%s otlp=%s sample=%v redis=%v "+
			"pepper=<%d bytes redacted> control_plane_token=<%d chars redacted>",
		c.ListenAddr, c.SnapshotFile, c.ControlPlaneURL, c.SnapshotInterval,
		c.OTLPEndpoint, c.TraceSampleRatio, c.RedisURL != "",
		len(c.KeyPepper), len(c.ControlPlaneToken))
}
