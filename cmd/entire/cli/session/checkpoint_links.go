package session

import (
	"slices"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// CheckpointLinkKind says where a session's checkpoint lives.
type CheckpointLinkKind string

const (
	// CheckpointLinkHome is a checkpoint in the session's home repository,
	// holding its transcript.
	CheckpointLinkHome CheckpointLinkKind = "home"
	// CheckpointLinkCrossRepo is a marker checkpoint another repository wrote
	// for a commit the session's agent made there.
	CheckpointLinkCrossRepo CheckpointLinkKind = "cross_repo"
)

// CheckpointLink is one entry of State.CheckpointLinks.
type CheckpointLink struct {
	CheckpointID id.CheckpointID `json:"checkpoint_id"`
	// RepoKey names the repository holding the checkpoint by its normalized
	// origin, else its directory name (never a local path); empty for a
	// migrated entry.
	RepoKey string `json:"repo_key,omitempty"`
	// LinkedCommits are the commits (full SHAs) the checkpoint was written
	// for, in order. Empty for a checkpoint no commit carries (session end,
	// explicit snapshot) or a migrated entry.
	LinkedCommits []string           `json:"linked_commits,omitempty"`
	TurnID        string             `json:"turn_id,omitempty"`
	Kind          CheckpointLinkKind `json:"kind"`
	At            time.Time          `json:"at,omitzero"`
	// CommittedAt orders the list: the git committer time of the first
	// linked commit, or the condensation time when no commit carries the
	// checkpoint. Entries written before it existed fall back to At.
	CommittedAt time.Time `json:"committed_at,omitzero"`
}

// orderKey is when the link sorts in the list; zero for a migrated entry
// with neither time, which keeps its position.
func (l CheckpointLink) orderKey() time.Time {
	if !l.CommittedAt.IsZero() {
		return l.CommittedAt
	}
	return l.At
}

// AddCheckpointLink records link, keyed on its checkpoint ID: a link already
// listed gains only the commits it did not name. It reports whether the list
// changed. A link without a checkpoint ID is ignored.
func (s *State) AddCheckpointLink(link CheckpointLink) bool {
	if link.CheckpointID.IsEmpty() {
		return false
	}
	i := slices.IndexFunc(s.CheckpointLinks, func(l CheckpointLink) bool { return l.CheckpointID == link.CheckpointID })
	if i < 0 {
		link.LinkedCommits = slices.Clone(link.LinkedCommits)
		s.CheckpointLinks = slices.Insert(s.CheckpointLinks, s.insertionIndex(link.orderKey()), link)
		return true
	}
	existing := &s.CheckpointLinks[i]
	changed := false
	for _, sha := range link.LinkedCommits {
		if sha != "" && !slices.Contains(existing.LinkedCommits, sha) {
			existing.LinkedCommits = append(existing.LinkedCommits, sha)
			changed = true
		}
	}
	return changed
}

// insertionIndex places a link with order key at in commit order: after
// every entry not later than it (so ties keep insertion order) and after any
// entry without a time, which stays where it was. A link without a time goes
// last.
func (s *State) insertionIndex(at time.Time) int {
	if at.IsZero() {
		return len(s.CheckpointLinks)
	}
	i := len(s.CheckpointLinks)
	for i > 0 {
		key := s.CheckpointLinks[i-1].orderKey()
		if key.IsZero() || !key.After(at) {
			break
		}
		i--
	}
	return i
}

// PreviousCheckpoint returns the checkpoint preceding cpID in commit order,
// or empty when cpID is its first or not listed.
func (s *State) PreviousCheckpoint(cpID id.CheckpointID) id.CheckpointID {
	i := slices.IndexFunc(s.CheckpointLinks, func(l CheckpointLink) bool { return l.CheckpointID == cpID })
	if i <= 0 {
		return id.EmptyCheckpointID
	}
	return s.CheckpointLinks[i-1].CheckpointID
}

// RemoveCheckpointLink drops cpID from the list (the checkpoint was deleted).
func (s *State) RemoveCheckpointLink(cpID id.CheckpointID) {
	s.CheckpointLinks = slices.DeleteFunc(s.CheckpointLinks, func(l CheckpointLink) bool { return l.CheckpointID == cpID })
}

// EntireSessionID is the session's identity across repositories; see
// EntireSessionIDFor.
func (s *State) EntireSessionID() string {
	return EntireSessionIDFor(s.AgentType, s.SessionID)
}

// EntireSessionIDFor derives an Entire session ID, "<agent>:<agent session
// ID>", where agent is the registry name (e.g. claude-code), or the agent type
// itself when it is not registered. It is never stored.
func EntireSessionIDFor(agentType types.AgentType, sessionID string) string {
	if agentType == "" || sessionID == "" {
		return sessionID
	}
	name := string(agentType)
	if ag, err := agent.GetByAgentType(agentType); err == nil {
		name = string(ag.Name())
	}
	return name + ":" + sessionID
}

// seedCheckpointLinks migrates a state written before CheckpointLinks
// existed: its most recent checkpoint becomes the list's first entry.
func (s *State) seedCheckpointLinks() {
	if len(s.CheckpointLinks) == 0 && !s.LastCheckpointID.IsEmpty() {
		s.CheckpointLinks = []CheckpointLink{{CheckpointID: s.LastCheckpointID, Kind: CheckpointLinkHome}}
	}
}
