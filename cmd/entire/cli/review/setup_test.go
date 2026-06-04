package review_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/huh/v2"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/review"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// SetExistingConfigForTest writes a minimal .entire/settings.json into the
// current working directory so RunSetup's preselect-from-saved branch has
// something to read.
func SetExistingConfigForTest(t *testing.T, reviewMap map[string]settings.ReviewConfig) {
	t.Helper()
	if err := settings.Save(context.Background(), &settings.EntireSettings{Review: reviewMap}); err != nil {
		t.Fatalf("SetExistingConfigForTest: settings.Save: %v", err)
	}
}

func TestRunSetup_NoInstalledAgents_ReturnsClearError(t *testing.T) {
	t.Parallel()
	getInstalled := func(_ context.Context) []types.AgentName { return nil }
	_, err := review.RunSetup(context.Background(), io.Discard, getInstalled, review.SetupForms{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no agents") {
		t.Errorf("error should mention no agents, got: %v", err)
	}
}

func TestBuildPickRolesFields_BuildsOneSelectPerAgentPlusLegend(t *testing.T) {
	t.Parallel()
	ptrs := map[string]*settings.Role{
		"claude-code": new(settings.Role),
		"codex":       new(settings.Role),
	}
	*ptrs["claude-code"] = settings.RoleReviewer
	*ptrs["codex"] = settings.RoleFixer
	fields := review.BuildPickRolesFields([]string{"claude-code", "codex"}, ptrs)
	// 2 Selects (one per agent) + 1 Note (the role legend).
	if len(fields) != 3 {
		t.Fatalf("expected 2 selects + 1 legend note, got %d fields", len(fields))
	}
	selectCount := 0
	noteCount := 0
	for _, f := range fields {
		switch f.(type) {
		case *huh.Select[settings.Role]:
			selectCount++
		case *huh.Note:
			noteCount++
		}
	}
	if selectCount != 2 {
		t.Errorf("expected 2 Select fields, got %d", selectCount)
	}
	if noteCount != 1 {
		t.Errorf("expected 1 Note (legend), got %d", noteCount)
	}
}

func TestRunSetup_DefaultsRolesFromExistingConfig(t *testing.T) {
	// Note: uses t.Chdir, so cannot use t.Parallel().
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	SetExistingConfigForTest(t, map[string]settings.ReviewConfig{
		"claude-code": {Role: settings.RoleReviewer, Skills: []string{"/review"}},
	})

	getInstalled := func(context.Context) []types.AgentName {
		return []types.AgentName{"claude-code", "codex"}
	}
	forms := review.SetupForms{
		PickRoles: func(_ context.Context, agents []string, current map[string]settings.Role) (map[string]settings.Role, error) {
			if current["claude-code"] != settings.RoleReviewer {
				t.Errorf("pre-seed claude-code = %q, want reviewer", current["claude-code"])
			}
			if current["codex"] != settings.RoleReviewer {
				t.Errorf("default codex = %q, want reviewer", current["codex"])
			}
			_ = agents
			return map[string]settings.Role{
				"claude-code": settings.RoleReviewer,
				"codex":       settings.RoleFixer,
			}, nil
		},
		PickSkills: func(_ context.Context, _ string, _ settings.ReviewConfig) (settings.ReviewConfig, error) {
			return settings.ReviewConfig{Skills: []string{"/review"}}, nil
		},
	}
	out, err := review.RunSetup(context.Background(), io.Discard, getInstalled, forms)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if out["codex"].Role != settings.RoleFixer {
		t.Errorf("codex role = %q, want fixer", out["codex"].Role)
	}
	if out["claude-code"].Role != settings.RoleReviewer {
		t.Errorf("claude-code role = %q, want reviewer", out["claude-code"].Role)
	}
}

func TestRunSetup_EnforcesAtMostOneFixerAfterPick(t *testing.T) {
	// Note: uses t.Chdir, so cannot use t.Parallel().
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	forms := review.SetupForms{
		PickRoles: func(_ context.Context, _ []string, _ map[string]settings.Role) (map[string]settings.Role, error) {
			return map[string]settings.Role{
				"claude-code": settings.RoleFixer,
				"codex":       settings.RoleFixer,
			}, nil
		},
		PickSkills: func(context.Context, string, settings.ReviewConfig) (settings.ReviewConfig, error) {
			return settings.ReviewConfig{}, nil
		},
	}
	out, err := review.RunSetup(context.Background(), io.Discard,
		func(context.Context) []types.AgentName {
			return []types.AgentName{"claude-code", "codex"}
		}, forms)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	fixers := 0
	for _, cfg := range out {
		if cfg.Role.IsFixer() {
			fixers++
		}
	}
	if fixers != 1 {
		t.Errorf("expected 1 fixer after normalization, got %d", fixers)
	}
}

// RunSetup must persist the review map to clone-local preferences
// (.git/entire/preferences.json), NOT the committed project settings.json.
// Writing the full merged settings object to the project file would promote
// clone-local / local-override values into the committed file.
func TestRunSetup_PersistsToCloneLocalNotProjectSettings(t *testing.T) {
	// Note: uses t.Chdir, so cannot use t.Parallel().
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	forms := review.SetupForms{
		PickRoles: func(_ context.Context, _ []string, _ map[string]settings.Role) (map[string]settings.Role, error) {
			return map[string]settings.Role{"claude-code": settings.RoleBoth}, nil
		},
		PickSkills: func(context.Context, string, settings.ReviewConfig) (settings.ReviewConfig, error) {
			return settings.ReviewConfig{Skills: []string{"/review"}}, nil
		},
	}
	if _, err := review.RunSetup(context.Background(), io.Discard,
		func(context.Context) []types.AgentName { return []types.AgentName{"claude-code"} },
		forms); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Landed in clone-local preferences.
	prefs, err := settings.LoadClonePreferences(context.Background())
	if err != nil {
		t.Fatalf("load clone prefs: %v", err)
	}
	if prefs == nil || prefs.Review["claude-code"].Role != settings.RoleBoth {
		t.Errorf("review config not persisted to clone-local prefs: %+v", prefs)
	}

	// Did NOT create the committed project settings file.
	if _, err := os.Stat(filepath.Join(dir, ".entire", "settings.json")); !os.IsNotExist(err) {
		t.Errorf("setup must not write .entire/settings.json (got stat err=%v)", err)
	}
}

func TestBuildSetupSkillsFields_UsesInputNotTextForInstructions(t *testing.T) {
	t.Parallel()
	var builtinPicks, discoveredPicks []string
	var prompt string
	fields := review.BuildSetupSkillsFields(
		"claude-code", nil, nil, nil, "",
		&builtinPicks, &discoveredPicks, &prompt,
	)
	foundInput, foundText := false, false
	for _, f := range fields {
		if _, ok := f.(*huh.Input); ok {
			foundInput = true
		}
		if _, ok := f.(*huh.Text); ok {
			foundText = true
		}
	}
	if !foundInput {
		t.Errorf("expected a huh.Input for instructions")
	}
	if foundText {
		t.Errorf("expected NO huh.Text — plain Enter would be ambiguous")
	}
}

func TestPrintSetupBanner_MultipleReviewersWithDisplayLabels(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	review.PrintSetupBanner(&buf, map[string]settings.ReviewConfig{
		"claude-code": {Role: settings.RoleReviewer},
		"gemini":      {Role: settings.RoleReviewer},
		"codex":       {Role: settings.RoleFixer},
	})
	got := buf.String()
	if !strings.Contains(got, "Reviewers: Claude Code, Gemini CLI") {
		t.Errorf("expected display-label list, got:\n%s", got)
	}
	if !strings.Contains(got, "Fixer:     Codex") {
		t.Errorf("expected fixer line, got:\n%s", got)
	}
	if !strings.Contains(got, "Edit later: entire review setup") {
		t.Errorf("expected edit-later pointer, got:\n%s", got)
	}
	if !strings.Contains(got, "Run: entire review") {
		t.Errorf("expected Run: pointer, got:\n%s", got)
	}
}

func TestSetupSubcommand_Registered(t *testing.T) {
	t.Parallel()
	root := review.NewCommand(testDepsForSetupSubcommand(t))
	setup, _, err := root.Find([]string{"setup"})
	if err != nil {
		t.Fatalf("setup subcommand not found: %v", err)
	}
	if setup.Use != "setup" {
		t.Errorf("got %q, want setup", setup.Use)
	}
}

func testDepsForSetupSubcommand(t *testing.T) review.Deps {
	t.Helper()
	return review.Deps{
		GetAgentsWithHooksInstalled: func(_ context.Context) []types.AgentName { return nil },
		NewSilentError:              func(err error) error { return err },
		HeadHasReviewCheckpoint:     func(_ context.Context) (bool, string) { return false, "" },
		ReviewerFor:                 func(string) reviewtypes.AgentReviewer { return nil },
	}
}
