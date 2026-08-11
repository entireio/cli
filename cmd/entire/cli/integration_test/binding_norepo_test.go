//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/binding"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// These tests exercise the no-repo evidence path end-to-end: the real binary's
// stop hook runs with a cwd OUTSIDE any git repo (the ~/dev/acme parent-dir
// launch, #1098) and must still record transcript evidence in the machine
// session record.
//
// CRITICAL harness detail: TestMain sets ONE shared process-wide
// ENTIRE_CONFIG_DIR for every spawned binary. Each test here gives its child a
// PER-TEST override (exec.Cmd keeps the last duplicate env entry), otherwise
// the record assertions would pollute each other.

// runNoRepoStopHook runs `entire hooks claude-code stop` from a non-repo cwd
// with a per-test config dir, and returns that config dir.
func runNoRepoStopHook(t *testing.T, nonRepoDir, sessionID, transcriptPath string) string {
	t.Helper()

	configDir := t.TempDir()
	payload, err := json.Marshal(map[string]string{
		"session_id":      sessionID,
		"transcript_path": transcriptPath,
	})
	require.NoError(t, err)

	cmd := execx.NonInteractive(context.Background(), getTestBinary(), "hooks", "claude-code", "stop")
	cmd.Dir = nonRepoDir
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Env = append(testutil.GitIsolatedEnv(), "ENTIRE_CONFIG_DIR="+configDir)

	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "stop hook in non-repo cwd must exit 0, output: %s", output)
	return configDir
}

// readNoRepoRecord reads the machine session record the CHILD wrote under its
// per-test config dir. binding.LoadRecord can't be used here: it resolves the
// TEST process' config dir, not the child's.
func readNoRepoRecord(t *testing.T, configDir, sessionID string) *binding.SessionRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(configDir, "sessions", sessionID+".json"))
	require.NoError(t, err)
	var rec binding.SessionRecord
	require.NoError(t, json.Unmarshal(data, &rec))
	return &rec
}

func claudeStopTranscriptLine(filePath string) string {
	return fmt.Sprintf(`{"type":"assistant","uuid":"u","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":%q}}]}}`, filePath)
}

func TestNoRepoStopHook_BindsForeignRepoFromTranscript(t *testing.T) {
	t.Parallel()

	// An enabled repo the transcript writes into.
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	require.NoError(t, os.MkdirAll(filepath.Join(repoDir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".entire", "settings.json"),
		[]byte(`{"enabled":true}`), 0o600))
	canonicalRepo, err := filepath.EvalSymlinks(repoDir) // macOS /var → /private/var
	require.NoError(t, err)

	nonRepoDir := t.TempDir()
	transcriptPath := filepath.Join(nonRepoDir, "transcript.jsonl")
	// Trailing newline: the cursor counts complete lines.
	require.NoError(t, os.WriteFile(transcriptPath,
		[]byte(claudeStopTranscriptLine(filepath.Join(canonicalRepo, "f.go"))+"\n"), 0o600))

	sessionID := "norepo-binding-positive"
	configDir := runNoRepoStopHook(t, nonRepoDir, sessionID, transcriptPath)

	rec := readNoRepoRecord(t, configDir, sessionID)
	require.Len(t, rec.BoundRepos, 1)
	require.Equal(t, canonicalRepo, rec.BoundRepos[0].WorktreeRoot)
	require.True(t, rec.BoundRepos[0].Enabled, "repo with .entire setup must record Enabled=true")
	require.Equal(t, 1, rec.LastScannedTranscriptCursor, "cursor must equal the transcript line count")
}

// Negative — consistent with always-advance semantics: a repo-free transcript
// still creates a cursor-only record (empty BoundRepos, cursor == line count).
// This pins both the wiring and the cursor-only-record semantics.
func TestNoRepoStopHook_RepoFreeTranscriptCreatesCursorOnlyRecord(t *testing.T) {
	t.Parallel()

	nonRepoDir := t.TempDir()
	transcriptPath := filepath.Join(nonRepoDir, "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath,
		[]byte(claudeStopTranscriptLine(filepath.Join(nonRepoDir, "scratch.txt"))+"\n"), 0o600))

	sessionID := "norepo-binding-negative"
	configDir := runNoRepoStopHook(t, nonRepoDir, sessionID, transcriptPath)

	rec := readNoRepoRecord(t, configDir, sessionID)
	require.Empty(t, rec.BoundRepos, "repo-free transcript must bind nothing")
	require.Equal(t, 1, rec.LastScannedTranscriptCursor, "cursor must still advance")
}
