// Package checkpoint provides types and interfaces for checkpoint storage.
//
// A checkpoint is the permanent record of a session's work: metadata and
// transcript stored on the entire/checkpoints/v1 branch (or a per-checkpoint
// ref) and linked to a code commit by its Entire-Checkpoint trailer. Work in
// progress between commits is tracked in session state, not in git.
//
// See docs/architecture/sessions-and-checkpoints.md for the full domain model.
package checkpoint
