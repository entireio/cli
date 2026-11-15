package strategy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// trunkHookLauncher is the file Trunk 1.25.0 writes for every git hook it
// manages, byte for byte from the trunk binary. It takes the callback name from
// its own file name (${0##*/}) and finds Trunk's per-repo directory, with
// tools/trunk and temp/ in it, as the parent of the directory it runs from
// (dirname "${BASH_SOURCE[0]}"/..). Run as <hook>.pre-entire, Trunk rejects the
// callback and exits 2, which aborts the commit or push (#2673).
const trunkHookLauncher = `#!/bin/bash --norc
set -euo pipefail
if [[ ! -d .trunk ]]; then
  exit 0
fi
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." >/dev/null 2>&1 && pwd)"
hook=${0##*/}
# If this git-hook provides extra information on stdin, then save that to a file and tell trunk where to find it.
if [[ ${hook} != "pre-rebase" ]]; then
  mkdir -p "${repo_dir}/temp"
  file=$(mktemp "${repo_dir}/temp/trunk-${hook}-XXXXXX")
  trap 'rm -f ${file}' EXIT
  cat >>"${file}" || true
  export TRUNK_GIT_STDIN_FILE=${file}
fi
# Figure out if we have a tty and make that stdin, otherwise make it null.
if [[ -n ${TRUNK_TTY_PATH-} ]]; then
  export TRUNK_STDIN_IS_TTY=1
  exec <"${TRUNK_TTY_PATH}"
elif [[ -t 2 ]]; then
  export TRUNK_STDIN_IS_TTY=1
  exec </dev/tty
else
  exec </dev/null
fi

trunk=${repo_dir}/tools/trunk

TRUNK_LAUNCHER_QUIET=true "${trunk}" git-hooks callback "${0##*/}" -- "$@"
`

// trunkStub stands in for Trunk's tools/trunk: like trunk, it refuses a
// callback that is not a git hook's name. It logs the callback, its arguments
// and the stdin the launcher saved, then exits with exit.
func trunkStub(log string, exit int) string {
	return `#!/bin/sh
hook=$3
case "$hook" in
prepare-commit-msg|commit-msg|post-commit|post-rewrite|pre-push) ;;
*) echo "Unknown git-hooks callback '$hook'" >&2; exit 2 ;;
esac
shift 3
[ "$1" = -- ] && shift
printf 'trunk %s %s stdin=%s\n' "$hook" "$*" "$(cat "$TRUNK_GIT_STDIN_FILE")" >> ` + log + `
exit ` + strconv.Itoa(exit) + `
`
}

// trunkFixture lays a repo out the way Trunk does: core.hooksPath is
// <trunk dir>/hooks, the launcher finds <trunk dir>/tools/trunk, and the hooks
// run from a worktree holding .trunk. Each hook is the launcher itself or,
// with shared set, a symlink to one launcher outside the hooks dir.
type trunkFixture struct {
	*hooksFixture

	trunkDir string
	worktree string
	bin      string
	path     string // PATH for the hooks: bin, then the test's own
	log      string
}

func newTrunkFixture(t *testing.T, shared bool, trunkExit int) *trunkFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("runs POSIX shell hooks")
	}
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("Trunk's launcher runs /bin/bash")
	}
	trunkDir := t.TempDir()
	hooksDir := filepath.Join(trunkDir, "hooks")
	worktree := t.TempDir()
	for _, dir := range []string{hooksDir, filepath.Join(trunkDir, "tools"), filepath.Join(worktree, ".trunk")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tf := &trunkFixture{
		hooksFixture: newHooksFixtureAt(t, hooksDir),
		trunkDir:     trunkDir,
		worktree:     worktree,
		bin:          t.TempDir(),
		log:          filepath.Join(t.TempDir(), "log"),
	}
	tf.path = tf.bin + string(os.PathListSeparator) + os.Getenv("PATH")
	writeExec(t, filepath.Join(trunkDir, "tools", "trunk"), trunkStub(tf.log, trunkExit))
	writeExec(t, filepath.Join(tf.bin, "entire"), "#!/bin/sh\necho \"entire $3\" >> "+tf.log+"\n")
	launcher := filepath.Join(trunkDir, "launcher")
	if shared {
		writeExec(t, launcher, trunkHookLauncher)
	}
	for _, hook := range gitHookNames {
		if !shared {
			tf.write(hook, trunkHookLauncher)
			continue
		}
		if err := os.Symlink(launcher, filepath.Join(hooksDir, hook)); err != nil {
			t.Fatal(err)
		}
	}
	return tf
}

// run runs hook as git would, from the worktree, and returns its exit code and
// what Entire and Trunk logged.
func (tf *trunkFixture) run(hook, stdin string, args ...string) (int, string) {
	tf.t.Helper()
	ctx, cancel := context.WithTimeout(tf.t.Context(), hookRunTimeout)
	defer cancel()
	out, err := runHookIn(ctx, tf.worktree, stdin, tf.path, filepath.Join(tf.dir, hook), args...)
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		tf.t.Fatalf("%s: %v\n%s", hook, err, out)
	}
	data, err := os.ReadFile(tf.log)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		tf.t.Fatal(err)
	}
	if code != 0 {
		tf.t.Logf("%s exited %d:\n%s", hook, code, out)
	}
	return code, strings.TrimSpace(string(data))
}

// Entire chains every hook Trunk manages, and Trunk's launcher must still get
// its own hook's name, its arguments and git's stdin (#2673). Run as
// <hook>.pre-entire, it exits 2 and git aborts the commit or push.
func TestInstallHooks_ChainedTrunkLauncherRunsAsItsHook(t *testing.T) {
	t.Parallel()
	const refs = "refs/heads/main 1111111111111111111111111111111111111111 refs/heads/main 2222222222222222222222222222222222222222"
	msg := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	for _, layout := range []struct {
		name   string
		shared bool
	}{
		{"launcher per hook", false},
		{"hooks symlinked to one launcher", true},
	} {
		t.Run(layout.name, func(t *testing.T) {
			t.Parallel()
			tf := newTrunkFixture(t, layout.shared, 0)
			tf.installExecutable(buildHookSpecs(bareEntireHookCmd)...)
			for _, tc := range []struct {
				hook, stdin string
				args        []string
				want        string
			}{
				{"prepare-commit-msg", "", []string{msg, "message"}, "entire prepare-commit-msg\ntrunk prepare-commit-msg " + msg + " message stdin="},
				{"commit-msg", "", []string{msg}, "entire commit-msg\ntrunk commit-msg " + msg + " stdin="},
				{"post-commit", "", nil, "entire post-commit\ntrunk post-commit  stdin="},
				{"post-rewrite", "1111 2222\n", []string{"amend"}, "entire post-rewrite\ntrunk post-rewrite amend stdin=1111 2222"},
				{"pre-push", refs + "\n", []string{"origin", "url"}, "entire pre-push\ntrunk pre-push origin url stdin=" + refs},
			} {
				if err := os.Remove(tf.log); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				code, got := tf.run(tc.hook, tc.stdin, tc.args...)
				if code != 0 {
					t.Errorf("%s exited %d, want 0", tc.hook, code)
				}
				if got != tc.want {
					t.Errorf("%s ran:\n%s\nwant:\n%s", tc.hook, got, tc.want)
				}
			}
			// The launcher's EXIT trap removes the stdin copies it made.
			if left, err := os.ReadDir(filepath.Join(tf.trunkDir, "temp")); err != nil || len(left) != 0 {
				t.Errorf("Trunk's temp dir holds %v (err %v), want it emptied by the launcher's trap", left, err)
			}
		})
	}
}

// A failing Trunk check still stops the commit or push it guards.
func TestInstallHooks_ChainedTrunkLauncherFailureFailsHook(t *testing.T) {
	t.Parallel()
	tf := newTrunkFixture(t, false, 1)
	tf.installExecutable(buildHookSpecs(bareEntireHookCmd)...)
	if code, got := tf.run("pre-push", "\n", "origin", "url"); code != 1 || !strings.Contains(got, "trunk pre-push origin url") {
		t.Errorf("pre-push exited %d after running:\n%s\nwant Trunk's pre-push check to run and its exit 1", code, got)
	}
	msg := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	if code, got := tf.run("commit-msg", "", msg); code != 1 || !strings.Contains(got, "trunk commit-msg "+msg) {
		t.Errorf("commit-msg exited %d after running:\n%s\nwant Trunk's commit-msg check to run and its exit 1", code, got)
	}
}

// The launcher runs where its #!/bin/bash does, whether or not bash is on PATH,
// as with a GUI git client whose PATH lacks /bin.
func TestInstallHooks_ChainedTrunkLauncherNeedsNoBashOnPath(t *testing.T) {
	t.Parallel()
	tf := newTrunkFixture(t, false, 0)
	tf.installExecutable(buildHookSpecs(bareEntireHookCmd)...)
	// Only what the hooks and the launcher run besides bash.
	tf.onlyOnPath(t, "cat", "dirname", "grep", "mkdir", "mktemp", "rm")
	if code, got := tf.run("pre-push", "\n", "origin", "url"); code != 0 || !strings.Contains(got, "trunk pre-push origin url") {
		t.Errorf("pre-push exited %d after running:\n%s\nwant Trunk's pre-push to run", code, got)
	}
}

// A PATH that lacks grep must still source Trunk's launcher under the hook's
// own name. The chain used to identify the launcher with grep -qxF, so a
// missing grep fell through to executing it as <hook>.pre-entire, and Trunk
// exited 2.
func TestInstallHooks_ChainedTrunkLauncherNeedsNoGrepOnPath(t *testing.T) {
	t.Parallel()
	tf := newTrunkFixture(t, false, 0)
	tf.installExecutable(buildHookSpecs(bareEntireHookCmd)...)
	tf.onlyOnPath(t, "cat", "dirname", "mkdir", "mktemp", "rm")
	if code, got := tf.run("pre-push", "\n", "origin", "url"); code != 0 || !strings.Contains(got, "trunk pre-push origin url") {
		t.Errorf("pre-push exited %d after running:\n%s\nwant Trunk's pre-push to run without grep on PATH", code, got)
	}
}

// A large compiled-like backup must not be read to EOF before it runs. The
// chain used to scan the whole backup for Trunk's callback, which took several
// seconds on a 15MB executable; the scan now stops at 64KiB. A callback placed
// past that cap is therefore not seen, and the backup runs as its own program.
// This checks the cap itself rather than a wall-clock bound, which a busy
// runner could miss.
func TestInstallHooks_ChainedBackupScanStopsAt64KiB(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("runs POSIX shell hooks")
	}
	f := newHooksFixture(t)
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "log")
	writeExec(t, filepath.Join(bin, "entire"), "#!/bin/sh\n")
	hook := filepath.Join(f.dir, "post-commit")
	writeExec(t, hook, "#!/bin/sh\necho \"$0\" >> "+log+"\nexit 0\n")
	pad, err := os.OpenFile(hook, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pad.WriteString(strings.Repeat("A", 1<<20) + "\n" + trunkHookCallback + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := pad.Close(); err != nil {
		t.Fatal(err)
	}
	f.installExecutable(specFor(t, "post-commit"))
	ctx, cancel := context.WithTimeout(t.Context(), hookRunTimeout)
	defer cancel()
	if out, err := runHook(ctx, bin, filepath.Join(f.dir, "post-commit")); err != nil {
		t.Fatalf("post-commit: %v\n%s", err, out)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(data)), filepath.Join(f.dir, "post-commit"+backupSuffix); got != want {
		t.Errorf("backup ran as %s, want %s: the scan read past 64KiB and found the callback", got, want)
	}
}

// Every other saved hook is still run as its own program, under its own path.
func TestInstallHooks_ChainedHookKeepsItsOwnPath(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("runs POSIX shell hooks")
	}
	f := newHooksFixture(t)
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "log")
	writeExec(t, filepath.Join(bin, "entire"), "#!/bin/sh\n")
	// Mentions Trunk's callback without being Trunk's launcher.
	f.write("post-commit", "#!/bin/sh\n# not: git-hooks callback \"${0##*/}\" -- \"$@\"\necho \"$0\" >> "+log+"\n")
	f.installExecutable(specFor(t, "post-commit"))
	if out, err := runHook(t.Context(), bin, filepath.Join(f.dir, "post-commit")); err != nil {
		t.Fatalf("post-commit: %v\n%s", err, out)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(data)), filepath.Join(f.dir, "post-commit"+backupSuffix); got != want {
		t.Errorf("saved hook ran as %s, want %s", got, want)
	}
}

// pre3051ChainCall is the chain Entire wrote before it sourced Trunk's
// launcher under the hook's own name. Frozen so this test still plants that
// text if chainCall changes again.
func pre3051ChainCall(hookName, redirect string) string {
	backup := hookName + backupSuffix
	keep := hookName + keepSuffix
	guard := "ENTIRE_CHAINING_" + strings.ToUpper(strings.ReplaceAll(hookName, "-", "_"))
	return fmt.Sprintf(`%s
_entire_hook_dir="$(dirname "$0")"
if [ -z "${%s:-}" ]; then
    %s=1
    export %s
    if [ -n "${PRE_COMMIT_RUNNING_LEGACY:-}" ] && grep -q '^%s' "$_entire_hook_dir/%s" 2>/dev/null; then
        if [ -x "$_entire_hook_dir/%s" ]; then
            "$_entire_hook_dir/%s" "$@"%s
        fi
    elif [ -x "$_entire_hook_dir/%s" ]; then
        "$_entire_hook_dir/%s" "$@"%s
    fi
fi
`, chainComment, guard, guard, guard, preCommitSignatures[0], backup, keep, keep, redirect, backup, backup, redirect)
}

// pre3051ChainedContent is today's hook body with the pre-sourcing chain, the
// text an existing install still has until something rewrites it.
func pre3051ChainedContent(baseContent, hookName string) string {
	return frozenChainedContent(baseContent, hookName, pre3051ChainCall)
}

// frozenChainedContent builds a hook body the way generateChainedContent does,
// with chain in place of chainCall, so a test can plant a chain Entire wrote
// before.
func frozenChainedContent(baseContent, hookName string, chain func(hookName, redirect string) string) string {
	if hookName == postRewriteHook {
		const original = `hooks git post-rewrite "$1" 2>/dev/null || true`
		const replacement = `hooks git post-rewrite "$1" < "$_entire_stdin" 2>/dev/null || true`
		replayPrefix := `#!/bin/sh
_entire_stdin="$(mktemp "${TMPDIR:-/tmp}/entire-post-rewrite.XXXXXX")"
cat > "$_entire_stdin"
trap 'rm -f "$_entire_stdin"' EXIT
`
		body := strings.TrimPrefix(baseContent, "#!/bin/sh\n")
		body = strings.Replace(body, original, replacement, 1)
		return replayPrefix + body + "\n" + chain(postRewriteHook, ` < "$_entire_stdin"`)
	}
	if hookName == prePushHook {
		baseContent += "_entire_status=$?\nif [ \"$_entire_status\" -ne 0 ]; then exit \"$_entire_status\"; fi\n"
	}
	return baseContent + chain(hookName, "")
}

func plantPre3051TrunkChain(t *testing.T, tf *trunkFixture) {
	t.Helper()
	plantChain(t, tf, pre3051ChainedContent)
}

// plantChain moves each installed hook in tf to its backup and writes the
// hook chained by chain in its place.
func plantChain(t *testing.T, tf *trunkFixture, chain func(baseContent, hookName string) string) {
	t.Helper()
	for _, spec := range buildHookSpecs(bareEntireHookCmd) {
		src := filepath.Join(tf.dir, spec.name)
		dst := filepath.Join(tf.dir, spec.name+backupSuffix)
		if err := os.Rename(src, dst); err != nil {
			t.Fatal(err)
		}
		tf.write(spec.name, chain(spec.content, spec.name))
	}
}

// grepTrunkChainCall is the chain Entire wrote when it identified Trunk's
// launcher with grep -qxF (#3051's final form, which also tests [ -f ]).
// Frozen so this test still plants that text if chainCall changes again.
func grepTrunkChainCall(hookName, redirect string) string {
	backup := hookName + backupSuffix
	keep := hookName + keepSuffix
	guard := "ENTIRE_CHAINING_" + strings.ToUpper(strings.ReplaceAll(hookName, "-", "_"))
	return fmt.Sprintf(`%s
_entire_hook_dir="$(dirname "$0")"
if [ -z "${%s:-}" ]; then
    %s=1
    export %s
    if [ -n "${PRE_COMMIT_RUNNING_LEGACY:-}" ] && grep -q '^%s' "$_entire_hook_dir/%s" 2>/dev/null; then
        if [ -x "$_entire_hook_dir/%s" ]; then
            "$_entire_hook_dir/%s" "$@"%s
        fi
    elif [ -f "$_entire_hook_dir/%s" ] && [ -x "$_entire_hook_dir/%s" ]; then
        if grep -qxF %s "$_entire_hook_dir/%s" 2>/dev/null; then
            /bin/bash --norc -c '_entire_saved_hook=$1; shift; . "$_entire_saved_hook"' "$_entire_hook_dir/%s" "$_entire_hook_dir/%s" "$@"%s
        else
            "$_entire_hook_dir/%s" "$@"%s
        fi
    fi
fi
`, chainComment, guard, guard, guard, preCommitSignatures[0], backup, keep, keep, redirect,
		backup, backup, shellQuote(trunkHookCallback), backup, hookName, backup, redirect, backup, redirect)
}

func grepTrunkChainedContent(baseContent, hookName string) string {
	return frozenChainedContent(baseContent, hookName, grepTrunkChainCall)
}

func plantGrepTrunkChain(t *testing.T, tf *trunkFixture) {
	t.Helper()
	plantChain(t, tf, grepTrunkChainedContent)
}

// writeGrepChain writes the managed hooks into dir with pre-push on the
// grep-based chain, and backup as pre-push's saved hook.
func writeGrepChain(t *testing.T, dir, backup string) {
	t.Helper()
	writeChain(t, dir, grepTrunkChainedContent, backup)
}

// writeChain writes the managed hooks into dir with pre-push chained by chain
// (generateChainedContent or a frozen earlier form), and backup as pre-push's
// saved hook.
func writeChain(t *testing.T, dir string, chain func(baseContent, hookName string) string, backup string) {
	t.Helper()
	writeChainedHooks(t, dir, chain)
	if err := os.WriteFile(filepath.Join(dir, prePushHook+backupSuffix), []byte(backup), 0o755); err != nil {
		t.Fatal(err)
	}
}

// writeChainedHooks writes the managed hooks into dir with pre-push chained by
// chain, and no saved hook.
func writeChainedHooks(t *testing.T, dir string, chain func(baseContent, hookName string) string) {
	t.Helper()
	for _, spec := range buildHookSpecs(bareEntireHookCmd) {
		content := spec.content
		if spec.name == prePushHook {
			content = chain(spec.content, spec.name)
		}
		if err := os.WriteFile(filepath.Join(dir, spec.name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// editPrePush replaces old with edited in dir's pre-push hook, as a user
// editing it by hand would.
func editPrePush(t *testing.T, dir, old, edited string) {
	t.Helper()
	hook := filepath.Join(dir, prePushHook)
	data, err := os.ReadFile(hook)
	if err != nil {
		t.Fatal(err)
	}
	content := strings.Replace(string(data), old, edited, 1)
	if content == string(data) {
		t.Fatalf("%s does not contain %q", prePushHook, old)
	}
	if err := os.WriteFile(hook, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

// wantHookState fails the test unless the hooks in dir read want.
func wantHookState(t *testing.T, dir string, want GitHookState, what string) {
	t.Helper()
	if got := gitHookStateInHooksDir(dir); got != want {
		t.Errorf("%s = %s, want %s", what, hookStateName(got), hookStateName(want))
	}
}

func hookStateName(state GitHookState) string {
	switch state {
	case GitHooksAbsent:
		return "GitHooksAbsent"
	case GitHooksCurrent:
		return "GitHooksCurrent"
	case GitHooksOutdated:
		return "GitHooksOutdated"
	}
	return fmt.Sprintf("GitHookState(%d)", int(state))
}

// onlyOnPath leaves the hooks a PATH of tf.bin holding just tools, linked to
// the test's own copies, plus the stub entire and trunk already there. It skips
// the test when one of tools is missing.
func (tf *trunkFixture) onlyOnPath(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		target, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("hooks need %s", tool)
		}
		if err := os.Symlink(target, filepath.Join(tf.bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	tf.path = tf.bin
}

func (tf *trunkFixture) useAsGitHooks(t *testing.T) {
	t.Helper()
	testutil.RunGit(t, tf.worktree, "init")
	testutil.RunGit(t, tf.worktree, "config", "core.hooksPath", tf.dir)
	paths.ClearWorktreeRootCache()
	t.Chdir(tf.worktree)
}

// An existing install that still has the pre-sourcing Trunk chain must read
// Outdated so ReinstallGitHooks (and EnsureSetup) rewrites it without the
// user running entire enable again. After the rewrite, pre-push reaches
// Trunk as pre-push, not as pre-push.pre-entire.
func TestReinstallGitHooks_Pre3051TrunkChainIsOutdated(t *testing.T) {
	tf := newTrunkFixture(t, false, 0)
	plantPre3051TrunkChain(t, tf)
	tf.useAsGitHooks(t)

	if got := CheckGitHookState(t.Context()); got != GitHooksOutdated {
		t.Fatalf("CheckGitHookState = %v, want GitHooksOutdated", got)
	}
	if _, err := ReinstallGitHooks(t.Context()); err != nil {
		t.Fatalf("ReinstallGitHooks: %v", err)
	}
	if got := CheckGitHookState(t.Context()); got != GitHooksCurrent {
		t.Fatalf("after ReinstallGitHooks, CheckGitHookState = %v, want GitHooksCurrent", got)
	}
	const refs = "refs/heads/main 1111111111111111111111111111111111111111 refs/heads/main 2222222222222222222222222222222222222222"
	if code, got := tf.run("pre-push", refs+"\n", "origin", "url"); code != 0 || !strings.Contains(got, "trunk pre-push origin url") {
		t.Errorf("pre-push exited %d after running:\n%s\nwant Trunk to run as pre-push", code, got)
	}
}

// Lines the user added to Entire's own hook, before or after the chain, must
// survive the upgrade: ReinstallGitHooks replaces only the outdated chain
// block, since a hook carrying Entire's marker is otherwise rewritten without
// a backup.
func TestReinstallGitHooks_OutdatedTrunkChainKeepsUserLines(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plant func(*testing.T, *trunkFixture)
	}{
		{"pre-sourcing chain", plantPre3051TrunkChain},
		{"grep chain", plantGrepTrunkChain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tf := newTrunkFixture(t, false, 0)
			tc.plant(t, tf)
			hook := filepath.Join(tf.dir, prePushHook)
			data, err := os.ReadFile(hook)
			if err != nil {
				t.Fatal(err)
			}
			before := "echo user-before >> " + tf.log + "\n"
			after := "echo user-after >> " + tf.log + "\n"
			shebang, rest, _ := strings.Cut(string(data), "\n")
			writeExec(t, hook, shebang+"\n"+before+rest+after)
			tf.useAsGitHooks(t)

			if got := CheckGitHookState(t.Context()); got != GitHooksOutdated {
				t.Fatalf("CheckGitHookState = %s, want GitHooksOutdated", hookStateName(got))
			}
			if _, err := ReinstallGitHooks(t.Context()); err != nil {
				t.Fatalf("ReinstallGitHooks: %v", err)
			}
			if got := CheckGitHookState(t.Context()); got != GitHooksCurrent {
				t.Fatalf("after ReinstallGitHooks, CheckGitHookState = %s, want GitHooksCurrent", hookStateName(got))
			}
			data, err = os.ReadFile(hook)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{before, after, chainCall(prePushHook, "")} {
				if !strings.Contains(string(data), want) {
					t.Fatalf("pre-push after ReinstallGitHooks lacks %q:\n%s", want, data)
				}
			}
			const refs = "refs/heads/main 1111111111111111111111111111111111111111 refs/heads/main 2222222222222222222222222222222222222222"
			code, got := tf.run("pre-push", refs+"\n", "origin", "url")
			if code != 0 || !strings.Contains(got, "user-before") || !strings.Contains(got, "trunk pre-push origin url") || !strings.Contains(got, "user-after") {
				t.Errorf("pre-push exited %d after running:\n%s\nwant the user's lines and Trunk as pre-push", code, got)
			}
		})
	}
}

// An existing install that still identifies Trunk's launcher with grep -qxF
// must read Outdated so ReinstallGitHooks rewrites it. After the rewrite,
// pre-push reaches Trunk as pre-push even when grep is not on PATH.
func TestReinstallGitHooks_GrepTrunkChainIsOutdated(t *testing.T) {
	tf := newTrunkFixture(t, false, 0)
	plantGrepTrunkChain(t, tf)
	tf.useAsGitHooks(t)

	if got := CheckGitHookState(t.Context()); got != GitHooksOutdated {
		t.Fatalf("CheckGitHookState = %v, want GitHooksOutdated", got)
	}
	if _, err := ReinstallGitHooks(t.Context()); err != nil {
		t.Fatalf("ReinstallGitHooks: %v", err)
	}
	if got := CheckGitHookState(t.Context()); got != GitHooksCurrent {
		t.Fatalf("after ReinstallGitHooks, CheckGitHookState = %v, want GitHooksCurrent", got)
	}
	tf.onlyOnPath(t, "cat", "dirname", "mkdir", "mktemp", "rm")
	const refs = "refs/heads/main 1111111111111111111111111111111111111111 refs/heads/main 2222222222222222222222222222222222222222"
	if code, got := tf.run("pre-push", refs+"\n", "origin", "url"); code != 0 || !strings.Contains(got, "trunk pre-push origin url") {
		t.Errorf("pre-push exited %d after running:\n%s\nwant Trunk to run as pre-push without grep on PATH", code, got)
	}
}

// With Trunk's hooks symlinked to one launcher, the backups are links out of
// the hooks dir, which the Outdated check does not follow. Such an install
// keeps the old chain until entire enable, which rewrites every hook.
func TestInstallGitHook_Pre3051TrunkChainToSymlinkedLauncher(t *testing.T) {
	tf := newTrunkFixture(t, true, 0)
	plantPre3051TrunkChain(t, tf)
	tf.useAsGitHooks(t)

	if got := CheckGitHookState(t.Context()); got != GitHooksCurrent {
		t.Fatalf("CheckGitHookState = %v, want GitHooksCurrent (symlinked backups are not read)", got)
	}
	if _, err := InstallGitHook(t.Context(), true, false); err != nil {
		t.Fatalf("InstallGitHook: %v", err)
	}
	const refs = "refs/heads/main 1111111111111111111111111111111111111111 refs/heads/main 2222222222222222222222222222222222222222"
	if code, got := tf.run("pre-push", refs+"\n", "origin", "url"); code != 0 || !strings.Contains(got, "trunk pre-push origin url") {
		t.Errorf("pre-push exited %d after running:\n%s\nwant Trunk to run as pre-push", code, got)
	}
}

// writeOldChain writes the managed hooks into dir with pre-push still on the
// pre-sourcing chain, and backup as pre-push's saved hook.
func writeOldChain(t *testing.T, dir, backup string) {
	t.Helper()
	writeChain(t, dir, pre3051ChainedContent, backup)
}

// A chained hook that is not Trunk's launcher must stay Current: rewriting it
// would drop lines the user added, because a hook carrying Entire's marker is
// replaced without a backup.
func TestCheckGitHookState_TrunkChainOutdatedIsSpecific(t *testing.T) {
	t.Parallel()

	t.Run("ordinary backup", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeOldChain(t, dir, "#!/bin/sh\necho user hook\n")
		wantHookState(t, dir, GitHooksCurrent, "old chain to a non-Trunk backup")
	})

	t.Run("callback only in a comment", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeOldChain(t, dir, "#!/bin/sh\n# "+trunkHookCallback+"\n")
		wantHookState(t, dir, GitHooksCurrent, "backup that only mentions the callback in a comment")
	})

	t.Run("current chain with Trunk backup", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeChain(t, dir, generateChainedContent, trunkHookLauncher)
		wantHookState(t, dir, GitHooksCurrent, "current chain with a Trunk backup")
	})

	t.Run("grep chain with Trunk backup", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeGrepChain(t, dir, trunkHookLauncher)
		wantHookState(t, dir, GitHooksOutdated, "grep chain to a Trunk backup")
	})

	t.Run("grep chain edited by hand", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeGrepChain(t, dir, trunkHookLauncher)
		probe := grepTrunkProbe(prePushHook + backupSuffix)
		editPrePush(t, dir, probe, strings.Replace(probe, "grep -qxF", "grep -qF", 1))
		wantHookState(t, dir, GitHooksCurrent, "grep chain the user edited")
	})

	t.Run("grep chain with ordinary backup", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeGrepChain(t, dir, "#!/bin/sh\necho user hook\n")
		wantHookState(t, dir, GitHooksCurrent, "grep chain to a non-Trunk backup")
	})

	t.Run("chain repaired by hand", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeOldChain(t, dir, trunkHookLauncher)
		backup := prePushHook + backupSuffix
		// What a user might have written to get past #2673 before this fix:
		// source the launcher with $0 set, no grep probe.
		editPrePush(t, dir, preSourcingBackupCall(backup),
			`    elif [ -x "$_entire_hook_dir/`+backup+`" ]; then
        /bin/bash -c '. "$1"' "$_entire_hook_dir/`+prePushHook+`" "$_entire_hook_dir/`+backup+`" "$@"`)
		wantHookState(t, dir, GitHooksCurrent, "chain the user edited to source Trunk's launcher")
	})

	// The backup is not followed out of the hooks dir to read the launcher it
	// links to: git hooks are not a symlink exception. That install picks up
	// the sourcing chain on entire enable instead.
	t.Run("symlinked Trunk launcher is not followed", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		launcher := filepath.Join(t.TempDir(), "launcher")
		if err := os.WriteFile(launcher, []byte(trunkHookLauncher), 0o755); err != nil {
			t.Fatal(err)
		}
		writeChainedHooks(t, dir, pre3051ChainedContent)
		if err := os.Symlink(launcher, filepath.Join(dir, prePushHook+backupSuffix)); err != nil {
			t.Skipf("symlink not supported: %v", err)
		}
		wantHookState(t, dir, GitHooksCurrent, "old chain to a symlinked Trunk launcher")
	})
}
