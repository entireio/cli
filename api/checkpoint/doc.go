// Package checkpoint defines the persistent-checkpoint storage contract: the
// persisted metadata documents, the option types, the reader/writer
// interfaces, and the Write request union.
//
// It is the pluggable surface from issue #1433: a storage backend implements
// these interfaces and operates on these types without depending on the CLI's
// heavy agent runtime, TUI, or git-implementation packages. (It depends only on
// leaf value packages — agent/types, checkpoint/id — redact, and go-git
// plumbing.) The git-backed implementation (GitStore, Open, the facade, and the
// ephemeral shadow-branch surface) lives in cmd/entire/cli/checkpoint, which
// imports this package and re-exports these symbols as aliases so existing CLI
// call sites are unaffected.
package checkpoint
