package recap

import "time"

const resumeThreshold = 6 * time.Hour

// ComputeBadges returns deterministic local badges for a session.
// priorSameBranch is the list of prior sessions on the same branch
// (sorted oldest first); it is used to detect 'resumed' gaps.
//
// Badge catalog:
//
//	active     - session has not ended (EndedAt is nil)
//	linked     - session has at least one linked commit via Entire-Checkpoint trailer
//	delegated  - session has at least one task checkpoint (subagent delegation)
//	resumed    - session started >= resumeThreshold after the prior same-branch session ended
//
// These are facts, not classifications. No server data required.
func ComputeBadges(s RecapSession, priorSameBranch []RecapSession) []string {
	badges := []string{}
	if s.EndedAt == nil {
		badges = append(badges, "active")
	}
	if len(s.LinkedCommits) > 0 {
		badges = append(badges, "linked")
	}
	for _, cp := range s.Checkpoints {
		if cp.IsTask {
			badges = append(badges, "delegated")
			break
		}
	}
	if len(priorSameBranch) > 0 {
		last := priorSameBranch[len(priorSameBranch)-1]
		endedAt := last.EndedAt
		if endedAt != nil && s.StartedAt.Sub(*endedAt) >= resumeThreshold {
			badges = append(badges, "resumed")
		}
	}
	return badges
}
