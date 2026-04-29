package checkpoint

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/redact"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gogit "github.com/go-git/go-git/v6"
)

// TestGmetaInterop_TreeLayout validates that the gmeta tree written by GmetaStore
// matches the gmeta exchange format spec. This is the core interop test — any
// compliant gmeta implementation should be able to read this tree.
//
// Validates:
//   - Ref exists at refs/meta/local/main
//   - Target path: change-id/<sha1(cpID)[:2]>/<cpID>/
//   - String values at <key>/__value
//   - List entries at <key>/__list/<timestamp-ms>-<hash5>
//   - Set entries at <key>/__set/<git-blob-sha1(value)>
//   - Session structure under session/<session-id>/
//   - Token usage under session/<id>/entire/token-usage/
func TestGmetaInterop_TreeLayout(t *testing.T) {
	t.Parallel()

	// Set up a real git repo (not go-git in-memory — we need git CLI access)
	dir := t.TempDir()
	gitInit(t, dir)

	repo, err := gogit.PlainOpen(dir)
	require.NoError(t, err)
	store := NewGmetaStore(repo)
	ctx := context.Background()

	cpID := id.MustCheckpointID("a3b2c4d5e6f7")
	sessionID := "2026-01-13-abc123"

	err = store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID:     cpID,
		SessionID:        sessionID,
		Strategy:         "manual-commit",
		Branch:           "main",
		Transcript:       redact.AlreadyRedacted([]byte(`{"role":"assistant","content":"Hello world"}`)),
		Prompts:          []string{"Build a feature"},
		FilesTouched:     []string{"src/foo.go", "src/bar.go"},
		CheckpointsCount: 3,
		AuthorName:       "Test User",
		AuthorEmail:      "test@example.com",
		Agent:            agent.AgentTypeClaudeCode,
		Model:            "claude-opus-4-6",
		TokenUsage: &agent.TokenUsage{
			InputTokens:         8500,
			OutputTokens:        3400,
			CacheReadTokens:     2100,
			CacheCreationTokens: 1200,
			APICallCount:        15,
		},
	})
	require.NoError(t, err)

	// --- Validate using git CLI (what any gmeta implementation would do) ---

	// 1. Ref must exist
	refHash := gitRevParse(t, dir, "refs/meta/local/main")
	require.NotEmpty(t, refHash, "refs/meta/local/main should exist")

	// 2. Walk the tree
	treeEntries := gitLsTreeRecursive(t, dir, "refs/meta/local/main")
	require.NotEmpty(t, treeEntries)

	// 3. Validate target path uses correct fanout
	expectedFanout := fmt.Sprintf("%02x", sha1.Sum([]byte("a3b2c4d5e6f7"))[0])
	expectedBase := "change-id/" + expectedFanout + "/a3b2c4d5e6f7/"

	// All entries should be under the expected base path
	for _, entry := range treeEntries {
		assert.True(t, strings.HasPrefix(entry.path, expectedBase),
			"entry %q should be under %q", entry.path, expectedBase)
	}

	// 4. Build a map of path -> content for easy assertions
	pathContent := make(map[string]string)
	for _, entry := range treeEntries {
		relPath := strings.TrimPrefix(entry.path, expectedBase)
		content := gitCatFile(t, dir, entry.hash)
		pathContent[relPath] = content
	}

	// --- Validate gmeta string values (key/__value) ---
	assert.Equal(t, "manual-commit", pathContent["entire/strategy/__value"])
	assert.Equal(t, "main", pathContent["entire/branch/__value"])
	assert.Equal(t, "3", pathContent["entire/checkpoints-count/__value"])
	assert.NotEmpty(t, pathContent["entire/cli-version/__value"])

	// --- Validate gmeta set (key/__set/<git-blob-sha1(value)>) ---
	setEntries := filterPrefix(pathContent, "entire/files-touched/__set/")
	assert.Len(t, setEntries, 2, "files-touched should have 2 set entries")

	// Validate set entry names are full SHA-1 values
	setValues := make(map[string]string) // filename -> content
	for path, content := range setEntries {
		filename := strings.TrimPrefix(path, "entire/files-touched/__set/")
		setValues[filename] = content

		// Verify filename = git blob sha1(content)
		expectedName := gitBlobObjectIDHex(content)
		assert.Equal(t, expectedName, filename,
			"set entry name should be git blob sha1(%q)", content)
	}

	// Verify actual file paths are in the set
	var fileValues []string
	for _, v := range setValues {
		fileValues = append(fileValues, v)
	}
	sort.Strings(fileValues)
	assert.Equal(t, []string{"src/bar.go", "src/foo.go"}, fileValues)

	// --- Validate session structure ---
	assert.Equal(t, string(agent.AgentTypeClaudeCode),
		pathContent["session/"+sessionID+"/agent/name/__value"])
	assert.Equal(t, "claude-opus-4-6",
		pathContent["session/"+sessionID+"/agent/model/__value"])
	assert.Equal(t, "Build a feature",
		pathContent["session/"+sessionID+"/prompt/__value"])

	// --- Validate gmeta list (key/__list/<timestamp-ms>-<hash5>) ---
	transcriptEntries := filterPrefix(pathContent, "session/"+sessionID+"/transcript/__list/")
	assert.NotEmpty(t, transcriptEntries, "transcript should have list entries")

	// Validate list entry ID format: <timestamp-ms>-<5-hex-chars>
	listEntryPattern := regexp.MustCompile(`^\d+-[0-9a-f]{5}$`)
	for path := range transcriptEntries {
		entryID := strings.TrimPrefix(path, "session/"+sessionID+"/transcript/__list/")
		assert.Regexp(t, listEntryPattern, entryID,
			"list entry ID %q should match <timestamp-ms>-<hash5>", entryID)
	}

	// --- Validate session IDs list ---
	idsEntries := filterPrefix(pathContent, "session/ids/__list/")
	assert.Len(t, idsEntries, 1, "should have 1 session ID list entry")
	for _, content := range idsEntries {
		assert.Equal(t, sessionID, content)
	}

	// --- Validate token usage ---
	assert.Equal(t, "8500", pathContent["session/"+sessionID+"/entire/token-usage/input/__value"])
	assert.Equal(t, "3400", pathContent["session/"+sessionID+"/entire/token-usage/output/__value"])
	assert.Equal(t, "2100", pathContent["session/"+sessionID+"/entire/token-usage/cache-read/__value"])
	assert.Equal(t, "1200", pathContent["session/"+sessionID+"/entire/token-usage/cache-creation/__value"])
	assert.Equal(t, "15", pathContent["session/"+sessionID+"/entire/token-usage/api-calls/__value"])
	assert.Equal(t, "15200", pathContent["session/"+sessionID+"/entire/token-usage/total/__value"])
}

func gitBlobObjectIDHex(content string) string {
	h := sha1.Sum(append([]byte(fmt.Sprintf("blob %d\x00", len(content))), content...)) //nolint:gosec // test helper matches Git object ID semantics
	return hex.EncodeToString(h[:])
}

// TestGmetaInterop_MultiSession validates multi-session layout.
func TestGmetaInterop_MultiSession(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	gitInit(t, dir)

	repo, err := gogit.PlainOpen(dir)
	require.NoError(t, err)
	store := NewGmetaStore(repo)
	ctx := context.Background()

	cpID := id.MustCheckpointID("bbbbbbbbbbbb")

	// Write two sessions
	for _, sid := range []string{"session-001", "session-002"} {
		writeErr := store.WriteCommitted(ctx, WriteCommittedOptions{
			CheckpointID: cpID,
			SessionID:    sid,
			Strategy:     "manual-commit",
			Transcript:   redact.AlreadyRedacted([]byte(`{"content":"hello from ` + sid + `"}`)),
			Prompts:      []string{"prompt for " + sid},
			AuthorName:   "Test",
			AuthorEmail:  "test@test.com",
			Agent:        agent.AgentTypeClaudeCode,
		})
		require.NoError(t, writeErr)
	}

	treeEntries := gitLsTreeRecursive(t, dir, "refs/meta/local/main")
	fanout := fmt.Sprintf("%02x", sha1.Sum([]byte("bbbbbbbbbbbb"))[0])
	base := "change-id/" + fanout + "/bbbbbbbbbbbb/"

	pathContent := make(map[string]string)
	for _, entry := range treeEntries {
		relPath := strings.TrimPrefix(entry.path, base)
		content := gitCatFile(t, dir, entry.hash)
		pathContent[relPath] = content
	}

	// Both sessions should exist
	assert.Equal(t, "prompt for session-001", pathContent["session/session-001/prompt/__value"])
	assert.Equal(t, "prompt for session-002", pathContent["session/session-002/prompt/__value"])

	// Session IDs list should have both entries in order
	idsEntries := filterPrefix(pathContent, "session/ids/__list/")
	assert.Len(t, idsEntries, 2)

	var idValues []string
	for _, v := range idsEntries {
		idValues = append(idValues, v)
	}
	sort.Strings(idValues)
	assert.Equal(t, []string{"session-001", "session-002"}, idValues)
}

// TestGmetaInterop_TaskCheckpoint validates task checkpoint layout.
func TestGmetaInterop_TaskCheckpoint(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	gitInit(t, dir)

	repo, err := gogit.PlainOpen(dir)
	require.NoError(t, err)
	store := NewGmetaStore(repo)
	ctx := context.Background()

	cpID := id.MustCheckpointID("cccccccccccc")

	err = store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID:   cpID,
		SessionID:      "session-001",
		Strategy:       "manual-commit",
		IsTask:         true,
		ToolUseID:      "toolu_abc123",
		AgentID:        "subagent-1",
		CheckpointUUID: "uuid-456",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
		Agent:          agent.AgentTypeClaudeCode,
	})
	require.NoError(t, err)

	treeEntries := gitLsTreeRecursive(t, dir, "refs/meta/local/main")
	fanout := fmt.Sprintf("%02x", sha1.Sum([]byte("cccccccccccc"))[0])
	base := "change-id/" + fanout + "/cccccccccccc/"

	pathContent := make(map[string]string)
	for _, entry := range treeEntries {
		relPath := strings.TrimPrefix(entry.path, base)
		content := gitCatFile(t, dir, entry.hash)
		pathContent[relPath] = content
	}

	taskPrefix := "session/session-001/task/toolu_abc123/"
	assert.Equal(t, "subagent-1", pathContent[taskPrefix+"agent-id/__value"])
	assert.Equal(t, "uuid-456", pathContent[taskPrefix+"checkpoint-uuid/__value"])
}

// TestGmetaInterop_RustCLI runs the gmeta Rust CLI against the tree if available.
// This test is skipped if the gmeta binary is not on PATH.
func TestGmetaInterop_RustCLI(t *testing.T) {
	t.Parallel()

	gmetaBin, err := exec.LookPath("gmeta")
	if err != nil {
		t.Skip("gmeta CLI not found on PATH — skipping Rust interop test")
	}

	srcDir := t.TempDir()
	gitInit(t, srcDir)

	repo, err := gogit.PlainOpen(srcDir)
	require.NoError(t, err)
	store := NewGmetaStore(repo)
	ctx := context.Background()

	cpID := id.MustCheckpointID("a3b2c4d5e6f7")
	sessionID := "2026-01-13-abc123"

	err = store.WriteCommitted(ctx, WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    sessionID,
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"role":"assistant","content":"Hello"}`)),
		Prompts:      []string{"Test prompt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
		Agent:        agent.AgentTypeClaudeCode,
		Model:        "claude-opus-4-6",
		TokenUsage: &agent.TokenUsage{
			InputTokens: 8500,
		},
	})
	require.NoError(t, err)

	remoteDir := t.TempDir()
	runCmd(t, ".", "git", "init", "--bare", remoteDir)
	runCmd(t, srcDir, "git", "push", remoteDir, GmetaRefName+":"+GmetaRemoteRefName)

	dstDir := t.TempDir()
	gitInit(t, dstDir)

	runCmd(t, dstDir, gmetaBin, "remote", "add", "--name", "meta", remoteDir)

	modelOutput := runCmdOutput(t, dstDir, gmetaBin, "get",
		"change-id:a3b2c4d5e6f7", "session:"+sessionID+":agent:model", "--json")
	assert.Contains(t, modelOutput, "claude-opus-4-6", "gmeta get should return model")

	promptOutput := runCmdOutput(t, dstDir, gmetaBin, "get",
		"change-id:a3b2c4d5e6f7", "session:"+sessionID+":prompt", "--json")
	assert.Contains(t, promptOutput, "Test prompt", "gmeta get should return prompt")

	usageOutput := runCmdOutput(t, dstDir, gmetaBin, "get",
		"change-id:a3b2c4d5e6f7", "session:"+sessionID+":entire:token-usage:input", "--json")
	assert.Contains(t, usageOutput, "8500", "gmeta get should return namespaced token usage")
}

// --- Git CLI helpers ---

type treeEntry struct {
	mode string
	kind string
	hash string
	path string
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	runCmd(t, dir, "git", "init")
	runCmd(t, dir, "git", "config", "user.name", "Test")
	runCmd(t, dir, "git", "config", "user.email", "test@test.com")
	runCmd(t, dir, "git", "config", "commit.gpgsign", "false")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("init"), 0o644))
	runCmd(t, dir, "git", "add", "README.md")
	runCmd(t, dir, "git", "commit", "-m", "initial")
}

func gitRevParse(t *testing.T, dir, ref string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "rev-parse", "--verify", ref)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitLsTreeRecursive(t *testing.T, dir, ref string) []treeEntry {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "ls-tree", "-r", ref)
	cmd.Dir = dir
	out, err := cmd.Output()
	require.NoError(t, err, "git ls-tree failed")

	var entries []treeEntry
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		// Format: <mode> <type> <hash>\t<path>
		tabIdx := strings.IndexByte(line, '\t')
		require.NotEqual(t, -1, tabIdx, "invalid ls-tree line: %q", line)

		meta := strings.Fields(line[:tabIdx])
		require.Len(t, meta, 3, "invalid ls-tree meta: %q", line)

		entries = append(entries, treeEntry{
			mode: meta[0],
			kind: meta[1],
			hash: meta[2],
			path: line[tabIdx+1:],
		})
	}
	return entries
}

func gitCatFile(t *testing.T, dir, hash string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "cat-file", "-p", hash)
	cmd.Dir = dir
	out, err := cmd.Output()
	require.NoError(t, err, "git cat-file failed for %s", hash)
	return string(out)
}

func runCmd(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "command %s %v failed: %s", name, args, out)
}

func runCmdOutput(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "command %s %v failed: %s", name, args, out)
	return string(out)
}

func filterPrefix(m map[string]string, prefix string) map[string]string {
	result := make(map[string]string)
	for k, v := range m {
		if strings.HasPrefix(k, prefix) {
			result[k] = v
		}
	}
	return result
}
