package agent_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// resetVouched restores the strict default, which is what every other test in
// the process expects. Not t.Parallel-safe: the policy is process-wide.
func resetVouched(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { agent.SetVouchedSymlinkedDirs("", nil) })
}

// The name check is the boundary that keeps this from being a general
// "follow symlinks" switch. It is separate from the trust gate: even a
// developer's own verified settings.local.json cannot vouch for these.
func TestSetVouchedSymlinkedDirs_RefusesAnythingButAnAgentConfigDirectory(t *testing.T) {
	resetVouched(t)

	forbidden := []string{
		".entire",
		".entire/metadata",
		".git/hooks",
		".git",
		"..",
		"/etc",
		"",
		".",
	}
	worktree := t.TempDir()
	rejected := agent.SetVouchedSymlinkedDirs(worktree, forbidden)
	if len(rejected) != len(forbidden) {
		t.Errorf("rejected %d of %d; every one of these must be unspellable: %v",
			len(rejected), len(forbidden), rejected)
	}
	if got := agent.VouchedSymlinkedDirs(worktree); len(got) != 0 {
		t.Errorf("vouched = %v, want none accepted", got)
	}
}

func TestSetVouchedSymlinkedDirs_AcceptsAgentDirectories(t *testing.T) {
	resetVouched(t)

	root := t.TempDir()
	if rejected := agent.SetVouchedSymlinkedDirs(root, []string{".claude"}); len(rejected) != 0 {
		t.Fatalf("rejected %v, want .claude accepted", rejected)
	}
	if got := agent.VouchedSymlinkedDirs(root); !slices.Equal(got, []string{".claude"}) {
		t.Errorf("vouched = %v, want [.claude]", got)
	}
	if got := agent.VouchedSymlinkedDirs(t.TempDir()); len(got) != 0 {
		t.Errorf("vouched = %v for another worktree, want none", got)
	}

	// Replaces rather than accumulates, so a settings change that removes an
	// entry stops the following in the same process.
	agent.SetVouchedSymlinkedDirs(root, nil)
	if got := agent.VouchedSymlinkedDirs(root); len(got) != 0 {
		t.Errorf("vouched = %v after clearing, want none", got)
	}
}

// The structural rule holds independently of the pinned list, so a careless
// edit to that list still cannot make an Entire-owned tree vouchable.
// TestVouchableDirsMatchTheBuiltInAgents (cli package) covers drift the other
// way, where every built-in agent is actually registered.
func TestVouchableSymlinkedDirs_NeverIncludesAnEntireOwnedTree(t *testing.T) {
	t.Parallel()

	vouchable := agent.VouchableSymlinkedDirs()
	for _, forbidden := range []string{".entire", ".entire/metadata", ".git", ".git/hooks"} {
		if slices.Contains(vouchable, forbidden) {
			t.Errorf("%s must never be vouchable", forbidden)
		}
	}
	if len(vouchable) == 0 {
		t.Error("no directory is vouchable at all; the pinned list has gone empty and the hatch is dead")
	}
}

// The default is strict, so a caller that never configures a policy behaves
// exactly as it did before the setting existed.
func TestOpenHookConfig_StillRefusesAnUnvouchedSymlinkedDir(t *testing.T) {
	resetVouched(t)
	agent.SetVouchedSymlinkedDirs("", nil)

	worktree := t.TempDir()
	dest := t.TempDir()
	if err := os.Symlink(dest, filepath.Join(worktree, ".claude")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	cfg, err := agent.OpenHookConfig(worktree, ".claude/settings.json")
	if err != nil {
		t.Fatalf("OpenHookConfig() error = %v", err)
	}
	if err := cfg.Write([]byte("{}"), 0o600); !isSymlinkRefusal(err) {
		t.Fatalf("Write() error = %v, want osroot.ErrSymlinkedPath", err)
	}
	entries, readErr := os.ReadDir(dest)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Errorf("wrote %d entries through an unvouched link", len(entries))
	}
}

// And with the entry present, the write lands at the far end. This is the whole
// point of the hatch: a dotfile-managed .claude keeps working.
func TestOpenHookConfig_FollowsAVouchedSymlinkedDir(t *testing.T) {
	resetVouched(t)

	worktree := t.TempDir()
	dest := t.TempDir()
	if err := os.Symlink(dest, filepath.Join(worktree, ".claude")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	agent.SetVouchedSymlinkedDirs(worktree, []string{".claude"})

	cfg, err := agent.OpenHookConfig(worktree, ".claude/settings.json")
	if err != nil {
		t.Fatalf("OpenHookConfig() error = %v", err)
	}
	if err := cfg.Write([]byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dest, "settings.json"))
	if err != nil {
		t.Fatalf("the write did not land at the link's target: %v", err)
	}
	if string(got) != `{"ok":true}` {
		t.Errorf("content = %q", got)
	}

	// Read and Exists must agree with Write, or callers choose between merging
	// and overwriting on a different file from the one they then write.
	if !cfg.Exists() {
		t.Error("Exists() = false for a file Write just created")
	}
	back, err := cfg.Read()
	if err != nil || string(back) != `{"ok":true}` {
		t.Errorf("Read() = %q, %v", back, err)
	}

	// The path stays the one the user recognises. It is what doctor prints and
	// what agents put in their own config, where following the link is the
	// agent's business and works.
	if want := filepath.Join(worktree, ".claude", "settings.json"); cfg.Path() != want {
		t.Errorf("Path() = %q, want the link's own path %q", cfg.Path(), want)
	}
}

// Vouching for one directory does not vouch for links inside it: below the
// anchor everything is a name in a root again.
func TestOpenHookConfig_VouchedDirDoesNotVouchForLinksBeneathIt(t *testing.T) {
	resetVouched(t)

	worktree := t.TempDir()
	dest := t.TempDir()
	if err := os.Symlink(dest, filepath.Join(worktree, ".pi")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	elsewhere := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dest, "extensions"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(dest, "extensions", "entire")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	agent.SetVouchedSymlinkedDirs(worktree, []string{".pi"})

	cfg, err := agent.OpenHookConfig(worktree, ".pi/extensions/entire/index.ts")
	if err != nil {
		t.Fatalf("OpenHookConfig() error = %v", err)
	}
	if err := cfg.Write([]byte("x"), 0o600); !isSymlinkRefusal(err) {
		t.Fatalf("Write() error = %v, want the inner link refused", err)
	}
	entries, readErr := os.ReadDir(elsewhere)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Errorf("wrote %d entries through an unvouched inner link", len(entries))
	}
}

// A vouched link that cannot be resolved is an error, not a silent fallback to
// refusing: "we could not follow it" is a different answer from "we would not".
func TestOpenHookConfig_VouchedButDanglingIsAnError(t *testing.T) {
	resetVouched(t)

	worktree := t.TempDir()
	if err := os.Symlink(filepath.Join(t.TempDir(), "nowhere"), filepath.Join(worktree, ".claude")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	agent.SetVouchedSymlinkedDirs(worktree, []string{".claude"})

	_, err := agent.OpenHookConfig(worktree, ".claude/settings.json")
	if err == nil {
		t.Fatal("OpenHookConfig() = nil error for a vouched but dangling link")
	}
	if !strings.Contains(err.Error(), ".claude") {
		t.Errorf("error = %q, want it to name the directory", err)
	}
}

func isSymlinkRefusal(err error) bool {
	return err != nil && strings.Contains(err.Error(), osroot.ErrSymlinkedPath.Error())
}

// The Entire-owned directory is the one directory Entire both creates and
// deletes, so it cannot also be a link the user manages. Vouching for it was
// accepted and then broke uninstall: the root anchored ON that directory, so
// RemoveDir had nothing above it to delete from and refused with "refusing to
// remove the worktree root" -- about a path that was neither -- leaving the pi
// extension in place for pi to keep discovering.
func TestSetVouchedSymlinkedDirs_RefusesTheEntireOwnedDirectory(t *testing.T) {
	resetVouched(t)

	rejected := agent.SetVouchedSymlinkedDirs(t.TempDir(), []string{".pi/extensions/entire"})
	if len(rejected) != 1 {
		t.Errorf("rejected = %v, want .pi/extensions/entire refused", rejected)
	}
	if got := agent.VouchedSymlinkedDirs(""); len(got) != 0 {
		t.Errorf("vouched = %v, want none", got)
	}
	if slices.Contains(agent.VouchableSymlinkedDirs(), ".pi/extensions/entire") {
		t.Error(".pi/extensions/entire must not appear in the vouchable set either")
	}
}

// Pi is still served by the hatch, at the levels the user actually owns, and
// uninstall works through the link. This is the test the coordinate split
// exists for: the ownership decision comes from the worktree-relative name
// (whose dir is `.pi/extensions/entire`, base `entire`) while the removal is
// performed on the root-relative name (`entire`) inside the anchored root.
// Reading either coordinate for both jobs gets one of them wrong.
func TestOpenHookConfig_VouchedPiExtensionsInstallsAndUninstalls(t *testing.T) {
	for _, vouch := range []string{".pi", ".pi/extensions"} {
		t.Run(vouch, func(t *testing.T) {
			resetVouched(t)

			worktree := t.TempDir()
			dest := t.TempDir()
			linkAt := filepath.Join(worktree, filepath.FromSlash(vouch))
			if err := os.MkdirAll(filepath.Dir(linkAt), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(dest, linkAt); err != nil {
				t.Skipf("symlink not supported: %v", err)
			}
			agent.SetVouchedSymlinkedDirs(worktree, []string{vouch})

			cfg, err := agent.OpenHookConfig(worktree, ".pi/extensions/entire/index.ts")
			if err != nil {
				t.Fatalf("OpenHookConfig() error = %v", err)
			}
			if err := cfg.Write([]byte("// entire\n"), 0o600); err != nil {
				t.Fatalf("Write() error = %v", err)
			}
			if !cfg.Exists() {
				t.Fatal("Exists() = false after Write")
			}

			if err := cfg.RemoveDir(); err != nil {
				t.Fatalf("RemoveDir() error = %v; pi discovers extensions by directory, so a "+
					"refused uninstall leaves one it still loads", err)
			}

			// The generated directory is gone from wherever it landed...
			if _, serr := os.Lstat(filepath.Join(worktree, ".pi", "extensions", "entire")); serr == nil {
				t.Error("the entire/ extension directory survived uninstall")
			}
			// ...and the user's own linked directory is not deleted with it.
			if _, serr := os.Lstat(dest); serr != nil {
				t.Errorf("the vouched link's target must survive uninstall: %v", serr)
			}
			if _, serr := os.Lstat(linkAt); serr != nil {
				t.Errorf("the user's own link at %s must survive uninstall: %v", vouch, serr)
			}
		})
	}
}

// RemoveDir's precondition still refuses a directory Entire did not create,
// and says which one, rather than reporting the worktree root.
func TestRemoveDir_RefusesADirectoryEntireDidNotCreate(t *testing.T) {
	resetVouched(t)

	worktree := t.TempDir()
	cfg, err := agent.OpenHookConfig(worktree, ".claude/settings.json")
	if err != nil {
		t.Fatalf("OpenHookConfig() error = %v", err)
	}
	err = cfg.RemoveDir()
	if err == nil {
		t.Fatal("RemoveDir() on .claude = nil error; it would delete the user's own config")
	}
	if !strings.Contains(err.Error(), ".claude") {
		t.Errorf("error = %q, want it to name the directory it refused", err)
	}
	if strings.Contains(err.Error(), "worktree root") {
		t.Errorf("error = %q, must not report the worktree root for a named directory", err)
	}
}

// The policy is a package global because the import direction forces it
// (settings may import agent, not the reverse), so it is scoped to the worktree
// it was loaded for. A process that loads settings for one tree and then writes
// an agent config for another must not follow a link only the first vouched
// for, and refusing is the safe direction: it degrades to the strict behaviour
// rather than to following someone else's link.
func TestOpenHookConfig_VouchIsScopedToItsWorktree(t *testing.T) {
	resetVouched(t)

	vouchedTree := t.TempDir()
	otherTree := t.TempDir()
	dest := t.TempDir()
	if err := os.Symlink(dest, filepath.Join(otherTree, ".claude")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	// The grant belongs to a different worktree than the one being written.
	agent.SetVouchedSymlinkedDirs(vouchedTree, []string{".claude"})

	cfg, err := agent.OpenHookConfig(otherTree, ".claude/settings.json")
	if err != nil {
		t.Fatalf("OpenHookConfig() error = %v", err)
	}
	if err := cfg.Write([]byte("{}"), 0o600); !isSymlinkRefusal(err) {
		t.Fatalf("Write() error = %v, want the link refused; the grant was for another worktree", err)
	}
	entries, readErr := os.ReadDir(dest)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Errorf("wrote %d entries through a link vouched for a different worktree", len(entries))
	}
}

// "Following symlinked agent directories" must be true when printed. A user may
// legitimately vouch for a path that is an ordinary directory, absent, or a
// dangling link on this machine, and in none of those is Entire following
// anything.
func TestFollowedSymlinkedDirs_OnlyReportsActualLinks(t *testing.T) {
	resetVouched(t)

	worktree := t.TempDir()
	// .claude: vouched and a real link -> followed.
	if err := os.Symlink(t.TempDir(), filepath.Join(worktree, ".claude")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	// .codex: vouched but an ordinary directory -> not followed.
	if err := os.MkdirAll(filepath.Join(worktree, ".codex"), 0o750); err != nil {
		t.Fatal(err)
	}
	// .cursor: vouched but absent -> not followed.
	// .gemini: vouched but dangling -> not followed (and nothing is written there).
	if err := os.Symlink(filepath.Join(t.TempDir(), "nowhere"), filepath.Join(worktree, ".gemini")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	agent.SetVouchedSymlinkedDirs(worktree, []string{".claude", ".codex", ".cursor", ".gemini"})

	if got := agent.FollowedSymlinkedDirs(worktree); !slices.Equal(got, []string{".claude", ".gemini"}) {
		t.Errorf("FollowedSymlinkedDirs() = %v, want only the paths that are symlinks on disk", got)
	}
	// The configuration is unchanged; the two answers are different questions.
	if got := agent.VouchedSymlinkedDirs(worktree); len(got) != 4 {
		t.Errorf("VouchedSymlinkedDirs() = %v, want the full configured set", got)
	}
	if got := agent.FollowedSymlinkedDirs(t.TempDir()); len(got) != 0 {
		t.Errorf("FollowedSymlinkedDirs() = %v for another worktree, want none", got)
	}
}
