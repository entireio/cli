package cli

import (
	"archive/zip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestWriteDoctorBundle_ContainsExpectedEntries(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)

	// Write a fixture log file under .entire/logs/.
	logsDir := filepath.Join(dir, logging.LogsDir)
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(logsDir, "entire.log"), []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	// Write a project settings file.
	entireDir := filepath.Join(dir, ".entire")
	if err := os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(`{"enabled":true}`), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	out := filepath.Join(dir, "bundle.zip")
	if err := writeDoctorBundle(context.Background(), dir, out, false); err != nil {
		t.Fatalf("writeDoctorBundle: %v", err)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat bundle: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("bundle permissions = %v, want 0600", got)
	}

	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zr.Close()

	got := make(map[string]bool, len(zr.File))
	for _, f := range zr.File {
		if strings.Contains(f.Name, `\`) {
			t.Errorf("zip entry %q contains backslash path separator", f.Name)
		}
		got[f.Name] = true
	}

	required := []string{
		"logs/entire.log",
		"settings/settings.json",
		"git-status.txt",
		"git-log.txt",
		"git-remote.txt",
		"version.txt",
	}
	for _, name := range required {
		if !got[name] {
			t.Errorf("missing entry %q in bundle. Have: %v", name, mapKeys(got))
		}
	}
}

func TestWriteDoctorBundle_RedactsCredentialedRemote(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	runDoctorBundleGit(t, dir, "remote", "add", "origin", "https://user:s3cr3tTOKEN12345@example.com/owner/repo.git")

	out := filepath.Join(dir, "bundle.zip")
	if err := writeDoctorBundle(context.Background(), dir, out, false); err != nil {
		t.Fatalf("writeDoctorBundle: %v", err)
	}

	content := readZipEntry(t, out, "git-remote.txt")
	if strings.Contains(content, "s3cr3tTOKEN12345") {
		t.Fatalf("git-remote.txt leaked credential: %q", content)
	}
}

func TestWriteDoctorBundle_OmitsAbsentLogsDirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)

	out := filepath.Join(dir, "bundle.zip")
	if err := writeDoctorBundle(context.Background(), dir, out, false); err != nil {
		t.Fatalf("writeDoctorBundle: %v", err)
	}

	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zr.Close()

	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, "logs/") {
			t.Errorf("expected no logs/ entries when dir absent, found %q", f.Name)
		}
	}
}

func mapKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func readZipEntry(t *testing.T, zipPath, name string) string {
	t.Helper()

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zr.Close()

	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open zip entry %s: %v", name, err)
		}
		defer rc.Close()
		data, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("read zip entry %s: %v", name, err)
		}
		return string(data)
	}

	t.Fatalf("zip entry %q not found", name)
	return ""
}

func runDoctorBundleGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", args...) //nolint:noctx // test helper, no context needed
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestWriteDoctorBundle_RedactsLogContents(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)

	logsDir := filepath.Join(dir, logging.LogsDir)
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	const apiKey = "sk-ant-api03-xK9mZ2vL8nQ5rT1wY4bC7dF0gH3jE6pA"
	logBody := "request issued\nAuthorization: Bearer " + apiKey + "\nDB_PASSWORD=hunter2supersecret\n"
	if err := os.WriteFile(filepath.Join(logsDir, "entire.log"), []byte(logBody), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	out := filepath.Join(dir, "bundle.zip")
	if err := writeDoctorBundle(context.Background(), dir, out, false); err != nil {
		t.Fatalf("writeDoctorBundle: %v", err)
	}

	got := readZipEntry(t, out, "logs/entire.log")
	if strings.Contains(got, apiKey) {
		t.Fatalf("redacted bundle leaked API key: %q", got)
	}
	if strings.Contains(got, "hunter2supersecret") {
		t.Fatalf("redacted bundle leaked DB_PASSWORD value: %q", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Fatalf("redacted bundle missing REDACTED marker: %q", got)
	}
}

func TestWriteDoctorBundle_RedactsSettings(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)

	entireDir := filepath.Join(dir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("mkdir .entire: %v", err)
	}
	const credURI = "postgres://app:s3cretP4ssw0rd@db.example.com:5432/app"
	settingsLocal := `{"strategy_options":{"checkpoint_remote":{"url":"` + credURI + `"}}}`
	if err := os.WriteFile(filepath.Join(entireDir, "settings.local.json"), []byte(settingsLocal), 0o600); err != nil {
		t.Fatalf("write settings.local.json: %v", err)
	}

	out := filepath.Join(dir, "bundle.zip")
	if err := writeDoctorBundle(context.Background(), dir, out, false); err != nil {
		t.Fatalf("writeDoctorBundle: %v", err)
	}

	got := readZipEntry(t, out, "settings/settings.local.json")
	if strings.Contains(got, "s3cretP4ssw0rd") {
		t.Fatalf("redacted bundle leaked DB password: %q", got)
	}
	if !strings.Contains(got, "checkpoint_remote") {
		t.Fatalf("redaction stripped structural keys: %q", got)
	}
}

func TestWriteDoctorBundle_RawSkipsRedaction(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)

	logsDir := filepath.Join(dir, logging.LogsDir)
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	const apiKey = "sk-ant-api03-xK9mZ2vL8nQ5rT1wY4bC7dF0gH3jE6pA"
	if err := os.WriteFile(filepath.Join(logsDir, "entire.log"), []byte("Authorization: Bearer "+apiKey+"\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	out := filepath.Join(dir, "bundle.zip")
	if err := writeDoctorBundle(context.Background(), dir, out, true); err != nil {
		t.Fatalf("writeDoctorBundle raw: %v", err)
	}

	got := readZipEntry(t, out, "logs/entire.log")
	if !strings.Contains(got, apiKey) {
		t.Fatalf("--raw bundle should preserve raw secret, got: %q", got)
	}
}

func TestDoctorBundleCmd_HelpAdvertisesRedaction(t *testing.T) {
	t.Parallel()

	cmd := newDoctorBundleCmd()
	help := cmd.Short + "\n" + cmd.Long
	for _, want := range []string{"redacted by default", "--raw"} {
		if !strings.Contains(help, want) {
			t.Errorf("doctor bundle help text missing %q. Help:\n%s", want, help)
		}
	}
}

func TestDoctorBundleCmd_StderrBannerNamesMode(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir for paths.WorktreeRoot.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	for _, tc := range []struct {
		name      string
		args      []string
		wantInErr string
	}{
		{name: "default redacted", args: []string{}, wantInErr: "redacted"},
		{name: "raw mode", args: []string{"--raw"}, wantInErr: "RAW"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outZip := filepath.Join(t.TempDir(), "bundle.zip")
			cmd := newDoctorBundleCmd()
			var stdout, stderr strings.Builder
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs(append([]string{"--out", outZip}, tc.args...))

			if err := cmd.Execute(); err != nil {
				t.Fatalf("execute: %v\nstderr: %s", err, stderr.String())
			}

			if !strings.Contains(stderr.String(), tc.wantInErr) {
				t.Errorf("stderr missing %q. Got: %s", tc.wantInErr, stderr.String())
			}
			if !strings.Contains(strings.TrimSpace(stdout.String()), outZip) {
				t.Errorf("stdout should contain bundle path %q. Got: %s", outZip, stdout.String())
			}
		})
	}
}
