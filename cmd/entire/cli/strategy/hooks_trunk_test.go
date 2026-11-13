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
	"time"

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
	for _, tool := range []string{"cat", "dirname", "grep", "mkdir", "mktemp", "rm"} {
		target, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("hooks need %s", tool)
		}
		if err := os.Symlink(target, filepath.Join(tf.bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	tf.path = tf.bin
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
	for _, tool := range []string{"cat", "dirname", "mkdir", "mktemp", "rm"} {
		target, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("hooks need %s", tool)
		}
		if err := os.Symlink(target, filepath.Join(tf.bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	tf.path = tf.bin
	if code, got := tf.run("pre-push", "\n", "origin", "url"); code != 0 || !strings.Contains(got, "trunk pre-push origin url") {
		t.Errorf("pre-push exited %d after running:\n%s\nwant Trunk's pre-push to run without grep on PATH", code, got)
	}
}

// A large compiled-like backup must still run promptly. The chain used to
// identify Trunk's launcher by reading the backup to EOF, which took several
// seconds on a 15MB executable; the scan is now capped at 64KiB.
func TestInstallHooks_ChainedLargeBackupRunsPromptly(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("runs POSIX shell hooks")
	}
	f := newHooksFixture(t)
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "log")
	writeExec(t, filepath.Join(bin, "entire"), "#!/bin/sh\n")
	hook := filepath.Join(f.dir, "post-commit")
	writeExec(t, hook, "#!/bin/sh\necho ran >> "+log+"\nexit 0\n")
	pad, err := os.OpenFile(hook, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	chunk := strings.Repeat("A", 1<<20)
	for range 8 {
		if _, err := pad.WriteString(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := pad.Close(); err != nil {
		t.Fatal(err)
	}
	f.installExecutable(specFor(t, "post-commit"))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if out, err := runHook(ctx, bin, filepath.Join(f.dir, "post-commit")); err != nil {
		t.Fatalf("post-commit: %v\n%s", err, out)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != "ran" {
		t.Errorf("backup ran %q, want ran", got)
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
		return replayPrefix + body + "\n" + pre3051ChainCall(postRewriteHook, ` < "$_entire_stdin"`)
	}
	if hookName == prePushHook {
		baseContent += "_entire_status=$?\nif [ \"$_entire_status\" -ne 0 ]; then exit \"$_entire_status\"; fi\n"
	}
	return baseContent + pre3051ChainCall(hookName, "")
}

func plantPre3051TrunkChain(t *testing.T, tf *trunkFixture) {
	t.Helper()
	for _, spec := range buildHookSpecs(bareEntireHookCmd) {
		src := filepath.Join(tf.dir, spec.name)
		dst := filepath.Join(tf.dir, spec.name+backupSuffix)
		if err := os.Rename(src, dst); err != nil {
			t.Fatal(err)
		}
		tf.write(spec.name, pre3051ChainedContent(spec.content, spec.name))
	}
}

// grepTrunkChainCall is the chain Entire wrote when it identified Trunk's
// launcher with grep -qxF. Frozen so this test still plants that text if
// chainCall changes again.
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
    elif [ -x "$_entire_hook_dir/%s" ]; then
        if grep -qxF %s "$_entire_hook_dir/%s" 2>/dev/null; then
            /bin/bash --norc -c '_entire_saved_hook=$1; shift; . "$_entire_saved_hook"' "$_entire_hook_dir/%s" "$_entire_hook_dir/%s" "$@"%s
        else
            "$_entire_hook_dir/%s" "$@"%s
        fi
    fi
fi
`, chainComment, guard, guard, guard, preCommitSignatures[0], backup, keep, keep, redirect,
		backup, shellQuote(trunkHookCallback), backup, hookName, backup, redirect, backup, redirect)
}

func grepTrunkChainedContent(baseContent, hookName string) string {
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
		return replayPrefix + body + "\n" + grepTrunkChainCall(postRewriteHook, ` < "$_entire_stdin"`)
	}
	if hookName == prePushHook {
		baseContent += "_entire_status=$?\nif [ \"$_entire_status\" -ne 0 ]; then exit \"$_entire_status\"; fi\n"
	}
	return baseContent + grepTrunkChainCall(hookName, "")
}

func plantGrepTrunkChain(t *testing.T, tf *trunkFixture) {
	t.Helper()
	for _, spec := range buildHookSpecs(bareEntireHookCmd) {
		src := filepath.Join(tf.dir, spec.name)
		dst := filepath.Join(tf.dir, spec.name+backupSuffix)
		if err := os.Rename(src, dst); err != nil {
			t.Fatal(err)
		}
		tf.write(spec.name, grepTrunkChainedContent(spec.content, spec.name))
	}
}

func writeGrepChain(t *testing.T, dir, hook, backup string) {
	t.Helper()
	for _, spec := range buildHookSpecs(bareEntireHookCmd) {
		content := spec.content
		if spec.name == hook {
			content = grepTrunkChainedContent(spec.content, spec.name)
		}
		if err := os.WriteFile(filepath.Join(dir, spec.name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, hook+backupSuffix), []byte(backup), 0o755); err != nil {
		t.Fatal(err)
	}
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
	for _, tool := range []string{"cat", "dirname", "mkdir", "mktemp", "rm"} {
		target, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("hooks need %s", tool)
		}
		if err := os.Symlink(target, filepath.Join(tf.bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	tf.path = tf.bin
	const refs = "refs/heads/main 1111111111111111111111111111111111111111 refs/heads/main 2222222222222222222222222222222222222222"
	if code, got := tf.run("pre-push", refs+"\n", "origin", "url"); code != 0 || !strings.Contains(got, "trunk pre-push origin url") {
		t.Errorf("pre-push exited %d after running:\n%s\nwant Trunk to run as pre-push without grep on PATH", code, got)
	}
}

func writeOldChain(t *testing.T, dir, hook, backup string) {
	t.Helper()
	for _, spec := range buildHookSpecs(bareEntireHookCmd) {
		content := spec.content
		if spec.name == hook {
			content = pre3051ChainedContent(spec.content, spec.name)
		}
		if err := os.WriteFile(filepath.Join(dir, spec.name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, hook+backupSuffix), []byte(backup), 0o755); err != nil {
		t.Fatal(err)
	}
}

// A chained hook that is not Trunk's launcher must stay Current: rewriting it
// would drop lines the user added, because a hook carrying Entire's marker is
// replaced without a backup.
func TestCheckGitHookState_TrunkChainOutdatedIsSpecific(t *testing.T) {
	t.Parallel()

	t.Run("ordinary backup", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeOldChain(t, dir, prePushHook, "#!/bin/sh\necho user hook\n")
		if got := gitHookStateInHooksDir(dir); got != GitHooksCurrent {
			t.Errorf("old chain to a non-Trunk backup = %v, want GitHooksCurrent", got)
		}
	})

	t.Run("callback only in a comment", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeOldChain(t, dir, prePushHook, "#!/bin/sh\n# "+trunkHookCallback+"\n")
		if got := gitHookStateInHooksDir(dir); got != GitHooksCurrent {
			t.Errorf("backup that only mentions the callback in a comment = %v, want GitHooksCurrent", got)
		}
	})

	t.Run("current chain with Trunk backup", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		for _, spec := range buildHookSpecs(bareEntireHookCmd) {
			content := spec.content
			if spec.name == prePushHook {
				content = generateChainedContent(spec.content, spec.name)
			}
			if err := os.WriteFile(filepath.Join(dir, spec.name), []byte(content), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, prePushHook+backupSuffix), []byte(trunkHookLauncher), 0o755); err != nil {
			t.Fatal(err)
		}
		if got := gitHookStateInHooksDir(dir); got != GitHooksCurrent {
			t.Errorf("current chain with a Trunk backup = %v, want GitHooksCurrent", got)
		}
	})

	t.Run("grep chain with Trunk backup", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeGrepChain(t, dir, prePushHook, trunkHookLauncher)
		if got := gitHookStateInHooksDir(dir); got != GitHooksOutdated {
			t.Errorf("grep chain to a Trunk backup = %v, want GitHooksOutdated", got)
		}
	})

	t.Run("grep chain with ordinary backup", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeGrepChain(t, dir, prePushHook, "#!/bin/sh\necho user hook\n")
		if got := gitHookStateInHooksDir(dir); got != GitHooksCurrent {
			t.Errorf("grep chain to a non-Trunk backup = %v, want GitHooksCurrent", got)
		}
	})

	t.Run("symlinked Trunk launcher", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		launcher := filepath.Join(t.TempDir(), "launcher")
		if err := os.WriteFile(launcher, []byte(trunkHookLauncher), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, spec := range buildHookSpecs(bareEntireHookCmd) {
			content := spec.content
			if spec.name == prePushHook {
				content = pre3051ChainedContent(spec.content, spec.name)
			}
			if err := os.WriteFile(filepath.Join(dir, spec.name), []byte(content), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(launcher, filepath.Join(dir, prePushHook+backupSuffix)); err != nil {
			t.Skipf("symlink not supported: %v", err)
		}
		if got := gitHookStateInHooksDir(dir); got != GitHooksOutdated {
			t.Errorf("old chain to a symlinked Trunk launcher = %v, want GitHooksOutdated", got)
		}
	})
}
