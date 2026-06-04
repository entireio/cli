package review

import (
	"context"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

func TestResolveRolesFromFlags_NoFlagsReturnsNil(t *testing.T) {
	t.Parallel()
	got, err := resolveRolesFromFlags(nil, "", []types.AgentName{"claude-code"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil when no flags set, got %v", got)
	}
}

func TestResolveRolesFromFlags_SingleReviewer(t *testing.T) {
	t.Parallel()
	got, err := resolveRolesFromFlags(
		[]string{"claude-code"}, "",
		[]types.AgentName{"claude-code", "codex"},
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	if got["claude-code"].Role != settings.RoleReviewer {
		t.Errorf("claude-code role = %q, want %q", got["claude-code"].Role, settings.RoleReviewer)
	}
}

func TestResolveRolesFromFlags_MultipleReviewers(t *testing.T) {
	t.Parallel()
	got, err := resolveRolesFromFlags(
		[]string{"claude-code", "codex"}, "",
		[]types.AgentName{"claude-code", "codex"},
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d: %+v", len(got), got)
	}
	if got["claude-code"].Role != settings.RoleReviewer {
		t.Errorf("claude-code role = %q", got["claude-code"].Role)
	}
	if got["codex"].Role != settings.RoleReviewer {
		t.Errorf("codex role = %q", got["codex"].Role)
	}
}

func TestResolveRolesFromFlags_AgentInBothListsGetsRoleBoth(t *testing.T) {
	t.Parallel()
	got, err := resolveRolesFromFlags(
		[]string{"codex"}, "codex",
		[]types.AgentName{"codex"},
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got["codex"].Role != settings.RoleBoth {
		t.Errorf("codex role = %q, want %q", got["codex"].Role, settings.RoleBoth)
	}
}

// TestResolveRolesFromFlags_FixerOnlyAssignsFixer documents that
// `--fixer X` without `--reviewers` is a legitimate combination at the
// flag-resolution layer: `entire review fix --fixer X` uses it to apply
// existing findings via a one-off fixer override. The "needs at least
// one reviewer" check is enforced higher up in `runReview` (cmd.go),
// not here, so this function can be shared with `entire review fix`.
func TestResolveRolesFromFlags_FixerOnlyAssignsFixer(t *testing.T) {
	t.Parallel()
	installed := []types.AgentName{"claude-code", "codex"}
	got, err := resolveRolesFromFlags(nil, "codex", installed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d: %+v", len(got), got)
	}
	if got["codex"].Role != settings.RoleFixer {
		t.Errorf("codex role = %q, want %q", got["codex"].Role, settings.RoleFixer)
	}
}

func TestResolveRolesFromFlags_UnknownReviewerErrors(t *testing.T) {
	t.Parallel()
	_, err := resolveRolesFromFlags(
		[]string{"no-such-agent"}, "",
		[]types.AgentName{"claude-code"},
	)
	if err == nil {
		t.Fatal("expected error for unknown reviewer")
	}
	if !strings.Contains(err.Error(), "no-such-agent") {
		t.Errorf("error should name the agent, got: %v", err)
	}
}

func TestResolveRolesFromFlags_UnknownFixerErrors(t *testing.T) {
	t.Parallel()
	_, err := resolveRolesFromFlags(
		nil, "no-such-agent",
		[]types.AgentName{"claude-code"},
	)
	if err == nil {
		t.Fatal("expected error for unknown fixer")
	}
	if !strings.Contains(err.Error(), "no-such-agent") {
		t.Errorf("error should name the agent, got: %v", err)
	}
}

func TestMergeFlagOverrideWithSavedSkills_CopiesSavedSkills(t *testing.T) {
	t.Parallel()
	override := map[string]settings.ReviewConfig{
		"claude-code": {Role: settings.RoleReviewer},
	}
	saved := map[string]settings.ReviewConfig{
		"claude-code": {Skills: []string{"/review", "/test-auditor"}, Prompt: "Focus on auth."},
		"codex":       {Skills: []string{"/codex-review"}}, // not in override; should be dropped
	}
	got := mergeFlagOverrideWithSavedSkills(context.Background(), override, saved)
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	cfg := got["claude-code"]
	if cfg.Role != settings.RoleReviewer {
		t.Errorf("role = %q", cfg.Role)
	}
	if len(cfg.Skills) != 2 {
		t.Errorf("skills = %v, want 2 entries", cfg.Skills)
	}
	if cfg.Prompt != "Focus on auth." {
		t.Errorf("prompt = %q", cfg.Prompt)
	}
}
