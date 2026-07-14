//go:build integration

package integration

import (
	"os"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// TestSupabaseSecretRedaction_FullHookFlow is the end-to-end regression for
// issue #1716. It drives the real entire hook binary (UserPromptSubmit ->
// mid-turn commit/condensation -> Stop/finalize) on a Claude Code session
// whose transcript embeds a Supabase sb_secret_ API key across the two vectors
// from the issue report (prompt text and shell-tool input/output), then reads
// the real entire/checkpoints/v1 transcript blob back and proves:
//   - the sb_secret_ value does NOT survive into the condensed blob,
//   - a REDACTED placeholder is present,
//   - a plain capture-control marker DID survive (so a zero secret count means
//     redaction happened, not that capture failed — mirroring the issue's
//     methodology),
//   - a sb_publishable_ key (public by design) is NOT over-redacted.
//
// Every sb_secret_ occurrence uses a low-entropy synthetic token, which the
// entropy layer (threshold 4.5) misses regardless of quoting or surrounding
// prose, and no *.supabase.co URL is co-present, so the composite betterleaks
// Supabase rule does not fire either — so redaction here is attributable to
// the deterministic provider-prefix layer added for this fix.
func TestSupabaseSecretRedaction_FullHookFlow(t *testing.T) {
	// Hook subprocesses share settings/env; do not run in parallel.
	// The sb_secret_ / sb_publishable_ prefixes are assembled from fragments so
	// a complete Supabase-shaped token never appears verbatim in source, keeping
	// secret scanners (including GitHub push protection) from flagging these
	// synthetic fixtures; the runtime values are complete.
	const (
		supabaseSecret      = "sb" + "_secret_" + "probe_20260710_7f91c2d8e4a6b3f0"
		supabasePublishable = "sb" + "_publishable_" + "probe_20260710_7f91c2d8e4a6b3f0"
		captureControl      = "CAPTURE_CONTROL_MARKER_9f"
	)

	env := NewFeatureBranchEnv(t)
	session := env.NewSession()

	// Author a Claude Code transcript: a prompt that names the secret (vector 1)
	// plus the publishable control and capture marker, a Bash tool_use whose
	// command exports the secret (vector 2 input), the shell tool_result echoing
	// the secret (vector 2 output), then a file-writing tool use so the commit
	// has attributable content.
	prompt := "Configure the backend. The service_role key is " + supabaseSecret +
		" and the public client key " + supabasePublishable +
		" is safe to commit. " + captureControl
	transcript := strings.Join([]string{
		`{"uuid":"u1","type":"user","message":{"role":"user","content":"` + prompt + `"},"timestamp":"2026-01-01T00:00:00Z"}`,
		`{"uuid":"a1","type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"export SUPABASE_SERVICE_ROLE_KEY='` + supabaseSecret + `'","description":"set service role key"}}]},"timestamp":"2026-01-01T00:00:01Z"}`,
		`{"uuid":"u2","type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"Applied. Wrote key ` + supabaseSecret + ` to env. ` + captureControl + `"}]},"timestamp":"2026-01-01T00:00:02Z"}`,
		`{"uuid":"a2","type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_2","name":"Write","input":{"file_path":"feature.go","content":"package main\n"}}]},"timestamp":"2026-01-01T00:00:03Z"}`,
		`{"uuid":"u3","type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_2","content":"Success"}]},"timestamp":"2026-01-01T00:00:04Z"}`,
		`{"uuid":"a3","type":"assistant","message":{"content":[{"type":"text","text":"done"}]},"timestamp":"2026-01-01T00:00:05Z"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(session.TranscriptPath, []byte(transcript), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	if err := env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(session.ID, prompt, session.TranscriptPath); err != nil {
		t.Fatalf("UserPromptSubmit: %v", err)
	}

	// Mid-turn commit -> post-commit condensation runs redaction (redact.JSONLBytes).
	env.WriteFile("feature.go", "package main\n")
	env.GitCommitWithShadowHooks("add feature", "feature.go")

	// Stop -> finalize rewrites the turn checkpoint with the full transcript.
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if !env.BranchExists(paths.MetadataBranchName) {
		t.Fatal("entire/checkpoints/v1 should exist after condensation")
	}
	cpID := env.GetLatestCheckpointIDFromHistory()
	if cpID == "" {
		t.Fatal("no checkpoint id found in history")
	}
	sessionPath := ShardedCheckpointPath(cpID) + "/0/"

	full, ok := env.ReadFileFromBranch(paths.MetadataBranchName, sessionPath+paths.TranscriptFileName)
	if !ok {
		t.Fatalf("full.jsonl missing at %s", sessionPath)
	}

	// Evidence: dump the actual checkpoint blob (equivalent to
	// `git show entire/checkpoints/v1:<path>`).
	t.Logf("checkpoint %s blob %s%s:\n%s", cpID, sessionPath, paths.TranscriptFileName, full)

	secretCount := strings.Count(full, supabaseSecret)
	t.Logf("occurrences in condensed blob: sb_secret_=%d REDACTED=%d publishable=%d capture-control=%d",
		secretCount, strings.Count(full, "REDACTED"),
		strings.Count(full, supabasePublishable), strings.Count(full, captureControl))

	// Capture control must survive, otherwise a zero secret count is meaningless.
	if !strings.Contains(full, captureControl) {
		t.Fatalf("capture-control marker %q missing from blob — transcript content did not reach the checkpoint, so the secret check is inconclusive", captureControl)
	}
	// The bug: sb_secret_ must not survive into the checkpoint blob.
	if secretCount != 0 {
		t.Fatalf("issue #1716 regression: sb_secret_ key survived redaction into the checkpoint blob (%d occurrences)", secretCount)
	}
	if !strings.Contains(full, "REDACTED") {
		t.Fatal("expected a REDACTED placeholder in the condensed transcript")
	}
	// Publishable keys are public by design and must not be over-redacted.
	if !strings.Contains(full, supabasePublishable) {
		t.Errorf("sb_publishable_ key was over-redacted; publishable keys are designed to be public and must survive")
	}
}
