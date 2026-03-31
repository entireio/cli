package testutil

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// DeepCheckpointValidation contains expected values for comprehensive checkpoint validation.
type DeepCheckpointValidation struct {
	CheckpointID              string
	Strategy                  string
	FilesTouched              []string
	ExpectedPrompts           []string
	ExpectedTranscriptContent []string
}

var hexIDPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

// AssertFileExists asserts that at least one file matches the glob pattern
// relative to dir.
func AssertFileExists(t *testing.T, dir string, glob string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, glob))
	require.NoError(t, err)
	assert.NotEmpty(t, matches, "expected files matching %s in %s", glob, dir)
}

// WaitForFileExists polls until at least one file matches the glob pattern
// relative to dir, or fails the test after timeout. Handles the race where an
// interactive agent's prompt pattern appears before file writes land on disk.
func WaitForFileExists(t *testing.T, dir string, glob string, timeout time.Duration) {
	t.Helper()
	pattern := filepath.Join(dir, glob)
	deadline := time.Now().Add(timeout)
	for {
		matches, err := filepath.Glob(pattern)
		require.NoError(t, err)
		if len(matches) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected files matching %s in %s within %s", glob, dir, timeout)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// AssertNewCommits polls until at least `atLeast` new commits exist since setup,
// or fails after 20 seconds. Polling handles the race where an interactive
// agent's prompt pattern appears before its git commit lands on disk.
func AssertNewCommits(t *testing.T, s *RepoState, atLeast int) {
	t.Helper()
	AssertNewCommitsWithTimeout(t, s, atLeast, 20*time.Second)
}

// AssertNewCommitsWithTimeout is like AssertNewCommits but with a configurable
// timeout. Use this when WaitFor may settle on stale pane content (e.g.
// multi-turn interactive tests where the previous turn's prompt is still
// visible), giving the agent more time to complete its commit.
func AssertNewCommitsWithTimeout(t *testing.T, s *RepoState, atLeast int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		out := GitOutput(t, s.Dir, "log", "--oneline", s.HeadBefore+"..HEAD")
		var lines []string
		if out != "" {
			lines = strings.Split(strings.TrimSpace(out), "\n")
		}
		if len(lines) >= atLeast {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected at least %d new commit(s), got %d after %s", atLeast, len(lines), timeout)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// WaitForCheckpoint polls until the checkpoint branch advances from its
// initial state, or fails the test after timeout. Use this before any
// assertions that depend on the checkpoint branch (post-commit hook is async).
func WaitForCheckpoint(t *testing.T, s *RepoState, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		after := GitOutput(t, s.Dir, "rev-parse", "entire/checkpoints/v1")
		if after != s.CheckpointBefore {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("checkpoint branch did not advance within %s", timeout)
}

// shadowBranches returns all shadow branches (entire/*) excluding entire/checkpoints/*.
func shadowBranches(t *testing.T, dir string) []string {
	t.Helper()
	branches := GitOutput(t, dir, "for-each-ref", "--format=%(refname:short)", "refs/heads/entire/")
	var shadow []string
	for _, b := range strings.Split(branches, "\n") {
		b = strings.TrimSpace(b)
		if b == "" || strings.HasPrefix(b, "entire/checkpoints") {
			continue
		}
		shadow = append(shadow, b)
	}
	return shadow
}

// WaitForNoShadowBranches polls until all shadow branches are cleaned up or
// the timeout expires. Shadow branch cleanup can lag slightly behind checkpoint
// condensation (carry-forward creates intermediate branches that are deleted
// asynchronously).
func WaitForNoShadowBranches(t *testing.T, dir string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		shadow := shadowBranches(t, dir)
		if len(shadow) == 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	shadow := shadowBranches(t, dir)
	require.Emptyf(t, shadow,
		"shadow branches should be cleaned up within %s after commit, found: %v", timeout, shadow)
}

// AssertHasShadowBranches asserts that at least one shadow branch (entire/*)
// exists, excluding entire/checkpoints/*. Use this when the shadow branch is
// expected to persist (e.g., session is still idle).
func AssertHasShadowBranches(t *testing.T, dir string) {
	t.Helper()
	shadow := shadowBranches(t, dir)
	assert.NotEmpty(t, shadow,
		"expected at least one shadow branch to persist, but none found")
}

// AssertCheckpointAdvanced asserts the checkpoint branch moved forward.
func AssertCheckpointAdvanced(t *testing.T, s *RepoState) {
	t.Helper()
	after := GitOutput(t, s.Dir, "rev-parse", "entire/checkpoints/v1")
	assert.NotEqual(t, s.CheckpointBefore, after, "checkpoint branch did not advance")
}

// AssertCheckpointNotAdvanced asserts the checkpoint branch has NOT moved.
func AssertCheckpointNotAdvanced(t *testing.T, s *RepoState) {
	t.Helper()
	after := GitOutput(t, s.Dir, "rev-parse", "entire/checkpoints/v1")
	assert.Equal(t, s.CheckpointBefore, after, "checkpoint branch advanced unexpectedly")
}

// AssertCheckpointIDFormat asserts the checkpoint ID is 12 lowercase hex chars.
func AssertCheckpointIDFormat(t *testing.T, checkpointID string) {
	t.Helper()
	assert.Regexp(t, hexIDPattern, checkpointID,
		"checkpoint ID %q should be 12 lowercase hex chars", checkpointID)
}

// AssertHasCheckpointTrailer asserts the commit has an Entire-Checkpoint trailer,
// validates its format, and returns its value.
func AssertHasCheckpointTrailer(t *testing.T, dir string, ref string) string {
	t.Helper()
	trailer := GetCheckpointTrailer(t, dir, ref)
	require.NotEmpty(t, trailer, "no Entire-Checkpoint trailer on %s", ref)
	AssertCheckpointIDFormat(t, trailer)
	return trailer
}

// AssertCheckpointInLastN asserts the given checkpoint ID appears in at
// least n commits on the checkpoint branch (e.g. initial + catchup).
// Uses --grep to find matching commits regardless of position, so extra
// commits from multi-commit agent turns don't cause false failures.
func AssertCheckpointInLastN(t *testing.T, dir string, checkpointID string, n int) {
	t.Helper()
	out := GitOutput(t, dir, "log", "--grep="+checkpointID,
		"--format=%s", "entire/checkpoints/v1")
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	assert.GreaterOrEqual(t, len(lines), n,
		"expected at least %d commits mentioning %s on checkpoint branch, got %d: %v",
		n, checkpointID, len(lines), lines)
}

// AssertCheckpointExists asserts that the checkpoint ID is mentioned on
// the checkpoint branch and that its metadata.json exists in the tree.
func AssertCheckpointExists(t *testing.T, dir string, checkpointID string) {
	t.Helper()
	out := GitOutput(t, dir, "log", "entire/checkpoints/v1", "--grep="+checkpointID, "--oneline")
	assert.NotEmpty(t, out, "checkpoint %s not found on checkpoint branch", checkpointID)

	path := CheckpointPath(checkpointID) + "/metadata.json"
	blob := "entire/checkpoints/v1:" + path
	raw := gitOutputSafe(dir, "show", blob)
	assert.NotEmpty(t, raw,
		"checkpoint %s metadata not found at %s", checkpointID, path)
}

// WaitForCheckpointExists polls until the checkpoint ID appears on the
// checkpoint branch and its metadata.json is readable, or fails after timeout.
func WaitForCheckpointExists(t *testing.T, dir string, checkpointID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out := gitOutputSafe(dir, "log", "entire/checkpoints/v1", "--grep="+checkpointID, "--oneline")
		if out != "" {
			path := CheckpointPath(checkpointID) + "/metadata.json"
			blob := "entire/checkpoints/v1:" + path
			raw := gitOutputSafe(dir, "show", blob)
			if raw != "" {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("checkpoint %s not found on checkpoint branch within %s", checkpointID, timeout)
}

// AssertCommitLinkedToCheckpoint asserts the trailer exists AND the
// checkpoint data exists on the checkpoint branch.
func AssertCommitLinkedToCheckpoint(t *testing.T, dir string, ref string) {
	t.Helper()
	trailer := AssertHasCheckpointTrailer(t, dir, ref)
	AssertCheckpointExists(t, dir, trailer)
}

// AssertCheckpointHasSingleSession asserts checkpoint metadata has exactly one session.
func AssertCheckpointHasSingleSession(t *testing.T, dir string, checkpointID string) {
	t.Helper()
	meta := ReadCheckpointMetadata(t, dir, checkpointID)
	assert.Len(t, meta.Sessions, 1,
		"expected 1 session in checkpoint %s, got %d", checkpointID, len(meta.Sessions))
}

// AssertCheckpointMetadataComplete asserts essential fields in checkpoint metadata are populated.
func AssertCheckpointMetadataComplete(t *testing.T, dir string, checkpointID string) {
	t.Helper()
	meta := ReadCheckpointMetadata(t, dir, checkpointID)
	assert.NotEmpty(t, meta.CLIVersion, "checkpoint %s: cli_version should be set", checkpointID)
	assert.NotEmpty(t, meta.Strategy, "checkpoint %s: strategy should be set", checkpointID)
	assert.NotEmpty(t, meta.Sessions, "checkpoint %s: should have at least 1 session", checkpointID)
	assert.Equal(t, checkpointID, meta.CheckpointID,
		"checkpoint metadata ID should match expected")
}

// WaitForCheckpointAdvanceFrom polls until the checkpoint branch advances from
// the given ref, or fails the test after timeout. Use this when waiting for a
// second (or subsequent) checkpoint after recording the branch position.
func WaitForCheckpointAdvanceFrom(t *testing.T, dir string, fromRef string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		after := GitOutput(t, dir, "rev-parse", "entire/checkpoints/v1")
		if after != fromRef {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("checkpoint branch did not advance from %s within %s", fromRef[:8], timeout)
}

// WaitForSessionIdle polls the session state files in .git/entire-sessions/
// until no session has phase "active", or fails the test after timeout.
// This handles the race where an agent's prompt pattern appears in the TUI
// before the turn-end hook has completed (transitioning ACTIVE → IDLE).
func WaitForSessionIdle(t *testing.T, dir string, timeout time.Duration) {
	t.Helper()
	stateDir := filepath.Join(dir, ".git", "entire-sessions")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(stateDir)
		if err != nil {
			// Directory may not exist yet; keep polling
			time.Sleep(200 * time.Millisecond)
			continue
		}
		anyActive := false
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || strings.HasSuffix(entry.Name(), ".tmp") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(stateDir, entry.Name()))
			if err != nil {
				continue
			}
			var state struct {
				Phase string `json:"phase"`
			}
			if err := json.Unmarshal(data, &state); err != nil {
				continue
			}
			if state.Phase == "active" {
				anyActive = true
				break
			}
		}
		if !anyActive {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("session(s) did not transition to idle within %s", timeout)
}

// AssertNoCheckpointTrailer asserts the commit does NOT have an Entire-Checkpoint trailer.
func AssertNoCheckpointTrailer(t *testing.T, dir string, ref string) {
	t.Helper()
	trailer := GetCheckpointTrailer(t, dir, ref)
	assert.Empty(t, trailer, "expected no Entire-Checkpoint trailer on %s, got %q", ref, trailer)
}

// AssertCheckpointFilesTouched asserts the checkpoint metadata lists exactly
// the expected files in files_touched (order-independent).
func AssertCheckpointFilesTouched(t *testing.T, dir string, checkpointID string, expected []string) {
	t.Helper()
	meta := ReadCheckpointMetadata(t, dir, checkpointID)
	assert.ElementsMatch(t, expected, meta.FilesTouched,
		"checkpoint %s: files_touched mismatch", checkpointID)
}

// AssertCheckpointFilesTouchedContains asserts files_touched contains a
// specific file path (subset check, unlike the exact-match AssertCheckpointFilesTouched).
func AssertCheckpointFilesTouchedContains(t *testing.T, dir string, checkpointID string, file string) {
	t.Helper()
	meta := ReadCheckpointMetadata(t, dir, checkpointID)
	assert.Contains(t, meta.FilesTouched, file,
		"checkpoint %s: files_touched should contain %s", checkpointID, file)
}

// AssertDistinctSessions asserts session metadata across checkpoints has unique session IDs.
func AssertDistinctSessions(t *testing.T, dir string, checkpointIDs []string) {
	t.Helper()
	seen := map[string]bool{}
	for _, cpID := range checkpointIDs {
		sm := ReadSessionMetadata(t, dir, cpID, 0)
		assert.False(t, seen[sm.SessionID],
			"duplicate session_id %s across checkpoints", sm.SessionID)
		seen[sm.SessionID] = true
	}
}

// ValidateCheckpointDeep performs comprehensive validation of checkpoint metadata
// on the checkpoint branch, including transcript JSONL validity, content hash
// verification, and prompt content checking.
func ValidateCheckpointDeep(t *testing.T, dir string, v DeepCheckpointValidation) {
	t.Helper()

	// Basic metadata validation
	AssertCheckpointExists(t, dir, v.CheckpointID)
	AssertCheckpointMetadataComplete(t, dir, v.CheckpointID)

	if v.Strategy != "" {
		meta := ReadCheckpointMetadata(t, dir, v.CheckpointID)
		assert.Equal(t, v.Strategy, meta.Strategy,
			"checkpoint %s: strategy mismatch", v.CheckpointID)
	}

	if len(v.FilesTouched) > 0 {
		AssertCheckpointFilesTouched(t, dir, v.CheckpointID, v.FilesTouched)
	}

	path := CheckpointPath(v.CheckpointID)

	// Validate session metadata exists and has checkpoint_id
	sessionBlob := fmt.Sprintf("entire/checkpoints/v1:%s/0/metadata.json", path)
	sessionRaw := gitOutputSafe(dir, "show", sessionBlob)
	if assert.NotEmpty(t, sessionRaw, "session metadata should exist at %s", sessionBlob) {
		var sessionMeta map[string]any
		if assert.NoError(t, json.Unmarshal([]byte(sessionRaw), &sessionMeta)) {
			assert.Equal(t, v.CheckpointID, sessionMeta["checkpoint_id"],
				"session metadata checkpoint_id should match")
			assert.NotEmpty(t, sessionMeta["created_at"], "session metadata should have created_at")
		}
	}

	// Validate transcript is valid JSONL
	transcriptBlob := fmt.Sprintf("entire/checkpoints/v1:%s/0/full.jsonl", path)
	transcriptRaw := gitOutputSafe(dir, "show", transcriptBlob)
	if assert.NotEmpty(t, transcriptRaw, "transcript should exist at %s", transcriptBlob) {
		lines := strings.Split(transcriptRaw, "\n")
		nonEmpty := 0
		for _, line := range lines {
			if strings.TrimSpace(line) != "" {
				nonEmpty++
			}
		}
		assert.Positive(t, nonEmpty, "transcript should have at least one line")

		for _, expected := range v.ExpectedTranscriptContent {
			assert.Contains(t, transcriptRaw, expected,
				"transcript should contain %q", expected)
		}

		// Validate content hash
		hashBlob := fmt.Sprintf("entire/checkpoints/v1:%s/0/content_hash.txt", path)
		hashRaw := gitOutputSafe(dir, "show", hashBlob)
		if hashRaw != "" {
			hash := sha256.Sum256([]byte(transcriptRaw))
			expectedHash := "sha256:" + hex.EncodeToString(hash[:])
			assert.Equal(t, expectedHash, strings.TrimSpace(hashRaw),
				"content hash should match transcript SHA-256")
		}
	}

	// Validate prompt.txt if expected prompts specified
	if len(v.ExpectedPrompts) > 0 {
		promptBlob := fmt.Sprintf("entire/checkpoints/v1:%s/0/prompt.txt", path)
		promptRaw := gitOutputSafe(dir, "show", promptBlob)
		for _, expected := range v.ExpectedPrompts {
			assert.Contains(t, promptRaw, expected,
				"prompt.txt should contain %q", expected)
		}
	}
}
