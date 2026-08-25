package cloudenv

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestMentionsEntireInstall_CommandString(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cases := []struct {
		install string
		want    bool
	}{
		{"npm ci", false},
		{"curl -fsSL https://entire.io/install.sh | bash", true},
		{"bash .entire/install-cli.sh", true},
		{"npm ci && bash .entire/install-cli.sh", true},
		{"ln -sf \"$PWD/entire\" /usr/local/bin/entire", true},
		{"if ! command -v entire >/dev/null; then echo missing; fi", true},
	}
	for _, tc := range cases {
		if got := MentionsEntireInstall(tc.install, root); got != tc.want {
			t.Errorf("MentionsEntireInstall(%q) = %v, want %v", tc.install, got, tc.want)
		}
	}
}

func TestMentionsEntireInstall_ReferencedScript(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	scriptDir := filepath.Join(root, ".cursor")
	if err := os.MkdirAll(scriptDir, 0o750); err != nil {
		t.Fatal(err)
	}
	body := "#!/usr/bin/env bash\nsudo ln -sf \"${REPO_ROOT}/entire\" /usr/local/bin/entire\n"
	if err := os.WriteFile(filepath.Join(scriptDir, "install.sh"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if !MentionsEntireInstall("bash .cursor/install.sh", root) {
		t.Fatal("expected referenced .cursor/install.sh that links entire onto PATH to count as already installing Entire")
	}
	if MentionsEntireInstall("bash .cursor/missing.sh", root) {
		t.Fatal("missing script should not count as installing Entire")
	}
}

func TestAppendAndStripInstallStep(t *testing.T) {
	t.Parallel()
	step := InstallCLIStep
	if got := AppendInstallStep("", step); got != step {
		t.Errorf("empty install: got %q", got)
	}
	if got := AppendInstallStep("npm ci", step); got != "npm ci && "+step {
		t.Errorf("append: got %q", got)
	}
	if got := AppendInstallStep("npm ci && ", step); got != "npm ci && "+step {
		t.Errorf("trim junk: got %q", got)
	}
	if got := StripInstallStep("npm ci && "+step, step); got != "npm ci" {
		t.Errorf("strip suffix: got %q", got)
	}
	if got := StripInstallStep(step, step); got != "" {
		t.Errorf("strip sole step: got %q", got)
	}
	if got := StripInstallStep("npm ci", step); got != "npm ci" {
		t.Errorf("strip unrelated: got %q", got)
	}
}

func TestWriteInstallScript_Idempotent(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ctx := t.Context()
	if err := WriteInstallScript(ctx); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".entire", InstallCLIScriptName)
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(installCLIScript) {
		t.Fatalf("wrote unexpected script:\n%s", first)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows does not set execute bits, so the mode check is Unix-only.
	if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
		t.Fatalf("install-cli.sh is not executable: %v", info.Mode())
	}
	if err := WriteInstallScript(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("second write changed the script")
	}
}
