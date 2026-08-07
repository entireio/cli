package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testMarker = "# Entire CLI test block"

func TestAppendMarkedBlock_CreatesFileAndDirectory(t *testing.T) {
	t.Parallel()

	rcFile := filepath.Join(t.TempDir(), "nested", "config.fish")
	if err := appendMarkedBlock(rcFile, testMarker, "load me"); err != nil {
		t.Fatalf("appendMarkedBlock() error = %v", err)
	}

	got := readFileString(t, rcFile)
	if want := "\n" + testMarker + "\nload me\n"; got != want {
		t.Errorf("rc file = %q, want %q", got, want)
	}
}

func TestRemoveMarkedBlock_LeavesUnrelatedContentIntact(t *testing.T) {
	t.Parallel()

	rcFile := filepath.Join(t.TempDir(), ".zshrc")
	original := "export PATH=/opt/bin:$PATH\n\n# my own comment\nalias ll='ls -l'\n"
	writeFileString(t, rcFile, original)

	if err := appendMarkedBlock(rcFile, testMarker, "load me"); err != nil {
		t.Fatalf("appendMarkedBlock() error = %v", err)
	}
	removed, err := removeMarkedBlock(rcFile, testMarker)
	if err != nil {
		t.Fatalf("removeMarkedBlock() error = %v", err)
	}
	if !removed {
		t.Fatal("removeMarkedBlock() = false, want true")
	}

	if got := readFileString(t, rcFile); got != original {
		t.Errorf("rc file after round trip = %q, want %q", got, original)
	}
}

func TestRemoveMarkedBlock_MissingFileAndMissingBlock(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	missing := filepath.Join(dir, "does-not-exist")
	removed, err := removeMarkedBlock(missing, testMarker)
	if err != nil || removed {
		t.Errorf("removeMarkedBlock(missing) = (%v, %v), want (false, nil)", removed, err)
	}

	rcFile := filepath.Join(dir, ".bashrc")
	writeFileString(t, rcFile, "alias g=git\n")
	removed, err = removeMarkedBlock(rcFile, testMarker)
	if err != nil || removed {
		t.Errorf("removeMarkedBlock(no block) = (%v, %v), want (false, nil)", removed, err)
	}
	if got := readFileString(t, rcFile); got != "alias g=git\n" {
		t.Errorf("rc file was rewritten to %q", got)
	}
}

func TestRemoveMarkedBlock_RemovesEveryOccurrence(t *testing.T) {
	t.Parallel()

	rcFile := filepath.Join(t.TempDir(), ".bashrc")
	writeFileString(t, rcFile, "first\n")
	for range 3 {
		if err := appendMarkedBlock(rcFile, testMarker, "load me"); err != nil {
			t.Fatalf("appendMarkedBlock() error = %v", err)
		}
	}

	if _, err := removeMarkedBlock(rcFile, testMarker); err != nil {
		t.Fatalf("removeMarkedBlock() error = %v", err)
	}
	got := readFileString(t, rcFile)
	if strings.Contains(got, testMarker) || strings.Contains(got, "load me") {
		t.Errorf("rc file still contains a block: %q", got)
	}
	if got != "first\n" {
		t.Errorf("rc file = %q, want %q", got, "first\n")
	}
}

func TestIsMarkerConfigured(t *testing.T) {
	t.Parallel()

	rcFile := filepath.Join(t.TempDir(), ".zshrc")
	if isMarkerConfigured(rcFile, testMarker) {
		t.Error("isMarkerConfigured on a missing file = true, want false")
	}
	if err := appendMarkedBlock(rcFile, testMarker, "load me"); err != nil {
		t.Fatalf("appendMarkedBlock() error = %v", err)
	}
	if !isMarkerConfigured(rcFile, testMarker) {
		t.Error("isMarkerConfigured after append = false, want true")
	}
}

func TestShellRCTarget_ExplicitShellOverridesEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/bash")

	kind, name, rcFile, err := shellRCTarget("fish")
	if err != nil {
		t.Fatalf("shellRCTarget(fish) error = %v", err)
	}
	if kind != shellFish || name != "Fish" {
		t.Errorf("kind/name = %q/%q, want fish/Fish", kind, name)
	}
	if want := filepath.Join(home, ".config", "fish", "config.fish"); rcFile != want {
		t.Errorf("rcFile = %q, want %q", rcFile, want)
	}
}

func TestShellRCTarget_BashPrefersBashProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/bash")

	_, _, rcFile, err := shellRCTarget("")
	if err != nil {
		t.Fatalf("shellRCTarget() error = %v", err)
	}
	if want := filepath.Join(home, ".bashrc"); rcFile != want {
		t.Errorf("rcFile without .bash_profile = %q, want %q", rcFile, want)
	}

	writeFileString(t, filepath.Join(home, ".bash_profile"), "")
	_, _, rcFile, err = shellRCTarget("")
	if err != nil {
		t.Fatalf("shellRCTarget() error = %v", err)
	}
	if want := filepath.Join(home, ".bash_profile"); rcFile != want {
		t.Errorf("rcFile with .bash_profile = %q, want %q", rcFile, want)
	}
}

func TestShellRCTarget_UnsupportedShell(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", "/usr/bin/nu")

	if _, _, _, err := shellRCTarget(""); !errors.Is(err, errUnsupportedShell) {
		t.Errorf("shellRCTarget() error = %v, want errUnsupportedShell", err)
	}
}

// TestShellCompletionTarget_UnchangedByRefactor pins the completion behavior
// that appendMarkedBlock/shellRCTarget were extracted out from: same rc file,
// same completion line, same marker comment, for every supported shell.
func TestShellCompletionTarget_UnchangedByRefactor(t *testing.T) {
	home := t.TempDir()

	for _, tc := range []struct {
		shellEnv   string
		wantName   string
		wantRCFile string
		wantLine   string
	}{
		{"/bin/zsh", "Zsh", ".zshrc", "autoload -Uz compinit && compinit && source <(entire completion zsh)"},
		{"/bin/bash", "Bash", ".bashrc", "source <(entire completion bash)"},
		{"/usr/bin/fish", "Fish", ".config/fish/config.fish", "entire completion fish | source"},
	} {
		t.Setenv("HOME", home)
		t.Setenv("SHELL", tc.shellEnv)

		name, rcFile, line, err := shellCompletionTarget()
		if err != nil {
			t.Fatalf("%s: shellCompletionTarget() error = %v", tc.shellEnv, err)
		}
		if name != tc.wantName {
			t.Errorf("%s: shellName = %q, want %q", tc.shellEnv, name, tc.wantName)
		}
		if want := filepath.Join(home, filepath.FromSlash(tc.wantRCFile)); rcFile != want {
			t.Errorf("%s: rcFile = %q, want %q", tc.shellEnv, rcFile, want)
		}
		if line != tc.wantLine {
			t.Errorf("%s: completionLine = %q, want %q", tc.shellEnv, line, tc.wantLine)
		}
	}
}

func TestAppendShellCompletion_WritesTheSameBlockAsBefore(t *testing.T) {
	t.Parallel()

	rcFile := filepath.Join(t.TempDir(), ".zshrc")
	writeFileString(t, rcFile, "export EDITOR=vim\n")

	const line = "source <(entire completion bash)"
	if err := appendShellCompletion(rcFile, line); err != nil {
		t.Fatalf("appendShellCompletion() error = %v", err)
	}

	want := "export EDITOR=vim\n\n" + shellCompletionComment + "\n" + line + "\n"
	if got := readFileString(t, rcFile); got != want {
		t.Errorf("rc file = %q, want %q", got, want)
	}
	if !isCompletionConfigured(rcFile) {
		t.Error("isCompletionConfigured() = false after appending completion")
	}
}

func TestIsCompletionConfigured_MatchesHandWrittenLine(t *testing.T) {
	t.Parallel()

	// The marker comment is Entire's; a user who added the completion by hand
	// must still count as configured, exactly as before the refactor.
	rcFile := filepath.Join(t.TempDir(), ".zshrc")
	writeFileString(t, rcFile, "source <(entire completion zsh)\n")

	if !isCompletionConfigured(rcFile) {
		t.Error("isCompletionConfigured() = false for a hand-written completion line")
	}
	if isMarkerConfigured(rcFile, shellCompletionComment) {
		t.Error("isMarkerConfigured() = true without the marker comment")
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	return string(data)
}

func writeFileString(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}
