package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorPrintsEntireEnvironment(t *testing.T) {
	var out bytes.Buffer
	err := Run(t.Context(), Options{
		Env: EntireEnv{
			CLIVersion:    "0.6.3",
			RepoRoot:      t.TempDir(),
			PluginDataDir: t.TempDir(),
		},
		Stdout: &out,
		Stderr: &out,
	}, []string{"doctor"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ENTIRE_CLI_VERSION=0.6.3", "ENTIRE_REPO_ROOT=", "ENTIRE_PLUGIN_DATA_DIR=", "repo_root="} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("doctor output missing %q:\n%s", want, out.String())
		}
	}
}

func TestAnalyzeJSONCommand(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Entire Sem Test")
	git(t, repo, "config", "user.email", "sem@example.com")
	write(t, repo, "auth.py", "def validate_token(token):\n    return bool(token)\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "initial")
	write(t, repo, "auth.py", "def validate_token(token, issuer=None):\n    return bool(token)\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "update")

	var out bytes.Buffer
	err := Run(t.Context(), Options{
		Env:    EntireEnv{RepoRoot: repo},
		Stdout: &out,
		Stderr: &out,
	}, []string{"analyze", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"dependents_count"`) {
		t.Fatalf("analyze json missing dependents_count:\n%s", out.String())
	}
}

func write(t *testing.T, repo, path, content string) {
	t.Helper()
	full := filepath.Join(repo, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
