package redact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePack_Valid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		sourcePath  string
		body        string
		wantRules   int
		wantSamples int
	}{
		{
			name:       "yaml",
			sourcePath: "acme-internal.yaml",
			body: `
name: acme-internal
version: 1.0.0
description: Internal ACME tokens
rules:
  - id: acme-token
    regex: 'ACME_TOKEN_[A-Za-z0-9]{20,}'
    samples:
      - { input: "ACME_TOKEN_abc123def456ghi789jkl", redacted: true }
      - { input: "ACME_TOKEN_short", redacted: false }
  - id: acme-session
    regex: 'asess_[a-f0-9]{32}'
`,
			wantRules:   2,
			wantSamples: 2,
		},
		{
			name:       "json",
			sourcePath: "acme-internal.json",
			body: `{
  "name": "acme-internal",
  "version": "1.0.0",
  "rules": [
    {
      "id": "acme-token",
      "regex": "ACME_TOKEN_[A-Za-z0-9]{20,}"
    }
  ]
}`,
			wantRules: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pack, err := ParsePack([]byte(tc.body), tc.sourcePath)
			if err != nil {
				t.Fatalf("ParsePack: %v", err)
			}
			if pack.Name != "acme-internal" {
				t.Errorf("Name: want acme-internal, have %q", pack.Name)
			}
			if pack.Version != "1.0.0" {
				t.Errorf("Version: want 1.0.0, have %q", pack.Version)
			}
			if len(pack.Rules) != tc.wantRules {
				t.Fatalf("Rules: want %d, have %d", tc.wantRules, len(pack.Rules))
			}
			if pack.Rules[0].ID != "acme-token" {
				t.Errorf("Rules[0].ID: want acme-token, have %q", pack.Rules[0].ID)
			}
			if len(pack.Rules[0].Samples) != tc.wantSamples {
				t.Errorf("Rules[0].Samples: want %d, have %d", tc.wantSamples, len(pack.Rules[0].Samples))
			}
		})
	}
}

func TestParsePack_RejectsInvalidPacks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		sourcePath string
		body       string
		want       []string
	}{
		{
			name:       "missing name",
			sourcePath: "noname.yaml",
			body: `
version: 1.0.0
rules:
  - id: x
    regex: 'X+'
`,
			want: []string{"name"},
		},
		{
			name:       "empty rules",
			sourcePath: "empty.yaml",
			body: `
name: empty
version: 1.0.0
rules: []
`,
			want: []string{"rules"},
		},
		{
			name:       "duplicate rule ids",
			sourcePath: "dupe.yaml",
			body: `
name: dupe
version: 1.0.0
rules:
  - id: same
    regex: 'A+'
  - id: same
    regex: 'B+'
`,
			want: []string{"duplicate"},
		},
		{
			name:       "name must match filename stem",
			sourcePath: "actual-filename.yaml",
			body: `
name: not-the-filename
version: 1.0.0
rules:
  - id: x
    regex: 'X+'
`,
			want: []string{"name", "filename"},
		},
		{
			name:       "unknown yaml field",
			sourcePath: "unknown-yaml.yaml",
			body: `
name: unknown-yaml
version: 1.0.0
rules:
  - id: x
    regex: 'X+'
    samplez: []
`,
			want: []string{"samplez"},
		},
		{
			name:       "unknown json field",
			sourcePath: "unknown-json.json",
			body: `{
  "name": "unknown-json",
  "version": "1.0.0",
  "rules": [
    {"id": "x", "regex": "X+", "samplez": []}
  ]
}`,
			want: []string{"samplez"},
		},
		{
			name:       "trailing json content",
			sourcePath: "trailing-json.json",
			body: `{
  "name": "trailing-json",
  "version": "1.0.0",
  "rules": [{"id": "x", "regex": "X+"}]
}
{
  "name": "second"
}`,
			want: []string{"trailing"},
		},
		{
			name:       "multiple yaml documents",
			sourcePath: "multiple-yaml.yaml",
			body: `
name: multiple-yaml
version: 1.0.0
rules:
  - id: x
    regex: 'X+'
---
name: second
`,
			want: []string{"trailing"},
		},
		{
			name:       "rule id with disallowed characters",
			sourcePath: "bad-id.yaml",
			body: `
name: bad-id
version: 1.0.0
rules:
  - id: 'has space'
    regex: 'X+'
`,
			want: []string{"rules[0].id", "characters"},
		},
		{
			name:       "rule id exceeds length cap",
			sourcePath: "long-id.yaml",
			body: `
name: long-id
version: 1.0.0
rules:
  - id: ` + strings.Repeat("a", maxIdentifierLen+1) + `
    regex: 'X+'
`,
			want: []string{"rules[0].id", "limit"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := ParsePack([]byte(tc.body), tc.sourcePath)
			if err == nil {
				t.Fatal("expected error")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error should mention %q, got: %v", want, err)
				}
			}
		})
	}
}

func TestLoadPacks_ReadsMultipleFilesAndIgnoresOthers(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	mustWrite(t, dir, "alpha.yaml", `
name: alpha
version: 1.0.0
rules:
  - id: a
    regex: 'A+'
`)
	mustWrite(t, dir, "beta.json", `{
  "name": "beta",
  "version": "1.0.0",
  "rules": [{"id": "b", "regex": "B+"}]
}`)
	mustWrite(t, dir, "ignored.txt", `not a pack`)
	mustWrite(t, dir, "broken.yaml", `name: [this is malformed`)

	packs, err := LoadPacks(dir)
	if err != nil {
		t.Fatalf("LoadPacks: %v", err)
	}
	if len(packs) != 2 {
		t.Fatalf("packs: want 2 (alpha, beta), have %d", len(packs))
	}

	got := map[string]bool{}
	for _, p := range packs {
		got[p.Name] = true
	}
	if !got["alpha"] || !got["beta"] {
		t.Errorf("expected packs alpha+beta, got %v", got)
	}
}

func TestLoadPacks_MissingDirReturnsEmpty(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "does-not-exist")
	packs, err := LoadPacks(dir)
	if err != nil {
		t.Fatalf("LoadPacks should be tolerant of missing dirs, got: %v", err)
	}
	if len(packs) != 0 {
		t.Errorf("packs: want 0, have %d", len(packs))
	}
}

func TestLoadPacks_RejectsNonDirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "redactors")
	if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadPacks(path)
	if err == nil {
		t.Fatal("expected error for non-directory redactors path")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error should mention non-directory path, got: %v", err)
	}
}

func TestLoadPacks_SkipsSymlinkedPackFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.yaml")
	if err := os.WriteFile(outside, []byte(`
name: symlinked
version: 1.0.0
rules:
  - id: x
    regex: 'X+'
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "symlinked.yaml")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	packs, err := LoadPacks(dir)
	if err != nil {
		t.Fatalf("LoadPacks: %v", err)
	}
	if len(packs) != 0 {
		t.Fatalf("symlinked pack should be skipped, loaded %d pack(s)", len(packs))
	}
}

// TestLoadPacks_DescendsIntoSubdirs verifies that packs in subdirectories
// (e.g. the conventional .entire/redactors/local/ for personal/uncommitted
// rules) are discovered. Without recursion, the docs' "personal-only"
// distribution path would silently no-op.
func TestLoadPacks_DescendsIntoSubdirs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mustWrite(t, dir, "team.yaml", `
name: team
version: 1.0.0
rules:
  - id: t
    regex: 'T+'
`)
	if err := os.MkdirAll(filepath.Join(dir, "local"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "local"), "personal.yaml", `
name: personal
version: 1.0.0
rules:
  - id: p
    regex: 'P+'
`)

	packs, err := LoadPacks(dir)
	if err != nil {
		t.Fatalf("LoadPacks: %v", err)
	}
	got := map[string]bool{}
	for _, p := range packs {
		got[p.Name] = true
	}
	if !got["team"] || !got["personal"] {
		t.Errorf("expected team+personal packs, got %v", got)
	}
}

func TestLoadPacks_SkipsOversizedFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mustWrite(t, dir, "tiny.yaml", `
name: tiny
version: 1.0.0
rules:
  - id: t
    regex: 'T+'
`)
	// Build a YAML body large enough to exceed maxPackFileBytes.
	var b strings.Builder
	b.WriteString("name: huge\nversion: 1.0.0\ndescription: ")
	b.WriteString(strings.Repeat("x", maxPackFileBytes+1))
	b.WriteString("\nrules:\n  - id: h\n    regex: 'H+'\n")
	mustWrite(t, dir, "huge.yaml", b.String())

	packs, err := LoadPacks(dir)
	if err != nil {
		t.Fatalf("LoadPacks: %v", err)
	}
	if len(packs) != 1 || packs[0].Name != "tiny" {
		t.Fatalf("expected only tiny pack, got %#v", packs)
	}
}

func mustWrite(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
