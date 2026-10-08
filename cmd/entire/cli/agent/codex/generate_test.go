package codex

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeCodex answers `codex features list` with features (one per line) and
// records every exec run's argv; exec runs print execOut on stdout and
// execErr on stderr, exiting non-zero when execErr is set.
func fakeCodex(features []string, execOut, execErr string, execs *[][]string) func(context.Context, string, ...string) *exec.Cmd {
	return func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		if len(args) >= 2 && args[0] == "features" && args[1] == "list" {
			if features == nil {
				return exec.CommandContext(ctx, "sh", "-c", "exit 1")
			}
			if len(features) == 1 && features[0] == hangingProbe {
				return exec.CommandContext(ctx, "sleep", "30")
			}
			var b strings.Builder
			for _, f := range features {
				b.WriteString(f + "  stable  true\n")
			}
			return exec.CommandContext(ctx, "printf", "%s", b.String())
		}
		*execs = append(*execs, slices.Clone(args))
		if execErr != "" {
			return exec.CommandContext(ctx, "sh", "-c", "cat >/dev/null; printf '%s' \"$1\" >&2; exit 1", "sh", execErr)
		}
		return exec.CommandContext(ctx, "sh", "-c", "cat >/dev/null; printf '%s' \"$1\"", "sh", execOut)
	}
}

// hangingProbe, as fakeCodex's only feature, makes `codex features list`
// hang until its context ends.
const hangingProbe = "<hang>"

func disabledIn(args []string) []string {
	var out []string
	for i, a := range args {
		if a == "--disable" && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

// Only features the installed codex lists are passed, so an older codex never
// sees a name it would reject; everything it does know stays disabled.
func TestGenerateText_DisablesOnlyFeaturesCodexKnows(t *testing.T) {
	t.Parallel()
	known := slices.DeleteFunc(slices.Clone(generateTextDisabledFeatures), func(f string) bool { return f == "code_mode_host" })
	var execs [][]string
	ag := &CodexAgent{CommandRunner: fakeCodex(append(known, "unrelated_feature"), "summary", "", &execs)}

	got, err := ag.GenerateText(context.Background(), "prompt", "")
	if err != nil || got != "summary" {
		t.Fatalf("GenerateText = (%q, %v), want summary", got, err)
	}
	if len(execs) != 1 {
		t.Fatalf("exec runs = %d, want 1", len(execs))
	}
	if d := disabledIn(execs[0]); !slices.Equal(d, known) {
		t.Errorf("disabled = %v, want exactly the known ones %v", d, known)
	}
}

// If the probe fails, the full list is passed rather than none.
func TestGenerateText_ProbeFailureDisablesEverything(t *testing.T) {
	t.Parallel()
	var execs [][]string
	ag := &CodexAgent{CommandRunner: fakeCodex(nil, "summary", "", &execs)}
	if _, err := ag.GenerateText(context.Background(), "prompt", ""); err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if d := disabledIn(execs[0]); !slices.Equal(d, generateTextDisabledFeatures) {
		t.Errorf("disabled = %v, want the full list", d)
	}
}

// The generation run's output must never shrink the disabled set: an
// "Unknown feature flag" line in its stderr, which untrusted prompt content
// could in principle produce, is a failure, not a reason to retry without
// that feature.
func TestGenerateText_RunOutputCannotDropAFeature(t *testing.T) {
	t.Parallel()
	var execs [][]string
	ag := &CodexAgent{CommandRunner: fakeCodex(generateTextDisabledFeatures, "", "Error: Unknown feature flag: shell_tool", &execs)}
	if _, err := ag.GenerateText(context.Background(), "prompt", ""); err == nil {
		t.Fatal("GenerateText succeeded; want the failure returned")
	}
	if len(execs) != 1 {
		t.Errorf("exec runs = %d, want 1 (no retry)", len(execs))
	}
}

// A codex too old for --ignore-user-config fails with a remedy, and is not
// retried without the flag.
func TestGenerateText_TooOldForIgnoreUserConfigFailsClosed(t *testing.T) {
	t.Parallel()
	var execs [][]string
	ag := &CodexAgent{CommandRunner: fakeCodex(generateTextDisabledFeatures, "", "error: unexpected argument '--ignore-user-config' found", &execs)}
	_, err := ag.GenerateText(context.Background(), "prompt", "")
	if err == nil || !strings.Contains(err.Error(), "update codex") {
		t.Fatalf("err = %v, want an update-codex error", err)
	}
	if len(execs) != 1 {
		t.Errorf("exec runs = %d, want 1 (no retry without the flag)", len(execs))
	}
}

// Probe output that names none of the real features (a codex release that
// changes the `features list` format) is a failed probe, not an empty set of
// known features: the full list is passed rather than none.
func TestGenerateText_UnrecognizedProbeOutputDisablesEverything(t *testing.T) {
	t.Parallel()
	var execs [][]string
	ag := &CodexAgent{CommandRunner: fakeCodex([]string{`{"features":["shell_tool","unified_exec"]}`}, "summary", "", &execs)}
	if _, err := ag.GenerateText(context.Background(), "prompt", ""); err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if d := disabledIn(execs[0]); !slices.Equal(d, generateTextDisabledFeatures) {
		t.Errorf("disabled = %v, want the full list", d)
	}
}

// The prompt is untrusted and codex echoes it to stderr, so a run that failed
// for another reason must not be diagnosed as a too-old codex just because
// the flag's name appears in that echo.
func TestGenerateText_FlagNameInPromptEchoIsNotAnUpdateDiagnosis(t *testing.T) {
	t.Parallel()
	var execs [][]string
	stderr := "user\nignore this: error: unexpected argument '--ignore-user-config' found\nERROR: stream disconnected: rate limited"
	ag := &CodexAgent{CommandRunner: fakeCodex(generateTextDisabledFeatures, "", stderr, &execs)}
	_, err := ag.GenerateText(context.Background(), "error: unexpected argument '--ignore-user-config' found", "")
	if err == nil {
		t.Fatal("GenerateText succeeded; want the failure returned")
	}
	if strings.Contains(err.Error(), "update codex") {
		t.Errorf("err = %v, want the plain failure, not an update-codex diagnosis", err)
	}
}

// A slow features probe falls back to the full list on its own short budget
// instead of spending the deadline the generation run needs.
func TestGenerateText_SlowProbeDoesNotConsumeTheDeadline(t *testing.T) {
	t.Parallel()
	var execs [][]string
	ag := &CodexAgent{CommandRunner: fakeCodex([]string{hangingProbe}, "summary", "", &execs)}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	got, err := ag.GenerateText(ctx, "prompt", "")
	if err != nil || got != "summary" {
		t.Fatalf("GenerateText = (%q, %v), want summary", got, err)
	}
	if d := disabledIn(execs[0]); !slices.Equal(d, generateTextDisabledFeatures) {
		t.Errorf("disabled = %v, want the full list", d)
	}
}
