package agents

import (
	"os"
	"path/filepath"
	"testing"
)

func writeOpenCodeDepsFixture(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"package.json", "package-lock.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBuildPluginDepsRepairsIncompleteCache(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"node_modules", "package.json", "package-lock.json"} {
		for _, wrongType := range []bool{false, true} {
			label := name + "/missing"
			if wrongType {
				label = name + "/wrong-type"
			}
			t.Run(label, func(t *testing.T) {
				t.Parallel()
				dir := filepath.Join(t.TempDir(), "deps")
				writeOpenCodeDepsFixture(t, dir)
				path := filepath.Join(dir, name)
				if err := os.RemoveAll(path); err != nil {
					t.Fatal(err)
				}
				if wrongType {
					if name == "node_modules" {
						if err := os.WriteFile(path, nil, 0o644); err != nil {
							t.Fatal(err)
						}
					} else if err := os.Mkdir(path, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				installs := 0
				install := func(staging string) error {
					installs++
					writeOpenCodeDepsFixture(t, staging)
					return nil
				}
				for range 2 {
					got, err := buildPluginDepsAt("1.0.0", dir, install)
					if err != nil || got != dir {
						t.Fatalf("buildPluginDepsAt = %q, %v", got, err)
					}
					if err := validateOpenCodePluginDeps(dir); err != nil {
						t.Fatal(err)
					}
				}
				if installs != 1 {
					t.Fatalf("installs = %d, want one repair followed by reuse", installs)
				}
			})
		}
	}
}

func TestBuildPluginDepsRejectsIncompleteInstall(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	dir := filepath.Join(parent, "deps")
	got, err := buildPluginDepsAt("1.0.0", dir, func(staging string) error {
		// npm may exit successfully without a lockfile when package-lock=false.
		return os.Mkdir(filepath.Join(staging, "node_modules"), 0o755)
	})
	if err == nil || got != "" {
		t.Fatalf("incomplete install accepted: %q, %v", got, err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "deps.lock" {
		t.Fatalf("failed install left entries other than the persistent lock: %v", entries)
	}
}

func TestBuildPluginDepsValidatesRaceWinner(t *testing.T) {
	t.Parallel()
	for _, complete := range []bool{false, true} {
		name := "incomplete"
		if complete {
			name = "complete"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "deps")
			got, err := buildPluginDepsAt("1.0.0", dir, func(staging string) error {
				writeOpenCodeDepsFixture(t, staging)
				writeOpenCodeDepsFixture(t, dir)
				if !complete {
					return os.Remove(filepath.Join(dir, "package-lock.json"))
				}
				return nil
			})
			if complete {
				if err != nil || got != dir {
					t.Fatalf("complete winner rejected: %q, %v", got, err)
				}
			} else if err == nil || got != "" {
				t.Fatalf("incomplete winner accepted: %q, %v", got, err)
			}
		})
	}
}
