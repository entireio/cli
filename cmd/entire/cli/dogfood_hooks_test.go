package cli

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// committedHookConfigAgents are the agents whose Entire hook config this repo
// commits: .claude/settings.json, .codex/hooks.json,
// .opencode/plugins/entire.ts, and .pi/extensions/entire/index.ts. Each must be
// found and current. The path travels with the agent name so the test can copy
// that exact committed config into its isolated repository.
//
// Named rather than counted. A count would let a newly added agent paper over a
// committed config that went missing — one appears, one disappears, the total
// still matches and the test keeps passing while silently guarding less.
var committedHookConfigAgents = []struct {
	name string
	path string
}{
	{name: "claude-code", path: ".claude/settings.json"},
	{name: "codex", path: ".codex/hooks.json"},
	{name: "opencode", path: ".opencode/plugins/entire.ts"},
	{name: "pi", path: ".pi/extensions/entire/index.ts"},
}

// TestCommittedHookConfigsAreCurrent pins this repo's own committed Entire hook
// configs against what the CLI would install today.
//
// This repo commits its dogfood hook configs so every clone gets checkpointing
// without each person running `entire enable`. Those copies are generated from
// templates that live in the same tree and keep moving, and nothing regenerates
// them — so they rot silently. Neither guard that looks at them catches it:
// AreHooksInstalled only greps the ownership marker, which survives any template
// change, and Pi's fireHook swallows every error by design. The committed Pi
// extension sat two commits behind its template for three weeks that way, which
// cost anyone dogfooding Pi here the ENTIRE_PI_NESTED guard — a subagent
// forwarded its own lifecycle as if it were the user's session.
//
// CheckHookConfig already detects this; it just had no caller on the CI path, so
// only whoever happened to run `entire doctor` would see it. Running it here
// fails the next template edit in CI instead of on a teammate's machine.
//
// When this fails: run `entire enable --force` at the repo root and commit the
// regenerated files.
func TestCommittedHookConfigsAreCurrent(t *testing.T) {
	// t.Chdir is process-global, so this test must not be parallel. It is needed
	// because CheckHookConfig resolves the config paths from the working
	// directory via paths.WorktreeRoot. That resolution is cached, but the cache
	// is keyed on the working directory, so chdir invalidates it rather than
	// handing us a root some earlier test left behind.
	//
	// Copy the committed files into a normal temporary repository before changing
	// directory. The source checkout may itself be a linked worktree, where an
	// agent can deliberately resolve its repository-wide config outside this
	// checkout. This test is about the committed files, not that surrounding Git
	// topology, so the fixture must make the checkout root authoritative.
	sourceRoot := repoRootFromSource(t)
	repoRoot := t.TempDir()
	testutil.InitRepo(t, repoRoot)
	copyCommittedHookConfigs(t, sourceRoot, repoRoot)
	t.Chdir(repoRoot)

	ctx := context.Background()
	checked := make(map[string]agent.HookConfigState)
	for _, name := range agent.List() {
		ag, err := agent.Get(name)
		if err != nil {
			t.Errorf("agent.Get(%s): %v", name, err)
			continue
		}
		hf, ok := agent.AsHookFreshness(ag)
		if !ok {
			continue // this agent has no drift check
		}
		state := hf.CheckHookConfig(ctx)
		checked[string(name)] = state
		if state == agent.HooksOutdated {
			t.Errorf("committed %s hook config is stale: it no longer matches what "+
				"InstallHooks writes today. Regenerate with `entire enable --force` "+
				"at the repo root and commit the result.", ag.Type())
		}
	}

	// Assert per agent, not in aggregate. Checking only that *something* was
	// compared would let this test keep passing while quietly no longer guarding
	// one of the configs — the same silent-rot failure mode it exists to catch.
	for _, config := range committedHookConfigAgents {
		state, ok := checked[config.name]
		switch {
		case !ok:
			t.Errorf("%s reported no hook-config drift check; this repo commits a "+
				"config for it, so it must still implement agent.HookFreshness", config.name)
		case state == agent.HooksAbsent:
			t.Errorf("no committed hook config found for %s; this repo is supposed to "+
				"commit one. If it was intentionally removed, drop %s from "+
				"committedHookConfigAgents", config.name, config.name)
		}
	}
}

func copyCommittedHookConfigs(t *testing.T, sourceRoot, destinationRoot string) {
	t.Helper()
	for _, config := range committedHookConfigAgents {
		data, err := os.ReadFile(filepath.Join(sourceRoot, config.path))
		if err != nil {
			t.Fatalf("read committed %s: %v", config.path, err)
		}
		testutil.WriteFile(t, destinationRoot, config.path, string(data))
	}
}

// repoRootFromSource returns this repo's root: the nearest directory at or above
// this file that holds a go.mod.
//
// Anchored on this file's own compiled-in path rather than the working
// directory, because package cli has tests that t.Chdir and the root must not
// depend on what an earlier one left behind.
func repoRootFromSource(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller: no caller information")
	}
	for dir := filepath.Dir(file); ; {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod at or above %s", filepath.Dir(file))
		}
		dir = parent
	}
}
