// Package core holds the gateway's domain vocabulary: the types, errors and port
// interfaces that every other package speaks.
//
// # The one rule
//
// core imports nothing from the rest of this module, and nothing outside the
// standard library. No HTTP, no database driver, no provider SDK, no logging
// framework. This is what keeps "every component is replaceable through a
// registry" true a year from now instead of only on day one. CI enforces it.
//
// # Immutability contract
//
// Values reachable from a [Snapshot] are built once by the control plane and then
// read concurrently by every in-flight request for the lifetime of that snapshot
// version. They are never mutated after construction. Accessors that return
// slices return snapshot-owned memory: callers may read it and must not write to
// it. This is the same contract the standard library uses for values like
// http.Header, and it is what lets the read path run without a lock.
package core
