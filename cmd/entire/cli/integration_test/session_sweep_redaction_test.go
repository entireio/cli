//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// The session sweep (`entire __sweep_sessions`, spawned detached by the
// session-start hook) condenses ENDED sessions that sat uncondensed past the
// carry-forward window. Its checkpoint ships on the next push like any other,
// so it must apply the repo's redaction settings, not only the always-on
// layers. These tests drive the real binary over a session recorded by real
// hooks; only the age of its EndedAt is forged.

const (
	// Each target is caught only by the setting named for it: none is
	// secret-shaped or high-entropy enough for the always-on layers.
	sweepInlineRuleTarget = "NIGHTJAR-4821"
	sweepPackRuleTarget   = "ACMEPACK_Q7Z2K9"
	sweepEmailTarget      = "nightjar.owner@example.com"
	sweepCaptureControl   = "SWEEP_CAPTURE_CONTROL_5c"
)

// recordAgedEndedSession records a session through real hooks that touches a
// file and never commits, ends it, and backdates EndedAt past the sweep's age
// threshold. Session end leaves it uncondensed because it has touched files
// (PostCommit owns those), which is the zombie shape the sweep exists for.
func recordAgedEndedSession(t *testing.T, env *TestEnv, prompt string) {
	t.Helper()

	sess := env.NewSession()
	transcript := strings.Join([]string{
		`{"uuid":"u1","type":"user","message":{"role":"user","content":"` + prompt + `"},"timestamp":"2026-01-01T00:00:00Z"}`,
		`{"uuid":"a1","type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Write","input":{"file_path":"feature.go","content":"package main\n"}}]},"timestamp":"2026-01-01T00:00:01Z"}`,
		`{"uuid":"u2","type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"Success"}]},"timestamp":"2026-01-01T00:00:02Z"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(sess.TranscriptPath, []byte(transcript), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	if err := env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, prompt, sess.TranscriptPath); err != nil {
		t.Fatalf("UserPromptSubmit: %v", err)
	}
	env.WriteFile("feature.go", "package main\n")
	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := env.SimulateSessionEnd(sess.ID); err != nil {
		t.Fatalf("SessionEnd: %v", err)
	}

	state, err := env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState: %v", err)
	}
	if state == nil {
		t.Fatal("session state missing after session end")
	}
	if state.Phase != session.PhaseEnded || state.FullyCondensed || !state.HasPendingWork() {
		t.Fatalf("want an ENDED session with uncondensed work, got phase=%s fullyCondensed=%v pendingWork=%v",
			state.Phase, state.FullyCondensed, state.HasPendingWork())
	}
	endedAt := time.Now().Add(-48 * time.Hour)
	state.EndedAt = &endedAt
	if err := env.WriteSessionState(sess.ID, state); err != nil {
		t.Fatalf("WriteSessionState: %v", err)
	}
}

// readOnlySessionContent returns the stored transcript and prompts of the
// repo's only checkpoint.
func readOnlySessionContent(t *testing.T, env *TestEnv) (transcript, prompts string) {
	t.Helper()

	infos := assertCheckpointCount(t, env, 1, "the sweep must condense the aged ended session")
	ctx := context.Background()
	repo, err := gitrepo.OpenPath(env.RepoDir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()
	stores, err := checkpoint.Open(ctx, repo, checkpoint.OpenOptions{})
	if err != nil {
		t.Fatalf("open checkpoint stores: %v", err)
	}
	summary, err := checkpoint.ReadCheckpoint(ctx, stores.Persistent, infos[0].CheckpointID)
	if err != nil {
		t.Fatalf("read checkpoint: %v", err)
	}
	content, err := checkpoint.ReadLatestSessionContent(ctx, stores.Persistent, infos[0].CheckpointID, summary)
	if err != nil {
		t.Fatalf("read session content: %v", err)
	}
	return string(content.Transcript), content.Prompts
}

// TestSessionSweep_AppliesRepoRedactionSettings is the regression for a sweep
// that condensed with only the always-on layers: it reached the settings load
// only through its dead-owner pass, which has nothing to do when the zombie is
// already ENDED.
func TestSessionSweep_AppliesRepoRedactionSettings(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.PatchSettings(map[string]any{
		"redaction": map[string]any{
			"custom_redactions": map[string]any{
				"nightjar": `NIGHTJAR-[0-9]{4}`,
			},
			"pii": map[string]any{"enabled": true},
		},
	})
	env.WriteFile(filepath.Join(".entire", "redactors", "acme.yaml"), `name: acme
version: 1.0.0
rules:
  - id: acme-token
    regex: 'ACMEPACK_[A-Z0-9]{6}'
`)

	prompt := "Ship " + sweepInlineRuleTarget + " with " + sweepPackRuleTarget +
		" and mail " + sweepEmailTarget + " " + sweepCaptureControl
	recordAgedEndedSession(t, env, prompt)

	env.RunCLI("__sweep_sessions")

	transcript, prompts := readOnlySessionContent(t, env)
	if !strings.Contains(transcript, sweepCaptureControl) {
		t.Fatalf("capture control %q missing from the stored transcript, so the redaction checks are inconclusive:\n%s",
			sweepCaptureControl, transcript)
	}
	for name, target := range map[string]string{
		"inline custom_redactions rule": sweepInlineRuleTarget,
		".entire/redactors pack rule":   sweepPackRuleTarget,
		"PII email":                     sweepEmailTarget,
	} {
		if strings.Contains(transcript, target) {
			t.Errorf("%s target %q survived into the swept checkpoint's transcript", name, target)
		}
		if strings.Contains(prompts, target) {
			t.Errorf("%s target %q survived into the swept checkpoint's prompts", name, target)
		}
	}
}

// TestSessionSweep_InvalidScannerConfigSkipsCondense pins the fail-closed
// side: with a scanner selection the settings reject, the sweep must refuse
// before condensing rather than fall back to the default scanner set, and a
// sweep after the config is fixed must still condense the session.
//
// The error is the assertion that proves the sweep's own refusal. The checkpoint
// writer also loads settings for an unrelated reason and fails there today, so
// the no-checkpoint assertion alone would pass even without that refusal.
func TestSessionSweep_InvalidScannerConfigSkipsCondense(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	recordAgedEndedSession(t, env, "Add the feature "+sweepCaptureControl)

	// Broken only after recording: hooks refuse to run on it, so it must not
	// be in place while the session is captured.
	env.PatchSettings(map[string]any{
		"redaction": map[string]any{
			"betterleaks": map[string]any{"enabled": false},
			"goredact":    map[string]any{"enabled": false},
		},
	})

	out, err := env.RunCLIWithError("__sweep_sessions")
	if err == nil || !strings.Contains(out, settings.ErrScannerConfig.Error()) {
		t.Errorf("want the sweep to fail with %q, got err=%v output:\n%s", settings.ErrScannerConfig, err, out)
	}

	assertCheckpointCount(t, env, 0, "the sweep must not condense with a scanner set the settings did not choose")

	env.PatchSettings(map[string]any{"redaction": map[string]any{}})
	env.RunCLI("__sweep_sessions")
	assertCheckpointCount(t, env, 1, "a sweep after the config is fixed must condense the session the refused sweep left")
}
