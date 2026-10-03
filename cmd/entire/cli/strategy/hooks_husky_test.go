package strategy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// The two wrappers Husky v9 writes at .husky/_/<hook>: 9.0.1–9.1.0, 9.1.1–9.1.7.
const (
	huskyWrapperParamExpansion = "#!/usr/bin/env sh\n. \"${0%/*}/h\""
	huskyWrapperDirname        = "#!/usr/bin/env sh\n. \"$(dirname \"$0\")/h\""
)

// Minimal stand-ins for Husky's .husky/_/h (MIT, github.com/typicode/husky),
// without the ~/.huskyrc and init.sh lookups so tests never read $HOME.
const (
	huskyH90 = `#!/usr/bin/env sh
h="${0##*/}"
s="${0%/*/*}/$h"
[ ! -f "$s" ] && exit 0
[ "${HUSKY-}" = "0" ] && exit 0
sh -e "$s" "$@"
`
	// 9.1.0–9.1.2 source the user's script under set -e with an EXIT trap.
	huskyH910 = `#!/usr/bin/env sh
n=$(basename "$0")
s=$(dirname "$(dirname "$0")")/$n
[ ! -f "$s" ] && exit 0
[ "${HUSKY-}" = "0" ] && exit 0
c=0
h() {
	[ $c = 0 ] && return
	exit 1
}
trap 'c=$?; h' EXIT
set -e
PATH=node_modules/.bin:$PATH
. "$s"
`
	huskyH913 = `#!/usr/bin/env sh
n=$(basename "$0")
s=$(dirname "$(dirname "$0")")/$n
[ ! -f "$s" ] && exit 0
[ "${HUSKY-}" = "0" ] && exit 0
export PATH="node_modules/.bin:$PATH"
sh -e "$s" "$@"
`
)

// stubEntire and userHuskyHook log "<ENTIRE_CHAINED_HOOK>|<args>"; the user's
// hook also records its stdin and exits with $USER_HOOK_EXIT.
const (
	stubEntire    = "#!/bin/sh\nprintf '%s|%s\\n' \"${ENTIRE_CHAINED_HOOK-}\" \"$*\" >> entire.log\n"
	userHuskyHook = `printf '%s|%s\n' "${ENTIRE_CHAINED_HOOK-}" "$*" >> user.log
cat >> user.stdin
exit "${USER_HOOK_EXIT:-0}"
`
)

// writeExecutable writes from a child process, so this process never holds a
// write fd to a file a shell later execs (see linkExecutable on ETXTBSY).
func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), requireShell(t), "-c", `cat > "$1" && chmod 755 "$1"`, "sh", path)
	cmd.Stdin = strings.NewReader(content)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("write %s: %v\n%s", path, err, out)
	}
}

// chainRun installs Entire's chained hook over backup in <dir>/.husky/_, the
// way InstallGitHook chains it, and runs it as git does.
type chainRun struct {
	hook, backup, h, userHook, stdin string
	env, args                        []string
}

type chainResult struct {
	dir, out string
	err      error
}

func (c chainRun) run(t *testing.T, shell string) chainResult {
	t.Helper()
	dir := t.TempDir()
	hooksDir := filepath.Join(dir, ".husky", "_")
	binDir := filepath.Join(dir, "bin")
	for _, d := range []string{hooksDir, binDir, filepath.Join(dir, "tmp")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{filepath.Join(hooksDir, "h"): c.h, filepath.Join(dir, ".husky", c.hook): c.userHook}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable(t, filepath.Join(hooksDir, c.hook+backupSuffix), c.backup)
	writeExecutable(t, filepath.Join(binDir, "entire"), stubEntire)

	spec := findHookSpec(t, buildHookSpecs(bareEntireHookCmd), c.hook)
	hookPath := filepath.Join(hooksDir, c.hook)
	content := generateChainedContent(spec.content, c.hook, chainFormFor(openTestRoot(t, hooksDir), c.hook))
	if err := os.WriteFile(hookPath, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(context.Background(), shell, append([]string{hookPath}, c.args...)...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(c.stdin)
	cmd.Env = append(envWithPath(binDir+string(os.PathListSeparator)+os.Getenv("PATH")),
		"TMPDIR="+filepath.Join(dir, "tmp"), "HUSKY=", ChainedHookEnvVar+"=")
	cmd.Env = append(cmd.Env, c.env...)
	out, err := cmd.CombinedOutput()
	return chainResult{dir: dir, out: string(out), err: err}
}

func (r chainResult) read(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(r.dir, name))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

// TestHuskyChain_UserHookDecides is the bug: executing the backed-up wrapper
// makes Husky look for .husky/<hook>.pre-entire, find nothing and exit 0.
func TestHuskyChain_UserHookDecides(t *testing.T) {
	t.Parallel()
	shells := []string{requireShell(t)}
	if bash, err := exec.LookPath("bash"); err == nil {
		shells = append(shells, bash)
	}
	variants := []struct{ name, h, wrapper string }{
		{"9.0.x", huskyH90, huskyWrapperParamExpansion},
		{"9.1.1-9.1.2", huskyH910, huskyWrapperDirname},
		{"9.1.3+", huskyH913, huskyWrapperDirname},
	}
	for _, v := range variants {
		for _, shell := range shells {
			t.Run(v.name+"/"+filepath.Base(shell), func(t *testing.T) {
				t.Parallel()
				c := chainRun{hook: "commit-msg", backup: v.wrapper, h: v.h, userHook: userHuskyHook, args: []string{"MSG"}}
				r := c.run(t, shell)
				if r.err != nil {
					t.Fatalf("hook failed: %v\n%s", r.err, r.out)
				}
				if got := r.read(t, "entire.log") + r.read(t, "user.log"); got != "|hooks git commit-msg MSG\ncommit-msg|MSG\n" {
					t.Errorf("Entire then the user's hook should run, got %q\n%s", got, r.out)
				}

				c.env = []string{"USER_HOOK_EXIT=3"}
				if r := c.run(t, shell); r.err == nil {
					t.Errorf("a failing .husky/commit-msg must fail the hook\n%s", r.out)
				}
			})
		}
	}
}

func TestHuskyChain_Behaviour(t *testing.T) {
	t.Parallel()
	const refs = "refs/heads/main 1111 refs/heads/main 2222\n"
	tests := []struct {
		name  string
		run   chainRun
		check func(t *testing.T, r chainResult)
	}{
		{"pre-push receives git's stdin", chainRun{hook: "pre-push", stdin: refs, args: []string{"origin", "url"}},
			func(t *testing.T, r chainResult) {
				t.Helper()
				if got := r.read(t, "user.stdin"); got != refs {
					t.Errorf("user pre-push stdin = %q", got)
				}
			}},
		// 9.1.0–9.1.2's h installs an EXIT trap: the subshell must keep it from
		// replacing the trap that removes the stdin copy.
		{"post-rewrite replays stdin and removes its copy", chainRun{hook: postRewriteHook, h: huskyH910, stdin: refs, args: []string{"amend"}},
			func(t *testing.T, r chainResult) {
				t.Helper()
				if got := r.read(t, "user.stdin"); got != refs {
					t.Errorf("user post-rewrite stdin = %q", got)
				}
				if left, err := os.ReadDir(filepath.Join(r.dir, "tmp")); err != nil || len(left) != 0 {
					t.Errorf("stdin copy left behind: %v %v", left, err)
				}
			}},
		{"HUSKY=0 skips the user's hook", chainRun{hook: "commit-msg", env: []string{"HUSKY=0", "USER_HOOK_EXIT=1"}, args: []string{"MSG"}},
			func(t *testing.T, r chainResult) {
				t.Helper()
				if got := r.read(t, "user.log"); got != "" {
					t.Errorf("user hook ran: %q", got)
				}
			}},
		// The user's own Entire call is skipped via the marker; a nested hook,
		// e.g. a pre-push script running `git push`, must still run Entire.
		{"only the user's direct Entire call is marked", chainRun{hook: "pre-push", env: []string{"NESTED="}, args: []string{"origin", "url"},
			userHook: "entire hooks git pre-push \"$1\"\nif [ -z \"$NESTED\" ]; then NESTED=1 sh .husky/_/pre-push nested url; fi\n"},
			func(t *testing.T, r chainResult) {
				t.Helper()
				want := "|hooks git pre-push origin\npre-push|hooks git pre-push origin\n|hooks git pre-push nested\npre-push|hooks git pre-push nested\n"
				if got := r.read(t, "entire.log"); got != want {
					t.Errorf("entire calls = %q, want %q", got, want)
				}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := tt.run
			c.backup = huskyWrapperDirname
			if c.h == "" {
				c.h = huskyH913
			}
			if c.userHook == "" {
				c.userHook = userHuskyHook
			}
			r := c.run(t, requireShell(t))
			if r.err != nil {
				t.Fatalf("hook failed: %v\n%s", r.err, r.out)
			}
			tt.check(t, r)
		})
	}
}

// TestHuskyChain_OtherBackupsAreStillExecuted: sourcing a bash script into sh
// would break it.
func TestHuskyChain_OtherBackupsAreStillExecuted(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	backup := "#!/usr/bin/env bash\n[[ -n \"$1\" ]] && echo \"ran $*\" >> user.log\n"
	r := chainRun{hook: "commit-msg", backup: backup, args: []string{"MSG"}}.run(t, requireShell(t))
	if got := r.read(t, "user.log"); r.err != nil || got != "ran MSG\n" {
		t.Errorf("backup should be executed with git's arguments: %v, log %q\n%s", r.err, got, r.out)
	}
}

func TestIsHuskyV9Wrapper(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, content string
		want          bool
	}{
		{"9.0.1-9.1.0 form", huskyWrapperParamExpansion, true},
		{"9.1.1+ form", huskyWrapperDirname, true},
		{"trailing newline", huskyWrapperDirname + "\n", true},
		{"extra line", huskyWrapperDirname + "\necho hi\n", false},
		{"bash shebang", "#!/bin/bash\n. \"$(dirname \"$0\")/h\"", false},
		{"husky v8 hook", "#!/usr/bin/env sh\n. \"$(dirname -- \"$0\")/_/husky.sh\"\n\nnpx lint-staged\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "hook"), []byte(tt.content), 0o755); err != nil {
				t.Fatal(err)
			}
			if got := isHuskyV9Wrapper(openTestRoot(t, dir), "hook"); got != tt.want {
				t.Errorf("isHuskyV9Wrapper(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}

	t.Run("symlink to a wrapper", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		target := filepath.Join(dir, "wrapper")
		if err := os.WriteFile(target, []byte(huskyWrapperDirname), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, "link")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if isHuskyV9Wrapper(openTestRoot(t, dir), "link") {
			t.Error("a symlink must not be treated as a Husky wrapper")
		}
	})
}

func openTestRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func chainedHook(t *testing.T, hook string, form chainForm) string {
	t.Helper()
	return generateChainedContent(findHookSpec(t, buildHookSpecs(bareEntireHookCmd), hook).content, hook, form)
}

// writeChainedHooks writes every managed hook chained in form over backup.
func writeChainedHooks(t *testing.T, dir, backup string, form chainForm) {
	t.Helper()
	for _, hook := range gitHookNames {
		if err := os.WriteFile(filepath.Join(dir, hook), []byte(chainedHook(t, hook, form)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, hook+backupSuffix), []byte(backup), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// mainPrePushOverBackup is pre-push as main wrote it over any backup. A
// literal, so a change to the exec form shows up as a repo no longer upgraded.
const mainPrePushOverBackup = `#!/bin/sh
# Entire CLI hooks
# Pre-push hook: push session logs alongside user's push
# $1 is the remote name (e.g., "origin")
if command -v entire >/dev/null 2>&1; then entire hooks git pre-push "$1"; else :; fi
# Chain: run pre-existing hook
_entire_hook_dir="$(dirname "$0")"
if [ -x "$_entire_hook_dir/pre-push.pre-entire" ]; then
    "$_entire_hook_dir/pre-push.pre-entire" "$@"
fi
`

func TestCheckGitHookState_HuskyChainForm(t *testing.T) {
	t.Parallel()
	const other = "#!/bin/sh\necho mine\n"
	type row struct {
		name, backup  string
		form          chainForm
		hook, content string
		want          GitHookState
	}
	rows := []row{
		{"sourcing over Husky", huskyWrapperDirname, chainSourceHusky, "", "", GitHooksCurrent},
		{"exec over another backup", other, chainExec, "", "", GitHooksCurrent},
		{"main's pre-push over Husky", huskyWrapperDirname, chainSourceHusky, "pre-push", mainPrePushOverBackup, GitHooksOutdated},
		{"hand-edited over Husky", huskyWrapperDirname, chainSourceHusky, "pre-push", mainPrePushOverBackup + "echo mine\n", GitHooksCurrent},
		{"sourcing over another backup", other, chainExec, "commit-msg", chainedHook(t, "commit-msg", chainSourceHusky), GitHooksOutdated},
	}
	for _, hook := range gitHookNames {
		rows = append(rows, row{"exec over Husky: " + hook, huskyWrapperParamExpansion, chainSourceHusky, hook, chainedHook(t, hook, chainExec), GitHooksOutdated})
	}
	for _, tt := range rows {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeChainedHooks(t, dir, tt.backup, tt.form)
			if tt.hook != "" {
				if err := os.WriteFile(filepath.Join(dir, tt.hook), []byte(tt.content), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if got := gitHookStateInHooksDir(dir); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// TestInstallGitHook_UpgradesHuskyChain repairs repos an earlier version broke.
func TestInstallGitHook_UpgradesHuskyChain(t *testing.T) {
	repoDir, _ := initHooksTestRepo(t)
	hooksDir := filepath.Join(repoDir, ".husky", "_")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.RunGit(t, repoDir, "config", "--local", "core.hooksPath", ".husky/_")
	ClearHooksDirCache()
	t.Cleanup(ClearHooksDirCache)
	writeChainedHooks(t, hooksDir, huskyWrapperDirname, chainExec)

	if got := CheckGitHookState(context.Background()); got != GitHooksOutdated {
		t.Fatalf("exec chains over Husky wrappers = %v, want GitHooksOutdated", got)
	}
	if _, err := InstallGitHook(context.Background(), true, false); err != nil {
		t.Fatalf("InstallGitHook() error = %v", err)
	}
	if got := CheckGitHookState(context.Background()); got != GitHooksCurrent {
		t.Fatalf("after reinstall = %v, want GitHooksCurrent", got)
	}
	for _, hook := range gitHookNames {
		data, err := os.ReadFile(filepath.Join(hooksDir, hook))
		if err != nil || string(data) != chainedHook(t, hook, chainSourceHusky) {
			t.Errorf("%s should source its Husky wrapper after reinstall (%v):\n%s", hook, err, data)
		}
	}
}
