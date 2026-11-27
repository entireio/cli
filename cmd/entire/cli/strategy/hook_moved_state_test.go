package strategy

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// preGuardChained is hook's spec content with the chain block Entire released
// before the chain guard: it runs <hook>.pre-entire whatever called it.
func preGuardChained(spec hookSpec) string {
	backup := spec.name + backupSuffix
	return spec.content + chainComment + `
_entire_hook_dir="$(dirname "$0")"
if [ -x "$_entire_hook_dir/` + backup + `" ]; then
    "$_entire_hook_dir/` + backup + `" "$@"
fi
`
}

// A copy of Entire's hook that a hook manager moved aside is refreshed only by
// an install, and the per-turn install skips hooks that read Current. So a
// moved copy that is stale where it runs makes its hook read Outdated, and one
// in the shape an install leaves it does not.
func TestCheckGitHookState_StaleMovedCopy(t *testing.T) {
	t.Parallel()
	commitMsg := specFor(t, "commit-msg")
	oldPrePush := prePushHeaderBeforeRefList + `if command -v entire >/dev/null 2>&1; then entire hooks git pre-push "$1"; else :; fi` + "\n"
	for name, tc := range map[string]struct {
		file, content string
		want          GitHookState
	}{
		"chain from before the guard": {
			"commit-msg.tool-old", preGuardChained(commitMsg), GitHooksOutdated,
		},
		"chain from before the guard in pre-commit's .legacy": {
			"commit-msg" + legacySuffix, preGuardChained(commitMsg), GitHooksOutdated,
		},
		"runs Entire from the working tree": {
			"commit-msg.tool-old", "#!/bin/sh\n# " + entireHookMarker + "\n./scripts/entire-dev hooks git commit-msg\n", GitHooksOutdated,
		},
		"pre-push from before the ref list": {
			"pre-push.old", oldPrePush, GitHooksOutdated,
		},
		"guarded chain": {
			"commit-msg.tool-old", generateChainedContent(commitMsg.content, commitMsg.name), GitHooksCurrent,
		},
		"what an install writes there": {
			"commit-msg.tool-old", commitMsg.content, GitHooksCurrent,
		},
		"names the backup only in a comment": {
			"commit-msg.saved", commitMsg.content + "# Original hook: commit-msg" + backupSuffix + "\n", GitHooksCurrent,
		},
		"names a working-tree launcher only in a comment": {
			"commit-msg.saved", commitMsg.content + "# was: ./scripts/entire-dev hooks git commit-msg\n", GitHooksCurrent,
		},
		"not Entire's hook": {
			"commit-msg.tool-old", "#!/bin/sh\n\"$(dirname \"$0\")/commit-msg.pre-entire\" \"$@\"\n", GitHooksCurrent,
		},
		"an older copy of the backup is not a moved copy": {
			"commit-msg" + backupSuffix + ".20261001T170405Z", preGuardChained(commitMsg), GitHooksCurrent,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeCurrentManagedHooks(t, dir)
			if err := os.WriteFile(filepath.Join(dir, tc.file), []byte(tc.content), 0o755); err != nil {
				t.Fatal(err)
			}
			if got := gitHookStateInHooksDir(dir); got != tc.want {
				t.Errorf("state = %v, want %v", got, tc.want)
			}
		})
	}
}

// What an earlier release left: Entire's current hook chains to a hook
// manager's wrapper, and the wrapper runs a copy of Entire's hook from before
// the chain guard, which chains back to the wrapper, so every commit recursed.
// The per-turn reinstall now repairs the copy, whether or not the wrapper names
// it, and keeps the hook itself, lines the user added included.
func TestReinstallGitHooks_RepairsStaleMovedCopyAndKeepsHook(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runs POSIX shell hooks")
	}
	spec := specFor(t, "commit-msg")
	for name, tc := range map[string]struct {
		// wrapper runs commit-msg.tool-old and logs "tool".
		wrapper func(log string) string
		want    string
	}{
		// reclaimFromWrappers gives the copy the placeholder.
		"wrapper names the copy": {func(log string) string {
			return toolWrapper("commit-msg.tool-old", log)
		}, "entire,tool,mine"},
		// refreshMovedEntireHooks rewrites the copy to Entire's unchained hook.
		"wrapper computes the copy's name": {func(log string) string {
			return "#!/bin/sh\n\"$(dirname \"$0\")/$(basename \"$0\" " + backupSuffix + ").tool-old\" \"$@\" || exit $?\necho tool >> " + log + "\n"
		}, "entire,entire,tool,mine"},
	} {
		t.Run(name, func(t *testing.T) {
			_, hooksDir := initHooksTestRepo(t)
			ctx := context.Background()
			if _, err := InstallGitHook(ctx, true, false); err != nil {
				t.Fatalf("InstallGitHook: %v", err)
			}
			bin := t.TempDir()
			log := filepath.Join(t.TempDir(), "log")
			writeExec(t, filepath.Join(bin, "entire"), "#!/bin/sh\necho entire >> "+log+"\n")
			edited := generateChainedContent(spec.content, spec.name) + "echo mine >> " + log + "\n"
			writeExec(t, filepath.Join(hooksDir, "commit-msg"), edited)
			writeExec(t, filepath.Join(hooksDir, "commit-msg"+backupSuffix), tc.wrapper(log))
			writeExec(t, filepath.Join(hooksDir, "commit-msg.tool-old"), preGuardChained(spec))

			if got := gitHookStateInHooksDir(hooksDir); got != GitHooksOutdated {
				t.Fatalf("state before = %v, want GitHooksOutdated", got)
			}
			if _, err := ReinstallGitHooks(ctx); err != nil {
				t.Fatalf("ReinstallGitHooks: %v", err)
			}

			data, err := os.ReadFile(filepath.Join(hooksDir, "commit-msg.tool-old"))
			if err != nil {
				t.Fatal(err)
			}
			if movedEntireHookOutdated(spec.name, string(data)) {
				t.Fatalf("commit-msg.tool-old still chains back without the guard:\n%s", data)
			}
			if got, err := os.ReadFile(filepath.Join(hooksDir, "commit-msg")); err != nil || string(got) != edited {
				t.Errorf("commit-msg = %q (err %v), want the hook with the user's line kept", got, err)
			}
			if got := gitHookStateInHooksDir(hooksDir); got != GitHooksCurrent {
				t.Errorf("state after = %v, want GitHooksCurrent", got)
			}

			runCtx, cancel := context.WithTimeout(t.Context(), hookRunTimeout) // a recursive chain never ends
			defer cancel()
			if out, err := runHook(runCtx, bin, filepath.Join(hooksDir, "commit-msg"), filepath.Join(t.TempDir(), "MSG")); err != nil {
				t.Fatalf("hook failed: %v\n%s", err, out)
			}
			ran, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(strings.Fields(string(ran)), ","); got != tc.want {
				t.Errorf("ran %s, want %s", got, tc.want)
			}
		})
	}
}

// Entire will not install through a symlinked hooks directory, so the moved
// copies there cannot be repaired by the per-turn reinstall. Counting them
// would send every agent turn to a reinstall that fails on that refusal, so a
// stale copy alone there does not trigger one. It still reads Outdated, so
// doctor reports it.
func TestHooksNeedReinstall_StaleMovedCopyBehindSymlinkedHooksDir(t *testing.T) {
	t.Parallel()
	target := t.TempDir()
	writeCurrentManagedHooks(t, target)
	if err := os.WriteFile(filepath.Join(target, "commit-msg.tool-old"), []byte(preGuardChained(specFor(t, "commit-msg"))), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "hooks")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	if got := gitHookStateInHooksDir(link); got != GitHooksOutdated {
		t.Errorf("state through the link = %v, want GitHooksOutdated", got)
	}
	if hooksNeedReinstall(link) {
		t.Error("a stale moved copy behind a symlinked hooks directory triggers a reinstall that is refused")
	}
	if !hooksNeedReinstall(target) {
		t.Error("the directory itself does not trigger a reinstall")
	}
}

// The per-turn reinstall rewrites only the moved copies that are stale. A
// current copy of Entire's hook beside a stale one may hold lines the user
// added and gets no backup, so it stays as it is, as a current hook does,
// whether or not the hook itself needed the reinstall. For a current hook that
// holds when the manager's wrapper in the backup runs the current copy by
// name: reclaimFromWrappers takes back only the stale one.
func TestReinstallGitHooks_KeepsCurrentMovedCopy(t *testing.T) {
	spec := specFor(t, "commit-msg")
	saved := spec.content + "echo mine\n"
	for name, tc := range map[string]struct {
		hookIsCurrent, wrapperRunsSaved bool
	}{
		"hook current":                        {hookIsCurrent: true},
		"hook outdated":                       {},
		"hook current, wrapper runs the copy": {hookIsCurrent: true, wrapperRunsSaved: true},
	} {
		t.Run(name, func(t *testing.T) {
			_, hooksDir := initHooksTestRepo(t)
			ctx := context.Background()
			if _, err := InstallGitHook(ctx, true, false); err != nil {
				t.Fatalf("InstallGitHook: %v", err)
			}
			if !tc.hookIsCurrent {
				writeExec(t, filepath.Join(hooksDir, "commit-msg"), "#!/bin/sh\n# "+entireHookMarker+"\n./scripts/entire-dev hooks git commit-msg\n")
			}
			if tc.wrapperRunsSaved {
				writeExec(t, filepath.Join(hooksDir, "commit-msg"), generateChainedContent(spec.content, spec.name))
				writeExec(t, filepath.Join(hooksDir, "commit-msg"+backupSuffix), toolWrapper("commit-msg.saved", "/dev/null"))
			}
			writeExec(t, filepath.Join(hooksDir, "commit-msg.tool-old"), preGuardChained(spec))
			writeExec(t, filepath.Join(hooksDir, "commit-msg.saved"), saved)

			if _, err := ReinstallGitHooks(ctx); err != nil {
				t.Fatalf("ReinstallGitHooks: %v", err)
			}

			if got, err := os.ReadFile(filepath.Join(hooksDir, "commit-msg.tool-old")); err != nil || movedEntireHookOutdated(spec.name, string(got)) {
				t.Errorf("commit-msg.tool-old = %q (err %v), want it repaired", got, err)
			}
			if got, err := os.ReadFile(filepath.Join(hooksDir, "commit-msg.saved")); err != nil || string(got) != saved {
				t.Errorf("commit-msg.saved = %q (err %v), want it kept with the user's line", got, err)
			}
			if got := gitHookStateInHooksDir(hooksDir); got != GitHooksCurrent {
				t.Errorf("state after = %v, want GitHooksCurrent", got)
			}
		})
	}
}

// The per-turn reinstall decides keepHook before it takes the hooks lock, and a
// hook manager does not take that lock. If the manager installs in between,
// moving Entire's current hook aside and putting its wrapper in its place, the
// copy it moved is the one the wrapper runs, and the install takes it back for
// the user's hook as any install would, so each hook runs once.
func TestInstallHooks_KeepHookRechecksTheHookUnderTheLock(t *testing.T) {
	t.Parallel()
	spec := specFor(t, "commit-msg")
	f := newHooksFixture(t)
	f.write("commit-msg", generateChainedContent(spec.content, spec.name))
	f.write("commit-msg"+backupSuffix, userHookV1)
	stale := spec
	stale.keepHook, stale.staleCopiesOnly = true, true // decided before the manager ran
	wrapper := toolWrapper("commit-msg.tool-old", "/dev/null")
	f.toolInstall(wrapper)

	f.install(stale)

	assertHooks(t, f, map[string]string{
		"commit-msg.tool-old":       userHookV1,
		"commit-msg" + backupSuffix: wrapper,
	})
	if !carriesEntireMarker(f.root, "commit-msg") {
		t.Errorf("commit-msg = %q, want Entire's hook", f.read("commit-msg"))
	}
}
