// Package ci hosts the `entire ci` command group: internal-only tooling for
// managing Entire's CI integrations (Buildkite, …).
//
// The command tree is compiled only under the `internal` build tag
// (register_internal.go). The public, untagged build gets a no-op Register
// (register_stub.go), so `entire ci` never appears in a released binary.
// This doc.go carries no build tag so exactly one package comment exists
// under either build configuration.
package ci
