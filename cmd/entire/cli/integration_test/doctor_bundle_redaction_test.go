//go:build integration

package integration

import (
	"archive/zip"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// TestDoctorBundle_AppliesCustomRedactionRules is the regression test for the
// support report where a diagnostic bundle leaked a project identifier that a
// user-defined rule was written to catch: redaction setup lived on the doctor
// command's PreRun, which cobra never runs for subcommands, so `doctor bundle`
// silently skipped .entire/redactors/ packs and redaction.custom_redactions.
// It must run the real binary — calling writeDoctorBundle directly (like the
// unit tests) bypasses the cobra wiring where the bug lived.
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
	env.WriteFile(filepath.Join(".entire", "redactors", "acme.yaml"), `name: acme
version: 1.0.0
rules:
  - id: acme-token
    regex: 'ACME_[A-Z0-9]{6}'
`)

	// Vector 1: identifiers in a collected log file.
	env.WriteFile(filepath.Join(".entire", "logs", "entire.log"),
		"deploying "+packTarget+" with tag "+inlineTarget+"\n")

	// Vector 2: identifier in a git commit subject, the reported leak — the
	// built-in secret layers leave it (it is not a secret), so only the
	// custom rule can catch it in git-log.txt.
	env.WriteFile("feature.txt", "content\n")
	env.GitAdd("feature.txt")
	env.GitCommit("rollout " + packTarget)

	outZip := filepath.Join(t.TempDir(), "bundle.zip")
	env.RunCLI("doctor", "bundle", "--out", outZip)

	for _, tc := range []struct {
		entry  string
		target string
		rule   string
	}{
		{"logs/entire.log", packTarget, "pack rule"},
		{"logs/entire.log", inlineTarget, "inline custom_redactions rule"},
		{"git-log.txt", packTarget, "pack rule"},
	} {
		content := readBundleEntry(t, outZip, tc.entry)
		if strings.Contains(content, tc.target) {
			t.Errorf("bundle entry %s leaked %s target %q:\n%s", tc.entry, tc.rule, tc.target, content)
		}
		// Positive control: the surviving text proves the entry carried the
		// line and redaction replaced the target, so the absence checks above
		// cannot pass vacuously on an empty or unredacted-but-missing entry.
		if !strings.Contains(content, "REDACTED") {
			t.Errorf("bundle entry %s missing REDACTED placeholder — redaction did not run on it:\n%s", tc.entry, content)
		}
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
