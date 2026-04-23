package recap

import (
	"context"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
)

// repoUnknown is the display slug returned when a session's remote can't be
// resolved. Exported-looking constant lives here so callers (including the
// future renderer) can compare against it without rebinding a literal.
const repoUnknown = "unknown"

// AggregateToolProfiles sums category counts and durations across a slice of
// checkpoints. Returns nil when no checkpoint carries a ToolProfile so
// renderers can treat absence as "no data" without branching on Total == 0.
func AggregateToolProfiles(cps []RecapCheckpoint) *ToolProfile {
	out := &ToolProfile{Categories: map[string]ToolCategoryMetrics{}}
	found := false
	for _, c := range cps {
		if c.ToolProfile == nil {
			continue
		}
		found = true
		out.Total += c.ToolProfile.Total
		for k, v := range c.ToolProfile.Categories {
			m := out.Categories[k]
			m.Count += v.Count
			m.DurationMs += v.DurationMs
			out.Categories[k] = m
		}
	}
	if !found {
		return nil
	}
	return out
}

// WorktreeRollup is the per-worktree summary shown in the bottom panel of
// Day / Week tabs. Groups parallel work streams within (and across) repos.
type WorktreeRollup struct {
	WorktreeID     string
	WorktreePath   string
	Repo           string
	SessionCount   int
	LastActivity   time.Time
	HasUncommitted bool // any session has checkpoints without a linked commit
	Agents         []string
}

// AggregateByWorktree groups sessions by WorktreeID (falling back to
// WorktreePath when ID is empty) and returns one rollup per distinct
// worktree, sorted by most recent LastActivity first. Ties broken by
// WorktreeID for deterministic output.
func AggregateByWorktree(sessions []RecapSession) []WorktreeRollup {
	type bucket struct {
		rollup WorktreeRollup
		agents map[string]bool
	}
	m := map[string]*bucket{}
	for _, s := range sessions {
		key := s.WorktreeID
		if key == "" {
			key = s.WorktreePath
		}
		if key == "" {
			continue
		}
		b, ok := m[key]
		if !ok {
			b = &bucket{
				rollup: WorktreeRollup{
					WorktreeID:   s.WorktreeID,
					WorktreePath: s.WorktreePath,
					Repo:         s.Repo,
				},
				agents: map[string]bool{},
			}
			m[key] = b
		}
		b.rollup.SessionCount++
		if s.LastInteraction.After(b.rollup.LastActivity) {
			b.rollup.LastActivity = s.LastInteraction
		}
		for _, a := range s.AgentsUsed {
			b.agents[a] = true
		}
		for _, cp := range s.Checkpoints {
			if cp.LinkedCommit == "" {
				b.rollup.HasUncommitted = true
				break
			}
		}
	}
	out := make([]WorktreeRollup, 0, len(m))
	for _, b := range m {
		for a := range b.agents {
			b.rollup.Agents = append(b.rollup.Agents, a)
		}
		sort.Strings(b.rollup.Agents)
		out = append(out, b.rollup)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastActivity.Equal(out[j].LastActivity) {
			return out[i].LastActivity.After(out[j].LastActivity)
		}
		return out[i].WorktreeID < out[j].WorktreeID
	})
	return out
}

// ResolveRepoFromWorktree returns the "owner/repo" slug for the git repo at
// worktreePath, using `git -C <path> remote get-url origin`. Returns
// "unknown" when the remote is absent or unparseable — never errors, since
// an unknown repo is a valid display state.
func ResolveRepoFromWorktree(ctx context.Context, worktreePath string) string {
	if worktreePath == "" {
		return repoUnknown
	}
	cmd := exec.CommandContext(ctx, "git", "-C", worktreePath, "remote", "get-url", "origin")
	raw, err := cmd.Output()
	if err != nil {
		return repoUnknown
	}
	info, err := gitremote.ParseURL(strings.TrimSpace(string(raw)))
	if err != nil || info == nil || info.Owner == "" || info.Repo == "" {
		return repoUnknown
	}
	return info.Owner + "/" + info.Repo
}
