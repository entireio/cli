package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// chdirLinkedWorktree creates a main worktree whose gitignored
// .entire/settings.local.json holds localJSON, adds a linked worktree (which
// git creates without that file), and moves into it. Not usable from parallel
// tests: t.Chdir.
func chdirLinkedWorktree(t *testing.T, localJSON string) (mainRoot, linked string) {
	t.Helper()
	mainRoot = t.TempDir()
	testutil.InitRepo(t, mainRoot)
	testutil.WriteFile(t, mainRoot, "f.txt", "x")
	testutil.WriteFile(t, mainRoot, ".entire/.gitignore", "settings.local.json\n")
	testutil.RunGit(t, mainRoot, "add", ".")
	testutil.RunGit(t, mainRoot, "commit", "-q", "-m", "init")
	testutil.WriteFile(t, mainRoot, settings.EntireSettingsLocalFile, localJSON)
	linked = filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, mainRoot, "worktree", "add", "-q", "-b", "feature", linked)
	t.Chdir(linked)
	return mainRoot, linked
}

func assertNoWorktreeLocalFile(t *testing.T, linked string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(linked, settings.EntireSettingsLocalFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a worktree-only local file would hide the inherited one; Lstat err = %v", err)
	}
}

// Picking a summary provider inside a linked worktree must update the file the
// worktree reads, keeping the inherited external_agents grant in effect.
func TestPersistSummaryProviderSelection_LinkedWorktreeWritesInheritedFile(t *testing.T) {
	// Cannot use t.Parallel(): t.Chdir mutates process-global cwd.
	mainRoot, linked := chdirLinkedWorktree(t, `{"enabled":true,"external_agents":true}`)

	if _, err := persistSummaryProviderSelection(context.Background(), agent.AgentNameClaudeCode, "", selectionByUser); err != nil {
		t.Fatalf("persistSummaryProviderSelection() error = %v", err)
	}

	assertNoWorktreeLocalFile(t, linked)
	s, err := settings.LoadFromFile(filepath.Join(mainRoot, settings.EntireSettingsLocalFile))
	if err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}
	if !s.ExternalAgents || s.SummaryGeneration == nil || s.SummaryGeneration.Provider != string(agent.AgentNameClaudeCode) {
		t.Fatalf("main worktree's local file should keep the grant and gain the provider, got %+v", s)
	}
}

// Doctor attributes a provider to the layer that supplies it; an inherited
// local file is that layer in a linked worktree.
func TestSummaryProviderSourceLayer_LinkedWorktreeInheritedLocal(t *testing.T) {
	// Cannot use t.Parallel(): t.Chdir mutates process-global cwd.
	chdirLinkedWorktree(t, `{"enabled":true,"summary_generation":{"provider":"claude-code"}}`)
	ctx := context.Background()

	merged, err := settings.Load(ctx)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if _, isLocal := summaryProviderSourceLayer(ctx, merged); !isLocal {
		t.Fatal("the provider comes from the inherited local file")
	}
}

// `entire configure --local` in a linked worktree read the worktree's own
// (missing) file but saved to the inherited one, replacing its contents and
// dropping the external_agents grant. Reads and writes must hit one file.
func TestUpdateStrategyOptions_LinkedWorktreeKeepsInheritedSettings(t *testing.T) {
	// Cannot use t.Parallel(): t.Chdir mutates process-global cwd.
	mainRoot, linked := chdirLinkedWorktree(t, `{"enabled":true,"external_agents":true}`)

	if err := updateStrategyOptions(context.Background(), io.Discard, EnableOptions{UseLocalSettings: true, SkipPushSessions: true}); err != nil {
		t.Fatalf("updateStrategyOptions() error = %v", err)
	}

	assertNoWorktreeLocalFile(t, linked)
	s, err := settings.LoadFromFile(filepath.Join(mainRoot, settings.EntireSettingsLocalFile))
	if err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}
	if !s.ExternalAgents {
		t.Fatal("the inherited external_agents grant was overwritten")
	}
}

func TestUpdateSummaryTimeoutSetting_LinkedWorktreeKeepsInheritedSettings(t *testing.T) {
	// Cannot use t.Parallel(): t.Chdir mutates process-global cwd.
	mainRoot, linked := chdirLinkedWorktree(t, `{"enabled":true,"external_agents":true}`)

	if err := updateSummaryTimeoutSetting(context.Background(), io.Discard, 30, EnableOptions{UseLocalSettings: true}); err != nil {
		t.Fatalf("updateSummaryTimeoutSetting() error = %v", err)
	}

	assertNoWorktreeLocalFile(t, linked)
	s, err := settings.LoadFromFile(filepath.Join(mainRoot, settings.EntireSettingsLocalFile))
	if err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}
	if !s.ExternalAgents || s.SummaryTimeoutSeconds != 30 {
		t.Fatalf("want the grant kept and the timeout added, got external_agents=%v timeout=%d", s.ExternalAgents, s.SummaryTimeoutSeconds)
	}
}

// Setting the checkpoint backend with --local in a linked worktree must read
// the file it saves (the inherited one), not the worktree's missing own file.
func TestUpdateCheckpointBackend_LinkedWorktreeKeepsInheritedSettings(t *testing.T) {
	// Cannot use t.Parallel(): t.Chdir mutates process-global cwd.
	mainRoot, linked := chdirLinkedWorktree(t, `{"enabled":true,"external_agents":true}`)

	if err := updateCheckpointBackend(context.Background(), io.Discard, EnableOptions{UseLocalSettings: true, CheckpointBackend: "refs"}); err != nil {
		t.Fatalf("updateCheckpointBackend() error = %v", err)
	}

	assertNoWorktreeLocalFile(t, linked)
	s, err := settings.LoadFromFile(filepath.Join(mainRoot, settings.EntireSettingsLocalFile))
	if err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}
	if !s.ExternalAgents {
		t.Fatalf("the inherited external_agents grant was dropped: %+v", s)
	}
}

// Recording an already-disabled Vercel deployment with --local in a linked
// worktree must keep the inherited file's settings.
func TestMaybePromptVercelDeploymentDisable_LinkedWorktreeKeepsInheritedSettings(t *testing.T) {
	// Cannot use t.Parallel(): t.Chdir mutates process-global cwd.
	mainRoot, linked := chdirLinkedWorktree(t, `{"enabled":true,"external_agents":true}`)
	testutil.WriteFile(t, linked, "vercel.json", `{"git":{"deploymentEnabled":{"entire/**":false}}}`)

	changed, err := maybePromptVercelDeploymentDisable(context.Background(), io.Discard, settings.EntireSettingsLocalFile, func() (bool, error) {
		t.Fatal("no prompt expected: vercel.json already disables entire/** deployments")
		return false, nil
	})
	if err != nil || !changed {
		t.Fatalf("maybePromptVercelDeploymentDisable() = %v, %v; want a settings change", changed, err)
	}

	assertNoWorktreeLocalFile(t, linked)
	s, err := settings.LoadFromFile(filepath.Join(mainRoot, settings.EntireSettingsLocalFile))
	if err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}
	if !s.ExternalAgents || !s.Vercel {
		t.Fatalf("want the inherited grant kept and vercel set, got %+v", s)
	}
}

// A --local write from a linked worktree changes the main worktree's file, so
// the output must name that file rather than the worktree-relative path.
func TestSettingsTargetFile_LinkedWorktreeNamesInheritedFile(t *testing.T) {
	// Cannot use t.Parallel(): t.Chdir mutates process-global cwd.
	mainRoot, _ := chdirLinkedWorktree(t, `{"enabled":true}`)

	_, display := settingsTargetFile(context.Background(), true, false)
	if !strings.Contains(display, "inherited from the main worktree") || !strings.Contains(display, mainRoot) {
		t.Fatalf("display = %q, want it to name the main worktree's file", display)
	}

	var out bytes.Buffer
	if err := runDisable(context.Background(), &out, false); err != nil {
		t.Fatalf("runDisable() error = %v", err)
	}
	if !strings.Contains(out.String(), mainRoot) {
		t.Fatalf("disable output = %q, want it to name the main worktree's file", out.String())
	}
}
