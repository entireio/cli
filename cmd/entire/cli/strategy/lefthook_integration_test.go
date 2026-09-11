package strategy

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestRenderLefthookScript(t *testing.T) {
	t.Parallel()
	for _, spec := range buildHookSpecs("entire") {
		t.Run(spec.name, func(t *testing.T) {
			t.Parallel()
			got := renderLefthookScript(spec)
			if !strings.Contains(got, "# "+lefthookOwnedMarker+"\n") {
				t.Errorf("renderLefthookScript() missing exact ownership marker\n%s", got)
			}
			if !strings.Contains(got, wantLefthookMissingEntireWarning(spec.name)) {
				t.Errorf("renderLefthookScript() missing warning for %s\n%s", spec.name, got)
			}
		})
	}
}

func TestInstallLefthookFiles(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	clearGlobalHooksPath(t, repoDir)
	if err := os.MkdirAll(filepath.Join(repoDir, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, ".git", "info", "exclude"), []byte("# user patterns\n*.scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "lefthook.yml"), []byte("pre_commit:\n  commands:\n    lint:\n      run: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoDir)
	paths.ClearWorktreeRootCache()
	session.ClearGitCommonDirCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	t.Cleanup(session.ClearGitCommonDirCache)
	excludePath := filepath.Join(repoDir, ".git", "info", "exclude")
	excludeBefore, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatal(err)
	}

	written, err := installLefthookFiles(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if written != 6 {
		t.Errorf("written = %d, want 6 (config plus five scripts)", written)
	}
	for _, hook := range gitHookNames {
		path := filepath.Join(repoDir, ".lefthook-local", hook, lefthookScriptName)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("stat %s: %v", path, err)
			continue
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s mode = %v, want executable", path, info.Mode())
		}
	}
	exclude, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(exclude, excludeBefore) {
		t.Errorf("existing user exclude patterns changed\nbefore:\n%s\nafter:\n%s", excludeBefore, exclude)
	}
	if count := strings.Count(string(exclude), "/"+lefthookLocalConfigName); count != 1 {
		t.Errorf("exclude count for config = %d, want 1\n%s", count, exclude)
	}
	for _, hook := range gitHookNames {
		want := "/.lefthook-local/" + hook + "/" + lefthookScriptName
		if count := strings.Count(string(exclude), want); count != 1 {
			t.Errorf("exclude count for %q = %d, want 1\n%s", want, count, exclude)
		}
	}

	written, err = installLefthookFiles(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if written != 0 {
		t.Errorf("second written = %d, want 0", written)
	}
	configPath := filepath.Join(repoDir, lefthookLocalConfigName)
	configBefore, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	configInfoBefore, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	publishes := 0
	written, err = installLefthookFilesAt(t.Context(), repoDir, false, func(string) error {
		publishes++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if written != 0 || publishes != 0 {
		t.Errorf("no-op reinstall = written %d, publish callbacks %d; want both zero", written, publishes)
	}
	configAfter, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	configInfoAfter, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(configAfter, configBefore) || configInfoAfter.Mode() != configInfoBefore.Mode() ||
		!configInfoAfter.ModTime().Equal(configInfoBefore.ModTime()) || !os.SameFile(configInfoBefore, configInfoAfter) {
		t.Errorf("no-op reinstall replaced or changed config: same bytes=%v mode before/after=%v/%v mtime before/after=%v/%v same file=%v",
			bytes.Equal(configAfter, configBefore), configInfoBefore.Mode(), configInfoAfter.Mode(), configInfoBefore.ModTime(), configInfoAfter.ModTime(), os.SameFile(configInfoBefore, configInfoAfter))
	}
}

func TestInstallLefthookFilesRejectsBroadMarkerScriptConflict(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	if err := os.WriteFile(filepath.Join(repoDir, "lefthook.yml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	scriptDir := filepath.Join(repoDir, lefthookLocalDir, "pre-push")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(scriptDir, lefthookScriptName)
	userScript := []byte("#!/bin/sh\n# user documentation mentions Entire CLI hooks\necho user\n")
	if err := os.WriteFile(scriptPath, userScript, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoDir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	_, err := installLefthookFiles(t.Context(), false)
	if !errors.Is(err, ErrLefthookOwnedEntryConflict) {
		t.Fatalf("error = %v, want ErrLefthookOwnedEntryConflict", err)
	}
	after, readErr := os.ReadFile(scriptPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(after, userScript) {
		t.Errorf("conflicting user script was overwritten\nbefore:\n%s\nafter:\n%s", userScript, after)
	}
}

func TestLefthookScriptRuntimeSemantics(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dir, "record")
	fake := "#!/bin/sh\nprintf '<%s>\\n' \"$@\" >> \"$ENTIRE_TEST_RECORD\"\n/bin/cat >> \"$ENTIRE_TEST_RECORD\"\nexit \"${ENTIRE_TEST_EXIT:-0}\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "entire"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}

	exitCode := ""
	run := func(t *testing.T, hook string, stdin string, args ...string) (string, error) {
		t.Helper()
		spec := findHookSpec(t, buildHookSpecs("entire"), hook)
		path := filepath.Join(dir, hook+".sh")
		if err := os.WriteFile(path, []byte(renderLefthookScript(spec)), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), path, args...)
		cmd.Env = append(os.Environ(), "PATH="+binDir, "ENTIRE_TEST_RECORD="+record, "ENTIRE_TEST_EXIT="+exitCode)
		cmd.Stdin = strings.NewReader(stdin)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		return stderr.String(), err
	}

	if _, err := run(t, "prepare-commit-msg", "", "message file", "commit"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "post-rewrite", "old new\n", "rebase"); err != nil {
		t.Fatal(err)
	}
	recorded, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	text := string(recorded)
	if !strings.Contains(text, "<hooks>\n<git>\n<prepare-commit-msg>\n<message file>\n<commit>") ||
		!strings.Contains(text, "<hooks>\n<git>\n<post-rewrite>\n<rebase>\nold new") {
		t.Errorf("argv/stdin not forwarded\n%s", text)
	}
	exitCode = "17"
	if _, err := run(t, "post-commit", ""); err != nil {
		t.Errorf("capture hook did not fail open: %v", err)
	}
	if _, err := run(t, "pre-push", "", "origin"); err == nil {
		t.Error("pre-push swallowed Entire failure")
	}
	exitCode = ""

	if err := os.Remove(filepath.Join(binDir, "entire")); err != nil {
		t.Fatal(err)
	}
	for _, hook := range gitHookNames {
		stderr, err := run(t, hook, "", "argument")
		if err != nil {
			t.Errorf("%s missing binary did not fail open: %v", hook, err)
		}
		if !strings.Contains(stderr, wantLefthookMissingEntireWarning(hook)) {
			t.Errorf("%s missing binary warning: stderr=%q", hook, stderr)
		}
	}
}

func wantLefthookMissingEntireWarning(hook string) string {
	return "[entire] Entire CLI is unavailable; skipping " + hook + " hook."
}

func TestSelectLefthookIntegrationManager(t *testing.T) {
	t.Parallel()
	lefthook := func(path string) hookManager {
		return hookManager{Name: lefthookManagerName, ConfigPath: path, OverwritesHooks: true, IntegrationKind: hookManagerIntegrationLefthook}
	}
	tests := []struct {
		name     string
		managers []hookManager
		wantOK   bool
		wantErr  bool
	}{
		{name: "exactly one main", managers: []hookManager{lefthook("lefthook.yml")}, wantOK: true},
		{name: "multiple main variants", managers: []hookManager{lefthook("lefthook.yml"), lefthook(".lefthook.yaml")}, wantErr: true},
		{name: "local only", managers: []hookManager{lefthook("lefthook-local.yml")}, wantErr: true},
		{name: "alternate local", managers: []hookManager{lefthook("lefthook.yml"), lefthook(".lefthook-local.yml")}, wantErr: true},
		{name: "with non overwriting manager", managers: []hookManager{lefthook("lefthook.yml"), {Name: "pre-commit"}}, wantOK: true},
		{name: "with Husky", managers: []hookManager{lefthook("lefthook.yml"), {Name: "Husky", OverwritesHooks: true}}, wantErr: true},
		{name: "with another overwriting manager", managers: []hookManager{lefthook("lefthook.yml"), {Name: "custom", OverwritesHooks: true}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, ok, err := selectLefthookIntegrationManager(tt.managers)
			if ok != tt.wantOK || (err != nil) != tt.wantErr {
				t.Errorf("select = ok %v err %v, want ok %v err %v", ok, err, tt.wantOK, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, ErrLefthookAmbiguous) {
				t.Errorf("error = %v, want ErrLefthookAmbiguous", err)
			}
		})
	}
}

func TestLefthookExcludeOwnershipPreservesPreexistingExactRules(t *testing.T) {
	t.Parallel()
	preexisting := []byte("# user rules\n/lefthook-local.yml\n/.lefthook-local/pre-push/entire.sh\n")
	installed := mergeLefthookInfoExclude(preexisting)
	if bytes.Count(installed, []byte("/lefthook-local.yml\n")) != 2 {
		t.Fatalf("installed exclude should retain user rule and add owned rule block:\n%s", installed)
	}
	removed := removeLefthookExcludeLines(installed)
	if string(removed) != string(preexisting) {
		t.Errorf("uninstall exclude = %q, want original %q", removed, preexisting)
	}
}
