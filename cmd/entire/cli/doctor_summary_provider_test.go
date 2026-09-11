package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// stubSummaryProviderSettings points the settings seam at one configured
// provider ("" for none), against a registry of codex (capable) + opencode
// (not), inside an isolated repo.
//
// The isolation is required, not hygiene: checkSummaryProvider's fault branch
// calls summaryProviderSourceLayer, which resolves the CURRENT repository and
// reads its real .entire/settings.local.json. Without a temp repo these tests
// read the developer's own settings and their output depends on whose machine
// they run on.
func stubSummaryProviderSettings(t *testing.T, configured string) {
	t.Helper()

	isolateRepoForSummaryProviderCheck(t)
	stubSummaryRegistry(t, []types.AgentName{"codex", "opencode"}, "codex")

	originalLoad := loadSummarySettings
	t.Cleanup(func() { loadSummarySettings = originalLoad })
	loadSummarySettings = func(context.Context) (*settings.EntireSettings, error) {
		s := &settings.EntireSettings{}
		if configured != "" {
			s.SummaryGeneration = &settings.SummaryGenerationSettings{Provider: configured}
		}
		return s, nil
	}
}

// isolateRepoForSummaryProviderCheck creates an empty repo with a .entire
// directory and points the process at it. Returns the repo root so callers can
// plant settings layers in it.
func isolateRepoForSummaryProviderCheck(t *testing.T) string {
	t.Helper()

	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	if err := os.MkdirAll(filepath.Join(tmpDir, ".entire"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(tmpDir)
	return tmpDir
}

func runCheckSummaryProvider(t *testing.T, configured string) string {
	t.Helper()

	stubSummaryProviderSettings(t, configured)
	cmd, out := newTestCmd(t)
	checkSummaryProvider(cmd)
	return out.String()
}

func TestCheckSummaryProvider_ReportsANonCapableProvider(t *testing.T) {
	// Cannot use t.Parallel(): mutates package-level resolution seams.
	got := runCheckSummaryProvider(t, "opencode")

	if !strings.Contains(got, "Summary provider: UNUSABLE") {
		t.Fatalf("no diagnosis printed:\n%s", got)
	}
	if !strings.Contains(got, "opencode") {
		t.Errorf("diagnosis does not name the provider:\n%s", got)
	}
	// The remedy must be runnable, not just a statement of the problem.
	if !strings.Contains(got, "entire configure --summarize-provider codex") {
		t.Errorf("diagnosis carries no runnable remedy command:\n%s", got)
	}
	// A <a|b|c> placeholder is not copy-pasteable: the shell reads < as a
	// redirect and | as a pipe, so the obvious paste fails before it runs.
	for _, meta := range []string{"<", "|", ">"} {
		if strings.Contains(got, meta) {
			t.Errorf("diagnosis contains shell metacharacter %q, so it cannot be pasted:\n%s", meta, got)
		}
	}
}

// Silence is the whole contract for the healthy cases: doctor's output is read
// as a fault list, so a line about a working setting is noise that trains
// people to skip the section.
func TestCheckSummaryProvider_SilentWhenNothingIsWrong(t *testing.T) {
	// Cannot use t.Parallel(): mutates package-level resolution seams.
	cases := []struct {
		name       string
		configured string
		why        string
	}{
		{
			name:       "capable provider",
			configured: "codex",
			why:        "a working provider is not a fault",
		},
		{
			name: "no provider configured",
			why:  "the field is optional; absent means the resolver picks one",
		},
		{
			name:       "unregistered provider",
			configured: "entire-agent-something",
			why:        "the external-plugin shape; reporting it would need an exec (see the doc comment)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runCheckSummaryProvider(t, tc.configured); got != "" {
				t.Errorf("expected silence (%s), got:\n%s", tc.why, got)
			}
		})
	}
}

// A settings file that will not load is a louder problem, reported elsewhere;
// this check must not add a second voice for it.
func TestCheckSummaryProvider_SilentWhenSettingsWillNotLoad(t *testing.T) {
	// Cannot use t.Parallel(): mutates package-level resolution seams.
	stubSummaryRegistry(t, []types.AgentName{"codex", "opencode"}, "codex")

	originalLoad := loadSummarySettings
	t.Cleanup(func() { loadSummarySettings = originalLoad })
	loadSummarySettings = func(context.Context) (*settings.EntireSettings, error) {
		return nil, errors.New("boom")
	}

	cmd, out := newTestCmd(t)
	checkSummaryProvider(cmd)
	if out.String() != "" {
		t.Errorf("expected silence, got:\n%s", out.String())
	}
}

// The remedy must name the layer `entire configure` would actually write.
// With no flag it writes the PROJECT file whenever one exists, so a provider
// coming from settings.local.json needs --local or the "fix" lands in a file
// the local layer still overrides.
//
// Runs against REAL settings loading and the REAL registry rather than the
// stubs the other tests use. Two reasons: opencode is genuinely incapable, so
// the registry needs no help; and the tracked-local case cannot be stubbed at
// all, because localLayerRejection is unexported — only a real Load over a real
// tracked file produces it.
func TestCheckSummaryProvider_RemedyTargetsTheLayerHoldingTheValue(t *testing.T) {
	// Cannot use t.Parallel(): t.Chdir is process-global.
	cases := []struct {
		name       string
		project    string
		local      string
		trackLocal bool
		wantFile   string
		wantLocal  bool
		why        string
	}{
		{
			name:      "provider from the project layer",
			project:   `{"enabled":true,"summary_generation":{"provider":"opencode"}}`,
			wantFile:  settings.EntireSettingsFile,
			wantLocal: false,
			why:       "only the project file carries it",
		},
		{
			name:      "provider from the local layer",
			project:   `{"enabled":true}`,
			local:     `{"summary_generation":{"provider":"opencode"}}`,
			wantFile:  settings.EntireSettingsLocalFile,
			wantLocal: true,
			why:       "the local layer supplies it, so configure needs --local",
		},
		{
			name:      "local layer supplies a different provider",
			project:   `{"enabled":true,"summary_generation":{"provider":"opencode"}}`,
			local:     `{"summary_generation":{"provider":"claude-code"}}`,
			wantFile:  settings.EntireSettingsFile,
			wantLocal: false,
			why:       "local wins, so a local claude-code means the fault is not reported at all",
		},
		{
			name:       "tracked local layer is ignored by the loader",
			project:    `{"enabled":true,"summary_generation":{"provider":"opencode"}}`,
			local:      `{"summary_generation":{"provider":"opencode"}}`,
			trackLocal: true,
			wantFile:   settings.EntireSettingsFile,
			wantLocal:  false,
			why:        "a tracked local file is dropped wholesale, so editing it fixes nothing",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := isolateRepoForSummaryProviderCheck(t)
			testutil.WriteFile(t, tmpDir, settings.EntireSettingsFile, tc.project)
			if tc.local != "" {
				testutil.WriteFile(t, tmpDir, settings.EntireSettingsLocalFile, tc.local)
				if tc.trackLocal {
					testutil.GitAdd(t, tmpDir, settings.EntireSettingsLocalFile)
				}
			}

			cmd, out := newTestCmd(t)
			checkSummaryProvider(cmd)
			got := out.String()

			// The third case reports nothing at all (a capable provider wins),
			// which is itself the correct answer; the rest must diagnose.
			if tc.name == "local layer supplies a different provider" {
				if got != "" {
					t.Fatalf("expected silence (%s), got:\n%s", tc.why, got)
				}
				return
			}
			if !strings.Contains(got, "Summary provider: UNUSABLE") {
				t.Fatalf("no diagnosis printed (%s):\n%s", tc.why, got)
			}
			if !strings.Contains(got, tc.wantFile) {
				t.Errorf("diagnosis does not name %s (%s):\n%s", tc.wantFile, tc.why, got)
			}
			if hasLocal := strings.Contains(got, "--local"); hasLocal != tc.wantLocal {
				t.Errorf("remedy --local = %v, want %v (%s):\n%s", hasLocal, tc.wantLocal, tc.why, got)
			}
		})
	}
}
