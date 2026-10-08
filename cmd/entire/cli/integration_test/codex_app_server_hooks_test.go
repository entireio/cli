//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

const (
	requireCodexAppServerEnv = "ENTIRE_TEST_REQUIRE_CODEX_APP_SERVER"
)

func TestCodexAppServerHooksList_LinkedWorktreeUsesPrimaryCheckout(t *testing.T) {
	t.Parallel()

	codexPath := requireCodexAppServer(t)
	env := NewTestEnv(t)
	env.InitRepo()

	const primaryMarker = "primary-checkout-hook-marker"
	const linkedMarker = "linked-worktree-hook-marker"
	env.WriteFile("README.md", "# Test Repository")
	env.WriteFile(filepath.Join(".codex", codex.HooksFileName), codexHookFixture(primaryMarker))
	env.GitAdd("README.md", filepath.Join(".codex", codex.HooksFileName))
	env.GitCommit("initial commit")

	linkedParent := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(linkedParent); err == nil {
		linkedParent = resolved
	}
	linkedDir := filepath.Join(linkedParent, "linked")
	testutil.RunGit(t, env.RepoDir, "worktree", "add", "-b", "codex-hooks-test", linkedDir)
	require.NoError(t, os.RemoveAll(filepath.Join(linkedDir, ".codex")))

	withoutProjectLayer := listCodexHooks(t, codexPath, linkedDir)
	require.Len(t, withoutProjectLayer.Data, 1)
	withoutProjectLayerEntry := withoutProjectLayer.Data[0]
	require.Equal(t, linkedDir, withoutProjectLayerEntry.CWD)
	require.Empty(t, withoutProjectLayerEntry.Warnings)
	require.Empty(t, withoutProjectLayerEntry.Errors)
	require.Empty(t, withoutProjectLayerEntry.Hooks)

	require.NoError(t, os.Mkdir(filepath.Join(linkedDir, ".codex"), 0o750))
	testutil.WriteFile(t, linkedDir, filepath.Join(".codex", codex.HooksFileName), codexHookFixture(linkedMarker))

	result := listCodexHooks(t, codexPath, linkedDir)
	require.Len(t, result.Data, 1)
	entry := result.Data[0]
	require.Equal(t, linkedDir, entry.CWD)
	require.Empty(t, entry.Warnings)
	require.Empty(t, entry.Errors)
	require.NotEmpty(t, entry.Hooks, "hooks/list result: %+v", result)

	primaryHooksPath := filepath.Join(env.RepoDir, ".codex", codex.HooksFileName)
	var foundPrimary bool
	for _, hook := range entry.Hooks {
		require.NotEqual(t, linkedMarker, hook.Command, "Codex must not use the linked worktree's shadow hook")
		if hook.Command == primaryMarker {
			foundPrimary = true
			require.Equal(t, primaryHooksPath, hook.SourcePath)
		}
	}
	require.True(t, foundPrimary, "hooks/list did not return the primary checkout hook")
}

func TestCodexAppServerHooksList_BareWorktreeUsesLayoutRoot(t *testing.T) {
	t.Parallel()

	codexPath := requireCodexAppServer(t)
	tmp := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = resolved
	}
	seedRoot := filepath.Join(tmp, "seed")
	layoutRoot := filepath.Join(tmp, "layout")
	bareRoot := filepath.Join(layoutRoot, ".bare")
	mainRoot := filepath.Join(layoutRoot, "main")
	linkedRoot := filepath.Join(layoutRoot, "feature")

	testutil.InitRepo(t, seedRoot)
	testutil.WriteFile(t, seedRoot, "README.md", "# Test Repository")
	testutil.GitAdd(t, seedRoot, "README.md")
	testutil.GitCommit(t, seedRoot, "initial commit")
	require.NoError(t, os.MkdirAll(layoutRoot, 0o750))
	testutil.RunGit(t, tmp, "clone", "--bare", seedRoot, bareRoot)
	require.NoError(t, os.WriteFile(filepath.Join(layoutRoot, ".git"), []byte("gitdir: ./.bare\n"), 0o600))
	testutil.RunGit(t, tmp, "--git-dir", bareRoot, "worktree", "add", mainRoot)
	testutil.RunGit(t, tmp, "--git-dir", bareRoot, "worktree", "add", "-b", "feature", linkedRoot)

	const layoutMarker = "bare-layout-root-hook-marker"
	const linkedMarker = "bare-linked-worktree-hook-marker"
	testutil.WriteFile(t, layoutRoot, filepath.Join(".codex", codex.HooksFileName), codexHookFixture(layoutMarker))
	testutil.WriteFile(t, linkedRoot, filepath.Join(".codex", codex.HooksFileName), codexHookFixture(linkedMarker))

	result := listCodexHooks(t, codexPath, linkedRoot)
	require.Len(t, result.Data, 1)
	entry := result.Data[0]
	require.Equal(t, linkedRoot, entry.CWD)
	require.Empty(t, entry.Warnings)
	require.Empty(t, entry.Errors)
	require.NotEmpty(t, entry.Hooks, "hooks/list result: %+v", result)

	layoutHooksPath := filepath.Join(layoutRoot, ".codex", codex.HooksFileName)
	var foundLayout bool
	for _, hook := range entry.Hooks {
		require.NotEqual(t, linkedMarker, hook.Command, "Codex must not use the linked worktree's shadow hook")
		if hook.Command == layoutMarker {
			foundLayout = true
			require.Equal(t, layoutHooksPath, hook.SourcePath)
		}
	}
	require.True(t, foundLayout, "hooks/list did not return the .bare layout-root hook")
}

func requireCodexAppServer(t *testing.T) string {
	t.Helper()
	required := os.Getenv(requireCodexAppServerEnv) == "1"

	path, err := exec.LookPath("codex")
	if err != nil {
		if required {
			t.Fatalf("%s=1 but codex is not installed: %v", requireCodexAppServerEnv, err)
		}
		t.Skip("codex is not installed")
	}

	cmd := exec.CommandContext(t.Context(), path, "--version")
	output, err := cmd.CombinedOutput()
	if err != nil {
		if required {
			t.Fatalf("%s=1 but codex --version failed: %v\n%s", requireCodexAppServerEnv, err, output)
		}
		t.Skipf("codex is not runnable: %v", err)
	}
	return path
}

func codexHookFixture(command string) string {
	return fmt.Sprintf(`{
  "hooks": {
    "SessionStart": [{
      "matcher": null,
      "hooks": [{"type": "command", "command": %q, "timeout": 30}]
    }]
  }
}
`, command)
}

type codexHooksListResponse struct {
	Data []struct {
		CWD      string              `json:"cwd"`
		Hooks    []codexHookMetadata `json:"hooks"`
		Warnings []string            `json:"warnings"`
		Errors   []json.RawMessage   `json:"errors"`
	} `json:"data"`
}

type codexHookMetadata struct {
	Command    string `json:"command"`
	SourcePath string `json:"sourcePath"`
}

func listCodexHooks(t *testing.T, codexPath, cwd string) codexHooksListResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	codexHome := t.TempDir()
	config := fmt.Sprintf("[features]\nhooks = true\n\n[projects.%q]\ntrust_level = \"trusted\"\n", cwd)
	testutil.WriteFile(t, codexHome, "config.toml", config)
	cmd := exec.CommandContext(ctx, codexPath, "app-server")
	cmd.Dir = cwd
	cmd.Env = append(testutil.GitIsolatedEnv(), "CODEX_HOME="+codexHome)

	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Start())
	defer func() {
		if err := stdin.Close(); err != nil {
			t.Errorf("close app-server stdin: %v", err)
		}
		if cmd.Process != nil {
			if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Errorf("kill app-server: %v", err)
			}
		}
		if err := cmd.Wait(); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Errorf("wait for app-server: %v", err)
			}
		}
	}()

	encoder := json.NewEncoder(stdin)
	require.NoError(t, encoder.Encode(map[string]any{
		"method": "initialize",
		"id":     1,
		"params": map[string]any{
			"clientInfo": map[string]string{
				"name":    "entire_cli_integration_test",
				"title":   "Entire CLI Integration Test",
				"version": "1.0.0",
			},
		},
	}))

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	readCodexAppServerResponse(t, scanner, 1, &stderr)
	require.NoError(t, encoder.Encode(map[string]any{"method": "initialized"}))
	require.NoError(t, encoder.Encode(map[string]any{
		"method": "hooks/list",
		"id":     2,
		"params": map[string]any{"cwds": []string{cwd}},
	}))

	resultJSON := readCodexAppServerResponse(t, scanner, 2, &stderr)
	var result codexHooksListResponse
	require.NoError(t, json.Unmarshal(resultJSON, &result))
	return result
}

func readCodexAppServerResponse(t *testing.T, scanner *bufio.Scanner, responseID int, stderr *bytes.Buffer) json.RawMessage {
	t.Helper()
	for scanner.Scan() {
		var response struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &response), "app-server output: %s", scanner.Bytes())
		if response.ID == nil || *response.ID != responseID {
			continue
		}
		if len(response.Error) > 0 && string(response.Error) != "null" {
			t.Fatalf("app-server response %d failed: %s\nstderr: %s", responseID, response.Error, stderr.String())
		}
		return response.Result
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read app-server response %d: %v\nstderr: %s", responseID, err, stderr.String())
	}
	t.Fatalf("app-server exited before response %d\nstderr: %s", responseID, stderr.String())
	return nil
}
