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

// SetExistingConfigForTest seeds clone-local review preferences (the same
// destination RunSetup writes to) so RunSetup's preselect-from-saved branch
// has something to read. Requires the test to have called testutil.InitRepo
// + t.Chdir so the git common dir resolves.
func SetExistingConfigForTest(t *testing.T, reviewMap map[string]settings.ReviewConfig) {
	t.Helper()
	if err := review.SaveReviewConfig(context.Background(), reviewMap); err != nil {
		t.Fatalf("SetExistingConfigForTest: SaveReviewConfig: %v", err)
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
			// claude-code is pre-seeded from saved config (Reviewer); codex has
			// no saved entry so it defaults to Skip (reviewing is opt-in).
			if current["claude-code"] != settings.RoleReviewer {
				t.Errorf("pre-seed claude-code = %q, want reviewer", current["claude-code"])
			}
			if current["codex"] != settings.RoleSkip {
				t.Errorf("default codex = %q, want skip (opt-in)", current["codex"])
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

	// Persistence target check: review config must land in clone-local
	// preferences (gitignored), NOT .entire/settings.json. Writing to the
	// committable file would re-trigger maybePromptReviewSettingsMigration
	// on the next `entire review`.
	prefs, err := settings.LoadClonePreferences(context.Background())
	if err != nil {
		t.Fatalf("LoadClonePreferences: %v", err)
	}
	if prefs == nil || prefs.Review["codex"].Role != settings.RoleFixer {
		t.Errorf("expected clone-local prefs to hold codex=Fixer, got: %+v", prefs)
	}
	// Project settings.json must NOT have a review key after setup.
	if _, projectRaw, exists, err := settings.LoadProjectRaw(context.Background()); err != nil {
		t.Fatalf("LoadProjectRaw: %v", err)
	} else if exists {
		if _, has := projectRaw["review"]; has {
			t.Errorf("project settings.json contains review key after setup; would trigger legacy migration nudge")
		}
	}
}

func TestRunSetup_NoReviewersErrors(t *testing.T) {
	// Uses t.Chdir; cannot t.Parallel.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	forms := review.SetupForms{
		PickRoles: func(_ context.Context, _ []string, _ map[string]settings.Role) (map[string]settings.Role, error) {
			return map[string]settings.Role{
				"claude-code": settings.RoleFixer,
				"codex":       settings.RoleSkip,
			}, nil
		},
		// PickSkills should NOT be called.
		PickSkills: func(_ context.Context, name string, _ settings.ReviewConfig) (settings.ReviewConfig, error) {
			t.Errorf("PickSkills should not be called when no reviewers configured; got call for %q", name)
			return settings.ReviewConfig{}, nil
		},
	}
	_, err := review.RunSetup(context.Background(), io.Discard,
		func(context.Context) []types.AgentName {
			return []types.AgentName{"claude-code", "codex"}
		}, forms)
	if err == nil {
		t.Fatal("expected error when no reviewers selected, got nil")
	}
	if !strings.Contains(err.Error(), "Reviewer or Both") {
		t.Errorf("expected error to mention 'Reviewer or Both', got: %v", err)
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
		"claude-code": {Role: settings.RoleReviewer, Skills: []string{"/review", "/security-review"}},
		"gemini":      {Role: settings.RoleReviewer},
		"codex":       {Role: settings.RoleFixer},
	})
	got := buf.String()
	// Reviewers are annotated with their configured skill count; zero skills
	// renders as a generic-review note rather than "(0 skills)".
	if !strings.Contains(got, "Claude Code (2 skills)") {
		t.Errorf("expected skill-count annotation, got:\n%s", got)
	}
	if !strings.Contains(got, "Gemini CLI (no skills — generic review)") {
		t.Errorf("expected generic-review annotation for skill-less reviewer, got:\n%s", got)
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
