package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
	"github.com/entireio/cli/cmd/entire/cli/validation"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// crossRepoLinkTimeout bounds one repository's install and sweep.
const crossRepoLinkTimeout = 30 * time.Second

// crossRepoActivity is what a session did outside its repository, as its
// hooks and transcript show: files its edit tools changed, directories its
// shell commands worked in, and the commits `git commit` reported.
type crossRepoActivity struct {
	edited  []string // absolute paths
	dirs    []string // absolute paths
	commits []string // abbreviated SHAs
}

func (a crossRepoActivity) empty() bool {
	return len(a.edited) == 0 && len(a.dirs) == 0
}

// linkSessionInOtherRepos is the agent-hook side of cross-repository linking
// (global enable only): for each other git repository the session worked in
// (act), it runs `entire hooks git link-session` there, which installs
// Entire's git hooks so the agent's next commits link to the session, and
// with sweep records links for commits already made this turn. Without sweep
// (mid-turn tool events) a repository whose hooks are already installed is
// left alone. Best effort.
func linkSessionInOtherRepos(ctx context.Context, sessionID, hookRoot string, act crossRepoActivity, sweep bool) {
	if act.empty() || validation.ValidateSessionID(sessionID) != nil || !settings.UserTierEnablesEverywhere(ctx) {
		return
	}
	hereCommon, err := gitdir.CommonDir(ctx)
	if err != nil {
		return
	}
	logCtx := logging.WithComponent(ctx, "cross-repo")
	since := turnStartTime(ctx, sessionID)
	for root, rels := range activityByOtherRepo(ctx, hereCommon, act) {
		if reason := crossRepoSkipReason(root, hookRoot, crossRepoProtectedDirs()); reason != "" {
			logging.Info(logCtx, "not linking a repository the session worked in",
				slog.String("repo", root), slog.String("skipped_reason", reason))
			continue
		}
		if !sweep && strategy.IsGitHookInstalledInDir(ctx, root) {
			continue
		}
		runLinkSession(ctx, root, linkSessionArgs{sessionID: sessionID, from: hookRoot, since: since, edited: rels, commits: act.commits, sweep: sweep})
	}
}

// linkSessionArgs are `entire hooks git link-session`'s inputs.
type linkSessionArgs struct {
	sessionID, from string
	since           time.Time
	edited          []string // relative to the repository
	commits         []string
	sweep           bool
}

// runLinkSession runs `entire hooks git link-session` in root: a child
// process, so every repository-scoped lookup (settings, hooks directory,
// checkpoint store) resolves there rather than in the session's repository.
func runLinkSession(ctx context.Context, root string, in linkSessionArgs) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	args := []string{hooksCmdName, hooksGitCmdName, "link-session", "--session", in.sessionID, "--from", in.from}
	if !in.since.IsZero() {
		args = append(args, "--since", in.since.Format(time.RFC3339))
	}
	if !in.sweep {
		args = append(args, "--no-sweep")
	}
	for _, rel := range in.edited {
		args = append(args, "--path", rel)
	}
	if in.sweep {
		for _, sha := range in.commits {
			args = append(args, "--commit", sha)
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, crossRepoLinkTimeout)
	defer cancel()
	cmd := execx.NonInteractive(runCtx, exe, args...)
	cmd.Dir = root
	cmd.Env = gitrepo.EnvWithoutRepoOverrides()
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		logging.Debug(logging.WithComponent(ctx, "cross-repo"), "link-session failed",
			slog.String("repo", root), slog.String("error", err.Error()))
	}
}

// turnCrossRepoActivity is a turn's activity outside the session's
// repository: the files it edited (edited; a relative path resolves against
// cwd, the session's worktree) and what its shell commands show, read from
// the transcript at transcriptRef from the turn's start on.
func turnCrossRepoActivity(ctx context.Context, transcriptRef, sessionID, cwd string, edited []string) crossRepoActivity {
	act := crossRepoActivity{edited: absolutePaths(cwd, edited)}
	if !settings.UserTierEnablesEverywhere(ctx) {
		return act
	}
	data, err := agent.ReadTranscriptFile(transcriptRef)
	if err != nil {
		return act
	}
	pre, err := LoadPrePromptState(ctx, sessionID)
	if err != nil {
		pre = nil
	}
	shell := scanShellActivity(data, turnStartTranscriptOffset(ctx, pre, sessionID), cwd)
	act.dirs, act.commits = shell.dirs, shell.commits
	return act
}

// absolutePaths resolves relative paths against cwd, dropping them when cwd
// is not absolute.
func absolutePaths(cwd string, files []string) []string {
	var out []string
	for _, f := range files {
		switch {
		case filepath.IsAbs(f):
			out = append(out, f)
		case f != "" && filepath.IsAbs(cwd):
			out = append(out, filepath.Join(cwd, f))
		}
	}
	return out
}

// linkSubagentInOtherRepos is linkSessionInOtherRepos for a subagent that
// ended: the files its own transcript shows it edited, and what its shell
// commands show. Every subagent end runs it, before the task record is
// completed or not (Claude Code's foreground SubagentStop completes nothing),
// and the parent's turn end never reads a subagent's shell commands.
func linkSubagentInOtherRepos(ctx context.Context, ag agent.Agent, event *agent.Event) {
	if !settings.UserTierEnablesEverywhere(ctx) {
		return
	}
	hookRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return
	}
	cwd := event.CWD
	if !filepath.IsAbs(cwd) {
		cwd = hookRoot
	}
	act := crossRepoActivity{edited: absolutePaths(cwd, event.ModifiedFiles)}
	transcriptPath := declaredSubagentTranscript(ctx, event)
	if transcriptPath == "" && event.SessionRef != "" {
		transcriptPath = ResolveAgentTranscriptPath(filepath.Dir(event.SessionRef), event.SessionID, event.SubagentID)
	}
	if transcriptPath != "" {
		if analyzer, ok := agent.AsTranscriptAnalyzer(ag); ok {
			if files, _, extractErr := analyzer.ExtractModifiedFilesFromOffset(ctx, transcriptPath, 0); extractErr == nil {
				act.edited = append(act.edited, absolutePaths(cwd, files)...)
			}
		}
		if data, readErr := agent.ReadTranscriptFile(transcriptPath); readErr == nil {
			shell := scanShellActivity(data, 0, cwd)
			act.dirs, act.commits = shell.dirs, shell.commits
		}
	}
	linkSessionInOtherRepos(ctx, event.SessionID, hookRoot, act, true)
}

// turnStartTime is when this session's current turn started, from its
// pre-prompt state; zero when unknown.
func turnStartTime(ctx context.Context, sessionID string) time.Time {
	pre, err := LoadPrePromptState(ctx, sessionID)
	if err != nil || pre == nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, pre.Timestamp)
	if err != nil {
		return time.Time{}
	}
	return t
}

// activityByOtherRepo groups the session's work by the other git
// repository (not one sharing hereCommon) it lies in, keyed by that
// repository's worktree root: the edited paths there, relative to it, and an
// entry with none for a repository only shell commands worked in.
func activityByOtherRepo(ctx context.Context, hereCommon string, act crossRepoActivity) map[string][]string {
	type repoDir struct{ top, common string }
	byDir := map[string]*repoDir{}
	repoFor := func(path string) *repoDir {
		dir := existingDir(path)
		r, seen := byDir[dir]
		if !seen {
			if top, common, ok := repoOf(ctx, dir); ok {
				r = &repoDir{top: top, common: common}
			}
			byDir[dir] = r
		}
		if r == nil || sameResolvedDir(r.common, hereCommon) {
			return nil
		}
		return r
	}
	out := map[string][]string{}
	for _, path := range act.edited {
		if !filepath.IsAbs(path) {
			continue
		}
		r := repoFor(filepath.Dir(path))
		if r == nil {
			continue
		}
		rel, err := filepath.Rel(resolvedPath(r.top), resolvedPath(path))
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") || strings.HasPrefix(filepath.ToSlash(rel), ".git/") {
			continue
		}
		out[r.top] = append(out[r.top], filepath.ToSlash(rel))
	}
	for _, dir := range act.dirs {
		if !filepath.IsAbs(dir) {
			continue
		}
		if r := repoFor(dir); r != nil {
			if _, ok := out[r.top]; !ok {
				out[r.top] = nil
			}
		}
	}
	return out
}

// repoOf asks git for the worktree root and common directory holding dir.
func repoOf(ctx context.Context, dir string) (top, common string, ok bool) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--show-toplevel", "--git-common-dir")
	cmd.Env = gitrepo.EnvWithoutRepoOverrides()
	out, err := cmd.Output()
	if err != nil {
		return "", "", false
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 || !filepath.IsAbs(lines[0]) {
		return "", "", false
	}
	common = lines[1]
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	return lines[0], common, true
}

// existingDir is dir or its nearest existing ancestor (an edit may have
// deleted the directory).
func existingDir(dir string) string {
	for cur := dir; ; cur = filepath.Dir(cur) {
		if info, err := os.Stat(cur); err == nil && info.IsDir() {
			return cur
		}
		if filepath.Dir(cur) == cur {
			return cur
		}
	}
}

// resolvedPath resolves symlinks in the longest existing prefix of p and
// keeps the rest, so a deleted file still resolves under its repository.
func resolvedPath(p string) string {
	cleaned := filepath.Clean(p)
	var missing []string
	for cur := cleaned; ; cur = filepath.Dir(cur) {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				r = filepath.Join(r, missing[i])
			}
			return r
		}
		if filepath.Dir(cur) == cur {
			return cleaned
		}
		missing = append(missing, filepath.Base(cur))
	}
}

func sameResolvedDir(a, b string) bool {
	return resolvedPath(a) == resolvedPath(b)
}

// crossRepoProtectedDirs are trees Entire never installs hooks in unasked:
// the home directory bounds where it may (index 0), and the rest are package
// caches, Homebrew, and Entire's own per-user directories.
func crossRepoProtectedDirs() []string {
	home, err := userdirs.Home()
	if err != nil {
		return nil
	}
	gopath := os.Getenv("GOPATH")
	if gopath == "" {
		gopath = filepath.Join(home, "go")
	}
	dirs := []string{home, filepath.Join(gopath, "pkg", "mod"), "/opt/homebrew", "/usr/local/Homebrew", "/usr/local/Cellar", "/home/linuxbrew"}
	if dir, err := userdirs.ConfigDirChecked(); err == nil {
		dirs = append(dirs, dir)
	}
	if dir, err := userdirs.CacheDirChecked(); err == nil {
		dirs = append(dirs, dir)
	}
	return dirs
}

// crossRepoSkipReason reports why Entire must not link the repository at
// root, or "". protected is crossRepoProtectedDirs: root must lie under the
// first and under none of the rest. A repository inside the session's own
// worktree is a submodule or a nested checkout of it.
func crossRepoSkipReason(root, hookRoot string, protected []string) string {
	switch {
	case len(protected) == 0 || !within(protected[0], root):
		return "outside the home directory"
	case within(hookRoot, root):
		return "inside the session's repository"
	}
	for _, dir := range protected[1:] {
		if within(dir, root) {
			return "in a package cache or tool directory"
		}
	}
	return ""
}

// within reports whether path is dir or lies beneath it.
func within(dir, path string) bool {
	rel, err := filepath.Rel(resolvedPath(dir), resolvedPath(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// newHooksGitLinkSessionCmd is `entire hooks git link-session`, run by
// linkSessionInOtherRepos in a repository other than the session's.
func newHooksGitLinkSessionCmd() *cobra.Command {
	var sessionID, from, since string
	var edited, commits []string
	var noSweep bool
	cmd := &cobra.Command{
		Use:   "link-session",
		Short: "Link this repository's commits to an agent session started in another repository",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if gitHooksDisabled || !settings.UserTierEnablesEverywhere(cmd.Context()) {
				return nil
			}
			if err := validation.ValidateSessionID(sessionID); err != nil {
				return fmt.Errorf("invalid --session: %w", err)
			}
			g := newGitHookContext(cmd.Context(), "link-session")
			defer g.span.End()
			return runLinkSessionHere(g, cmd, linkSessionFlags{sessionID: sessionID, from: from, since: since, edited: edited, commits: commits, noSweep: noSweep})
		},
	}
	cmd.Flags().StringVar(&sessionID, "session", "", "session ID")
	cmd.Flags().StringVar(&from, "from", "", "the session's worktree")
	cmd.Flags().StringVar(&since, "since", "", "turn start (RFC 3339)")
	cmd.Flags().StringArrayVar(&edited, "path", nil, "a path the session edited here, relative to this repository")
	cmd.Flags().StringArrayVar(&commits, "commit", nil, "an abbreviated commit SHA the agent's tool output reported")
	cmd.Flags().BoolVar(&noSweep, "no-sweep", false, "only install the git hooks")
	return cmd
}

// linkSessionFlags are link-session's flags.
type linkSessionFlags struct {
	sessionID, from, since string
	edited, commits        []string
	noSweep                bool
}

// runLinkSessionHere installs the git hooks unless something blocks it, then
// sweeps either way: a recorded link needs no hooks.
func runLinkSessionHere(g *gitHookContext, cmd *cobra.Command, f linkSessionFlags) error {
	ctx := g.ctx
	errW := cmd.ErrOrStderr()
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil //nolint:nilerr // not a repository: nothing to link
	}
	skipped, installed, err := strategy.EnsureCrossRepoSetup(ctx)
	switch {
	case skipped != "":
		logging.Info(ctx, "not installing git hooks for a session started in another repository",
			slog.String("repo", root), slog.String("skipped_reason", skipped))
	case err != nil:
		logging.Warn(ctx, "could not install git hooks for a session started in another repository",
			slog.String("repo", root), slog.String("error", err.Error()))
	case installed:
		fmt.Fprintf(errW, "[entire] Installed Entire's git hooks in %s so the agent's commits there link to its session (global enable is on); run 'entire disable' there to stop.\n",
			tuiutil.SanitizeDisplayText(root))
	}
	if f.noSweep {
		return nil
	}
	start, err := time.Parse(time.RFC3339, f.since)
	if err != nil {
		return nil //nolint:nilerr // no turn start, no window to sweep
	}
	linked, err := g.strategy.LinkCrossRepoCommits(ctx, f.sessionID, f.from, start, f.edited, f.commits)
	if err != nil {
		logging.Warn(ctx, "could not link commits to a session started in another repository",
			slog.String("repo", root), slog.String("error", err.Error()))
	}
	if linked > 0 {
		fmt.Fprintf(errW, "[entire] Linked %d commit(s) in %s to session %s.\n", linked, tuiutil.SanitizeDisplayText(root), f.sessionID)
	}
	return nil
}
