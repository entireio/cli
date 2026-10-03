package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/stretchr/testify/require"
)

// isolateAgentHomesConfig points ENTIRE_CONFIG_DIR at a fresh t.TempDir() so
// the registry tests never touch the developer's real config directory (or
// each other's, via the per-process go-test fallback — see
// internal/testdirs). t.Setenv forbids t.Parallel(), which is correct here:
// these tests mutate process-global environment.
func isolateAgentHomesConfig(t *testing.T) {
	t.Helper()
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
}

func canonicalAgentHomeForTest(t *testing.T, home string) string {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	return canonical
}

func TestRememberAgentHome_RecordAndReadBack(t *testing.T) {
	isolateAgentHomesConfig(t)

	home := canonicalAgentHomeForTest(t, t.TempDir())
	if err := RememberAgentHome(types.AgentType("Claude Code"), home); err != nil {
		t.Fatalf("RememberAgentHome() error = %v", err)
	}

	got := KnownAgentHomes(types.AgentType("Claude Code"))
	if len(got) != 1 || got[0] != home {
		t.Fatalf("KnownAgentHomes() = %v, want [%s]", got, home)
	}
}

func TestRememberAgentHome_RetargetedAliasPreservesHistory(t *testing.T) {
	isolateAgentHomesConfig(t)
	first, second := t.TempDir(), t.TempDir()
	alias := filepath.Join(t.TempDir(), "active-home")
	if err := os.Symlink(first, alias); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	kind := types.AgentType("Claude Code")
	require.NoError(t, RememberAgentHome(kind, alias))
	require.NoError(t, os.Remove(alias))
	require.NoError(t, os.Symlink(second, alias))
	require.NoError(t, RememberAgentHome(kind, alias))
	firstCanonical, err := filepath.EvalSymlinks(first)
	require.NoError(t, err)
	secondCanonical, err := filepath.EvalSymlinks(second)
	require.NoError(t, err)
	require.Equal(t, []string{firstCanonical, secondCanonical}, KnownAgentHomes(kind))
}

func TestRememberAgentHome_IdempotentNoWrite(t *testing.T) {
	isolateAgentHomesConfig(t)

	home := canonicalAgentHomeForTest(t, t.TempDir())
	agentType := types.AgentType("Claude Code")
	if err := RememberAgentHome(agentType, home); err != nil {
		t.Fatalf("first RememberAgentHome() error = %v", err)
	}

	root, err := configRootForTest(t)
	if err != nil {
		t.Fatal(err)
	}
	info, err := root.Stat(agentHomesFileName)
	if err != nil {
		t.Fatalf("stat registry file after first write: %v", err)
	}
	mtimeAfterFirstWrite := info.ModTime()

	// Make sure a second write, if it happened, would be observable: mtimes
	// on some filesystems have coarse resolution.
	time.Sleep(10 * time.Millisecond)

	if err := RememberAgentHome(agentType, home); err != nil {
		t.Fatalf("second (idempotent) RememberAgentHome() error = %v", err)
	}

	info, err = root.Stat(agentHomesFileName)
	if err != nil {
		t.Fatalf("stat registry file after second call: %v", err)
	}
	if !info.ModTime().Equal(mtimeAfterFirstWrite) {
		t.Fatalf("re-recording a known home modified the file: mtime %v, want unchanged %v", info.ModTime(), mtimeAfterFirstWrite)
	}
}

func TestRememberAgentHome_PerAgentSeparation(t *testing.T) {
	isolateAgentHomesConfig(t)

	claudeHome := canonicalAgentHomeForTest(t, t.TempDir())
	codexHome := canonicalAgentHomeForTest(t, t.TempDir())
	if err := RememberAgentHome(types.AgentType("Claude Code"), claudeHome); err != nil {
		t.Fatal(err)
	}
	if err := RememberAgentHome(types.AgentType("Codex"), codexHome); err != nil {
		t.Fatal(err)
	}

	gotClaude := KnownAgentHomes(types.AgentType("Claude Code"))
	if len(gotClaude) != 1 || gotClaude[0] != claudeHome {
		t.Fatalf("KnownAgentHomes(Claude Code) = %v, want [%s]", gotClaude, claudeHome)
	}
	gotCodex := KnownAgentHomes(types.AgentType("Codex"))
	if len(gotCodex) != 1 || gotCodex[0] != codexHome {
		t.Fatalf("KnownAgentHomes(Codex) = %v, want [%s]", gotCodex, codexHome)
	}
}

func TestRememberAgentHome_RejectsNonAbsolute(t *testing.T) {
	isolateAgentHomesConfig(t)

	err := RememberAgentHome(types.AgentType("Claude Code"), filepath.Join("relative", "dir"))
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("RememberAgentHome() error = %v, want a refusal naming an absolute path requirement", err)
	}
	if got := KnownAgentHomes(types.AgentType("Claude Code")); len(got) != 0 {
		t.Fatalf("KnownAgentHomes() = %v, want nothing recorded after a rejected write", got)
	}
}

func TestRememberAgentHome_CleansUntidyAbsoluteValue(t *testing.T) {
	isolateAgentHomesConfig(t)

	home := canonicalAgentHomeForTest(t, t.TempDir())
	untidy := home + string(filepath.Separator) + "." + string(filepath.Separator)
	if err := RememberAgentHome(types.AgentType("Claude Code"), untidy); err != nil {
		t.Fatalf("RememberAgentHome() error = %v", err)
	}

	want := filepath.Clean(untidy)
	got := KnownAgentHomes(types.AgentType("Claude Code"))
	if len(got) != 1 || got[0] != want {
		t.Fatalf("KnownAgentHomes() = %v, want [%s] (cleaned)", got, want)
	}
}

func TestKnownAgentHomes_SkipsMissingHome(t *testing.T) {
	isolateAgentHomesConfig(t)

	gone := filepath.Join(t.TempDir(), "no-longer-here")
	present := canonicalAgentHomeForTest(t, t.TempDir())
	agentType := types.AgentType("Claude Code")
	if err := RememberAgentHome(agentType, gone); err != nil {
		t.Fatal(err)
	}
	if err := RememberAgentHome(agentType, present); err != nil {
		t.Fatal(err)
	}

	got := KnownAgentHomes(agentType)
	if len(got) != 1 || got[0] != present {
		t.Fatalf("KnownAgentHomes() = %v, want only the still-present home [%s]", got, present)
	}
}

func TestKnownAgentHomes_MissingFileYieldsNil(t *testing.T) {
	isolateAgentHomesConfig(t)

	if got := KnownAgentHomes(types.AgentType("Claude Code")); got != nil {
		t.Fatalf("KnownAgentHomes() = %v, want nil with no registry file", got)
	}
}

func TestAgentHomesRegistry_RefusesLinksOnReadAndWrite(t *testing.T) {
	for _, dangling := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing target", true: "dangling target"}[dangling], func(t *testing.T) {
			isolateAgentHomesConfig(t)
			agentType := types.AgentType("Claude Code")
			require.NoError(t, RememberAgentHome(agentType, t.TempDir()))
			root, err := configRootForTest(t)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, root.Close()) })
			registry := filepath.Join(root.Name(), agentHomesFileName)
			target := filepath.Join(t.TempDir(), "registry.json")
			data, err := os.ReadFile(registry)
			require.NoError(t, err)
			require.NoError(t, os.Rename(registry, target))
			if dangling {
				require.NoError(t, os.Remove(target))
			}
			if err := os.Symlink(target, registry); err != nil {
				t.Skipf("symlink not supported: %v", err)
			}
			require.Nil(t, KnownAgentHomes(agentType), "a linked registry must not grant trust")
			home := canonicalAgentHomeForTest(t, t.TempDir())
			require.Error(t, RememberAgentHome(agentType, home))
			require.Nil(t, KnownAgentHomes(agentType))
			info, err := os.Lstat(registry)
			require.NoError(t, err)
			require.NotZero(t, info.Mode()&os.ModeSymlink, "a refused registry must be preserved")
			if dangling {
				_, err := os.Lstat(target)
				require.True(t, os.IsNotExist(err), "writing the registry must not create the link target")
			} else {
				after, err := os.ReadFile(target)
				require.NoError(t, err)
				require.Equal(t, data, after, "writing the registry must not overwrite the link target")
			}
		})
	}
}

func TestKnownAgentHomes_MalformedFileYieldsNil(t *testing.T) {
	isolateAgentHomesConfig(t)

	root, err := configRootForTest(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root.Name(), agentHomesFileName), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := KnownAgentHomes(types.AgentType("Claude Code")); got != nil {
		t.Fatalf("KnownAgentHomes() = %v, want nil for a malformed registry file", got)
	}
}

func TestKnownAgentHomes_FutureVersionYieldsNil(t *testing.T) {
	isolateAgentHomesConfig(t)

	home := canonicalAgentHomeForTest(t, t.TempDir())
	agentType := types.AgentType("Claude Code")
	if err := RememberAgentHome(agentType, home); err != nil {
		t.Fatal(err)
	}

	root, err := configRootForTest(t)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root.Name(), agentHomesFileName))
	if err != nil {
		t.Fatal(err)
	}
	bumped := strings.Replace(string(data), `"version": 1`, `"version": 999`, 1)
	if bumped == string(data) {
		t.Fatal("test fixture did not find the version field to bump — update the replacement to match MarshalIndentWithNewline's formatting")
	}
	if err := os.WriteFile(filepath.Join(root.Name(), agentHomesFileName), []byte(bumped), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := KnownAgentHomes(agentType); got != nil {
		t.Fatalf("KnownAgentHomes() = %v, want nil for a future-version registry file", got)
	}
}

func TestRememberAgentHome_CapEvictsOldest(t *testing.T) {
	isolateAgentHomesConfig(t)

	agentType := types.AgentType("Claude Code")
	var homes []string
	for range maxHomesPerAgent + 5 {
		h := canonicalAgentHomeForTest(t, t.TempDir())
		homes = append(homes, h)
		if err := RememberAgentHome(agentType, h); err != nil {
			t.Fatal(err)
		}
	}

	got := KnownAgentHomes(agentType)
	if len(got) != maxHomesPerAgent {
		t.Fatalf("KnownAgentHomes() returned %d homes, want the cap of %d", len(got), maxHomesPerAgent)
	}
	// The oldest 5 must be gone; the most recent maxHomesPerAgent must remain.
	wantRemaining := homes[len(homes)-maxHomesPerAgent:]
	for _, w := range wantRemaining {
		found := false
		for _, g := range got {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected recent home %s to survive eviction; got %v", w, got)
		}
	}
	for _, evicted := range homes[:5] {
		for _, g := range got {
			if g == evicted {
				t.Fatalf("expected oldest home %s to be evicted; still present in %v", evicted, got)
			}
		}
	}
}

// configRootForTest opens the per-user config directory the same way
// RememberAgentHome does, for assertions that need to inspect the registry
// file directly (mtime, raw bytes).
func configRootForTest(t *testing.T) (*os.Root, error) {
	t.Helper()
	dir := os.Getenv("ENTIRE_CONFIG_DIR")
	if dir == "" {
		t.Fatal("ENTIRE_CONFIG_DIR not set — call isolateAgentHomesConfig first")
	}
	return os.OpenRoot(dir)
}
