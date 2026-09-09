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
	"gopkg.in/yaml.v3"
)

func TestMergeLefthookLocalConfigOwnsSourceDirAndPostRewriteStdin(t *testing.T) {
	t.Parallel()
	merged, err := mergeLefthookLocalConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(merged, &document); err != nil {
		t.Fatal(err)
	}
	root := document.Content[0]
	sourceKey, sourceValue := testYAMLPair(t, root, "source_dir_local")
	if sourceValue.Value != lefthookLocalDir || !lefthookNodeOwned(sourceKey, sourceValue) {
		t.Errorf("source_dir_local = value %q owned %v, want %q with marker\n%s", sourceValue.Value, lefthookNodeOwned(sourceKey, sourceValue), lefthookLocalDir, merged)
	}
	postRewrite := testLefthookEntry(t, root, "post-rewrite")
	_, useStdin := testYAMLPair(t, postRewrite, "use_stdin")
	if useStdin.Value != "true" {
		t.Errorf("post-rewrite use_stdin = %q, want true", useStdin.Value)
	}
	prePush := testLefthookEntry(t, root, "pre-push")
	if _, ok := mappingValue(prePush, "use_stdin"); ok {
		t.Error("pre-push must not claim stdin because Entire's handler does not read it")
	}
}

func TestMergeLefthookLocalConfigRejectsConflictingSourceDirLocal(t *testing.T) {
	t.Parallel()
	input := []byte("source_dir_local: user-hooks\n# keep me\n")
	before := bytes.Clone(input)
	_, err := mergeLefthookLocalConfig(input)
	if !errors.Is(err, ErrLefthookOwnedEntryConflict) {
		t.Fatalf("error = %v, want ErrLefthookOwnedEntryConflict", err)
	}
	if !bytes.Equal(input, before) {
		t.Errorf("input mutated\nbefore=%q\nafter=%q", before, input)
	}
}

func TestSetOwnedLefthookSourceDirPreservesCompatibleUnownedSettingAndComments(t *testing.T) {
	t.Parallel()
	key := &yaml.Node{Kind: yaml.ScalarNode, Value: "source_dir_local", HeadComment: "user head", LineComment: "user line", FootComment: "user foot"}
	value := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: lefthookLocalDir, HeadComment: "value head", LineComment: "value line", FootComment: "value foot"}
	root := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{key, value}}
	if err := setOwnedLefthookSourceDir(root); err != nil {
		t.Fatalf("setOwnedLefthookSourceDir() error = %v", err)
	}
	if lefthookNodeOwned(key, value) {
		t.Fatal("compatible user source_dir_local was claimed by Entire")
	}
	if key.HeadComment != "user head" || key.LineComment != "user line" || key.FootComment != "user foot" ||
		value.HeadComment != "value head" || value.LineComment != "value line" || value.FootComment != "value foot" {
		t.Errorf("user comments changed: key=%+v value=%+v", key, value)
	}
}

func TestMergeLefthookLocalConfigRejectsDuplicateManagedMappings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
	}{
		{name: "managed hook", input: "pre-push: {}\npre-push: {}\n"},
		{name: "scripts", input: "pre-push:\n  scripts: {}\n  scripts: {}\n"},
		{name: "source dir local", input: "source_dir_local: .lefthook-local\nsource_dir_local: .lefthook-local\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := mergeLefthookLocalConfig([]byte(tt.input)); err == nil {
				t.Fatal("expected duplicate mapping error")
			}
		})
	}
}

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
		merged, mergeErr := mergeLefthookLocalConfig(nil)
		if mergeErr != nil {
			t.Fatal(mergeErr)
		}
		if writeErr := os.WriteFile(filepath.Join(repoDir, lefthookLocalConfigName), merged, 0o644); writeErr != nil {
			t.Fatal(writeErr)
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

func TestInstallLefthookFilesRollsBackPublishedArtifacts(t *testing.T) {
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
	publishes := 0
	failPublish := func(string) error {
		publishes++
		if publishes == 3 {
			return errors.New("injected publish failure")
		}
		return nil
	}
	if _, err := installLefthookFilesAt(t.Context(), repoDir, false, failPublish); err == nil {
		t.Fatal("expected injected publish failure")
	}
	after := snapshotTestFiles(t, repoDir, pathsToSnapshot)
	for path, want := range before {
		got := after[path]
		if !bytes.Equal(got.data, want.data) || got.mode != want.mode {
			t.Errorf("%s changed after rollback: got mode %v bytes %q, want mode %v bytes %q", path, got.mode, got.data, want.mode, want.data)
		}
	}
	if matches, err := filepath.Glob(filepath.Join(repoDir, ".lefthook-local", "**", "*.tmp")); err != nil || len(matches) != 0 {
		t.Errorf("staged temp artifacts remain: %v, err=%v", matches, err)
	}
}

func TestValidateLefthookMainConfig(t *testing.T) {
	tests := []struct {
		name       string
		configName string
		config     string
		asDir      bool
		setup      func(t *testing.T, repoDir string)
	}{
		{name: "malformed", configName: "lefthook.yml", config: "pre-push: [\n"},
		{name: "directory", configName: "lefthook.yml", asDir: true},
		{name: "custom local source", configName: "lefthook.yml", config: "source_dir_local: custom-local\n"},
		{name: "source dirs collide", configName: "lefthook.yml", config: "source_dir: .lefthook-local\n"},
		{name: "unsupported toml", configName: "lefthook.toml", config: "source_dir = '.lefthook'\n"},
		{
			name:       "managed script collides in source dir",
			configName: "lefthook.yml",
			config:     "source_dir: custom-hooks\n",
			setup: func(t *testing.T, repoDir string) {
				t.Helper()
				dir := filepath.Join(repoDir, "custom-hooks", "pre-push")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, lefthookScriptName), []byte("user\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoDir := t.TempDir()
			testutil.InitRepo(t, repoDir)
			path := filepath.Join(repoDir, tt.configName)
			if tt.asDir {
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte(tt.config), 0o644); err != nil {
				t.Fatal(err)
			}
			if tt.setup != nil {
				tt.setup(t, repoDir)
			}
			t.Chdir(repoDir)
			paths.ClearWorktreeRootCache()
			t.Cleanup(paths.ClearWorktreeRootCache)
			if _, err := installLefthookFiles(t.Context(), false); err == nil {
				t.Fatal("expected main config validation error")
			}
			health := CheckGitHookIntegration(t.Context())
			if health.State == GitHookIntegrationCurrent {
				t.Errorf("health = %+v, must not be current", health)
			}
			if tt.asDir && (health.Mode != GitHookIntegrationLefthook || health.State != GitHookIntegrationError || health.ReasonCode != "hook_manager_detection_error") {
				t.Errorf("directory candidate health = %+v, want Lefthook manager-detection error", health)
			}
		})
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

func testYAMLPair(t *testing.T, mapping *yaml.Node, key string) (*yaml.Node, *yaml.Node) {
	t.Helper()
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i], mapping.Content[i+1]
		}
	}
	t.Fatalf("missing YAML key %q", key)
	return nil, nil
}

func testLefthookEntry(t *testing.T, root *yaml.Node, hook string) *yaml.Node {
	t.Helper()
	_, hookNode := testYAMLPair(t, root, hook)
	_, scripts := testYAMLPair(t, hookNode, "scripts")
	_, entry := testYAMLPair(t, scripts, lefthookScriptName)
	return entry
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
