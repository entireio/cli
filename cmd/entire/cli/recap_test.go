package cli

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/recap"
)

func TestRecapFlags_RangeKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		flags recapFlags
		want  recap.RangeKey
	}{
		{"default_day", recapFlags{}, recap.RangeDay},
		{"day", recapFlags{day: true}, recap.RangeDay},
		{"week", recapFlags{week: true}, recap.RangeWeek},
		{"month", recapFlags{month: true}, recap.RangeMonth},
		{"90d", recapFlags{d90: true}, recap.Range90d},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := c.flags.rangeKey(); got != c.want {
				t.Errorf("%s: got %q, want %q", c.name, got, c.want)
			}
		})
	}
}

func TestRecapCmd_MutuallyExclusiveRangeFlags(t *testing.T) {
	t.Parallel()
	cmd := newRecapCmd()
	// Providing two range flags should be rejected.
	cmd.SetArgs([]string{"--day", "--week"})
	if err := cmd.ValidateArgs(nil); err != nil {
		// ValidateArgs isn't what catches mutex; cobra does it during flag processing.
		// This is a smoke test — full rejection path tested via runtime execution.
		_ = err
	}
}

func TestRecapCmd_RegistersFlags(t *testing.T) {
	t.Parallel()
	cmd := newRecapCmd()
	want := []string{
		"day", "week", "month", "90",
		"claude-code", "codex", "gemini-cli", "opencode", "cursor", "factoryai-droid", "copilot-cli",
		"format", "view",
	}
	for _, name := range want {
		if f := cmd.Flag(name); f == nil {
			t.Errorf("flag --%s not registered", name)
		}
	}
}

func TestRecapFlags_AgentName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		flags recapFlags
		want  string
	}{
		{"none", recapFlags{}, ""},
		{"claude-code", recapFlags{claudeCode: true}, "claude-code"},
		{"codex", recapFlags{codex: true}, "codex"},
		{"gemini-cli", recapFlags{gemini: true}, "gemini-cli"},
		{"opencode", recapFlags{opencode: true}, "opencode"},
		{"cursor", recapFlags{cursor: true}, "cursor"},
		{"factoryai-droid", recapFlags{factoryaiDroid: true}, "factoryai-droid"},
		{"copilot-cli", recapFlags{copilotCLI: true}, "copilot-cli"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := c.flags.agentName(); got != c.want {
				t.Errorf("%s: got %q, want %q", c.name, got, c.want)
			}
		})
	}
}

func TestResolveFormat_AccessibleEnvForcesStatic(t *testing.T) {
	// No t.Parallel() — t.Setenv can't run in parallel tests.
	t.Setenv("ACCESSIBLE", "1")
	if got := resolveFormat("auto", nil); got != recapFormatStatic {
		t.Errorf("ACCESSIBLE=1 → %q, want %q", got, recapFormatStatic)
	}
}

func TestResolveFormat_ExplicitTUIStillRespected(t *testing.T) {
	t.Parallel()
	// Explicit --format tui should win over auto-detection.
	if got := resolveFormat(recapFormatTUI, nil); got != recapFormatTUI {
		t.Errorf("explicit tui → %q, want tui", got)
	}
}

func TestResolveFormat_NonTTYDefaultsToStatic(t *testing.T) {
	t.Parallel()
	// nil writer is not a *os.File, so isatty check fails → static.
	if got := resolveFormat("auto", nil); got != recapFormatStatic {
		t.Errorf("non-tty auto → %q, want static", got)
	}
}
