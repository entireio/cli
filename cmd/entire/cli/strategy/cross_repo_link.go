package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// prepareCrossRepoCommitMsg stamps a trailer on a commit no session of this
// repository claims when the committing process descends from an agent whose
// session is homed in another repository (crossRepoCommitSession). Commit
// linking in a repository that is on only through global enable is the
// agent's own commit by ancestry, so there is no prompt.
func (s *ManualCommitStrategy) prepareCrossRepoCommitMsg(ctx context.Context, commitMsgFile string, listErr error) {
	if listErr != nil {
		return
	}
	home := crossRepoCommitSession(ctx)
	if home == nil {
		return
	}
	content, err := os.ReadFile(commitMsgFile) //nolint:gosec // path from the git hook
	if err != nil {
		return
	}
	message := string(content)
	rules := messageCommentRules(ctx, message)
	if _, found := stampedTrailer(ctx, message, rules, nil); found {
		return
	}
	cpID, err := id.Generate()
	if err != nil || !recordStampedTrailer(ctx, cpID) {
		return
	}
	message = addCheckpointTrailerAboveComments(message, cpID, rules)
	if err := os.WriteFile(commitMsgFile, []byte(message), 0o600); err != nil { //nolint:gosec // path from the git hook
		return
	}
	logging.Info(logging.WithComponent(ctx, "cross-repo"), "prepare-commit-msg: trailer added for a session homed in another repository",
		slog.String("session_id", home.state.SessionID), slog.String("checkpoint_id", cpID.String()))
}

// postCommitUnclaimed handles a stamped commit no session of this repository
// claims: one an agent homed elsewhere made, else a warning.
func (s *ManualCommitStrategy) postCommitUnclaimed(ctx context.Context, listErr error, repo *git.Repository, commit *object.Commit, cpID id.CheckpointID, worktreePath string) {
	if listErr == nil && s.postCommitCrossRepo(ctx, repo, commit, cpID, worktreePath) {
		return
	}
	logging.Warn(logging.WithComponent(ctx, "checkpoint"), "post-commit: no active sessions despite trailer",
		slog.String("strategy", "manual-commit"),
		slog.String("checkpoint_id", cpID.String()),
	)
}

// postCommitCrossRepo writes the marker for the session prepareCrossRepoCommitMsg
// stamped the commit for, and reports whether it
// handled the commit.
func (s *ManualCommitStrategy) postCommitCrossRepo(ctx context.Context, repo *git.Repository, commit *object.Commit, cpID id.CheckpointID, worktreePath string) bool {
	home := crossRepoCommitSession(ctx)
	if home == nil {
		return false
	}
	committed := commitFiles(ctx, worktreePath, commit)
	if err := s.writeCrossRepoMarker(ctx, repo, cpID, home, commit.Hash.String(), committed, linkedByTrailer); err != nil {
		logging.Warn(logging.WithComponent(ctx, "cross-repo"), "post-commit: could not link a session homed in another repository",
			slog.String("session_id", home.state.SessionID), slog.String("error", err.Error()))
	}
	return true
}

// commitFiles is filesChangedInCommit resolving the trees itself.
func commitFiles(ctx context.Context, worktreePath string, commit *object.Commit) map[string]struct{} {
	var headTree, parentTree *object.Tree
	if t, err := commit.Tree(); err == nil {
		headTree = t
	}
	if commit.NumParents() > 0 {
		if parent, err := commit.Parent(0); err == nil {
			if t, err := parent.Tree(); err == nil {
				parentTree = t
			}
		}
	}
	return filesChangedInCommit(ctx, worktreePath, commit, headTree, parentTree)
}

// crossRepoLinkedFile, in this repository's git common dir, lists the commits
// LinkCrossRepoCommits already recorded links for.
const crossRepoLinkedFile = "entire-cross-repo-linked.json"

// maxCrossRepoSweep bounds how many recent commits one sweep inspects.
const maxCrossRepoSweep = 50

// LinkCrossRepoCommits records links (a checkpoint naming the commit in
// LinkedCommits, never an amend) in this repository for commits its hooks did
// not stamp: made since the turn started (since), carrying no trailer, and
// changing a path the session edited here (edited, repo-relative). It covers
// commits made before Entire's git hooks were installed here, and platforms
// without process ancestry. The session is read from its home repository,
// homeWorktree. Returns how many commits it linked.
func (s *ManualCommitStrategy) LinkCrossRepoCommits(ctx context.Context, sessionID, homeWorktree string, since time.Time, edited []string) (int, error) {
	home, err := loadCrossRepoHome(ctx, sessionID, homeWorktree)
	if errors.Is(err, errNotCrossRepo) || (err == nil && len(edited) == 0) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	worktreePath, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return 0, fmt.Errorf("resolve this worktree: %w", err)
	}
	repo, err := OpenRepository(ctx)
	if err != nil {
		return 0, err
	}
	defer repo.Close()
	gitRoot, err := gitdir.Open(ctx)
	if err != nil {
		return 0, fmt.Errorf("open the git directory: %w", err)
	}
	linked := readCrossRepoLinked(gitRoot)
	count := 0
	for _, sha := range commitsSince(ctx, worktreePath, since) {
		commit, err := repo.CommitObject(plumbing.NewHash(sha))
		if err != nil || slices.Contains(linked, sha) || commit.Committer.When.Before(since) {
			continue
		}
		if _, found := trailers.ParseCheckpoint(commit.Message); found {
			continue
		}
		committed := commitFiles(ctx, worktreePath, commit)
		if !editedOverlap(committed, edited) {
			continue
		}
		cpID, err := id.Generate()
		if err != nil {
			return count, fmt.Errorf("generate checkpoint ID: %w", err)
		}
		if err := s.writeCrossRepoMarker(ctx, repo, cpID, home, sha, committed, linkedBySweep); err != nil {
			return count, err
		}
		linked = append(linked, sha)
		count++
	}
	if count > 0 {
		if err := writeCrossRepoLinked(gitRoot, linked); err != nil {
			return count, err
		}
	}
	return count, nil
}

// commitsSince lists HEAD's commits committed at or after since, newest first.
func commitsSince(ctx context.Context, worktreePath string, since time.Time) []string {
	cmd := exec.CommandContext(ctx, "git", "-C", worktreePath, "rev-list",
		fmt.Sprintf("--max-count=%d", maxCrossRepoSweep), fmt.Sprintf("--since=%d", since.Unix()), "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

func readCrossRepoLinked(root *os.Root) []string {
	data, err := osroot.ReadFileNoFollow(root, crossRepoLinkedFile)
	if err != nil {
		return nil
	}
	var shas []string
	if json.Unmarshal(data, &shas) != nil {
		return nil
	}
	return shas
}

func writeCrossRepoLinked(root *os.Root, shas []string) error {
	if len(shas) > maxCrossRepoSweep*4 {
		shas = shas[len(shas)-maxCrossRepoSweep*4:]
	}
	data, err := json.Marshal(shas)
	if err != nil {
		return fmt.Errorf("encode linked commits: %w", err)
	}
	return jsonutil.WriteFileAtomicIn(root, crossRepoLinkedFile, data, 0o600) //nolint:wrapcheck // callers log it
}

// EnsureCrossRepoSetup installs Entire's git hooks in this repository for a
// session homed in another one, unless they would displace hooks Entire does
// not manage. It returns a reason when it skipped, and whether it installed.
func EnsureCrossRepoSetup(ctx context.Context) (skipped string, installed bool, err error) {
	if state := CheckGitHookState(ctx); state == GitHooksAbsent {
		if reason := crossRepoInstallBlocker(ctx); reason != "" {
			return reason, false, nil
		}
	}
	installed, err = EnsureSetupReport(ctx)
	if err != nil || !installed {
		return "", installed, err
	}
	if root, rootErr := paths.WorktreeRoot(ctx); rootErr == nil {
		if recErr := recordCrossRepoInstall(root); recErr != nil {
			logging.Debug(ctx, "could not record a cross-repository hook install", slog.String("error", recErr.Error()))
		}
	}
	return "", true, nil
}

// crossRepoInstallBlocker reports why Entire must not install its git hooks
// here unasked: a hooks directory outside this repository's git directory
// (core.hooksPath naming a shared one another tool owns), or hooks Entire did
// not write.
func crossRepoInstallBlocker(ctx context.Context) string {
	hooksDir, err := GetHooksDir(ctx)
	if err != nil {
		return "the hooks directory could not be resolved"
	}
	commonDir, err := gitdir.CommonDir(ctx)
	if err != nil || !sameDir(hooksDir, filepath.Join(commonDir, "hooks")) {
		return "core.hooksPath names a hooks directory outside the repository"
	}
	root, err := hooksRootForRemoval(hooksDir)
	if err != nil {
		return ""
	}
	for _, hook := range gitHookNames {
		if _, err := root.Lstat(hook); err == nil {
			return "existing git hooks"
		}
	}
	return ""
}
