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
	// RepoKey names the repository holding the checkpoint; empty for a home
	// checkpoint whose repository key was unknown, or a migrated entry.
	RepoKey string `json:"repo_key,omitempty"`
	// LinkedCommits are the commits (full SHAs) the checkpoint was written
	// for, in order. Empty for a checkpoint no commit carries (session end,
	// explicit snapshot) or a migrated entry.
	LinkedCommits []string           `json:"linked_commits,omitempty"`
	TurnID        string             `json:"turn_id,omitempty"`
	Kind          CheckpointLinkKind `json:"kind"`
	At            time.Time          `json:"at,omitzero"`
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
		s.CheckpointLinks = append(s.CheckpointLinks, link)
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

// PreviousCheckpoint returns the checkpoint this session recorded before cpID,
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
