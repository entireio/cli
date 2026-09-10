package agents

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// TestOpenCodeSeedRepoPlantsDeps runs SeedRepo the way SetupRepo does: from a
// process that never called Bootstrap.
//
// That is the whole point of the test. Bootstrap runs in `go run ./e2e/bootstrap`
// and SetupRepo in `go test ./e2e/tests`, so an implementation that hands the
// dependency tree between them through a package variable seeds nothing — and
// seeding degrades quietly, so the suite reports a fast bootstrap while every
// test still pays opencode's ~61MB per-directory install (run 33865617639).
func TestOpenCodeSeedRepoPlantsDeps(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath(openCodeBinary); err != nil {
		t.Skip("opencode not installed")
	}
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not installed")
	}

	dir := t.TempDir()
	agent := &openCodeAgent{model: "test"}
	if err := agent.SeedRepo(dir); err != nil {
		t.Fatalf("SeedRepo: %v", err)
	}

	// node_modules is the one that matters: opencode reinstalls without it.
	//
	// Windows asserts the documented degradation instead. linkFile copies there
	// rather than linking, and a copy cannot be a 61MB directory, so SeedRepo
	// warns and leaves the repo to opencode. Everything else it plants is
	// platform-independent, which is why this branches instead of excluding the
	// whole test — the cross-process resolution above is what it exists to pin.
	link := filepath.Join(dir, ".opencode", "node_modules")
	if runtime.GOOS == "windows" {
		if _, err := os.Lstat(link); !os.IsNotExist(err) {
			t.Errorf("expected SeedRepo to degrade without node_modules on windows, got err=%v", err)
		}
	} else {
		target, err := os.Readlink(link)
		if err != nil {
			t.Fatalf("node_modules is not a symlink into the shared tree: %v", err)
		}
		if _, err := os.Stat(filepath.Join(target, "@opencode-ai", "plugin")); err != nil {
			t.Fatalf("linked tree does not contain %s: %v", openCodePluginPkg, err)
		}
	}

	for _, name := range []string{"package.json", "package-lock.json", "opencode.json"} {
		at := filepath.Join(dir, ".opencode", name)
		if name == "opencode.json" {
			at = filepath.Join(dir, name) // opencode's config sits at the root
		}
		if _, err := os.Stat(at); err != nil {
			t.Errorf("seeded %s missing: %v", name, err)
		}
	}

	// Everything planted must be invisible to git: these repos are the subject
	// of checkpoint assertions, and an untracked 61MB tree in the working set
	// would change what the CLI sees. The .gitignore names itself for that
	// reason, so a shortened list is a real regression rather than a detail.
	ignored, err := os.ReadFile(filepath.Join(dir, ".opencode", ".gitignore"))
	if err != nil {
		t.Fatalf("read seeded .gitignore: %v", err)
	}
	lines := strings.Split(string(ignored), "\n")
	for _, want := range []string{"node_modules", "package.json", "package-lock.json", ".gitignore"} {
		if !slices.Contains(lines, want) {
			t.Errorf("seeded .gitignore does not ignore %q:\n%s", want, ignored)
		}
	}
}
