package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/spf13/cobra"
)

// entireDirCheckExemptions is the full set of commands allowed to run when
// `.entire` is not a real directory, and why each one is allowed. Guarded is
// the default; a command belongs here only when it neither reads nor writes
// anything under `.entire`.
//
// Keys are command paths as cobra reports them, and the annotation is
// inherited, so a group root covers its subcommands. TestEntireDirCheckExemptions
// fails on an exemption missing from this map and on an entry that no longer
// matches the tree, which is what stops an exemption being added quietly to
// make a failing test pass.
//
// Deliberately absent: `help` and `agent-help`. Someone asking what they can do
// in this repo is told the repo is broken rather than handed a working command
// list. `entire <command> --help` is unaffected either way — cobra returns
// flag.ErrHelp before it runs any PersistentPreRunE.
var entireDirCheckExemptions = map[string]string{
	"entire auth":       "reads ~/.config/entire and the OS keyring, never the repo",
	"entire login":      "control-plane login; user-level credentials only",
	"entire logout":     "control-plane logout; user-level credentials only",
	"entire org":        "control-plane only",
	"entire project":    "control-plane only",
	"entire repo":       "control-plane only; git content operations are out of scope",
	"entire grant":      "control-plane only",
	"entire api":        "authenticated passthrough; no local state",
	"entire plugin":     "managed installs live under the per-user dirs",
	"entire version":    "prints build information; no repo state",
	"entire labs":       "prints a static list of experimental workflows",
	"entire completion": "prints a shell script; users eval it from a shell rc, which a broken repo must not break",
	"entire doctor":     "diagnostic; has to run ON a broken repo in order to report it",
}

// collectExemptions walks the tree and returns every command path whose own
// annotations carry the exemption. Children are excluded because they inherit.
func collectExemptions(root *cobra.Command) []string {
	var found []string
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Annotations[skipEntireDirCheckAnnotation] == skipEntireDirCheckEnabled {
			found = append(found, c.CommandPath())
			return
		}
		for _, child := range c.Commands() {
			walk(child)
		}
	}
	walk(root)
	sort.Strings(found)
	return found
}

func TestEntireDirCheckExemptions(t *testing.T) {
	t.Parallel()

	got := collectExemptions(NewRootCmd())

	want := make([]string, 0, len(entireDirCheckExemptions))
	for path := range entireDirCheckExemptions {
		want = append(want, path)
	}
	sort.Strings(want)

	gotSet := make(map[string]bool, len(got))
	for _, path := range got {
		gotSet[path] = true
	}

	for _, path := range got {
		if _, ok := entireDirCheckExemptions[path]; !ok {
			t.Errorf("%q is exempt from the .entire check but is not in entireDirCheckExemptions.\n"+
				"Exempting a command means it runs in a repo whose .entire is a file or a symlink.\n"+
				"Add an entry saying why it needs no access to .entire, or drop the annotation.", path)
		}
	}
	for _, path := range want {
		if !gotSet[path] {
			t.Errorf("entireDirCheckExemptions lists %q, but no such command carries the exemption annotation.\n"+
				"Remove the stale entry, or restore the annotation if it was dropped by accident.", path)
		}
	}
	for path, reason := range entireDirCheckExemptions {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("exemption for %q has no reason", path)
		}
	}
}

// The annotation is set on group roots, so inheritance is what actually exempts
// most commands. Cobra does not propagate Annotations, hence the parent walk.
func TestSkipsEntireDirCheck_InheritedFromParent(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()

	exempt, _, err := root.Find([]string{"doctor", "logs"})
	if err != nil {
		t.Fatalf("find doctor logs: %v", err)
	}
	if !skipsEntireDirCheck(exempt) {
		t.Errorf("%q should inherit the exemption from `entire doctor`", exempt.CommandPath())
	}

	guarded, _, err := root.Find([]string{"session", "list"})
	if err != nil {
		t.Fatalf("find session list: %v", err)
	}
	if skipsEntireDirCheck(guarded) {
		t.Errorf("%q must be guarded", guarded.CommandPath())
	}
}

// exemptFromEntireDirCheck must not discard annotations a command already
// carries — several commands are also annotated for agent-help.
func TestExemptFromEntireDirCheck_PreservesExistingAnnotations(t *testing.T) {
	t.Parallel()

	cmd := &cobra.Command{
		Use:         "thing",
		Annotations: map[string]string{agentHelpAnnotation: agentHelpAnnotationEnabled},
	}
	exemptFromEntireDirCheck(cmd)

	if !skipsEntireDirCheck(cmd) {
		t.Error("command is not exempt after exemptFromEntireDirCheck")
	}
	if cmd.Annotations[agentHelpAnnotation] != agentHelpAnnotationEnabled {
		t.Error("exemptFromEntireDirCheck dropped a pre-existing annotation")
	}
}

// symlinkEntireDir replaces the repo's `.entire` with a symlink to a directory
// outside it, and returns the target. A symlink to a valid directory is the
// case that a Stat-based check would wave through.
func symlinkEntireDir(t *testing.T, repoDir string) string {
	t.Helper()

	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(repoDir, paths.EntireDir)); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	return target
}

// newRepoWithSymlinkedEntireDir builds an isolated repo whose `.entire` is a
// symlink, points the process at it, and returns the directory the symlink
// points at. Not parallel: t.Chdir is process-global, and the CWD is what the
// worktree-root resolution reads.
func newRepoWithSymlinkedEntireDir(t *testing.T) (target string) {
	t.Helper()

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	target = symlinkEntireDir(t, repoDir)

	t.Chdir(repoDir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	return target
}

func TestCheckEntireDirBeforeRun_GuardedCommandFails(t *testing.T) {
	newRepoWithSymlinkedEntireDir(t)

	var stderr bytes.Buffer
	cmd := &cobra.Command{Use: "status"}
	cmd.SetContext(context.Background())
	cmd.SetErr(&stderr)

	safe, err := checkEntireDirBeforeRun(cmd)
	if err == nil {
		t.Fatal("checkEntireDirBeforeRun returned nil error for a guarded command")
	}
	if safe {
		t.Error("checkEntireDirBeforeRun reported the path safe")
	}
	if !errors.Is(err, paths.ErrEntireDirNotDirectory) {
		t.Errorf("error does not wrap ErrEntireDirNotDirectory: %v", err)
	}

	// SilentError, because the remedy is printed here rather than by main.go.
	var silent *SilentError
	if !errors.As(err, &silent) {
		t.Errorf("error is not a SilentError, so main.go will print it a second time: %v", err)
	}

	msg := stderr.String()
	if !strings.Contains(msg, "symbolic link") {
		t.Errorf("message does not say what was found:\n%s", msg)
	}
	if !strings.Contains(msg, "entire doctor") {
		t.Errorf("message does not point at a command that still works:\n%s", msg)
	}
}

func TestCheckEntireDirBeforeRun_ExemptCommandRunsSilentlyAndIsNotSafe(t *testing.T) {
	newRepoWithSymlinkedEntireDir(t)

	var stderr bytes.Buffer
	cmd := exemptFromEntireDirCheck(&cobra.Command{Use: "version"})
	cmd.SetContext(context.Background())
	cmd.SetErr(&stderr)

	safe, err := checkEntireDirBeforeRun(cmd)
	if err != nil {
		t.Fatalf("exempt command was stopped: %v", err)
	}
	// Not safe: the command runs, but it must not go on to build a logger under
	// .entire, which is a write through the symlink we are refusing.
	if safe {
		t.Error("checkEntireDirBeforeRun reported the path safe for an exempt command")
	}
	if stderr.Len() != 0 {
		t.Errorf("exempt command emitted output; doctor owns the reporting:\n%s", stderr.String())
	}
}

func TestCheckEntireDirBeforeRun_HealthyRepoIsSafe(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	if err := os.Mkdir(filepath.Join(repoDir, paths.EntireDir), 0o750); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoDir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	cmd := &cobra.Command{Use: "status"}
	cmd.SetContext(context.Background())

	safe, err := checkEntireDirBeforeRun(cmd)
	if err != nil {
		t.Fatalf("checkEntireDirBeforeRun: %v", err)
	}
	if !safe {
		t.Error("a real .entire directory was not reported safe")
	}
}

// The log sink lives under .entire, so building a logger is a write through the
// same path. Without this, an exempt command creates .entire/logs in whatever
// directory the symlink points at.
func TestNewLogger_RefusesSymlinkedEntireDir(t *testing.T) {
	target := newRepoWithSymlinkedEntireDir(t)

	if _, err := newLogger(context.Background()); err == nil {
		t.Fatal("newLogger built a logger through a symlinked .entire")
	}

	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("newLogger created %d entries in the symlink target", len(entries))
	}
}

// doctor is exempt so that it can run on a broken repo, which is only useful if
// it says what is wrong. The report lands in the doctor group's persistent
// pre-run, ahead of doctor's own PreRunE (which reads redaction settings
// through .entire) and ahead of `doctor logs`/`doctor bundle` (which read
// .entire/logs).
func TestDoctor_ReportsBrokenEntireDir(t *testing.T) {
	newRepoWithSymlinkedEntireDir(t)

	for _, args := range [][]string{{"doctor"}, {"doctor", "logs"}, {"doctor", "bundle"}} {
		name := strings.Join(args, " ")
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			root := NewRootCmd()
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs(args)

			err := root.ExecuteContext(context.Background())
			if err == nil {
				t.Fatalf("`entire %s` succeeded on a repo with a symlinked .entire", name)
			}
			if !errors.Is(err, paths.ErrEntireDirNotDirectory) {
				t.Fatalf("error does not wrap ErrEntireDirNotDirectory: %v", err)
			}

			report := stdout.String() + stderr.String()
			if !strings.Contains(report, "symbolic link") {
				t.Errorf("report does not say what was found:\n%s", report)
			}
			if !strings.Contains(report, paths.EntireDir) {
				t.Errorf("report does not name %s:\n%s", paths.EntireDir, report)
			}
		})
	}
}

func TestDoctor_HealthyEntireDirIsNotReported(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	if err := os.Mkdir(filepath.Join(repoDir, paths.EntireDir), 0o750); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoDir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	cmd := &cobra.Command{Use: "doctor"}
	cmd.SetContext(context.Background())
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)

	if err := reportBrokenEntireDir(cmd); err != nil {
		t.Fatalf("reportBrokenEntireDir on a healthy repo: %v", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("healthy repo produced a report:\n%s", stdout.String())
	}
}

// The root pre-run does not cover everything. External plugins are dispatched
// before cobra ever runs, and exempt commands reach settings through the
// post-run telemetry path. Settings are read FROM .entire, so loading them is
// the one operation every such caller has in common — checking here means the
// guard runs at least once even where the pre-run did not. Callers that already
// went through the pre-run just pay a second Lstat.
func TestLoadEntireSettings_RefusesSymlinkedEntireDir(t *testing.T) {
	newRepoWithSymlinkedEntireDir(t)

	_, err := LoadEntireSettings(context.Background())
	if err == nil {
		t.Fatal("LoadEntireSettings read settings through a symlinked .entire")
	}
	if !errors.Is(err, paths.ErrEntireDirNotDirectory) {
		t.Errorf("error does not wrap ErrEntireDirNotDirectory: %v", err)
	}
}

func TestLoadEntireSettings_HealthyRepoLoads(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	if err := os.Mkdir(filepath.Join(repoDir, paths.EntireDir), 0o750); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoDir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	if _, err := LoadEntireSettings(context.Background()); err != nil {
		t.Fatalf("LoadEntireSettings on a healthy repo: %v", err)
	}
}

// Outside a repository the check is skipped, so settings still resolve to
// defaults rather than failing with a message about `.entire`.
func TestLoadEntireSettings_OutsideRepositoryStillLoads(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	if _, err := LoadEntireSettings(context.Background()); err != nil {
		t.Fatalf("LoadEntireSettings outside a repository: %v", err)
	}
}

// withoutGit strips git from PATH so worktree-root discovery fails for a reason
// that is not "no repository". Not parallel: PATH is process-global.
func withoutGit(t *testing.T) {
	t.Helper()

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	t.Chdir(repoDir)
	t.Setenv("PATH", t.TempDir())
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
}

// A discovery failure has established nothing about `.entire`. Sending the user
// off to replace a file that may be perfectly fine is worse than saying
// nothing, so the two failures carry different remedies.
func TestCheckEntireDirBeforeRun_DiscoveryFailureDoesNotBlameTheDirectory(t *testing.T) {
	withoutGit(t)

	var stderr bytes.Buffer
	cmd := &cobra.Command{Use: "status"}
	cmd.SetContext(context.Background())
	cmd.SetErr(&stderr)

	safe, err := checkEntireDirBeforeRun(cmd)
	if err == nil {
		t.Fatal("a guarded command ran with the worktree root unresolved")
	}
	if safe {
		t.Error("checkEntireDirBeforeRun reported the path safe")
	}
	if errors.Is(err, paths.ErrEntireDirNotDirectory) {
		t.Errorf("a discovery failure was reported as a wrong file type: %v", err)
	}

	msg := stderr.String()
	if strings.Contains(msg, "replace it with a real directory") {
		t.Errorf("remedy blames .entire for a failure that says nothing about it:\n%s", msg)
	}
	if !strings.Contains(msg, "safe.directory") {
		t.Errorf("remedy does not name the common cause:\n%s", msg)
	}
}

func TestDoctor_ReportsUnverifiedWhenDiscoveryFails(t *testing.T) {
	withoutGit(t)

	var stdout bytes.Buffer
	cmd := &cobra.Command{Use: "doctor"}
	cmd.SetContext(context.Background())
	cmd.SetOut(&stdout)

	if err := reportBrokenEntireDir(cmd); err == nil {
		t.Fatal("doctor did not report the unresolved worktree root")
	}

	report := stdout.String()
	if !strings.Contains(report, "UNVERIFIED") {
		t.Errorf("report is not labelled UNVERIFIED:\n%s", report)
	}
	if strings.Contains(report, "BROKEN") {
		t.Errorf("report calls .entire broken on evidence that says nothing about it:\n%s", report)
	}
}

// The printed remedy is branched on which condition actually occurred. These
// drive the writers with a synthesized error for each: staging a real
// unreadable .entire at CLI level would need an unreadable repo root, which
// breaks worktree-root discovery first and so tests the wrong branch.
func TestEntireDirRemedyMatchesTheCondition(t *testing.T) {
	t.Parallel()

	wrongType := fmt.Errorf("/repo/.entire is a symbolic link, %w", paths.ErrEntireDirNotDirectory)
	symlinkedEntry := fmt.Errorf("/repo/.entire/settings.local.json is a symbolic link to /elsewhere.json, %w",
		paths.ErrEntireDirUnsupportedEntry)
	pipedEntry := fmt.Errorf("/repo/.entire/settings.json is a named pipe, %w",
		paths.ErrEntireDirUnsupportedEntry)
	unreadable := fmt.Errorf("/repo/.entire %w: %w", paths.ErrEntireDirUnreadable, fs.ErrPermission)
	unresolved := fmt.Errorf("%w, so .entire cannot be verified: %w",
		paths.ErrRepositoryUnresolved, errors.New("fatal: detected dubious ownership"))
	unknown := errors.New("something nobody classified")

	tests := []struct {
		name    string
		err     error
		want    []string
		exclude []string
	}{
		{
			name:    "wrong file type points at the path",
			err:     wrongType,
			want:    []string{"replace it with a real directory"},
			exclude: []string{"safe.directory", "PATH", "permissions", "real file or directory"},
		},
		{
			// Shares the wrong-type row's shape but not its sentence:
			// settings.local.json is not required to be a directory, so being
			// told it is not one names the wrong problem.
			name:    "symlinked entry points at the entry",
			err:     symlinkedEntry,
			want:    []string{"replace it with a real file or directory"},
			exclude: []string{"safe.directory", "PATH", "ownership", "replace it with a real directory"},
		},
		{
			// An entry can be unsupported without being a link, and the remedy
			// is the same one. The wording must not assume a far end to inspect.
			name:    "unsupported entry that is not a link shares the remedy",
			err:     pipedEntry,
			want:    []string{"replace it with a real file or directory"},
			exclude: []string{"safe.directory", "PATH", "ownership", "replace it with a real directory"},
		},
		{
			name:    "unreadable points at the filesystem",
			err:     unreadable,
			want:    []string{"ownership", "permissions"},
			exclude: []string{"safe.directory", "replace it with a real directory"},
		},
		{
			name:    "unresolved repository points at git",
			err:     unresolved,
			want:    []string{"safe.directory"},
			exclude: []string{"replace it with a real directory", "real file or directory"},
		},
		{
			name: "unclassified invents no remedy",
			err:  unknown,
			// No remedy can be right for an error nobody classified, and a
			// wrong one costs more than none.
			exclude: []string{
				"safe.directory", "replace it with a real directory",
				"real file or directory", "ownership",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			for _, write := range []struct {
				label string
				fn    func(io.Writer, error)
			}{
				{"guard", writeEntireDirRemedy},
				{"doctor", writeEntireDirDiagnosis},
			} {
				var buf bytes.Buffer
				write.fn(&buf, tt.err)
				got := buf.String()

				if !strings.Contains(got, tt.err.Error()) {
					t.Errorf("%s: output does not restate the error:\n%s", write.label, got)
				}
				for _, want := range tt.want {
					if !strings.Contains(got, want) {
						t.Errorf("%s: output is missing %q:\n%s", write.label, want, got)
					}
				}
				for _, exclude := range tt.exclude {
					if strings.Contains(got, exclude) {
						t.Errorf("%s: output wrongly mentions %q:\n%s", write.label, exclude, got)
					}
				}
			}
		})
	}
}

// symlinkEntireDirEntry builds a repo whose `.entire` is a real directory
// holding one symlinked entry, points the process at it, and returns the
// symlink's target. Named entries are the ones Entire actually reads and
// writes, so callers pass the one whose redirection they care about.
//
// Not parallel: t.Chdir is process-global, and the CWD is what the
// worktree-root resolution reads.
func newRepoWithSymlinkedEntireDirEntry(t *testing.T, entry string) (target string) {
	t.Helper()

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)

	entireDir := filepath.Join(repoDir, paths.EntireDir)
	if err := os.Mkdir(entireDir, 0o750); err != nil {
		t.Fatal(err)
	}
	target = filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(entireDir, entry)); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	t.Chdir(repoDir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	return target
}

// A real `.entire` holding a redirected entry is the case a check that stopped
// at `.entire` itself waves through, which is the whole reason the scan goes one
// level down.
func TestCheckEntireDirBeforeRun_SymlinkedEntryFailsGuardedCommand(t *testing.T) {
	for _, entry := range []string{
		"settings.json",
		"settings.local.json",
		"metadata",
		"logs",
		"tmp",
	} {
		t.Run(entry, func(t *testing.T) {
			newRepoWithSymlinkedEntireDirEntry(t, entry)

			var stderr bytes.Buffer
			cmd := &cobra.Command{Use: "status"}
			cmd.SetContext(context.Background())
			cmd.SetErr(&stderr)

			safe, err := checkEntireDirBeforeRun(cmd)
			if err == nil {
				t.Fatalf("checkEntireDirBeforeRun accepted a symlinked %s", entry)
			}
			if safe {
				t.Error("checkEntireDirBeforeRun reported the path safe")
			}
			if !errors.Is(err, paths.ErrEntireDirUnsupportedEntry) {
				t.Errorf("error does not wrap ErrEntireDirUnsupportedEntry: %v", err)
			}
			if report := stderr.String(); !strings.Contains(report, entry) {
				t.Errorf("stderr does not name the offending entry:\n%s", report)
			}
		})
	}
}

// The settings files are the ones with teeth: settings.local.json names the
// command Entire executes at pre-push, and both decide what is redacted before
// it is committed. os.OpenRoot, which the loader already opens them through,
// refuses only a link that leaves `.entire` — so the in-directory case below is
// the one it lets past.
func TestLoadEntireSettings_RefusesSymlinkedSettingsFile(t *testing.T) {
	for _, entry := range []string{"settings.json", "settings.local.json"} {
		t.Run(entry, func(t *testing.T) {
			newRepoWithSymlinkedEntireDirEntry(t, entry)

			_, err := LoadEntireSettings(context.Background())
			if err == nil {
				t.Fatalf("LoadEntireSettings read settings through a symlinked %s", entry)
			}
			if !errors.Is(err, paths.ErrEntireDirUnsupportedEntry) {
				t.Errorf("error does not wrap ErrEntireDirUnsupportedEntry: %v", err)
			}
		})
	}
}

// Staying inside `.entire` is not the invariant. This is the case os.OpenRoot
// confinement follows without complaint, so it needs its own coverage rather
// than sharing the escaping-link cases above.
func TestLoadEntireSettings_RefusesSettingsSymlinkedWithinEntireDir(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)

	entireDir := filepath.Join(repoDir, paths.EntireDir)
	if err := os.Mkdir(entireDir, 0o750); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(entireDir, "planted.json")
	if err := os.WriteFile(planted, []byte(`{"enabled":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(planted, filepath.Join(entireDir, "settings.local.json")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	t.Chdir(repoDir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	_, err := LoadEntireSettings(context.Background())
	if err == nil {
		t.Fatal("LoadEntireSettings read settings through a link pointing inside .entire")
	}
	if !errors.Is(err, paths.ErrEntireDirUnsupportedEntry) {
		t.Errorf("error does not wrap ErrEntireDirUnsupportedEntry: %v", err)
	}
}

// A symlinked .entire/logs is the log sink itself being redirected, so the
// refusal has to happen before anything is written — the target must come out
// of this untouched.
func TestNewLogger_RefusesSymlinkedLogsDir(t *testing.T) {
	target := newRepoWithSymlinkedEntireDirEntry(t, "logs")

	if _, err := newLogger(context.Background()); err == nil {
		t.Fatal("newLogger built a logger through a symlinked .entire/logs")
	}

	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("newLogger created %d entries in the symlink target", len(entries))
	}
}

func TestDoctor_ReportsSymlinkedEntireDirEntry(t *testing.T) {
	newRepoWithSymlinkedEntireDirEntry(t, "settings.local.json")

	for _, args := range [][]string{{"doctor"}, {"doctor", "logs"}, {"doctor", "bundle"}} {
		name := strings.Join(args, " ")
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			root := NewRootCmd()
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs(args)

			err := root.ExecuteContext(context.Background())
			if err == nil {
				t.Fatalf("`entire %s` succeeded on a repo with a symlinked .entire entry", name)
			}
			if !errors.Is(err, paths.ErrEntireDirUnsupportedEntry) {
				t.Fatalf("error does not wrap ErrEntireDirUnsupportedEntry: %v", err)
			}

			report := stdout.String() + stderr.String()
			for _, want := range []string{"symbolic link", "settings.local.json"} {
				if !strings.Contains(report, want) {
					t.Errorf("report is missing %q:\n%s", want, report)
				}
			}
		})
	}
}

// Everything Entire itself puts under `.entire` is a real file or directory, so
// a populated repo has to pass. Without this, the scan is one stray symlink in
// our own layout away from failing every command in every repo.
func TestCheckEntireDirBeforeRun_PopulatedEntireDirIsSafe(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)

	entireDir := filepath.Join(repoDir, paths.EntireDir)
	for _, sub := range []string{"", "logs", "metadata", "tmp", "runners", "redactors"} {
		if err := os.Mkdir(filepath.Join(entireDir, sub), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{".gitignore", "settings.json", "settings.local.json"} {
		if err := os.WriteFile(filepath.Join(entireDir, file), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Chdir(repoDir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	cmd := &cobra.Command{Use: "status"}
	cmd.SetContext(context.Background())

	safe, err := checkEntireDirBeforeRun(cmd)
	if err != nil {
		t.Fatalf("checkEntireDirBeforeRun on a populated .entire: %v", err)
	}
	if !safe {
		t.Error("a populated .entire directory was not reported safe")
	}
}
