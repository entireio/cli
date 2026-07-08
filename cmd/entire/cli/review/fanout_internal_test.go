package review

import (
	"context"
	"errors"
	"testing"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// TestExplodeSkillWorkers_SplitsMultiSkillWorker verifies a worker with N
// skills becomes N workers, one skill each, running as ordinary parallel
// slots — wait becomes the slowest skill, not the sum.
func TestExplodeSkillWorkers_SplitsMultiSkillWorker(t *testing.T) {
	t.Parallel()
	profile := settings.ReviewProfileConfig{
		Task: "user task",
		Agents: map[string]settings.ReviewConfig{
			"claude-code": {
				Skills: []string{"/review", "/pr-review-toolkit:review-pr"},
				Model:  "opus",
				Prompt: "focus on auth",
			},
		},
	}
	got := explodeSkillWorkers(profile, allAdapters)

	if len(got.Agents) != 2 {
		t.Fatalf("Agents = %v, want 2 exploded workers", got.Agents)
	}
	if got.Task != "user task" {
		t.Errorf("Task = %q, want preserved", got.Task)
	}
	seenSkills := map[string]bool{}
	for key, cfg := range got.Agents {
		if len(cfg.Skills) != 1 {
			t.Errorf("worker %q Skills = %v, want exactly one", key, cfg.Skills)
		} else {
			seenSkills[cfg.Skills[0]] = true
		}
		if reviewAgentName(key, cfg) != "claude-code" {
			t.Errorf("worker %q resolves agent %q, want claude-code", key, reviewAgentName(key, cfg))
		}
		if cfg.Model != "opus" {
			t.Errorf("worker %q Model = %q, want opus preserved", key, cfg.Model)
		}
		if cfg.Prompt != "focus on auth" {
			t.Errorf("worker %q Prompt = %q, want preserved", key, cfg.Prompt)
		}
	}
	if !seenSkills["/review"] || !seenSkills["/pr-review-toolkit:review-pr"] {
		t.Errorf("skills split incorrectly: %v", seenSkills)
	}
}

// TestExplodeSkillWorkers_PassThrough verifies single-skill and skill-less
// workers are untouched (same keys, same configs).
func TestExplodeSkillWorkers_PassThrough(t *testing.T) {
	t.Parallel()
	profile := settings.ReviewProfileConfig{
		Agents: map[string]settings.ReviewConfig{
			"codex": {Skills: []string{"/review"}},
			"pi":    {Prompt: "review the change"},
		},
	}
	got := explodeSkillWorkers(profile, allAdapters)
	if len(got.Agents) != 2 {
		t.Fatalf("Agents = %v, want unchanged worker count", got.Agents)
	}
	if _, ok := got.Agents["codex"]; !ok {
		t.Error("single-skill worker key changed")
	}
	if _, ok := got.Agents["pi"]; !ok {
		t.Error("skill-less worker key changed")
	}
}

// TestExplodeSkillWorkers_DeterministicDistinctKeys verifies exploded keys
// are stable across calls and never collide, including with an existing
// worker whose name matches a derived key.
func TestExplodeSkillWorkers_DeterministicDistinctKeys(t *testing.T) {
	t.Parallel()
	profile := settings.ReviewProfileConfig{
		Agents: map[string]settings.ReviewConfig{
			"claude-code":        {Skills: []string{"/review", "/security-review"}},
			"claude-code:review": {Agent: "claude-code", Prompt: "existing worker with colliding name"},
		},
	}
	a := explodeSkillWorkers(profile, allAdapters)
	b := explodeSkillWorkers(profile, allAdapters)
	if len(a.Agents) != 3 {
		t.Fatalf("Agents = %v, want 3 (2 exploded + 1 pass-through)", a.Agents)
	}
	for key := range a.Agents {
		if _, ok := b.Agents[key]; !ok {
			t.Errorf("keys not deterministic: %q missing on second call", key)
		}
	}
	if cfg, ok := a.Agents["claude-code:review"]; !ok || cfg.Prompt != "existing worker with colliding name" {
		t.Error("existing worker was clobbered by an exploded key")
	}
}

type fanoutStubReviewer struct{ name string }

func (r fanoutStubReviewer) Name() string { return r.name }
func (r fanoutStubReviewer) Start(context.Context, reviewtypes.RunConfig) (reviewtypes.Process, error) {
	return nil, errors.New("not started in tests")
}

// TestPlannedAgentRunsCarrySkills verifies the planned runs propagate each
// worker's skills into the summary AgentRuns — without this, the manifest's
// skills-based session disambiguation never sees a signal in production.
func TestPlannedAgentRunsCarrySkills(t *testing.T) {
	t.Parallel()
	reviewers := []reviewtypes.AgentReviewer{
		&perAgentConfiguredReviewer{
			name:  "claude-code:review",
			inner: fanoutStubReviewer{name: "claude-code"},
			cfg:   reviewtypes.RunConfig{Skills: []string{"/review"}},
		},
		&perAgentConfiguredReviewer{
			name:  "claude-code:security-review",
			inner: fanoutStubReviewer{name: "claude-code"},
			cfg:   reviewtypes.RunConfig{Skills: []string{"/security-review"}},
		},
	}
	planned := plannedAgentRunsForReviewers(reviewers, reviewtypes.RunConfig{})
	if len(planned) != 2 {
		t.Fatalf("planned = %d runs, want 2", len(planned))
	}
	if len(planned[0].Skills) != 1 || planned[0].Skills[0] != "/review" {
		t.Errorf("planned[0].Skills = %v, want [/review]", planned[0].Skills)
	}
	if len(planned[1].Skills) != 1 || planned[1].Skills[0] != "/security-review" {
		t.Errorf("planned[1].Skills = %v, want [/security-review]", planned[1].Skills)
	}
}

// TestExplodeSkillWorkers_SkipsAgentsWithoutRunnerAdapter pins the
// marker-fallback escape hatch: a multi-skill worker whose agent has no
// review-runner adapter must NOT be exploded. Before this guard, explosion
// pushed such profiles into the multi-agent branch, which hard-fails on
// adapter-less agents ("without review runner adapters in a fan-out run"),
// while the suggested --agent workaround matched all exploded workers and
// dead-ended circularly — a regression from the working single-agent
// RunMarkerFallback path.
func TestExplodeSkillWorkers_SkipsAgentsWithoutRunnerAdapter(t *testing.T) {
	t.Parallel()
	profile := settings.ReviewProfileConfig{
		Agents: map[string]settings.ReviewConfig{
			"cursor":     {Skills: []string{"/review", "/security-review"}},
			tAgentClaude: {Skills: []string{"/review", "/simplify"}},
		},
	}
	hasAdapter := func(agentName string) bool { return agentName == tAgentClaude }
	got := explodeSkillWorkers(profile, hasAdapter)

	if cfg, ok := got.Agents["cursor"]; !ok || len(cfg.Skills) != 2 {
		t.Fatalf("adapter-less worker must pass through unexploded; got %+v", got.Agents)
	}
	if _, ok := got.Agents[tAgentClaude+":review"]; !ok {
		t.Fatalf("adapter-backed worker should still explode; got keys %v", mapKeys(got.Agents))
	}
	if _, ok := got.Agents[tAgentClaude]; ok {
		t.Fatalf("exploded worker's original key should be replaced; got keys %v", mapKeys(got.Agents))
	}
}

func mapKeys(m map[string]settings.ReviewConfig) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// allAdapters treats every agent as having a review-runner adapter — the
// common case for the explosion tests above.
func allAdapters(string) bool { return true }
