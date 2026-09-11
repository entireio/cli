//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// stopHookCmd builds a Stop-hook subprocess for a session without running it,
// so callers can start several concurrently.
func stopHookCmd(env *TestEnv, sessionID, transcriptPath string) *exec.Cmd {
	input, err := json.Marshal(map[string]string{
		"session_id":      sessionID,
		"transcript_path": transcriptPath,
	})
	if err != nil {
		env.T.Fatalf("marshal stop input: %v", err)
	}
	cmd := exec.CommandContext(context.Background(), getTestBinary(), "hooks", agentClaudeCode, "stop")
	cmd.Dir = env.RepoDir
	cmd.Stdin = bytes.NewReader(input)
	cmd.Env = append(testutil.GitIsolatedEnv(),
		"ENTIRE_TEST_CLAUDE_PROJECT_DIR="+env.ClaudeProjectDir,
	)
	cmd.Env = append(cmd.Env, env.ExtraEnv...)
	cmd.Env = append(cmd.Env, env.checkpointStoreEnv()...)
	return cmd
}

// checkpointBlob reads a path inside a stored checkpoint, resolving the storage
// topology per backend: a subtree of the shared v1 branch (git-branch) or the
// root tree of the checkpoint's own ref (git-refs).
func checkpointBlob(env *TestEnv, checkpointID, relPath string) (string, bool) {
	env.T.Helper()
	spec := paths.MetadataBranchName + ":" + id.CheckpointID(checkpointID).Path() + "/" + relPath
	if env.usingGitRefs() {
		spec = checkpointRefName(checkpointID) + ":" + relPath
	}
	cmd := exec.CommandContext(env.T.Context(), "git", "show", spec)
	cmd.Dir = env.RepoDir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return string(out), true
}

// TestConcurrent_TwoStopHooks_SameCheckpoint covers two agents working in one
// worktree whose uncondensed work lands in a single commit. Post-commit
// condenses both sessions into ONE checkpoint and records it as each session's
// turn checkpoint, so when both turns end, both Stop hooks finalize the SAME
// checkpoint's transcript.
//
// Each Stop hook holds only its own per-session state lock, so the persistent
// store must serialize writers at the shared checkpoint ref. The ref update
// must also use compare-and-swap and rebuild from a fresh tip after a conflict;
// otherwise both hooks can build sibling commits and lose one finalize.
//
// The sequential subtest is the control: identical scenario, one Stop after the
// other, both finalizes survive. Only the interleaving differs.
func TestConcurrent_TwoStopHooks_SameCheckpoint(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		t.Run("sequential", func(t *testing.T) {
			t.Parallel()
			runTwoStopHooksScenario(t, backend, false)
		})
		t.Run("concurrent", func(t *testing.T) {
			t.Parallel()
			runTwoStopHooksScenario(t, backend, true)
		})
	})
}

func runTwoStopHooksScenario(t *testing.T, backend string, concurrent bool) {
	t.Helper()
	const (
		markerA = "MARKER-SESSION-A-AFTER-COMMIT"
		markerB = "MARKER-SESSION-B-AFTER-COMMIT"
	)

	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = backend

	sessA := env.NewSession()
	sessB := env.NewSession()
	if err := env.SimulateUserPromptSubmitWithTranscriptPath(sessA.ID, sessA.TranscriptPath); err != nil {
		t.Fatalf("prompt A: %v", err)
	}
	if err := env.SimulateUserPromptSubmitWithTranscriptPath(sessB.ID, sessB.TranscriptPath); err != nil {
		t.Fatalf("prompt B: %v", err)
	}

	// Each agent writes a file. The padding widens the finalize window
	// (transcript redaction + blob writes) so the overlap is reliable rather
	// than a hairline.
	padding := strings.Repeat("// filler line to widen the write window\n", 400)
	contentA := "package a\n" + padding
	contentB := "package b\n" + padding
	env.WriteFile("agent_a.go", contentA)
	env.WriteFile("agent_b.go", contentB)
	sessA.CreateTranscript("write file a", []FileChange{{Path: "agent_a.go", Content: contentA}})
	sessB.CreateTranscript("write file b", []FileChange{{Path: "agent_b.go", Content: contentB}})

	// One commit, condensing both sessions into a single checkpoint.
	env.GitCommitWithShadowHooksAsAgent("add agent files", "agent_a.go", "agent_b.go")

	checkpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if checkpointID == "" {
		t.Fatal("commit has no Entire-Checkpoint trailer; scenario setup failed")
	}

	summaryJSON, ok := checkpointBlob(env, checkpointID, paths.MetadataFileName)
	if !ok {
		t.Fatalf("checkpoint %s not stored after post-commit", checkpointID)
	}
	var before struct {
		Sessions []json.RawMessage `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(summaryJSON), &before); err != nil {
		t.Fatalf("parse checkpoint summary: %v", err)
	}
	t.Logf("checkpoint %s holds %d sessions after post-commit", checkpointID, len(before.Sessions))
	if len(before.Sessions) < 2 {
		t.Fatalf("expected a 2-session checkpoint, got %d - scenario setup failed", len(before.Sessions))
	}

	// Each agent keeps working after the commit, so each Stop-hook finalize has
	// distinct new transcript content. The markers show which finalize landed.
	sessA.TranscriptBuilder.AddAssistantMessage(markerA)
	if err := sessA.TranscriptBuilder.WriteToFile(sessA.TranscriptPath); err != nil {
		t.Fatalf("append marker A: %v", err)
	}
	sessB.TranscriptBuilder.AddAssistantMessage(markerB)
	if err := sessB.TranscriptBuilder.WriteToFile(sessB.TranscriptPath); err != nil {
		t.Fatalf("append marker B: %v", err)
	}

	cmdA := stopHookCmd(env, sessA.ID, sessA.TranscriptPath)
	cmdB := stopHookCmd(env, sessB.ID, sessB.TranscriptPath)
	var outA, outB []byte
	var errA, errB error
	if concurrent {
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; outA, errA = cmdA.CombinedOutput() }()
		go func() { defer wg.Done(); <-start; outB, errB = cmdB.CombinedOutput() }()
		close(start)
		wg.Wait()
	} else {
		outA, errA = cmdA.CombinedOutput()
		outB, errB = cmdB.CombinedOutput()
	}
	if errA != nil {
		t.Fatalf("stop A failed: %v\n%s", errA, outA)
	}
	if errB != nil {
		t.Fatalf("stop B failed: %v\n%s", errB, outB)
	}

	// Both hooks reported success, so both finalizes must be readable from the
	// checkpoint the ref now points at.
	var stored strings.Builder
	for i := range before.Sessions {
		content, found := checkpointBlob(env, checkpointID, fmt.Sprintf("%d/full.jsonl", i))
		if !found {
			t.Errorf("session %d transcript missing from stored checkpoint", i)
			continue
		}
		stored.WriteString(content)
	}
	for name, marker := range map[string]string{"A": markerA, "B": markerB} {
		if strings.Contains(stored.String(), marker) {
			t.Logf("session %s: finalize landed", name)
			continue
		}
		t.Errorf("LOST UPDATE: session %s's post-commit transcript content is absent from "+
			"checkpoint %s - its Stop-hook finalize was silently clobbered by the concurrent one "+
			"(both hooks exited 0)", name, checkpointID)
	}
}
