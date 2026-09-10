package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
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
	if !strings.Contains(got, "entire configure --summarize-provider") {
		t.Errorf("diagnosis carries no remedy command:\n%s", got)
	}
	if !strings.Contains(got, "codex") {
		t.Errorf("remedy does not name a usable provider:\n%s", got)
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
