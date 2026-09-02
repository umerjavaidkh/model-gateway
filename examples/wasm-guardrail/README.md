# A guardrail as a WASM module

An in-process component, as a publisher would write one. Plain Go — the whole
toolchain is:

```bash
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o module.wasm .
```

or from the repository root:

```bash
make wasm-example
```

which prints the digest a manifest must carry.

## The interface

Two exported functions and no host functions at all:

| | |
|---|---|
| `alloc(size) -> ptr` | The host asks for space; the guest owns its allocator |
| `handle(ptr, len) -> ptr<<32\|len` | The request goes in, the response comes back |

The pair is packed into one `i64` because a WASM function returns a single
value. The request and response are the same JSON the sidecar protocol uses, so
moving a component between execution modes changes its build, not its logic.
`payload` is base64 in JSON — that is what Go's `encoding/json` does with
`[]byte`, and a guest in another language has to match it.

There is deliberately nothing else. Every host function would be a capability
granted to code nobody has vetted, and the reason this boundary is worth having
is that there are none: the module cannot open a file, dial a socket, read an
environment variable, or address anything but its own linear memory.

## What to know before shipping one

**A fresh instance runs every call**, so nothing survives between requests —
that is what makes running a third party's code in the worker's process
defensible. It costs about 1.6ms for a guest compiled from Go, most of it
reserving the module's linear memory. Declare a latency budget that accounts
for it: a binding that allows less time than the component needs fails at
admission rather than in production.

A leaner guest is much faster. TinyGo and Rust produce modules a fraction of
the size with correspondingly smaller memories.

**Keep allocations alive.** The host writes into the address `alloc` returned,
and if the collector frees that buffer first the corruption is intermittent and
gets blamed on anything but this. See `keepAlive` in `main.go`.

**Your module is addressed by its own digest.** Register it with
`"execution": "in_process"` and `"module": "sha256:<digest>"`; the runner and
every worker verify the bytes before compiling.
