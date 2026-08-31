// Package contracts holds the per-port contract suites: a fixed battery that
// every implementation of a port must pass.
//
// They serve two purposes, and the second is why they live in a normal package
// rather than in _test.go files. First, they are how we test our own adapters
// without writing the same twelve assertions per provider. Second, they are the
// admission gate for the component registry: a third-party component cannot
// enter a snapshot until it has passed the suite for the port it claims to fill.
//
// That second use has a security consequence worth stating early. Running a
// contract suite against an untrusted component means executing untrusted code,
// so when the registry lands the runner must be an ephemeral sandbox — never
// in-process in the control plane. Moving the code execution somewhere is not
// the same as removing it.
package contracts
