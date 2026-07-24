package geminicli

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// TestGenerateText_AuthPhraseFromGeminiStderr covers the gemini-only path
// where stderr contains "Please set an Auth method" (no HTTP status). The
// shared HTTP baseline in agent.HandleTextGenResult would classify this as
// Unknown; geminicli's extraClassify hook upgrades it to Auth. The generic
// scenarios (CLIMissing, 401, empty, success) are exercised across all
// non-Claude agents by TestGenerateText_Matrix in agent/.
func TestGenerateText_AuthPhraseFromGeminiStderr(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell")
	}
	ag := &GeminiCLIAgent{
		CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "sh", "-c",
				`printf '%s' 'Please set an Auth method in your settings.json or specify one of: GEMINI_API_KEY' 1>&2; exit 41`)
		},
	}
	_, err := ag.GenerateText(context.Background(), "prompt", "")
	var tge *agent.TextGenError
	if !errors.As(err, &tge) {
		t.Fatalf("err = %v; want *agent.TextGenError", err)
	}
	if tge.Kind != agent.TextGenErrorAuth {
		t.Errorf("Kind = %q; want auth (from inline phrase)", tge.Kind)
	}
}

// TestClassifyGeminiAuthPhrase_NotTriggeredByEnvVarMention pins the narrowing
// of the gemini hook: naming GEMINI_API_KEY is not itself an auth failure.
// Quota and deprecation messages mention the variable, and forcing them to
// Auth sends the user to re-authenticate a working credential.
func TestClassifyGeminiAuthPhrase_NotTriggeredByEnvVarMention(t *testing.T) {
	t.Parallel()
	for _, stderr := range []string{
		"Quota exceeded for GEMINI_API_KEY; try again later",
		"warning: GEMINI_API_KEY is deprecated, prefer GOOGLE_API_KEY",
	} {
		if got := classifyGeminiAuthPhrase(stderr); got != agent.TextGenErrorUnknown {
			t.Errorf("classifyGeminiAuthPhrase(%q) = %q; want unknown", stderr, got)
		}
	}
	// The fixture phrase still classifies.
	fixture := "Please set an Auth method in your settings.json or specify one of: GEMINI_API_KEY"
	if got := classifyGeminiAuthPhrase(fixture); got != agent.TextGenErrorAuth {
		t.Errorf("classifyGeminiAuthPhrase(fixture) = %q; want auth", got)
	}
}

// TestGenerateText_AuthPhraseSurvivesLongStderr pins that classification runs
// on the FULL stderr, not the 500-byte-truncated Message. Gemini emits banner
// and warning noise ahead of the real error, so the auth phrase routinely lands
// past the cap.
func TestGenerateText_AuthPhraseSurvivesLongStderr(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell")
	}
	noise := strings.Repeat("warning: ignoring unknown setting\n", 20) // ~680 bytes
	ag := &GeminiCLIAgent{
		CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "sh", "-c",
				`printf '%s' '`+noise+`Please set an Auth method in your settings.json' 1>&2; exit 41`)
		},
	}
	_, err := ag.GenerateText(context.Background(), "prompt", "")
	var tge *agent.TextGenError
	if !errors.As(err, &tge) {
		t.Fatalf("err = %v; want *agent.TextGenError", err)
	}
	if tge.Kind != agent.TextGenErrorAuth {
		t.Errorf("Kind = %q; want auth (phrase sits past the 500-byte Message cap)", tge.Kind)
	}
}
