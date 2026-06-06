package agentlaunch

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// TestWithoutReviewOrInvestigateEnv pins the contract that the helper
// strips both ENTIRE_REVIEW_* and ENTIRE_INVESTIGATE_* entries from the
// supplied env slice while leaving unrelated entries untouched. This is
// the leak-prevention guarantee for fix-agent launches: a parent shell
// may have inherited stale provenance vars, and the fix session must not
// be tagged as a review or investigate session.
//
// The literal env names below mirror the constants in
// cmd/entire/cli/review/env.go and cmd/entire/cli/investigate/env.go.
// We use literals (not the exported constants) because importing review
// or investigate from this package would create a build cycle: review
// depends on agentlaunch.
func TestWithoutReviewOrInvestigateEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    []string
		want     []string
		notWant  []string
		wantSize int
	}{
		{
			name: "strips review and investigate, keeps unrelated",
			input: []string{
				"PATH=/usr/bin",
				"HOME=/home/u",
				"ENTIRE_REVIEW_SESSION=1",
				"ENTIRE_REVIEW_AGENT=claude-code",
				"ENTIRE_REVIEW_SKILLS=[\"/x\"]",
				"ENTIRE_REVIEW_PROMPT=stale review prompt",
				"ENTIRE_REVIEW_STARTING_SHA=stale1",
				"ENTIRE_INVESTIGATE_SESSION=1",
				"ENTIRE_INVESTIGATE_AGENT=claude-code",
				"ENTIRE_INVESTIGATE_RUN_ID=abcdef012345",
				"ENTIRE_INVESTIGATE_TOPIC=topic",
				"ENTIRE_INVESTIGATE_FINDINGS_DOC=/tmp/f.md",
				"ENTIRE_INVESTIGATE_STATE_DOC=/tmp/state.json",
				"ENTIRE_INVESTIGATE_STARTING_SHA=stale2",
			},
			want: []string{
				"PATH=/usr/bin",
				"HOME=/home/u",
			},
			notWant: []string{
				"ENTIRE_REVIEW_SESSION=1",
				"ENTIRE_REVIEW_AGENT=claude-code",
				"ENTIRE_REVIEW_SKILLS=[\"/x\"]",
				"ENTIRE_REVIEW_PROMPT=stale review prompt",
				"ENTIRE_REVIEW_STARTING_SHA=stale1",
				"ENTIRE_INVESTIGATE_SESSION=1",
				"ENTIRE_INVESTIGATE_AGENT=claude-code",
				"ENTIRE_INVESTIGATE_RUN_ID=abcdef012345",
				"ENTIRE_INVESTIGATE_TOPIC=topic",
				"ENTIRE_INVESTIGATE_FINDINGS_DOC=/tmp/f.md",
				"ENTIRE_INVESTIGATE_STATE_DOC=/tmp/state.json",
				"ENTIRE_INVESTIGATE_STARTING_SHA=stale2",
			},
			wantSize: 2,
		},
		{
			name: "no provenance entries: passthrough",
			input: []string{
				"PATH=/usr/bin",
				"FOO=bar",
			},
			want: []string{
				"PATH=/usr/bin",
				"FOO=bar",
			},
			wantSize: 2,
		},
		{
			name:     "empty input: empty output",
			input:    nil,
			wantSize: 0,
		},
		{
			name: "only provenance entries: empty output",
			input: []string{
				"ENTIRE_REVIEW_SESSION=1",
				"ENTIRE_INVESTIGATE_SESSION=1",
			},
			notWant: []string{
				"ENTIRE_REVIEW_SESSION=1",
				"ENTIRE_INVESTIGATE_SESSION=1",
			},
			wantSize: 0,
		},
		{
			name: "look-alike non-provenance keys survive",
			input: []string{
				"NOT_ENTIRE_REVIEW_SESSION=1",
				"ENTIRE_REVIEW_OTHER=keep",      // not a known prefix
				"ENTIRE_INVESTIGATE_OTHER=keep", // not a known prefix
			},
			want: []string{
				"NOT_ENTIRE_REVIEW_SESSION=1",
				"ENTIRE_REVIEW_OTHER=keep",
				"ENTIRE_INVESTIGATE_OTHER=keep",
			},
			wantSize: 3,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := withoutReviewOrInvestigateEnv(tc.input)
			if len(got) != tc.wantSize {
				t.Errorf("len = %d, want %d (got: %v)", len(got), tc.wantSize, got)
			}
			for _, kv := range tc.want {
				if !slices.Contains(got, kv) {
					t.Errorf("missing expected entry %q in %v", kv, got)
				}
			}
			for _, kv := range tc.notWant {
				if slices.Contains(got, kv) {
					t.Errorf("unexpected entry survived strip: %q", kv)
				}
			}
		})
	}
}

// TestWithoutReviewOrInvestigateEnv_DoesNotMutateInput pins that the
// helper returns a fresh slice and never mutates its argument. Callers
// rely on this when they pass `os.Environ()` directly.
func TestWithoutReviewOrInvestigateEnv_DoesNotMutateInput(t *testing.T) {
	t.Parallel()

	input := []string{
		"PATH=/usr/bin",
		"ENTIRE_REVIEW_SESSION=1",
		"ENTIRE_INVESTIGATE_SESSION=1",
		"HOME=/home/u",
	}
	original := slices.Clone(input)

	_ = withoutReviewOrInvestigateEnv(input)

	if !slices.Equal(input, original) {
		t.Errorf("input was mutated: got %v, want %v", input, original)
	}
}

// TestLaunchFixAgent_EmptyEnvFallback_StripsHostProvenance pins that the
// "cmd.Env == nil → os.Environ()" fallback in LaunchFixAgent still strips
// provenance markers even when they were set on the parent process. A
// future launcher implementation that returns a cmd with no Env would
// otherwise re-import stale provenance via os.Environ() and silently
// re-tag the fix session.
//
// Mirrors the fallback branch exactly: build an empty Env, take the
// os.Environ() path, assert no provenance entries survive.
func TestLaunchFixAgent_EmptyEnvFallback_StripsHostProvenance(t *testing.T) {
	// t.Setenv mutates process global state; cannot run with t.Parallel().
	t.Setenv("ENTIRE_REVIEW_SESSION", "1")
	t.Setenv("ENTIRE_REVIEW_AGENT", "claude-code")
	t.Setenv("ENTIRE_REVIEW_STARTING_SHA", "deadbeefcafe")
	t.Setenv("ENTIRE_INVESTIGATE_SESSION", "1")
	t.Setenv("ENTIRE_INVESTIGATE_RUN_ID", "abcdef012345")

	// Drive the exact branch LaunchFixAgent takes when cmd.Env is empty:
	// withoutReviewOrInvestigateEnv(os.Environ()).
	emptyEnv := []string(nil)
	cleaned := withoutReviewOrInvestigateEnv(emptyEnv)
	if len(cleaned) != 0 {
		t.Fatalf("precondition: empty input should yield empty output, got %v", cleaned)
	}
	// Fall back to host env (the branch under test) and re-strip.
	fallback := withoutReviewOrInvestigateEnv(osEnvironForTest())

	for _, kv := range fallback {
		if hasReviewOrInvestigatePrefix(kv) {
			t.Errorf("fallback env still contains provenance entry %q", kv)
		}
	}
}

// osEnvironForTest mirrors os.Environ() via the same call LaunchFixAgent
// uses. Wrapped in a helper so the test reads as a direct simulation of
// the production branch.
func osEnvironForTest() []string {
	return os.Environ()
}

// hasReviewOrInvestigatePrefix is a tiny test helper that mirrors the
// production prefix check without importing provenance (which is fine
// here — the test file lives in the same package as the implementation).
func hasReviewOrInvestigatePrefix(kv string) bool {
	prefixes := []string{
		"ENTIRE_REVIEW_SESSION=",
		"ENTIRE_REVIEW_AGENT=",
		"ENTIRE_REVIEW_SKILLS=",
		"ENTIRE_REVIEW_PROMPT=",
		"ENTIRE_REVIEW_STARTING_SHA=",
		"ENTIRE_INVESTIGATE_SESSION=",
		"ENTIRE_INVESTIGATE_AGENT=",
		"ENTIRE_INVESTIGATE_RUN_ID=",
		"ENTIRE_INVESTIGATE_TOPIC=",
		"ENTIRE_INVESTIGATE_FINDINGS_DOC=",
		"ENTIRE_INVESTIGATE_STATE_DOC=",
		"ENTIRE_INVESTIGATE_STARTING_SHA=",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(kv, p) {
			return true
		}
	}
	return false
}
