package recap

import "time"

// ActionHint names the primary next action surfaced on a session row.
// One hint per session — determined by NextAction's priority ordering.
type ActionHint string

const (
	ActionNone   ActionHint = ""
	ActionResume ActionHint = "resume"
	ActionCommit ActionHint = "commit"
	ActionPush   ActionHint = "push"
	ActionClean  ActionHint = "clean"
)

// NextAction picks a single primary action for the session. First match
// wins, so the order encodes priority:
//
//  1. Commit — any checkpoint isn't linked to a commit yet
//  2. Clean  — session ended more than 7 days ago with no follow-up
//  3. Resume — still active or idle with no pending commit work
//  4. None   — everything's settled
//
// Push is reserved for a future "pushed" marker on checkpoints. Today no
// source populates that, so Push is dormant — it lives in the enum so
// rendering can wire it in without re-ordering the priority later.
func NextAction(s RecapSession) ActionHint {
	if hasUncommittedCheckpoints(s) {
		return ActionCommit
	}
	if s.EndedAt != nil && time.Since(*s.EndedAt) > 7*24*time.Hour {
		return ActionClean
	}
	if s.IsActive || s.Phase == "IDLE" {
		return ActionResume
	}
	return ActionNone
}

func hasUncommittedCheckpoints(s RecapSession) bool {
	for _, cp := range s.Checkpoints {
		if cp.LinkedCommit == "" {
			return true
		}
	}
	return false
}
