// Package checkpoint defines the committed-checkpoint storage contract: the
// persisted metadata documents, the option types, the reader/writer
// interfaces, and the Write request union.
//
// It is the pluggable surface from issue #1433: a storage backend implements
// these interfaces and operates on these types without depending on the CLI's
// agent, TUI, or git-implementation packages. The git-backed implementation
// (GitStore, Open, the shadow-branch/temporary machinery) lives in the
// cmd/entire/cli/checkpoint package, which imports this one and re-exports
// these types as aliases so existing CLI call sites are unaffected.
package checkpoint
