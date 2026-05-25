package session

// AttachReason describes why a session was (or was not) attached to a
// checkpoint during PostCommit condensation. Attach reasons are persisted
// on CommittedMetadata; skip reasons are log-only.
type AttachReason string

const (
	// AttachReasonActiveRecentInteraction: ACTIVE session within the
	// activeSessionInteractionThreshold. PrepareCommitMsg already established
	// commit relatedness via the trailer, so no overlap check is required.
	AttachReasonActiveRecentInteraction AttachReason = "active_recent_interaction"

	// AttachReasonFileOverlap: stale ACTIVE or IDLE/ENDED, attached because
	// tracked files overlap the committed file set and contents match the
	// shadow branch.
	AttachReasonFileOverlap AttachReason = "file_overlap"

	// AttachReasonManual: session was imported via `entire attach` (or
	// `entire review attach`). Bypasses heuristic gates — user intent wins.
	AttachReasonManual AttachReason = "manual_attach"

	AttachSkipNoNewContent AttachReason = "skip_no_new_content"

	// AttachSkipReadOnlyActive: avoids attaching read-only agents (e.g. a
	// summarize tool) to commits owned by a different session.
	AttachSkipReadOnlyActive AttachReason = "skip_read_only_active"

	AttachSkipNoTrackedFiles     AttachReason = "skip_no_tracked_files"
	AttachSkipNoCommittedOverlap AttachReason = "skip_no_committed_overlap"
	AttachSkipContentMismatch    AttachReason = "skip_content_mismatch"
)

// IsAttach reports whether this reason resulted in the session being
// attached to the checkpoint. New AttachReason values must be added to one
// of the cases below — the exhaustive linter will flag forgotten additions.
func (r AttachReason) IsAttach() bool {
	switch r {
	case AttachReasonActiveRecentInteraction, AttachReasonFileOverlap, AttachReasonManual:
		return true
	case AttachSkipNoNewContent,
		AttachSkipReadOnlyActive,
		AttachSkipNoTrackedFiles,
		AttachSkipNoCommittedOverlap,
		AttachSkipContentMismatch:
		return false
	}
	return false
}
