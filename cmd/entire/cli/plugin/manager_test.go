package plugin

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestValidName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"foo", true},
		{"foo-bar", true},
		{"foo123", true},
		{"123", true},
		{"", false},
		{"-foo", false},
		{"Foo", false},
		{"foo_bar", false},
		{"foo.bar", false},
		{"foo/bar", false},
	}
	for _, c := range cases {
		if got := ValidName(c.in); got != c.want {
			t.Errorf("ValidName(%q) = %v; want %v", c.in, got, c.want)
		}
	}
}

func TestDefaultRoot_HonorsEnv(t *testing.T) {
	// t.Setenv mutates process state and is incompatible with t.Parallel().
	t.Setenv("ENTIRE_PLUGIN_DIR", "/tmp/test-plugins")
	got, err := DefaultRoot()
	if err != nil {
		t.Fatalf("DefaultRoot: %v", err)
	}
	if got != "/tmp/test-plugins" {
		t.Errorf("DefaultRoot = %q; want /tmp/test-plugins", got)
	}
}

func TestList_EmptyOrMissingRoot(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "missing")
	m := &Manager{Root: dir}
	got, err := m.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List = %v; want empty", got)
	}
}

func TestList_ClassifiesAllKinds(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == osWindows {
		t.Skip("symlink classification path uses Unix layout")
	}

	root := t.TempDir()
	m := &Manager{Root: root}

	// Binary plugin: directory + manifest + executable.
	binDir := filepath.Join(root, "entire-bin")
	mustMkdir(t, binDir)
	mustWriteExec(t, filepath.Join(binDir, "entire-bin"), "#!/bin/sh\necho bin\n")
	mf := &BinaryManifest{Owner: "octocat", Name: "bin", Host: "github.com", Tag: "v1.0.0"}
	if err := mf.Save(filepath.Join(binDir, ManifestFileName)); err != nil {
		t.Fatalf("save manifest: %v", err)
	}

	// Script plugin: directory + executable, no manifest.
	scriptDir := filepath.Join(root, "entire-script")
	mustMkdir(t, scriptDir)
	mustWriteExec(t, filepath.Join(scriptDir, "entire-script"), "#!/bin/sh\necho script\n")

	// Local plugin: symlink to a dev directory.
	devDir := filepath.Join(t.TempDir(), "entire-local")
	mustMkdir(t, devDir)
	mustWriteExec(t, filepath.Join(devDir, "entire-local"), "#!/bin/sh\necho local\n")
	if err := os.Symlink(devDir, filepath.Join(root, "entire-local")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// Noise: a plain file at root level should be ignored.
	if err := os.WriteFile(filepath.Join(root, "entire-bogus"), []byte("not a plugin"), 0o644); err != nil {
		t.Fatalf("write bogus: %v", err)
	}
	// Noise: directory not prefixed.
	mustMkdir(t, filepath.Join(root, "random"))

	plugins, err := m.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(plugins) != 3 {
		t.Fatalf("List returned %d plugins, want 3: %+v", len(plugins), plugins)
	}

	got := map[string]Kind{}
	for _, p := range plugins {
		got[p.Name] = p.Kind
	}
	want := map[string]Kind{"bin": KindBinary, "local": KindLocal, "script": KindScript}
	for name, kind := range want {
		if got[name] != kind {
			t.Errorf("plugin %q kind = %v, want %v", name, got[name], kind)
		}
	}
}

func TestFind_ReturnsNilForUnknown(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	m := &Manager{Root: root}
	p, err := m.Find("nope")
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if p != nil {
		t.Errorf("Find(nope) = %+v; want nil", p)
	}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
}

func mustWriteExec(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}
