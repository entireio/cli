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
// (not).
func stubSummaryProviderSettings(t *testing.T, configured string) {
	t.Helper()

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
func TestCheckSummaryProvider_RemedyTargetsTheLayerHoldingTheValue(t *testing.T) {
	// Cannot use t.Parallel(): t.Chdir and package-level resolution seams.
	cases := []struct {
		name      string
		localFile string
		wantFile  string
		wantLocal bool
	}{
		{
			name:      "provider from the project layer",
			localFile: "",
			wantFile:  settings.EntireSettingsFile,
			wantLocal: false,
		},
		{
			name:      "provider from the local layer",
			localFile: `{"summary_generation":{"provider":"opencode"}}`,
			wantFile:  settings.EntireSettingsLocalFile,
			wantLocal: true,
		},
		{
			name:      "local layer exists but supplies a different provider",
			localFile: `{"summary_generation":{"provider":"codex"}}`,
			wantFile:  settings.EntireSettingsFile,
			wantLocal: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			testutil.InitRepo(t, tmpDir)
			if err := os.MkdirAll(filepath.Join(tmpDir, ".entire"), 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.localFile != "" {
				if err := os.WriteFile(filepath.Join(tmpDir, settings.EntireSettingsLocalFile), []byte(tc.localFile), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			t.Chdir(tmpDir)

			stubSummaryProviderSettings(t, "opencode")
			cmd, out := newTestCmd(t)
			checkSummaryProvider(cmd)
			got := out.String()

			if !strings.Contains(got, tc.wantFile) {
				t.Errorf("diagnosis does not name %s:\n%s", tc.wantFile, got)
			}
			hasLocalFlag := strings.Contains(got, "--local")
			if hasLocalFlag != tc.wantLocal {
				t.Errorf("remedy --local = %v, want %v:\n%s", hasLocalFlag, tc.wantLocal, got)
			}
		})
	}
}
