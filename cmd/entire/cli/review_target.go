package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	cliReview "github.com/entireio/cli/cmd/entire/cli/review"
)

func prepareReviewTarget(ctx context.Context, out, errOut io.Writer, selector string) (cliReview.TargetWorktree, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return cliReview.TargetWorktree{}, errors.New("review target cannot be empty")
	}

	// A plain, non-numeric branch needs no trail remote or API. Positive
	// integers are reserved for trail numbers so a branch named "42" cannot
	// silently shadow trail #42; use a non-numeric branch name to target git.
	branch := ""
	if reviewTargetMayBeBranch(selector) && reviewTargetBranchExists(ctx, selector) {
		branch = selector
	} else {
		forge, owner, repo, err := resolveTrailRemote(ctx)
		if err != nil {
			return cliReview.TargetWorktree{}, err
		}
		normalized, _, normalizeErr := normalizeReviewTargetSelector(selector, forge, owner, repo)
		if normalizeErr != nil {
			return cliReview.TargetWorktree{}, normalizeErr
		}
		err = runAuthenticatedTrailAPI(ctx, errOut, false, "", func(ctx context.Context, client *api.Client) error {
			found, findErr := resolveTrailBySelector(ctx, client, forge, owner, repo, normalized, "")
			if findErr != nil {
				return findErr
			}
			branch = strings.TrimSpace(found.Branch)
			if branch == "" {
				return fmt.Errorf("%s has no branch to review", describeTrailRef(found))
			}
			return nil
		})
		if err != nil {
			return cliReview.TargetWorktree{}, err
		}
	}

	root, err := trailWorktreeBaseRoot(ctx)
	if err != nil {
		return cliReview.TargetWorktree{}, fmt.Errorf("resolve review worktree root: %w", err)
	}
	_, existed, err := findWorktreeForBranch(ctx, branch, root)
	if err != nil {
		return cliReview.TargetWorktree{}, err
	}
	worktreeRoot, err := checkoutReviewWorktree(ctx, io.Discard, errOut, branch)
	if err != nil {
		return cliReview.TargetWorktree{}, err
	}
	if worktreeRoot == "" {
		return cliReview.TargetWorktree{}, errors.New("review target checkout was cancelled")
	}
	fmt.Fprintf(out, "Reviewing branch %s in %s.\n", branch, worktreeRoot)
	return cliReview.TargetWorktree{Path: worktreeRoot, Created: !existed}, nil
}

func removeReviewTarget(ctx context.Context, worktreeRoot string) error {
	root, err := trailWorktreeBaseRoot(ctx)
	if err != nil {
		return fmt.Errorf("resolve managed worktree root: %w", err)
	}
	managedRoot := normalizeWorktreePath(filepath.Join(root, filepath.FromSlash(trailWorktreesRelDir)))
	target := normalizeWorktreePath(worktreeRoot)
	if target == managedRoot || !strings.HasPrefix(target, managedRoot+string(filepath.Separator)) {
		return fmt.Errorf("refusing to remove worktree outside %s", managedRoot)
	}
	repo, err := gitrepo.OpenPath(worktreeRoot)
	if err != nil {
		return fmt.Errorf("open review worktree: %w", err)
	}
	status, statusErr := gitrepo.Status(ctx, repo)
	closeErr := repo.Close()
	if statusErr != nil {
		return fmt.Errorf("check review worktree status: %w", statusErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close review worktree: %w", closeErr)
	}
	if !status.IsClean() {
		return errors.New("review worktree has uncommitted changes; keeping it")
	}

	// --force permits ignored review logs and copied .worktreeinclude files to
	// be removed after the tracked/untracked status check above proved the
	// checkout has no user changes.
	cmd := exec.CommandContext(ctx, "git", "worktree", "remove", "--force", "--", worktreeRoot)
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree remove: %s: %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}

func reviewTargetMayBeBranch(selector string) bool {
	if strings.Contains(selector, "://") {
		return false
	}
	_, numeric := parseTrailNumberSelector(selector)
	return !numeric
}

func reviewTargetBranchExists(ctx context.Context, branch string) bool {
	if strings.TrimSpace(branch) == "" || ValidateBranchName(ctx, branch) != nil {
		return false
	}
	if exists, err := BranchExistsLocally(ctx, branch); err == nil && exists {
		return true
	}
	exists, err := BranchExistsOnRemote(ctx, branch)
	return err == nil && exists
}

func normalizeReviewTargetSelector(raw, forge, owner, repo string) (string, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false, errors.New("review target cannot be empty")
	}
	if !strings.Contains(raw, "://") {
		return raw, false, nil
	}
	u, parseErr := url.Parse(raw)
	if parseErr != nil || u.Scheme == "" || u.Host == "" {
		return "", true, fmt.Errorf("invalid review target URL %q", raw)
	}
	if u.Scheme != schemeHTTPS && u.Scheme != schemeHTTP {
		return "", true, fmt.Errorf("unsupported review target URL scheme %q", u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	if host != "entire.io" && !strings.HasSuffix(host, ".entire.io") {
		return "", true, errors.New("review target URL must be an Entire trail URL")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 5 || parts[3] != "trails" || strings.TrimSpace(parts[4]) == "" {
		return "", true, fmt.Errorf("invalid Entire trail URL %q", raw)
	}
	if parts[0] != forge || parts[1] != owner || parts[2] != repo {
		return "", true, fmt.Errorf("trail URL targets %s/%s/%s, but this clone is %s/%s/%s", parts[0], parts[1], parts[2], forge, owner, repo)
	}
	return parts[4], true, nil
}
