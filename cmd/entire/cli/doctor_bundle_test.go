package cli

import (
	"archive/zip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
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

// The bundle must record entire's git refs.
func TestWriteDoctorBundle_CapturesEntireRefs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")

	runDoctorBundleGit(t, dir, "update-ref", "refs/heads/entire/checkpoints/v1", "HEAD")

	out := filepath.Join(dir, "bundle.zip")
	if err := writeDoctorBundle(context.Background(), dir, out, false); err != nil {
		t.Fatalf("writeDoctorBundle: %v", err)
	}

	content := readZipEntry(t, out, "entire-refs.txt")
	if !strings.Contains(content, "refs/heads/entire/checkpoints/v1") {
		t.Errorf("entire-refs.txt missing v1 branch ref, got:\n%s", content)
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

	testutil.RunGit(t, dir, args...)
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

// Not parallel: WithScannerDegradedSole mutates process-global scanner state.
// The bundle leaves the machine, so a JSON entry that redaction cannot certify
// (scanner degraded, or a line it could not rewrite) must be withheld rather
// than downgraded to the byte-level scrubber.
func TestRedactBundleEntry_WithholdsJSONWhenScannerDegraded(t *testing.T) {
	redact.WithScannerDegradedSole(t)

	got := string(redactBundleEntry("entry.jsonl", []byte(`{"msg":"hello"}`)))
	if !strings.Contains(got, "entry withheld") {
		t.Fatalf("degraded-scanner JSON entry must be withheld, got %q", got)
	}
	if strings.Contains(got, "hello") {
		t.Fatalf("withheld entry must not carry the original content, got %q", got)
	}
}

// Runs through the doctor parent so pre-run inheritance is exercised. Only the
// redacted bundle consults redaction settings: an invalid scanner config must
// fail it (the archive leaves the machine) but must not block --raw or the
// read-only migrate report, which are how a user diagnoses that same setup.
func TestDoctorSubcommands_InvalidScannerConfigBlocksOnlyRedactedBundle(t *testing.T) {
	// Cannot use t.Parallel(): t.Chdir and the process-wide redaction Once.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	// Both scanners off — settings.Load fails with ErrScannerConfig.
	testutil.WriteFile(t, dir, ".entire/settings.json",
		`{"enabled": true, "redaction": {"betterleaks": {"enabled": false}, "goredact": {"enabled": false}}}`)
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()

	run := func(args ...string) error {
		strategy.ResetRedactionConfiguredForTest()
		t.Cleanup(strategy.ResetRedactionConfiguredForTest)
		cmd := newDoctorCmd()
		var out strings.Builder
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		cmd.SetContext(context.Background())
		return cmd.Execute()
	}

	outZip := filepath.Join(t.TempDir(), "bundle.zip")
	if err := run("bundle", "--raw", "--out", outZip); err != nil {
		t.Errorf("doctor bundle --raw must not consult redaction settings: %v", err)
	}
	if err := run("migrate-checkpoints", "--dry-run"); err != nil {
		t.Errorf("doctor migrate-checkpoints --dry-run must not consult redaction settings: %v", err)
	}
	if err := run("bundle", "--out", outZip); !errors.Is(err, settings.ErrScannerConfig) {
		t.Errorf("redacted doctor bundle must fail closed on invalid scanner config, got: %v", err)
	}
}

// Settings that fail to load leave hooks on the built-in layers, but a
// redacted bundle must refuse rather than ship without the user's own rules.
func TestDoctorBundle_RefusesWhenCustomRulesFailToLoad(t *testing.T) {
	// Cannot use t.Parallel(): t.Chdir and the process-wide redaction Once.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	testutil.WriteFile(t, dir, ".entire/settings.json", `{"enabled": true, "redaction": {`)
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()

	run := func(args ...string) error {
		strategy.ResetRedactionConfiguredForTest()
		t.Cleanup(strategy.ResetRedactionConfiguredForTest)
		cmd := newDoctorCmd()
		var out strings.Builder
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		cmd.SetContext(context.Background())
		return cmd.Execute()
	}

	outZip := filepath.Join(t.TempDir(), "bundle.zip")
	err := run("bundle", "--out", outZip)
	if err == nil || !strings.Contains(err.Error(), "refusing to write a bundle without your custom redaction rules") {
		t.Fatalf("redacted bundle must refuse when settings fail to load, got: %v", err)
	}
	if _, statErr := os.Stat(outZip); !os.IsNotExist(statErr) {
		t.Errorf("refused bundle must not leave an archive behind (stat err: %v)", statErr)
	}
	if err := run("bundle", "--raw", "--out", outZip); err != nil {
		t.Errorf("doctor bundle --raw must still work: %v", err)
	}
}

// runDoctorInRepo runs a doctor subcommand through the doctor parent in a
// fresh repo whose .entire holds files, resetting the process-wide redaction
// Once around it.
func runDoctorInRepo(t *testing.T, files map[string]string, args ...string) error {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	for name, content := range files {
		testutil.WriteFile(t, dir, name, content)
	}
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	strategy.ResetRedactionConfiguredForTest()
	t.Cleanup(strategy.ResetRedactionConfiguredForTest)

	cmd := newDoctorCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	cmd.SetContext(context.Background())
	return cmd.Execute()
}

// A rule the user wrote that silently does not apply is the original leak, so
// a skipped pack file or an inline regex that fails to compile must stop a
// redacted bundle too. Settings that fail to load without declaring redaction
// (e.g. an unknown key from a newer CLI) hold no such rules: the bundle falls
// back to the built-ins rather than pushing the user to --raw.
func TestDoctorBundle_RefusesOnlyWhenUserRulesMayBeMissing(t *testing.T) {
	// Cannot use t.Parallel(): t.Chdir and the process-wide redaction Once.
	for _, tc := range []struct {
		name    string
		files   map[string]string
		refuses bool
	}{
		{
			name:    "invalid pack file",
			files:   map[string]string{".entire/redactors/acme.yaml": "name: acme\nrules:\n - id: x\n  regex: 'ACME_[A-Z]{6}'\n"},
			refuses: true,
		},
		{
			name:    "inline rule that fails to compile",
			files:   map[string]string{".entire/settings.json": `{"enabled": true, "redaction": {"custom_redactions": {"broken": "INLINE_[A-Z0-9"}}}`},
			refuses: true,
		},
		{
			name:    "unknown settings key without redaction",
			files:   map[string]string{".entire/settings.json": `{"enabled": true, "future_feature": true}`},
			refuses: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outZip := filepath.Join(t.TempDir(), "bundle.zip")
			err := runDoctorInRepo(t, tc.files, "bundle", "--out", outZip)
			refused := err != nil && strings.Contains(err.Error(), "refusing to write a bundle without your custom redaction rules")
			if refused != tc.refuses {
				t.Fatalf("refused = %v, want %v (err: %v)", refused, tc.refuses, err)
			}
			if !tc.refuses && err != nil {
				t.Fatalf("bundle should fall back to built-in redaction: %v", err)
			}
		})
	}
}
