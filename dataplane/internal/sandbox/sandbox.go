// Package sandbox runs an untrusted component under a container runtime with
// every isolation the runtime offers, so a contract suite can be run against it
// without trusting it.
//
// This is the security boundary of the whole component registry. Everything
// else in the registry decides *whether* a component may be bound; this is the
// only place that decides what a component can do while it is being decided
// about, and admission is the one moment the gateway deliberately executes code
// nobody has vetted yet.
//
// # What the boundary is, and is not
//
// It is a container with no network, a read-only root, no capabilities, no
// privilege escalation, a memory and process cap, a non-root user, and a hard
// wall-clock deadline. That stops the ordinary failures: a component that
// dials out to fetch its real payload, that writes to the host, that forks
// until the box falls over, that never exits.
//
// It is not a defence against a kernel escape. A container shares a kernel, so
// a component with a local privilege-escalation exploit is not contained by
// any flag in this file. Deployments that admit genuinely untrusted third-party
// code should point Runtime at a VM-isolated runtime — gVisor or Kata present
// the same command-line interface — and this package deliberately does not
// hardcode which binary that is.
//
// Nothing here runs in the control plane. See docs/adr/0009.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/umerjavaidkh/model-gateway/dataplane/internal/core"
)

// Defaults are deliberately small. A contract suite exercises a handful of
// payloads; a component that needs more than this to answer them is not
// something to admit on the strength of a suite run.
const (
	DefaultMemoryMB  = 512
	DefaultCPUs      = "1.0"
	DefaultPidsLimit = 64
	DefaultTimeout   = 2 * time.Minute
	// DefaultRuntime is the command used to run a container. Overridable
	// because the right answer differs per deployment: docker locally, and a
	// VM-isolated runtime anywhere genuinely untrusted code is admitted.
	DefaultRuntime = "docker"
)

// Limits are the resource bounds a sandboxed component runs under.
type Limits struct {
	MemoryMB  int
	CPUs      string
	PidsLimit int
	// Timeout is the wall clock for the whole run, enforced by this package
	// rather than by the runtime. A container told to stop is a container that
	// might not; the process is killed either way.
	Timeout time.Duration
}

func (l Limits) withDefaults() Limits {
	if l.MemoryMB <= 0 {
		l.MemoryMB = DefaultMemoryMB
	}
	if l.CPUs == "" {
		l.CPUs = DefaultCPUs
	}
	if l.PidsLimit <= 0 {
		l.PidsLimit = DefaultPidsLimit
	}
	if l.Timeout <= 0 {
		l.Timeout = DefaultTimeout
	}
	return l
}

// Spec is one component to run.
type Spec struct {
	// Image must be pinned by digest. The registry enforces that on the
	// manifest; this enforces it again, because a sandbox that runs whatever a
	// tag currently points at is testing something other than what was
	// submitted.
	Image string
	// SocketDir is a host directory mounted into the container, where the
	// component is expected to bind its Unix socket. It is the only writable
	// path the component gets.
	SocketDir string
	// SocketName is the file the component binds inside SocketDir.
	SocketName string
	Limits     Limits
}

// Runner starts a container and reports how to reach it.
//
// An interface so the isolation flags can be asserted without a container
// runtime installed. A test that needs Docker to check that "--network=none"
// is passed is a test that does not run, and the flags are the part most worth
// checking.
type Runner interface {
	Start(ctx context.Context, spec Spec) (*Handle, error)
}

// Handle is a running sandbox.
type Handle struct {
	// SocketPath is the host path of the component's socket.
	SocketPath string

	stop     func()
	waitErr  chan error
	stopOnce sync.Once
}

// NewHandle builds a Handle for an alternative Runner.
//
// Exported because Runner is an interface: a deployment with its own isolation
// mechanism — a VM per run, a remote sandbox service — implements Start and
// needs to return the same handle type the runner closes.
func NewHandle(socketPath string, stop func()) *Handle {
	exited := make(chan error, 1)
	exited <- nil
	return &Handle{SocketPath: socketPath, stop: stop, waitErr: exited}
}

// Close tears the sandbox down. Safe to call more than once.
func (h *Handle) Close() error {
	h.stopOnce.Do(h.stop)
	select {
	case err := <-h.waitErr:
		return err
	case <-time.After(10 * time.Second):
		return core.New(core.CodeUnavailable, "the sandbox did not exit")
	}
}

// Container runs components under a container runtime.
type Container struct {
	runtime string
	// name prefixes the container's name, so a leaked one is identifiable.
	name func() string
}

// Option configures a Container.
type Option func(*Container)

// WithRuntime sets the container command. Use it to point at a VM-isolated
// runtime where the extra isolation is warranted.
func WithRuntime(command string) Option {
	return func(c *Container) {
		if command != "" {
			c.runtime = command
		}
	}
}

// New returns a Container runner.
func New(opts ...Option) *Container {
	c := &Container{
		runtime: DefaultRuntime,
		name: func() string {
			return "gw-admission-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Args builds the runtime invocation for a spec.
//
// Exported so the isolation can be asserted directly. These flags are the
// security boundary, and a boundary that is only exercised when a container
// runtime happens to be installed is one that silently stops being applied.
func (c *Container) Args(spec Spec, name string) ([]string, error) {
	if err := validate(spec); err != nil {
		return nil, err
	}
	limits := spec.Limits.withDefaults()

	return []string{
		"run", "--rm", "--name", name,
		// No network at all. A component that fetches its real behaviour at
		// startup would otherwise pass a suite that tested something else.
		"--network=none",
		// The filesystem is read-only except the socket directory, which is
		// the one thing the component has to write.
		"--read-only",
		"--tmpfs", "/tmp:rw,noexec,nosuid,size=16m",
		"--volume", spec.SocketDir + ":" + containerSocketDir + ":rw",
		// Nothing the component does can gain a capability it was not started
		// with, including through a setuid binary in its own image.
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		// Not root, even inside a namespace: a container escape starts from
		// whatever the process already is.
		"--user", "65534:65534",
		"--memory", strconv.Itoa(limits.MemoryMB) + "m",
		"--memory-swap", strconv.Itoa(limits.MemoryMB) + "m",
		"--cpus", limits.CPUs,
		"--pids-limit", strconv.Itoa(limits.PidsLimit),
		"--env", "COMPONENT_SOCKET=" + filepath.Join(containerSocketDir, spec.SocketName),
		spec.Image,
	}, nil
}

// containerSocketDir is where the host's socket directory is mounted. Fixed
// rather than configurable: the component learns the path from an environment
// variable, and two ways to say the same thing is one of them being wrong.
const containerSocketDir = "/run/component"

// Start launches the component and waits for its socket to appear.
func (c *Container) Start(ctx context.Context, spec Spec) (*Handle, error) {
	name := c.name()
	args, err := c.Args(spec, name)
	if err != nil {
		return nil, err
	}
	limits := spec.Limits.withDefaults()

	// The deadline is this package's, not the runtime's. A container told to
	// stop is a container that might not, and the wall clock has to be
	// enforced by something that can kill the process.
	runCtx, cancel := context.WithTimeout(ctx, limits.Timeout)

	cmd := exec.CommandContext(runCtx, c.runtime, args...) //nolint:gosec // the runtime is operator-configured
	var output strings.Builder
	cmd.Stdout = &output
	cmd.Stderr = &output

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, core.Wrapf(core.CodeUnavailable, err, "starting %s", c.runtime)
	}

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	handle := &Handle{
		SocketPath: filepath.Join(spec.SocketDir, spec.SocketName),
		waitErr:    waitErr,
		stop: func() {
			cancel()
			// Killing the client process leaves the container running under
			// most runtimes, so the container is removed explicitly. Best
			// effort: a leaked container is a problem, and failing the run
			// because cleanup failed does not un-leak it.
			removeCtx, removeCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			defer removeCancel()
			_ = exec.CommandContext(removeCtx, c.runtime, "rm", "-f", name).Run() //nolint:gosec // same
		},
	}

	if err := awaitSocket(runCtx, handle.SocketPath, waitErr); err != nil {
		_ = handle.Close()
		return nil, core.Wrapf(core.CodeUnavailable, err,
			"the component never bound %s (output: %s)",
			spec.SocketName, truncate(output.String()))
	}
	return handle, nil
}

// awaitSocket waits for the component to bind, or for it to exit first.
//
// Both outcomes matter: a component that crashes on startup would otherwise be
// waited on until the deadline, and the report would say "timed out" when what
// happened is that it exited immediately with an error worth reading.
func awaitSocket(ctx context.Context, path string, exited chan error) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		if info, err := os.Stat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
			return nil
		}
		select {
		case err := <-exited:
			// Put it back so Close can report it too. The channel is buffered
			// with room for exactly this value and was just drained, so the
			// send cannot block.
			exited <- err
			if err != nil {
				return fmt.Errorf("the component exited during startup: %w", err)
			}
			return errors.New("the component exited during startup")
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func validate(spec Spec) error {
	if spec.Image == "" {
		return core.New(core.CodeInvalidRequest, "a sandbox needs an image")
	}
	if !strings.Contains(spec.Image, "@sha256:") {
		// The registry already refuses a manifest pinned by tag. Checked again
		// here because this is the process that actually runs the bytes, and a
		// boundary that trusts its caller to have validated is not one.
		return core.Newf(core.CodeInvalidRequest,
			"image %q is not pinned by digest; the sandbox would run whatever the tag points at now",
			spec.Image)
	}
	if spec.SocketDir == "" || spec.SocketName == "" {
		return core.New(core.CodeInvalidRequest, "a sandbox needs a socket directory and name")
	}
	if strings.ContainsAny(spec.SocketName, `/\`) {
		return core.Newf(core.CodeInvalidRequest,
			"socket name %q must be a file name, not a path", spec.SocketName)
	}
	return nil
}

func truncate(s string) string {
	const limit = 2000
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
