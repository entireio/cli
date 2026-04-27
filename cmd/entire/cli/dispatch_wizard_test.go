package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/entireio/cli/cmd/entire/cli/api"
	dispatchpkg "github.com/entireio/cli/cmd/entire/cli/dispatch"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/spf13/cobra"
)

func TestNewDispatchWizardState_Defaults(t *testing.T) {
	t.Parallel()

	state := newDispatchWizardState()
	if state.modeChoice != dispatchWizardModeLocal {
		t.Fatalf("expected local mode default, got %q", state.modeChoice)
	}
	if state.timeWindowPreset != "7d" {
		t.Fatalf("expected 7d default, got %q", state.timeWindowPreset)
	}
	if state.localBranchMode != dispatchWizardBranchCurrent {
		t.Fatalf("expected current-branch mode default, got %q", state.localBranchMode)
	}
	if state.voicePreset != testDispatchVoicePresetNeutral {
		t.Fatalf("expected neutral voice preset default, got %q", state.voicePreset)
	}
	if state.voiceCustom != "" {
		t.Fatalf("expected empty custom voice default, got %q", state.voiceCustom)
	}
	if !state.confirmRun {
		t.Fatal("expected run confirmation to default to true")
	}
}

func TestDispatchWizardState_ResolveLocalAllBranches(t *testing.T) {
	t.Parallel()

	state := newDispatchWizardState()
	state.currentBranchErr = errors.New("not on a branch (detached HEAD)")
	state.localBranchMode = dispatchWizardBranchAll

	opts, err := state.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if !opts.AllBranches {
		t.Fatal("expected all branches")
	}
}

func TestDispatchWizardState_CloudIgnoresLocalBranchMode(t *testing.T) {
	t.Parallel()

	state := newDispatchWizardState()
	state.modeChoice = dispatchWizardModeServer
	state.currentBranch = testDispatchPreviewBranch
	state.selectedRepos = []string{"entireio/cli"}
	state.localBranchMode = dispatchWizardBranchAll

	opts, err := state.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if opts.AllBranches {
		t.Fatal("cloud mode should not honor local branch toggle")
	}
	if opts.Branches != nil {
		t.Fatalf("expected nil branches in cloud mode, got %v", opts.Branches)
	}
}

func TestDispatchWizardState_LocalBranchModes(t *testing.T) {
	t.Parallel()

	values := optionValues(newDispatchWizardState().localBranchModeOptions())
	if got := strings.Join(values, ","); got != dispatchWizardBranchCurrent+","+dispatchWizardBranchAll {
		t.Fatalf("unexpected local branch modes: %v", values)
	}
}

func TestBuildDispatchRepoOptions_UsesFullSlugLabels(t *testing.T) {
	t.Parallel()

	options := buildDispatchRepoOptions([]string{"entireio/entire.io", "entireio/cli"})
	if got := strings.Join(optionKeys(options), ","); got != "entireio/entire.io,entireio/cli" {
		t.Fatalf("expected repo options to use org/repo labels in caller order, got %q", got)
	}
}

func TestDispatchWizardState_CloudResolvesSelectedRepos(t *testing.T) {
	t.Parallel()

	state := newDispatchWizardState()
	state.modeChoice = dispatchWizardModeServer
	state.currentBranch = testDispatchPreviewBranch
	state.selectedRepos = []string{"entireio/cli"}

	opts, err := state.resolve()
	if err != nil {
		t.Fatalf("expected cloud mode to resolve selected repos, got %v", err)
	}
	if got := strings.Join(opts.RepoPaths, ","); got != "entireio/cli" {
		t.Fatalf("expected selected repo path to propagate, got %q", got)
	}
}

func TestDispatchWizardState_ShowsRepoPickerOnlyForCloud(t *testing.T) {
	t.Parallel()

	state := newDispatchWizardState()
	if state.showRepoPicker() {
		t.Fatal("did not expect repo picker in local mode")
	}

	state.modeChoice = dispatchWizardModeServer
	if !state.showRepoPicker() {
		t.Fatal("expected repo picker in cloud mode")
	}
}

func TestDispatchWizardState_ShowsLocalBranchModeOnlyForLocal(t *testing.T) {
	t.Parallel()

	state := newDispatchWizardState()
	if !state.showLocalBranchMode() {
		t.Fatal("expected local branch mode step in local mode")
	}

	state.modeChoice = dispatchWizardModeServer
	if state.showLocalBranchMode() {
		t.Fatal("did not expect local branch mode step in cloud mode")
	}
}

func TestDispatchWizardState_ResolveVoiceInput(t *testing.T) {
	t.Parallel()

	state := newDispatchWizardState()
	state.currentBranch = testDispatchPreviewBranch
	state.voicePreset = testDispatchVoicePresetMarvin
	opts, err := state.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if opts.Voice != testDispatchVoicePresetMarvin {
		t.Fatalf("expected marvin voice, got %q", opts.Voice)
	}

	state.voicePreset = testDispatchVoicePresetNeutral
	opts, err = state.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if opts.Voice != testDispatchVoicePresetNeutral {
		t.Fatalf("expected neutral voice, got %q", opts.Voice)
	}
	state.voicePreset = testDispatchVoicePresetCustom
	state.voiceCustom = "dry, skeptical release note narrator"
	opts, err = state.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if opts.Voice != "dry, skeptical release note narrator" {
		t.Fatalf("expected custom voice, got %q", opts.Voice)
	}
}

func TestDispatchWizardState_ResolveEmptyVoiceDefaultsToNeutral(t *testing.T) {
	t.Parallel()

	state := newDispatchWizardState()
	state.currentBranch = testDispatchPreviewBranch
	state.voicePreset = testDispatchVoicePresetCustom
	state.voiceCustom = "   "

	opts, err := state.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if opts.Voice != testDispatchVoicePresetNeutral {
		t.Fatalf("expected neutral voice fallback, got %q", opts.Voice)
	}
}

func TestDispatchWizardState_ShowsCustomVoiceInputOnlyForCustomPreset(t *testing.T) {
	t.Parallel()

	state := newDispatchWizardState()
	if state.showCustomVoiceInput() {
		t.Fatal("did not expect custom voice input for default preset")
	}

	state.voicePreset = testDispatchVoicePresetCustom
	if !state.showCustomVoiceInput() {
		t.Fatal("expected custom voice input for custom preset")
	}
}

func TestBuildDispatchWizardSummary(t *testing.T) {
	t.Parallel()

	summary := buildDispatchWizardSummary(dispatchpkg.Options{
		Mode:        dispatchpkg.ModeLocal,
		RepoPaths:   []string{"/tmp/repo-a", "/tmp/repo-b"},
		Branches:    nil,
		AllBranches: false,
	}, "")
	if !strings.Contains(summary, "Mode: local") {
		t.Fatalf("expected local mode in summary, got %q", summary)
	}
	if !strings.Contains(summary, "Scope: repos:/tmp/repo-a, /tmp/repo-b") {
		t.Fatalf("expected repo scope in summary, got %q", summary)
	}
	if !strings.Contains(summary, "Branches: current branch") {
		t.Fatalf("expected branches in summary, got %q", summary)
	}

	summary = buildDispatchWizardSummary(dispatchpkg.Options{
		Mode:        dispatchpkg.ModeServer,
		RepoPaths:   []string{"entireio/cli"},
		AllBranches: false,
	}, "")
	if !strings.Contains(summary, "Mode: cloud") {
		t.Fatalf("expected cloud mode in summary, got %q", summary)
	}
	if !strings.Contains(summary, "Branches: default branches") {
		t.Fatalf("expected default branches in cloud summary, got %q", summary)
	}
}

func TestBuildDispatchCommand(t *testing.T) {
	t.Parallel()

	command := buildDispatchCommand(dispatchpkg.Options{
		Mode:        dispatchpkg.ModeServer,
		Since:       "7d",
		Branches:    nil,
		Voice:       testDispatchVoicePresetMarvin,
		RepoPaths:   []string{"entireio/cli"},
		AllBranches: false,
	})
	if !strings.Contains(command, "entire dispatch") {
		t.Fatalf("expected base command, got %q", command)
	}
	if !strings.Contains(command, "--voice marvin") {
		t.Fatalf("expected preset voice flag, got %q", command)
	}
	if !strings.Contains(command, "--repos entireio/cli") {
		t.Fatalf("expected cloud repos flag, got %q", command)
	}
	if strings.Contains(command, "--local") {
		t.Fatalf("did not expect local flag for cloud mode, got %q", command)
	}
	if strings.Contains(command, "--all-branches") {
		t.Fatalf("did not expect all-branches flag when AllBranches is false, got %q", command)
	}
	if strings.Contains(command, "--org") {
		t.Fatalf("did not expect --org flag in rendered command, got %q", command)
	}
}

func TestBuildDispatchCommand_AllBranches(t *testing.T) {
	t.Parallel()

	command := buildDispatchCommand(dispatchpkg.Options{
		Mode:        dispatchpkg.ModeLocal,
		Since:       "7d",
		Voice:       testDispatchVoicePresetMarvin,
		AllBranches: true,
	})
	if !strings.Contains(command, "--all-branches") {
		t.Fatalf("expected all-branches flag, got %q", command)
	}
	if !strings.Contains(command, "--local") {
		t.Fatalf("expected local flag, got %q", command)
	}
}

func TestBuildDispatchRepoOptions_DedupesAndPreservesOrder(t *testing.T) {
	t.Parallel()

	options := buildDispatchRepoOptions([]string{"entireio/entire.io", "entireio/cli", "entireio/cli"})
	if got := strings.Join(optionValues(options), ","); got != "entireio/entire.io,entireio/cli" {
		t.Fatalf("unexpected repo options: %v", optionValues(options))
	}
}

func TestDiscoverLocalRepoRoots_LimitsConcurrentResolution(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	currentRepo := filepath.Join(parent, "repo-0")
	for i := range 12 {
		repoDir := filepath.Join(parent, fmt.Sprintf("repo-%d", i))
		if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	oldResolve := resolveDispatchWizardTopLevel
	var mu sync.Mutex
	current := 0
	maxConcurrent := 0
	resolveDispatchWizardTopLevel = func(_ context.Context, path string) (string, error) {
		mu.Lock()
		current++
		if current > maxConcurrent {
			maxConcurrent = current
		}
		mu.Unlock()

		time.Sleep(20 * time.Millisecond)

		mu.Lock()
		current--
		mu.Unlock()
		return path, nil
	}
	t.Cleanup(func() {
		resolveDispatchWizardTopLevel = oldResolve
	})

	roots := discoverLocalRepoRoots(context.Background(), currentRepo)
	if len(roots) != 12 {
		t.Fatalf("expected 12 repo roots, got %d", len(roots))
	}
	if maxConcurrent > dispatchWizardRepoDiscoveryConcurrencyLimit {
		t.Fatalf("expected max concurrency <= %d, got %d", dispatchWizardRepoDiscoveryConcurrencyLimit, maxConcurrent)
	}
}

func TestRunDispatchWizard_ProceedsWhenCurrentBranchCannotBeResolved(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	oldGetCurrentBranch := getDispatchWizardCurrentBranch
	getDispatchWizardCurrentBranch = func(context.Context) (string, error) {
		return "", errors.New("not on a branch (detached HEAD)")
	}
	t.Cleanup(func() {
		getDispatchWizardCurrentBranch = oldGetCurrentBranch
	})

	// Stub form execution so the test does not block on a TTY when run from a
	// terminal. The "run dispatch wizard" error wrapper only exists after the
	// wizard proceeds past branch resolution, so the assertion below is
	// sufficient to prove form execution was reached.
	oldRunForm := runDispatchWizardForm
	runDispatchWizardForm = func(*huh.Form) error {
		return errors.New("form execution stubbed")
	}
	t.Cleanup(func() {
		runDispatchWizardForm = oldRunForm
	})

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	_, err := runDispatchWizard(cmd)
	if err == nil || !strings.Contains(err.Error(), "run dispatch wizard") {
		t.Fatalf("expected form execution error instead of eager branch failure, got %v", err)
	}
}

func TestDispatchWizardState_CloudIgnoresCurrentBranchResolutionError(t *testing.T) {
	t.Parallel()

	state := newDispatchWizardState()
	state.modeChoice = dispatchWizardModeServer
	state.currentBranchErr = errors.New("not on a branch (detached HEAD)")
	state.selectedRepos = []string{"entireio/cli"}

	opts, err := state.resolve()
	if err != nil {
		t.Fatalf("expected cloud mode to ignore current branch resolution error, got %v", err)
	}
	if got := strings.Join(opts.RepoPaths, ","); got != "entireio/cli" {
		t.Fatalf("expected selected repo path to propagate, got %q", got)
	}
}

func TestDiscoverAuthenticatedDispatchWizardRepos_FiltersEmptyCheckpointsAndPreservesRecentOrder(t *testing.T) {
	t.Parallel()

	old := listDispatchWizardRepoResources
	listDispatchWizardRepoResources = func(context.Context) ([]api.Repository, error) {
		return []api.Repository{
			{FullName: "entireio/most-recent", CheckpointCount: 3},
			{FullName: "entireio/never-dispatched", CheckpointCount: 0},
			{FullName: "entireio/older", CheckpointCount: 1},
			{FullName: "", CheckpointCount: 5},
		}, nil
	}
	t.Cleanup(func() {
		listDispatchWizardRepoResources = old
	})

	slugs, err := discoverAuthenticatedDispatchWizardRepos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(slugs, ","); got != "entireio/most-recent,entireio/older" {
		t.Fatalf("expected recent-first order with empty-checkpoint and blank repos filtered, got %q", got)
	}
}

func optionValues(options []huh.Option[string]) []string {
	values := make([]string, 0, len(options))
	for _, option := range options {
		values = append(values, option.Value)
	}
	return values
}

func optionKeys(options []huh.Option[string]) []string {
	keys := make([]string, 0, len(options))
	for _, option := range options {
		keys = append(keys, option.Key)
	}
	return keys
}
