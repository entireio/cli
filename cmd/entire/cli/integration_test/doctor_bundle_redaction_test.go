//go:build integration

package integration

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDoctorBundle_AppliesCustomRedactionRules is the regression test for the
// support report where a diagnostic bundle leaked a project identifier that a
// user-defined redaction rule was written to catch: EnsureRedactionConfigured
// hung on the doctor command's PreRun, which cobra does not run for
// subcommands, so `doctor bundle` redacted with the built-in layers only and
// silently ignored .entire/redactors/ packs and redaction.custom_redactions.
// It must run the real binary so the cobra pre-run wiring is exercised —
// calling writeDoctorBundle directly (like the unit tests) bypasses the bug.
func TestDoctorBundle_AppliesCustomRedactionRules(t *testing.T) {
	t.Parallel()

	const (
		packTarget   = "ACME_ABC123"   // matched by the pack rule below
		inlineTarget = "INLINE_ZZ99XX" // matched by the inline custom_redactions rule
	)

	env := NewFeatureBranchEnv(t)

	env.PatchSettings(map[string]any{
		"redaction": map[string]any{
			"custom_redactions": map[string]any{
				"inline-acme": "INLINE_[A-Z0-9]{6}",
			},
		},
	})
	redactorsDir := filepath.Join(env.RepoDir, ".entire", "redactors")
	if err := os.MkdirAll(redactorsDir, 0o755); err != nil {
		t.Fatalf("mkdir redactors: %v", err)
	}
	packYAML := `name: acme
version: 1.0.0
rules:
  - id: acme-token
    regex: 'ACME_[A-Z0-9]{6}'
`
	if err := os.WriteFile(filepath.Join(redactorsDir, "acme.yaml"), []byte(packYAML), 0o644); err != nil {
		t.Fatalf("write pack: %v", err)
	}

	// Vector 1: identifier in a collected log file.
	logsDir := filepath.Join(env.RepoDir, ".entire", "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	logBody := "deploying " + packTarget + " with tag " + inlineTarget + "\n"
	if err := os.WriteFile(filepath.Join(logsDir, "entire.log"), []byte(logBody), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	// Vector 2: identifier in a git commit subject, the reported leak — the
	// built-in secret layers leave it (it is not a secret), so only the
	// custom rule can catch it in git-log.txt.
	env.WriteFile("feature.txt", "content\n")
	env.GitAdd("feature.txt")
	env.GitCommit("rollout " + packTarget)

	outZip := filepath.Join(t.TempDir(), "bundle.zip")
	out := env.RunCLI("doctor", "bundle", "--out", outZip)
	t.Logf("doctor bundle output: %s", out)

	for entry, target := range map[string]string{
		"logs/entire.log": packTarget,
		"git-log.txt":     packTarget,
	} {
		content := readBundleEntry(t, outZip, entry)
		if strings.Contains(content, target) {
			t.Errorf("bundle entry %s leaked custom-rule target %q:\n%s", entry, target, content)
		}
	}
	if content := readBundleEntry(t, outZip, "logs/entire.log"); strings.Contains(content, inlineTarget) {
		t.Errorf("bundle entry logs/entire.log leaked inline custom_redactions target %q:\n%s", inlineTarget, content)
	}
}

func readBundleEntry(t *testing.T, zipPath, name string) string {
	t.Helper()

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("open bundle zip: %v", err)
	}
	defer zr.Close()

	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open bundle entry %s: %v", name, err)
		}
		defer rc.Close()
		data, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("read bundle entry %s: %v", name, err)
		}
		return string(data)
	}
	t.Fatalf("bundle entry %q not found", name)
	return ""
}
