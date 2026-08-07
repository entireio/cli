package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/internal/coreapi"
)

// explainRepoRef is a parsed `explain --repo` value: either an owner/name slug
// (possibly given with a gh/ prefix) or a raw repo ULID that still needs a
// control-plane lookup to become owner/name.
type explainRepoRef struct {
	owner, repo string // owner/name form (lowercased)
	repoID      string // bare-ULID form
}

// parseExplainRepoFlag parses the --repo flag value. Accepted shapes:
// `owner/name`, `gh/owner/name` (leading slash optional), or a bare repo ULID.
// owner/name reuse the GitHub charset validation of the mirror clone-ref
// parser and come back lowercased, matching what the control plane persists.
func parseExplainRepoFlag(value string) (explainRepoRef, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return explainRepoRef{}, errors.New("--repo requires a value: owner/name, gh/owner/name, or a repo ID")
	}
	if strings.Contains(v, "/") {
		// Normalize owner/name to the gh/owner/name clone-ref shape so
		// parseMirrorCloneRef's charset validation applies to both forms.
		ref := strings.TrimPrefix(v, "/")
		if !strings.HasPrefix(ref, "gh/") {
			ref = "gh/" + ref
		}
		_, owner, repo, err := parseMirrorCloneRef(ref)
		if err != nil {
			return explainRepoRef{}, fmt.Errorf("invalid --repo %q: expected owner/name, gh/owner/name, or a repo ID: %w", value, err)
		}
		return explainRepoRef{owner: owner, repo: repo}, nil
	}
	if !looksLikeULID(v) {
		return explainRepoRef{}, fmt.Errorf("invalid --repo %q: expected owner/name, gh/owner/name, or a repo ID (ULID)", value)
	}
	return explainRepoRef{repoID: v}, nil
}

// resolveExplainRepoID resolves a raw repo ULID to its owner/name via the
// control plane's by-id lookup (the same call `entire repo get <ulid>` uses).
func resolveExplainRepoID(cmd *cobra.Command, repoID string) (owner, repo string, err error) {
	var fullName string
	err = runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
		r, err := c.GetRepo(ctx, coreapi.GetRepoParams{RepoId: repoID})
		if err != nil {
			if isCoreNotFound(err) {
				return fmt.Errorf("no repository with ID %q visible from your login", repoID)
			}
			return fmt.Errorf("look up repository %q: %w", repoID, err)
		}
		fullName = r.FullName.Or("")
		return nil
	})
	if err != nil {
		return "", "", err
	}
	owner, repo, ok := strings.Cut(strings.ToLower(fullName), "/")
	if !ok || owner == "" || repo == "" {
		return "", "", fmt.Errorf("repository %q has an unexpected full name %q (want owner/name)", repoID, fullName)
	}
	return owner, repo, nil
}

// explainRepoIsCurrent reports whether owner/repo names the same repository as
// the cwd worktree's origin remote (handles ssh, https, and entire:// mirror
// URL forms). Best-effort: any lookup or parse failure returns false, so
// explain proceeds with the cross-repo fetch path.
func explainRepoIsCurrent(ctx context.Context, owner, repo string) bool {
	_, curOwner, curRepo, err := gitremote.ResolveRemoteRepo(ctx, "origin")
	if err != nil {
		return false
	}
	return strings.EqualFold(curOwner, owner) && strings.EqualFold(curRepo, repo)
}

// resolveExplainRepoFetchURL resolves the entire:// mirror URL to fetch a
// foreign repo's checkpoint ref from: pullable placements via the core API
// (the same authority `repo clone` uses), then the shared placement picker
// with explain-specific wording and the --cluster escape hatch.
func resolveExplainRepoFetchURL(cmd *cobra.Command, owner, repo, clusterFlag string) (string, error) {
	placements, err := listPullablePlacements(cmd, owner, repo, clusterFlag)
	if err != nil {
		return "", err
	}
	if len(placements) == 0 {
		return "", fmt.Errorf("no mirror found for /gh/%s/%s; cross-repo explain fetches checkpoints from the repo's Entire mirror", owner, repo)
	}
	chosen, err := selectPlacement(cmd, placements, clusterFlag, placementPicker{
		selector: "--cluster",
		title:    "This repo is mirrored on more than one cluster — pick one to fetch the checkpoint from",
		action:   "Explain",
	})
	if err != nil {
		return "", err
	}
	// chosen.ClusterHost is server-provided but interpolated into the entire://
	// URL like a user-supplied --cluster, so apply the same anti-token-leak
	// guard before building it (see repo clone).
	if err := validateClusterHost(chosen.ClusterHost); err != nil {
		return "", fmt.Errorf("mirror has an invalid cluster host %q: %w", chosen.ClusterHost, err)
	}
	return mirrorCloneURL(chosen.ClusterHost, owner, repo), nil
}

// prepareCrossRepoExplain resolves --repo/--cluster and, when the target repo
// differs from the cwd repo, fetches the checkpoint's per-checkpoint ref from
// that repo's Entire mirror into the cwd repo's object store. After a nil
// return, the normal explain flow resolves the checkpoint as if it were local.
// When --repo names the cwd repo it is a no-op (behavior identical to no flag).
func prepareCrossRepoExplain(cmd *cobra.Command, repoFlag, clusterFlag, target string) error {
	ref, err := parseExplainRepoFlag(repoFlag)
	if err != nil {
		return err
	}
	// Everything past flag parsing is a runtime failure (network, git), not a
	// usage mistake — don't dump usage text on it.
	cmd.SilenceUsage = true
	owner, repoName := ref.owner, ref.repo
	if ref.repoID != "" {
		owner, repoName, err = resolveExplainRepoID(cmd, ref.repoID)
		if err != nil {
			return err
		}
	}
	if explainRepoIsCurrent(cmd.Context(), owner, repoName) {
		return nil
	}
	// A prefix cannot name a per-checkpoint ref, so cross-repo needs the full ID.
	cid, err := id.NewCheckpointID(target)
	if err != nil {
		return fmt.Errorf("--repo requires a full checkpoint ID (12-char hex or 26-char ULID); a prefix cannot be resolved cross-repo: %w", err)
	}
	url, err := resolveExplainRepoFetchURL(cmd, owner, repoName, clusterFlag)
	if err != nil {
		return err
	}
	return fetchCrossRepoCheckpoint(cmd.Context(), cmd.ErrOrStderr(), url, owner+"/"+repoName, cid)
}

// fetchCrossRepoCheckpoint fetches cid's per-checkpoint ref
// (refs/entire/checkpoints/<shard>/<id>) from url into the cwd repo's object
// store. Separated from URL resolution so tests can exercise the fetch +
// explain flow against a file:// remote without the core API or
// git-remote-entire.
func fetchCrossRepoCheckpoint(ctx context.Context, errW io.Writer, url, ownerRepo string, cid id.CheckpointID) error {
	refName, err := checkpoint.RefName(cid)
	if err != nil {
		// Defensive only: cid is already validated by the caller.
		return err //nolint:wrapcheck // RefName's error is self-describing
	}
	stop := startSpinner(errW, "Fetching checkpoint from "+ownerRepo)
	fetchErr := remote.FetchCheckpointRefFrom(ctx, url, refName)
	stop(fetchErr == nil)
	if fetchErr == nil {
		return nil
	}
	if errors.Is(fetchErr, plumbing.ErrReferenceNotFound) {
		return fmt.Errorf("checkpoint %s not found in %s's mirror; repos using the legacy branch-based checkpoint store (or an external checkpoint_remote) are not supported cross-repo yet", cid, ownerRepo)
	}
	return fmt.Errorf("fetch checkpoint %s from %s: %w", cid, ownerRepo, fetchErr)
}
