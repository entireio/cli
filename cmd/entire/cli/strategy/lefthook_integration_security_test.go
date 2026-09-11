package strategy

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestInstallLefthookFilesRejectsSymlinkEscapes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink assertions require Unix test privileges")
	}

	t.Run("local config leaf", func(t *testing.T) {
		repoDir := newLefthookTestRepo(t)
		external := filepath.Join(t.TempDir(), "external.yml")
		original := []byte("external: unchanged\n")
		if err := os.WriteFile(external, original, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, filepath.Join(repoDir, lefthookLocalConfigName)); err != nil {
			t.Fatal(err)
		}
		_, err := installLefthookFiles(t.Context(), false)
		if err == nil {
			t.Fatal("expected symlink rejection")
		}
		assertFileBytes(t, external, original)
	})

	t.Run("script parent", func(t *testing.T) {
		repoDir := newLefthookTestRepo(t)
		externalDir := t.TempDir()
		sentinel := filepath.Join(externalDir, "sentinel")
		original := []byte("external unchanged\n")
		if err := os.WriteFile(sentinel, original, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(repoDir, lefthookLocalDir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(externalDir, filepath.Join(repoDir, lefthookLocalDir, "prepare-commit-msg")); err != nil {
			t.Fatal(err)
		}
		_, err := installLefthookFiles(t.Context(), false)
		if err == nil {
			t.Fatal("expected symlinked parent rejection")
		}
		assertFileBytes(t, sentinel, original)
		if _, statErr := os.Lstat(filepath.Join(externalDir, lefthookScriptName)); !os.IsNotExist(statErr) {
			t.Errorf("external script was created through symlink: %v", statErr)
		}
		if _, statErr := os.Lstat(filepath.Join(repoDir, lefthookLocalConfigName)); !os.IsNotExist(statErr) {
			t.Errorf("activating config exists after failure: %v", statErr)
		}
		// Make the config side look installed so health reaches the script
		// check and reports the symlinked parent rather than "not configured".
		if writeErr := os.WriteFile(filepath.Join(repoDir, entireLefthookConfigName), renderEntireLefthookConfig(), 0o644); writeErr != nil {
			t.Fatal(writeErr)
		}
		extended, extendErr := os.OpenRoot(repoDir)
		if extendErr != nil {
			t.Fatal(extendErr)
		}
		defer extended.Close()
		if _, extendErr := ensureLefthookExtends(extended); extendErr != nil {
			t.Fatal(extendErr)
		}
		if health := CheckGitHookIntegration(t.Context()); health.State != GitHookIntegrationError {
			t.Errorf("health = %+v, want error for symlinked script parent", health)
		}
	})

	t.Run("git info parent", func(t *testing.T) {
		repoDir := newLefthookTestRepo(t)
		infoDir := filepath.Join(repoDir, ".git", "info")
		if err := os.RemoveAll(infoDir); err != nil {
			t.Fatal(err)
		}
		externalDir := t.TempDir()
		externalExclude := filepath.Join(externalDir, "exclude")
		original := []byte("external-only\n")
		if err := os.WriteFile(externalExclude, original, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(externalDir, infoDir); err != nil {
			t.Fatal(err)
		}
		_, err := installLefthookFiles(t.Context(), false)
		if err == nil {
			t.Fatal("expected symlinked git info rejection")
		}
		assertFileBytes(t, externalExclude, original)
		if _, statErr := os.Lstat(filepath.Join(repoDir, lefthookLocalConfigName)); !os.IsNotExist(statErr) {
			t.Errorf("activating config exists after failure: %v", statErr)
		}
	})
}

// A failed install must leave every artifact exactly as it was found.
//
// This drives EnsureGitHookIntegration rather than the installer directly.
// The installer used to carry its own staging-and-rollback layer, which this
// test injected into; that layer was redundant — EnsureGitHookIntegration
// snapshots the same paths plus the native hooks and their backups, and
// restores all of them on any error, so the inner one could only ever undo a
// subset of what the outer one already undoes. The property under test is
// unchanged: partial work is not left behind.
func TestEnsureGitHookIntegrationRollsBackPublishedArtifacts(t *testing.T) {
	repoDir := newLefthookTestRepo(t)
	if _, err := installLefthookFiles(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	pathsToSnapshot := []string{lefthookLocalConfigName, filepath.Join(".git", "info", "exclude")}
	for _, hook := range gitHookNames {
		path := filepath.Join(lefthookLocalDir, hook, lefthookScriptName)
		pathsToSnapshot = append(pathsToSnapshot, path)
		fullPath := filepath.Join(repoDir, path)
		data, err := os.ReadFile(fullPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, append(data, []byte("# stale\n")...), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(repoDir, lefthookLocalConfigName)
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	config = bytes.Replace(config, []byte("runner: bash"), []byte("runner: stale"), 1)
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}

	before := snapshotTestFiles(t, repoDir, pathsToSnapshot)
	// Fail after the artifacts have been written, so rollback has real work.
	hookIntegrationFault = func(stage, _ string) error {
		if stage == "artifact-verification" {
			return errors.New("injected artifact verification failure")
		}
		return nil
	}
	t.Cleanup(func() { hookIntegrationFault = nil })

	if _, err := EnsureGitHookIntegration(t.Context(), false); err == nil {
		t.Fatal("expected injected artifact verification failure")
	}
	after := snapshotTestFiles(t, repoDir, pathsToSnapshot)
	for path, want := range before {
		got := after[path]
		if !bytes.Equal(got.data, want.data) || got.mode != want.mode {
			t.Errorf("%s changed after rollback: got mode %v bytes %q, want mode %v bytes %q", path, got.mode, got.data, want.mode, want.data)
		}
	}
	if matches, err := filepath.Glob(filepath.Join(repoDir, ".lefthook-local", "**", "*.tmp")); err != nil || len(matches) != 0 {
		t.Errorf("temp artifacts remain: %v, err=%v", matches, err)
	}
}
func TestDetectHookManagersForIntegrationPropagatesIOErrors(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, ".config"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := detectHookManagersForIntegration(repoDir); err == nil {
		t.Fatal("expected non-not-exist detection error")
	}
}

func newLefthookTestRepo(t *testing.T) string {
	t.Helper()
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	if err := os.WriteFile(filepath.Join(repoDir, "lefthook.yml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoDir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	return repoDir
}

func assertFileBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s = %q, want %q", path, got, want)
	}
}

type testFileSnapshot struct {
	data []byte
	mode os.FileMode
}

func snapshotTestFiles(t *testing.T, root string, names []string) map[string]testFileSnapshot {
	t.Helper()
	result := make(map[string]testFileSnapshot, len(names))
	for _, name := range names {
		path := filepath.Join(root, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		result[name] = testFileSnapshot{data: data, mode: info.Mode().Perm()}
	}
	return result
}

func TestLefthookOptionalBinarySmoke(t *testing.T) {
	binary := os.Getenv("LEFTHOOK_TEST_BINARY")
	if binary == "" {
		t.Skip("set LEFTHOOK_TEST_BINARY to run the real Lefthook stdin smoke test")
	}
	if !filepath.IsAbs(binary) || strings.TrimSpace(binary) != binary {
		t.Fatalf("LEFTHOOK_TEST_BINARY must be a clean absolute path, got %q", binary)
	}
	if _, err := os.Stat(binary); err != nil {
		t.Fatal(err)
	}
	repoDir := newLefthookTestRepo(t)
	if _, err := installLefthookFiles(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	fakeBin := t.TempDir()
	output := filepath.Join(t.TempDir(), "stdin")
	fakeEntire := filepath.Join(fakeBin, "entire")
	fake := "#!/bin/sh\nif [ \"$1\" = hooks ] && [ \"$2\" = git ] && [ \"$3\" = post-rewrite ]; then cat > \"$LEFTHOOK_SMOKE_OUTPUT\"; fi\n"
	if err := os.WriteFile(fakeEntire, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	install := exec.CommandContext(t.Context(), binary, "install", "post-rewrite")
	install.Dir = repoDir
	install.Env = append(os.Environ(), "PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if out, err := install.CombinedOutput(); err != nil {
		t.Fatalf("lefthook install: %v\n%s", err, out)
	}
	hookPath := filepath.Join(repoDir, ".git", "hooks", "post-rewrite")
	generated, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	if !looksLikeLefthookHook(generated, "post-rewrite") {
		t.Fatal("pinned Lefthook v2 launcher was not recognized as an active hook")
	}
	hook := exec.CommandContext(t.Context(), hookPath, "rebase")
	hook.Dir = repoDir
	hook.Stdin = strings.NewReader("old new\n")
	hook.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"LEFTHOOK_SMOKE_OUTPUT="+output,
	)
	if out, err := hook.CombinedOutput(); err != nil {
		t.Fatalf("generated post-rewrite hook: %v\n%s", err, out)
	}
	assertFileBytes(t, output, []byte("old new\n"))
}
