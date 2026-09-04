package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/runnerdefaults"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// TestResolveRunnerSetupMode_FlagPrecedence pins the mode each flag combination
// selects. The point of the table is that -y resolves to the FULL action:
// tailoring used to need its own flag, so -y left the job half done.
func TestResolveRunnerSetupMode_FlagPrecedence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts runnerSetupOptions
		want runnerSetupMode
	}{
		{"yes means create and tailor", runnerSetupOptions{assumeYes: true}, setupModeAdapt},
		{"defaults-only stops after creating", runnerSetupOptions{defaultsOnly: true}, setupModeDefaults},
		{"print-prompt emits the prompt", runnerSetupOptions{printPrompt: true}, setupModePrintPrompt},
		{"dry-run previews", runnerSetupOptions{dryRun: true}, setupModeDryRun},
		// An explicit mode outranks -y, so `-y --dry-run` still writes nothing.
		{"dry-run outranks yes", runnerSetupOptions{assumeYes: true, dryRun: true}, setupModeDryRun},
		{"defaults-only outranks yes", runnerSetupOptions{assumeYes: true, defaultsOnly: true}, setupModeDefaults},
		{"print-prompt outranks yes", runnerSetupOptions{assumeYes: true, printPrompt: true}, setupModePrintPrompt},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveRunnerSetupMode(context.Background(), io.Discard, tt.opts, false)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("mode = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestResolveRunnerSetupMode_NonInteractiveNeedsAnAction covers the case that
// used to hand back a wall of prompt text nobody asked for: with no terminal
// and no flag, setup names the choices instead of picking one.
func TestResolveRunnerSetupMode_NonInteractiveNeedsAnAction(t *testing.T) {
	t.Parallel()

	// interactive.CanPromptInteractively() is false under `go test`.
	_, err := resolveRunnerSetupMode(context.Background(), io.Discard, runnerSetupOptions{}, false)
	if err == nil {
		t.Fatal("expected an error when no terminal and no action flag")
	}
	for _, flag := range []string{"--yes", "--defaults-only", "--print-prompt", "--dry-run"} {
		if !strings.Contains(err.Error(), flag) {
			t.Errorf("error should name %s, got: %v", flag, err)
		}
	}
}

// TestRunRunnerSetup_DryRunCreatesNothing is the invariant that separates
// --dry-run from every other mode: it must not scaffold the defaults, so a
// preview of a fresh repo leaves that repo untouched.
func TestRunRunnerSetup_DryRunCreatesNothing(t *testing.T) {
	repoRoot := newRunnerSetupRepo(t)
	stubUnavailableSummaryProvider(t)

	var out, errOut bytes.Buffer
	err := runRunnerSetup(context.Background(), &out, &errOut, runnerSetupOptions{
		dryRun:  true,
		sources: []string{"repo"}, // local signal only: no gh, no API
		limit:   1,
	})
	// The stubbed provider is unresolvable, so tailoring cannot run — which is
	// exactly where this test wants to stop. What matters is what is on disk.
	if err == nil {
		t.Fatal("expected the stubbed provider to fail the tailoring step")
	}
	if _, statErr := os.Stat(runnersDir(repoRoot)); !os.IsNotExist(statErr) {
		t.Errorf("--dry-run created .entire/runners (stat err = %v); it must write nothing", statErr)
	}
}

// TestRunRunnerSetup_YesCreatesDefaultsBeforeTailoring is the counterpart: -y
// scaffolds without asking, and does so before the provider is involved, so a
// provider failure still leaves a working generic set behind.
func TestRunRunnerSetup_YesCreatesDefaultsBeforeTailoring(t *testing.T) {
	repoRoot := newRunnerSetupRepo(t)
	stubUnavailableSummaryProvider(t)

	var out, errOut bytes.Buffer
	if err := runRunnerSetup(context.Background(), &out, &errOut, runnerSetupOptions{
		assumeYes: true,
		sources:   []string{"repo"},
		limit:     1,
	}); err == nil {
		t.Fatal("expected the stubbed provider to fail the tailoring step")
	}

	if written := runnerFiles(t, repoRoot); len(written) != wantDefaultCount(t) {
		t.Errorf("-y wrote %d runner file(s), want the full default set (%d)", len(written), wantDefaultCount(t))
	}
}

// TestRunRunnerSetup_DefaultsOnlyMakesNoProviderCall pins that --defaults-only
// is the offline mode: it returns successfully even though the configured
// provider cannot be resolved, because it never asks for one.
func TestRunRunnerSetup_DefaultsOnlyMakesNoProviderCall(t *testing.T) {
	repoRoot := newRunnerSetupRepo(t)
	stubUnavailableSummaryProvider(t)

	var out, errOut bytes.Buffer
	if err := runRunnerSetup(context.Background(), &out, &errOut, runnerSetupOptions{
		defaultsOnly: true,
		limit:        1,
	}); err != nil {
		t.Fatalf("--defaults-only should not need a provider: %v", err)
	}

	if written := runnerFiles(t, repoRoot); len(written) != wantDefaultCount(t) {
		t.Errorf("--defaults-only wrote %d runner file(s), want the full default set (%d)", len(written), wantDefaultCount(t))
	}
	// And it says how to tailor them, rather than leaving them looking finished.
	if !strings.Contains(errOut.String(), "-y") {
		t.Errorf("expected a pointer at tailoring, got %q", errOut.String())
	}
}

// TestRunRunnerSetup_DefaultsOnlyIsANoopWhenConfigured covers the re-run case:
// there is nothing to create and --defaults-only asks for nothing else.
func TestRunRunnerSetup_DefaultsOnlyIsANoopWhenConfigured(t *testing.T) {
	repoRoot := newRunnerSetupRepo(t)
	if err := os.MkdirAll(runnersDir(repoRoot), 0o755); err != nil {
		t.Fatal(err)
	}
	writeRunner(t, runnersDir(repoRoot), "trail-risk", "existing")

	var out, errOut bytes.Buffer
	if err := runRunnerSetup(context.Background(), &out, &errOut, runnerSetupOptions{
		defaultsOnly: true,
		limit:        1,
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(errOut.String(), "Nothing to do") {
		t.Errorf("expected a no-op notice, got %q", errOut.String())
	}
	if after := runnerFiles(t, repoRoot); len(after) != 1 {
		t.Errorf("expected the existing runner left alone, got %d files", len(after))
	}
}

func TestDefaultTuneRunners(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir() // deliberately empty: the embedded set needs no repo
	runners, err := defaultTuneRunners(repoRoot, "")
	if err != nil {
		t.Fatalf("defaultTuneRunners: %v", err)
	}
	if len(runners) != wantDefaultCount(t) {
		t.Fatalf("expected the %d embedded runners, got %d", wantDefaultCount(t), len(runners))
	}
	for _, r := range runners {
		if r.ID == "" || r.Template == "" || len(r.Raw) == 0 {
			t.Errorf("%s: incomplete tuneRunner (template_empty=%v raw=%d)", r.ID, r.Template == "", len(r.Raw))
		}
		// Paths describe where the file WOULD go, so messages match the on-disk case.
		if want := filepath.Join(repoRoot, ".entire", "runners"); filepath.Dir(r.Path) != want {
			t.Errorf("%s: path %q not under %q", r.ID, r.Path, want)
		}
	}
	if _, err := os.Stat(filepath.Join(repoRoot, ".entire")); !os.IsNotExist(err) {
		t.Errorf("defaultTuneRunners touched the repo (stat err = %v)", err)
	}

	// The filter accepts an id with or without the "trail-" prefix.
	for _, filter := range []string{"risk", "trail-risk"} {
		one, err := defaultTuneRunners(repoRoot, filter)
		if err != nil {
			t.Fatalf("filter %q: %v", filter, err)
		}
		if len(one) != 1 || normalizeRunnerID(one[0].ID) != "risk" {
			t.Errorf("filter %q selected %d runner(s): %+v", filter, len(one), one)
		}
	}
	if _, err := defaultTuneRunners(repoRoot, "nope"); err == nil {
		t.Error("expected an error when the filter matches nothing")
	}
}

func TestRenderTemplateDiff(t *testing.T) {
	t.Parallel()

	oldText := "keep one\nkeep two\ndrop me\nkeep three\n"
	newText := "keep one\nkeep two\nadd me\nkeep three\n"
	got := renderTemplateDiff(oldText, newText)

	for _, want := range []string{"-drop me", "+add me", " keep one"} {
		if !strings.Contains(got, want) {
			t.Errorf("diff missing %q:\n%s", want, got)
		}
	}
}

// TestRenderTemplateDiff_CollapsesLongUnchangedRuns keeps the preview from
// becoming the very wall of text --dry-run exists to avoid: a tailored template
// shares a long unchanged tail (the output-JSON contract) with the original.
func TestRenderTemplateDiff_CollapsesLongUnchangedRuns(t *testing.T) {
	t.Parallel()

	tail := strings.Repeat("unchanged tail\n", 40)
	got := renderTemplateDiff("old head\n"+tail, "new head\n"+tail)

	if !strings.Contains(got, "@@ ") {
		t.Errorf("expected a collapse marker for the unchanged tail:\n%s", got)
	}
	if n := strings.Count(got, "unchanged tail"); n > 2*diffContextLines {
		t.Errorf("kept %d unchanged lines, want at most %d", n, 2*diffContextLines)
	}
	if !strings.Contains(got, "-old head") || !strings.Contains(got, "+new head") {
		t.Errorf("collapse dropped the actual change:\n%s", got)
	}
}

func TestPreviewTunedRunners_ReportsWithoutWriting(t *testing.T) {
	t.Parallel()

	runners, err := defaultTuneRunners(t.TempDir(), "risk")
	if err != nil {
		t.Fatal(err)
	}
	changes := []tunedRunner{{
		runner:   runners[0],
		newRaw:   []byte("{}"), // never written in a preview
		template: "a tailored template\n",
	}}

	var out, errOut bytes.Buffer
	previewTunedRunners(&out, &errOut, len(runners), changes, 1 /* skipped */)

	if !strings.Contains(out.String(), "+a tailored template") {
		t.Errorf("expected the tailored template as an added line:\n%s", out.String())
	}
	for _, want := range []string{"Nothing was written", "--yes", "1 proposal(s) rejected"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("summary missing %q:\n%s", want, errOut.String())
		}
	}
}

// newRunnerSetupRepo makes an isolated git repo and chdirs into it, so
// paths.WorktreeRoot resolves there rather than in the developer's checkout.
func newRunnerSetupRepo(t *testing.T) string {
	t.Helper()
	repoRoot := t.TempDir()
	testutil.InitRepo(t, repoRoot)
	testutil.WriteFile(t, repoRoot, "README.md", "# fixture\n")
	testutil.GitAdd(t, repoRoot, "README.md")
	testutil.GitCommit(t, repoRoot, "init")
	// The worktree-root cache is keyed on cwd, so reset it either side of the
	// chdir like the other git fixtures in this package do.
	paths.ClearWorktreeRootCache()
	t.Chdir(repoRoot)
	t.Cleanup(paths.ClearWorktreeRootCache)
	// t.TempDir is a symlink on macOS (/var -> /private/var); git reports the
	// resolved path, so resolve here too or the glob checks look at a ghost.
	resolved, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	return resolved
}

// stubUnavailableSummaryProvider points summary generation at a provider that
// cannot resolve, so a test reaching the tailoring step fails there instead of
// making a real model call with whatever agent the developer has installed.
// Discovery is stubbed out as well: an unknown provider name otherwise reaches
// external.DiscoverAndRegisterAlways, which globs the real $PATH for
// entire-agent-* plugins and execs each match, making the test depend on the
// developer's machine.
func stubUnavailableSummaryProvider(t *testing.T) {
	t.Helper()
	originalLoad, originalGet := loadSummarySettings, getSummaryAgent
	originalDiscover, originalDiscoverAlways := discoverSummaryProviders, discoverSummaryProvidersAlways
	t.Cleanup(func() {
		loadSummarySettings = originalLoad
		getSummaryAgent = originalGet
		discoverSummaryProviders = originalDiscover
		discoverSummaryProvidersAlways = originalDiscoverAlways
	})
	loadSummarySettings = func(context.Context) (*settings.EntireSettings, error) {
		return &settings.EntireSettings{
			SummaryGeneration: &settings.SummaryGenerationSettings{Provider: "not-a-real-agent"},
		}, nil
	}
	getSummaryAgent = func(name types.AgentName) (agent.Agent, error) {
		return nil, fmt.Errorf("stub: no agent %s", name)
	}
	// No-ops, not assertions: an unresolvable provider name legitimately reaches
	// discovery (discoverSummaryProviderIfMissing calls it when the agent lookup
	// fails). Replacing it is what keeps the test off the real $PATH.
	discoverSummaryProviders = func(context.Context) {}
	discoverSummaryProvidersAlways = func(context.Context) {}
}

// runnerFiles lists the runner configs on disk, for the several tests whose
// whole assertion is whether a mode wrote the set or left the repo alone.
func runnerFiles(t *testing.T, repoRoot string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(runnersDir(repoRoot), "*.json"))
	if err != nil {
		t.Fatalf("globbing runner files: %v", err)
	}
	return files
}

// wantDefaultCount is the size of the embedded default set, derived rather
// than hardcoded so adding or removing a default does not need a test edit.
func wantDefaultCount(t *testing.T) int {
	t.Helper()
	files, err := runnerdefaults.Files()
	if err != nil {
		t.Fatalf("runnerdefaults.Files: %v", err)
	}
	return len(files)
}

// TestClassifyTuneProposals covers the accept/reject rules against canned
// proposals, so the outcome of a tailoring run is pinned without a provider
// call. Every rejection is per-runner: the accepted one must survive them.
func TestClassifyTuneProposals(t *testing.T) {
	t.Parallel()

	runners, err := defaultTuneRunners(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]tuneRunner, len(runners))
	for _, r := range runners {
		byID[normalizeRunnerID(r.ID)] = r
	}
	risk, drift := byID["risk"], byID["drift"]

	// A rewrite may drop a placeholder but not invent one, so reuse the risk
	// template's own placeholders in the accepted proposal.
	accepted := "Tailored for this repo.\n" + risk.Template

	var errOut bytes.Buffer
	changes, skipped := classifyTuneProposals(&errOut, runners, map[string]string{
		"trail-risk":     accepted,
		"trail-drift":    drift.Template,             // verbatim: a benign no-op
		"trail-nonsense": "whatever",                 // not a runner in scope
		"trail-security": "score {{invented_thing}}", // invents a placeholder
	})

	if len(changes) != 1 || normalizeRunnerID(changes[0].runner.ID) != "risk" {
		t.Fatalf("expected only trail-risk to change, got %d: %+v", len(changes), changes)
	}
	if changes[0].template != accepted {
		t.Error("accepted change did not carry the proposed template")
	}
	// The verbatim proposal is dropped silently; the other two are counted.
	if skipped != 2 {
		t.Errorf("skipped = %d, want 2 (out-of-scope + invented placeholder)", skipped)
	}
	for _, want := range []string{`skip "trail-nonsense"`, "trail-security", "{{invented_thing}}"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("rejection messages missing %q:\n%s", want, errOut.String())
		}
	}
	if strings.Contains(errOut.String(), "trail-drift") {
		t.Errorf("a verbatim proposal should be a silent no-op:\n%s", errOut.String())
	}

	// The new file bytes must be valid JSON with only the template changed.
	var doc struct {
		ID     string `json:"id"`
		Output struct {
			ResultType string `json:"result_type"`
		} `json:"output"`
		Prompt struct {
			Template string `json:"template"`
		} `json:"prompt"`
	}
	if err := json.Unmarshal(changes[0].newRaw, &doc); err != nil {
		t.Fatalf("rewritten runner is not valid JSON: %v", err)
	}
	if doc.Prompt.Template != accepted {
		t.Error("rewritten file does not carry the new template")
	}
	if doc.ID != risk.ID || doc.Output.ResultType == "" {
		t.Errorf("rewrite lost structural fields: id=%q result_type=%q", doc.ID, doc.Output.ResultType)
	}
}

// TestRunRunnerSetup_BadGatherFlagsWriteNothing pins that a usage error leaves
// the repo alone. The gather flags are validated for the modes that read them,
// but ahead of the scaffold — validating after it meant `-y --limit 0` on a
// fresh repo created .entire/runners and then failed.
func TestRunRunnerSetup_BadGatherFlagsWriteNothing(t *testing.T) {
	repoRoot := newRunnerSetupRepo(t)
	stubUnavailableSummaryProvider(t)

	for _, tc := range []struct {
		name string
		opts runnerSetupOptions
	}{
		{"yes with a bad limit", runnerSetupOptions{assumeYes: true, limit: 0}},
		{"yes with bad sources", runnerSetupOptions{assumeYes: true, limit: 1, sources: []string{"nope"}}},
		{"print-prompt with a bad limit", runnerSetupOptions{printPrompt: true, limit: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := runRunnerSetup(context.Background(), io.Discard, io.Discard, tc.opts); err == nil {
				t.Fatal("expected a usage error")
			}
			if written := runnerFiles(t, repoRoot); len(written) != 0 {
				t.Errorf("a usage error wrote %d runner file(s); it must write none", len(written))
			}
		})
	}
}
