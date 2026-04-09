package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

func TestCheckPRBinaries_AddThenDeleteStillFails(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "README.md", "init\n")
	testutil.GitAdd(t, repoDir, "README.md")
	testutil.GitCommit(t, repoDir, "init")
	baseSHA := testutil.GetHeadHash(t, repoDir)

	largeBinary := bytes.Repeat([]byte{0}, 1048577)
	err := os.WriteFile(filepath.Join(repoDir, "oversized.bin"), largeBinary, 0o644)
	require.NoError(t, err)
	testutil.GitAdd(t, repoDir, "oversized.bin")
	testutil.GitCommit(t, repoDir, "add oversized binary")

	headWithBinary := testutil.GetHeadHash(t, repoDir)
	output, err := runBinaryCheckScript(t, repoDir, baseSHA, headWithBinary)
	require.Error(t, err)
	require.Contains(t, output, "oversized.bin")

	runGitCommand(t, repoDir, "rm", "oversized.bin")
	testutil.GitCommit(t, repoDir, "delete oversized binary")

	output, err = runBinaryCheckScript(t, repoDir, baseSHA, testutil.GetHeadHash(t, repoDir))
	require.Error(t, err)
	require.Contains(t, output, "oversized.bin")
}

func TestCheckPRBinaries_MergeCommitStillFails(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "README.md", "init\n")
	testutil.GitAdd(t, repoDir, "README.md")
	testutil.GitCommit(t, repoDir, "init")
	baseSHA := testutil.GetHeadHash(t, repoDir)

	runGitCommand(t, repoDir, "checkout", "-b", "feature")
	testutil.WriteFile(t, repoDir, "feature.txt", "feature\n")
	testutil.GitAdd(t, repoDir, "feature.txt")
	testutil.GitCommit(t, repoDir, "feature change")

	runGitCommand(t, repoDir, "checkout", "-b", "side", baseSHA)
	largeBinary := bytes.Repeat([]byte{0}, 1048577)
	err := os.WriteFile(filepath.Join(repoDir, "oversized.bin"), largeBinary, 0o644)
	require.NoError(t, err)
	testutil.GitAdd(t, repoDir, "oversized.bin")
	testutil.GitCommit(t, repoDir, "add oversized binary")

	runGitCommand(t, repoDir, "checkout", "feature")
	runGitCommand(t, repoDir, "merge", "--no-ff", "side", "-m", "merge side")

	output, err := runBinaryCheckScript(t, repoDir, baseSHA, testutil.GetHeadHash(t, repoDir))
	require.Error(t, err)
	require.Contains(t, output, "oversized.bin")
}

func TestCheckPRBinaries_SmallBinaryPasses(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "README.md", "init\n")
	testutil.GitAdd(t, repoDir, "README.md")
	testutil.GitCommit(t, repoDir, "init")
	baseSHA := testutil.GetHeadHash(t, repoDir)

	writeBinaryFile(t, repoDir, "small.bin", bytes.Repeat([]byte{0}, 1024))
	testutil.GitAdd(t, repoDir, "small.bin")
	testutil.GitCommit(t, repoDir, "add small binary")

	output, err := runBinaryCheckScript(t, repoDir, baseSHA, testutil.GetHeadHash(t, repoDir))
	require.NoError(t, err)
	require.Contains(t, output, "No oversized binary files found.")
}

func TestCheckPRBinaries_ModifiedOversizedBinaryFails(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "README.md", "init\n")
	testutil.GitAdd(t, repoDir, "README.md")
	writeBinaryFile(t, repoDir, "asset.bin", bytes.Repeat([]byte{0}, 1024))
	testutil.GitAdd(t, repoDir, "asset.bin")
	testutil.GitCommit(t, repoDir, "init")
	baseSHA := testutil.GetHeadHash(t, repoDir)

	writeBinaryFile(t, repoDir, "asset.bin", bytes.Repeat([]byte{1}, 1048577))
	testutil.GitAdd(t, repoDir, "asset.bin")
	testutil.GitCommit(t, repoDir, "grow binary")

	output, err := runBinaryCheckScript(t, repoDir, baseSHA, testutil.GetHeadHash(t, repoDir))
	require.Error(t, err)
	require.Contains(t, output, "asset.bin")
}

func TestCheckPRBinaries_LargeTextFilePasses(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "README.md", "init\n")
	testutil.GitAdd(t, repoDir, "README.md")
	testutil.GitCommit(t, repoDir, "init")
	baseSHA := testutil.GetHeadHash(t, repoDir)

	largeText := bytes.Repeat([]byte("a"), 1048577)
	err := os.WriteFile(filepath.Join(repoDir, "large.txt"), largeText, 0o644)
	require.NoError(t, err)
	testutil.GitAdd(t, repoDir, "large.txt")
	testutil.GitCommit(t, repoDir, "add large text file")

	output, err := runBinaryCheckScript(t, repoDir, baseSHA, testutil.GetHeadHash(t, repoDir))
	require.NoError(t, err)
	require.Contains(t, output, "No oversized binary files found.")
}

func TestCheckPRBinaries_CustomThreshold(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "README.md", "init\n")
	testutil.GitAdd(t, repoDir, "README.md")
	testutil.GitCommit(t, repoDir, "init")
	baseSHA := testutil.GetHeadHash(t, repoDir)

	writeBinaryFile(t, repoDir, "large.bin", bytes.Repeat([]byte{0}, 1572864))
	testutil.GitAdd(t, repoDir, "large.bin")
	testutil.GitCommit(t, repoDir, "add medium binary")

	output, err := runBinaryCheckScriptWithEnv(t, repoDir, baseSHA, testutil.GetHeadHash(t, repoDir), map[string]string{
		"MAX_BINARY_SIZE_BYTES": "2097152",
	})
	require.NoError(t, err)
	require.Contains(t, output, "No oversized binary files found.")
}

func TestCheckPRBinaries_InvalidThresholdFails(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "README.md", "init\n")
	testutil.GitAdd(t, repoDir, "README.md")
	testutil.GitCommit(t, repoDir, "init")

	output, err := runBinaryCheckScriptWithEnv(t, repoDir, testutil.GetHeadHash(t, repoDir), "HEAD", map[string]string{
		"MAX_BINARY_SIZE_BYTES": "not-a-number",
	})
	require.Error(t, err)
	require.Contains(t, output, "MAX_BINARY_SIZE_BYTES must be an integer")
}

func runBinaryCheckScript(t *testing.T, repoDir, baseSHA, headSHA string) (string, error) {
	t.Helper()

	return runBinaryCheckScriptWithEnv(t, repoDir, baseSHA, headSHA, nil)
}

func runBinaryCheckScriptWithEnv(t *testing.T, repoDir, baseSHA, headSHA string, extraEnv map[string]string) (string, error) {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok)

	scriptPath := filepath.Join(filepath.Dir(filename), "..", "..", "..", "scripts", "check-pr-binaries.sh")
	cmd := exec.CommandContext(context.Background(), "bash", scriptPath, baseSHA, headSHA)
	cmd.Dir = repoDir
	cmd.Env = os.Environ()
	for key, value := range extraEnv {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func writeBinaryFile(t *testing.T, repoDir, path string, content []byte) {
	t.Helper()

	err := os.WriteFile(filepath.Join(repoDir, path), content, 0o644)
	require.NoError(t, err)
}

func runGitCommand(t *testing.T, repoDir string, args ...string) {
	t.Helper()

	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = repoDir
	output, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %v failed: %s", args, output)
}
