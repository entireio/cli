package checkpoint

import (
	"context"
	"fmt"
)

// EphemeralWriteRequest is a single shadow-branch (ephemeral) write command.
// The set is closed via the unexported marker; the store dispatches on the
// concrete type, mirroring the persistent WriteRequest union.
type EphemeralWriteRequest interface {
	isEphemeralWriteRequest()
}

// Step captures working-tree changes as an ephemeral (shadow-branch) checkpoint
// for a session step.
type Step WriteEphemeralOptions

// TaskStep captures a completed subagent task as an ephemeral (shadow-branch)
// checkpoint for a task step.
type TaskStep WriteEphemeralTaskOptions

func (Step) isEphemeralWriteRequest()     {}
func (TaskStep) isEphemeralWriteRequest() {}

// Write dispatches an ephemeral write request to the matching shadow-branch
// operation. The result carries the created (or existing) commit hash.
func (s *ephemeralStore) Write(ctx context.Context, req EphemeralWriteRequest) (WriteEphemeralResult, error) {
	switch r := req.(type) {
	case Step:
		return s.writeCheckpoint(ctx, WriteEphemeralOptions(r))
	case TaskStep:
		hash, err := s.writeTask(ctx, WriteEphemeralTaskOptions(r))
		return WriteEphemeralResult{CommitHash: hash}, err
	default:
		return WriteEphemeralResult{}, fmt.Errorf("checkpoint: unsupported ephemeral write request %T", req)
	}
}
