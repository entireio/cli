package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPluginManifest_Roundtrip(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	in := &PluginManifest{
		Name:        "run",
		RepoURL:     "https://github.com/entireio/entire-run",
		Tag:         "v1.2.3",
		Asset:       "entire-run_1.2.3_darwin_arm64.tar.gz",
		SHA256:      "abc",
		Pinned:      true,
		InstalledAt: time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC),
		Requires:    []PluginRequirement{{Name: "sem", MinVersion: "v0.2.0"}},
	}
	if err := SavePluginManifest(in); err != nil {
		t.Fatalf("SavePluginManifest: %v", err)
	}
	out, err := LoadPluginManifest("run")
	if err != nil {
		t.Fatalf("LoadPluginManifest: %v", err)
	}
	if out == nil || out.Tag != in.Tag || out.RepoURL != in.RepoURL || !out.Pinned || len(out.Requires) != 1 || out.Requires[0].MinVersion != "v0.2.0" {
		t.Errorf("roundtrip mismatch: %+v", out)
	}
}

func TestLoadPluginManifest_AbsentIsNilNil(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	m, err := LoadPluginManifest("ghost")
	if err != nil || m != nil {
		t.Errorf("LoadPluginManifest(ghost) = %v, %v; want nil, nil", m, err)
	}
}

func TestListPluginManifests_SortedAndTolerant(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	for _, name := range []string{"zeta", "alpha"} {
		if err := SavePluginManifest(&PluginManifest{Name: name, RepoURL: "https://x.example/" + name, Tag: "v1.0.0"}); err != nil {
			t.Fatal(err)
		}
	}
	// A pkg dir without a manifest (half-removed plugin) must not break listing.
	if _, err := EnsurePluginPkgDir("broken"); err != nil {
		t.Fatal(err)
	}
	got, err := ListPluginManifests()
	if err != nil {
		t.Fatalf("ListPluginManifests: %v", err)
	}
	if len(got) != 2 || got[0].Name != "alpha" || got[1].Name != "zeta" {
		t.Errorf("ListPluginManifests = %+v, want [alpha zeta]", got)
	}
}

func TestRemovePluginPkg(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	if err := SavePluginManifest(&PluginManifest{Name: "run", RepoURL: "https://x.example/run", Tag: "v1.0.0"}); err != nil {
		t.Fatal(err)
	}
	if err := RemovePluginPkg("run"); err != nil {
		t.Fatalf("RemovePluginPkg: %v", err)
	}
	if m, err := LoadPluginManifest("run"); err != nil || m != nil {
		t.Error("manifest survived RemovePluginPkg")
	}
	// Removing a never-installed pkg is not an error.
	if err := RemovePluginPkg("ghost"); err != nil {
		t.Errorf("RemovePluginPkg(ghost) = %v, want nil", err)
	}
}

func TestParsePluginMetadata(t *testing.T) {
	t.Parallel()
	meta, err := ParsePluginMetadata([]byte(`
name: brain
description: Repository memory
download_url: "https://dl.example.com/{tag}/{asset}"
requires:
  - name: sem
    repo_url: https://github.com/entireio/entire-sem
    min_version: v0.2.0
`))
	if err != nil {
		t.Fatalf("ParsePluginMetadata: %v", err)
	}
	if meta.Name != "brain" || len(meta.Requires) != 1 || meta.Requires[0].Name != "sem" {
		t.Errorf("parsed %+v", meta)
	}

	// Unknown keys are ignored, not rejected. entire-plugin.yml is read by
	// every shipped CLI version and has no version field, so refusing a file
	// that carries a field this binary predates would break installs
	// permanently for everyone on that version. The known fields must still
	// decode alongside the unknown one.
	fwd, err := ParsePluginMetadata([]byte("name: x\nmin_cli_version: v9.9.9\ndescription: still parsed\n"))
	if err != nil {
		t.Errorf("ParsePluginMetadata rejected a forward-compatible file: %v", err)
	} else if fwd.Name != "x" || fwd.Description != "still parsed" {
		t.Errorf("known fields lost alongside an unknown one: %+v", fwd)
	}
	// Reserved names rejected.
	if _, err := ParsePluginMetadata([]byte("name: agent-evil\n")); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("ParsePluginMetadata(agent-evil) = %v, want reserved-name error", err)
	}
	if _, err := ParsePluginMetadata([]byte("name: ok\nrequires:\n  - name: agent-evil\n")); err == nil {
		t.Error("ParsePluginMetadata accepted reserved requirement name")
	}
	// A malformed min_version must fail rather than silently removing the floor:
	// x/mod/semver ranks an invalid string below every valid one, so the
	// comparison in dependencySatisfied would report any installed version as
	// acceptable. Ranges are the likeliest author mistake, since the field is
	// deliberately a minimum only.
	for _, bad := range []string{"vtypo", "latest", ">=1.0", "1.x"} {
		yml := "name: ok\nrequires:\n  - name: dep\n    min_version: \"" + bad + "\"\n"
		_, err := ParsePluginMetadata([]byte(yml))
		if err == nil {
			t.Errorf("ParsePluginMetadata accepted min_version %q", bad)
			continue
		}
		if !strings.Contains(err.Error(), "min_version") {
			t.Errorf("min_version %q: err = %v, want it to name the field", bad, err)
		}
	}
	// Both spellings of a valid tag are accepted, and an absent minimum is fine.
	for _, good := range []string{"v0.2.0", "0.2.0", "v1", ""} {
		yml := "name: ok\nrequires:\n  - name: dep\n    min_version: \"" + good + "\"\n"
		if _, err := ParsePluginMetadata([]byte(yml)); err != nil {
			t.Errorf("ParsePluginMetadata rejected min_version %q: %v", good, err)
		}
	}

	// requires[] carries only a name and an optional minimum. A repo_url left
	// over from the old schema is ignored rather than honored, so a published
	// file cannot steer a dependency install at a URL its author chose.
	legacy, err := ParsePluginMetadata([]byte("name: ok\nrequires:\n  - name: dep\n    repo_url: https://evil.example/entire-dep\n"))
	if err != nil {
		t.Fatalf("a legacy repo_url should be ignored, not rejected: %v", err)
	}
	if len(legacy.Requires) != 1 || legacy.Requires[0].Name != "dep" {
		t.Errorf("requirement lost: %+v", legacy.Requires)
	}
}

// entire-plugin.yml is documented as optional and a missing file is handled, so
// a committed placeholder must not be fatal at every tag. An empty stream
// decodes to io.EOF, which used to abort the whole install.
func TestParsePluginMetadata_EmptyIsNotAnError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ label, body string }{
		{"empty", ""},
		{"comment only", "# nothing yet\n"},
		{"whitespace", "\n\n  \n"},
		{"explicit empty document", "---\n"},
	} {
		meta, err := ParsePluginMetadata([]byte(tc.body))
		if err != nil {
			t.Errorf("%s: ParsePluginMetadata = %v, want no error", tc.label, err)
			continue
		}
		if meta == nil {
			t.Errorf("%s: meta is nil, want an empty metadata value", tc.label)
			continue
		}
		if meta.Name != "" || len(meta.Requires) != 0 {
			t.Errorf("%s: meta = %+v, want zero value", tc.label, meta)
		}
	}
	// Genuine syntax errors must still fail.
	if _, err := ParsePluginMetadata([]byte("name: [unclosed\n")); err == nil {
		t.Error("ParsePluginMetadata accepted malformed YAML")
	}
}

// Credentials embedded in a remote must not be persisted: manifest.yml is mode
// 0644 and upgrades re-resolve auth through git's credential helpers.
func TestSavePluginManifest_StripsCredentials(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	if err := SavePluginManifest(&PluginManifest{
		Name:     "demo",
		RepoURL:  "https://bob:hunter2@git.example.com/o/entire-demo",
		Tag:      "v1.0.0",
		Requires: []PluginRequirement{{Name: "dep"}},
	}); err != nil {
		t.Fatal(err)
	}
	m, err := LoadPluginManifest("demo")
	if err != nil || m == nil {
		t.Fatalf("LoadPluginManifest = %v, %v", m, err)
	}
	if strings.Contains(m.RepoURL, "hunter2") || strings.Contains(m.RepoURL, "bob") {
		t.Errorf("repo_url kept credentials: %q", m.RepoURL)
	}
	if len(m.Requires) != 1 || m.Requires[0].Name != "dep" {
		t.Errorf("requirements not round-tripped: %+v", m.Requires)
	}
}

// os.ReadFile sizes its buffer from the file and reads to EOF, so every caller
// inherits whatever is on disk. These files come from a cloned remote (the
// catalog) or a user-writable directory (the manifest), so the read is bounded.
func TestReadFileLimited(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "f")

	if err := os.WriteFile(path, []byte(strings.Repeat("a", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readFileLimited(path, 100)
	if err != nil {
		t.Fatalf("exactly at the limit should read: %v", err)
	}
	if len(got) != 100 {
		t.Errorf("read %d bytes, want 100", len(got))
	}
	// One byte over must error rather than hand back a truncated file, which
	// would parse as valid-but-wrong.
	if _, err := readFileLimited(path, 99); err == nil {
		t.Error("oversize file was accepted")
	} else if !strings.Contains(err.Error(), "limit") {
		t.Errorf("err = %v, want it to name the limit", err)
	}
	if _, err := readFileLimited(filepath.Join(dir, "absent"), 100); err == nil {
		t.Error("missing file should still error")
	}
}
