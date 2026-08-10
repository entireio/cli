package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
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
// A clusterFlag naming a cluster in a different federation dials that
// cluster's core instead of the active context's, matching how the placement
// lookup routes (see listPullablePlacements).
func resolveExplainRepoID(cmd *cobra.Command, repoID, clusterFlag string) (owner, repo string, err error) {
	runWithCore, err := coreRunnerForCluster(clusterFlag)
	if err != nil {
		return "", "", err
	}
	var fullName string
	err = runWithCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
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
// URL forms). --repo is GitHub-scoped (gh/owner/name), so a non-GitHub origin
// with a coincidentally matching owner/name must not count as the current
// repo. Best-effort: any lookup or parse failure returns false, so explain
// proceeds with the cross-repo fetch path.
func explainRepoIsCurrent(ctx context.Context, owner, repo string) bool {
	curForge, curOwner, curRepo, err := gitremote.ResolveRemoteRepo(ctx, "origin")
	if err != nil || curForge != "gh" {
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

// crossRepoExplainRequest carries the explain flag values the cross-repo
// dispatch needs, so maybeRunCrossRepoExplain can live outside the already
// maintidx-capped newExplainCmd closure.
type crossRepoExplainRequest struct {
	repoFlag, clusterFlag, positional, checkpointFlag string
	json, transcript, rawTranscript                   bool
	sessionIndex                                      int
	noPager, short, full, searchAll                   bool
}

// maybeRunCrossRepoExplain runs the cross-repo explain flow when --repo names
// a repo other than the cwd one: it fetches the checkpoint into a throwaway
// temp repo (see openCrossRepoExplain) and renders it through the normal
// explain pipeline over that repo's lookup. handled=false (with nil error)
// means --repo names the cwd repo and the caller falls through to the local
// flow unchanged.
func maybeRunCrossRepoExplain(cmd *cobra.Command, req crossRepoExplainRequest) (handled bool, err error) {
	target := req.positional
	if target == "" {
		target = req.checkpointFlag
	}
	lookup, cleanup, err := openCrossRepoExplain(cmd, req.repoFlag, req.clusterFlag, target)
	if err != nil {
		return true, err
	}
	if lookup == nil {
		return false, nil
	}
	defer cleanup()
	if req.json || req.transcript || (req.rawTranscript && cmd.Flags().Changed("session-index")) {
		// The export flow closes the lookup it is handed.
		return true, runExplainExport(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), explainExportOptions{
			checkpointFlag: req.checkpointFlag,
			target:         req.positional,
			json:           req.json,
			transcript:     req.transcript,
			rawTranscript:  req.rawTranscript,
			sessionIndex:   req.sessionIndex,
			lookup:         lookup,
		})
	}
	defer func() { _ = lookup.Close() }()
	// --generate/--force are mutually exclusive with --repo, so this is
	// always a read-only render (generate=false, force=false, timeout 0).
	return true, runExplainCheckpointWithLookup(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), target,
		req.noPager, !req.short, req.full, req.rawTranscript, false, false, req.searchAll, lookup, nil, 0)
}

// openCrossRepoExplain resolves --repo/--cluster and, when the target repo
// differs from the cwd repo, fetches the checkpoint's per-checkpoint ref from
// that repo's Entire mirror into a throwaway temp repo and returns an explain
// lookup over it — a pure lookup that never writes a ref, object, or config
// entry into the cwd repo, so a foreign checkpoint can never join (or count
// as) the local checkpoint namespace. cleanup removes the temp repo; the
// caller owns closing the lookup before running cleanup. When --repo names
// the cwd repo both returns are nil and the caller falls through to the
// normal local flow (behavior identical to no flag).
func openCrossRepoExplain(cmd *cobra.Command, repoFlag, clusterFlag, target string) (lookup *explainCheckpointLookup, cleanup func(), err error) {
	ref, err := parseExplainRepoFlag(repoFlag)
	if err != nil {
		return nil, nil, err
	}
	// Everything past flag parsing is a runtime failure (network, git), not a
	// usage mistake — don't dump usage text on it.
	cmd.SilenceUsage = true
	owner, repoName := ref.owner, ref.repo
	if ref.repoID != "" {
		owner, repoName, err = resolveExplainRepoID(cmd, ref.repoID, clusterFlag)
		if err != nil {
			return nil, nil, err
		}
	}
	if explainRepoIsCurrent(cmd.Context(), owner, repoName) {
		return nil, nil, nil
	}
	// A prefix cannot name a per-checkpoint ref, so cross-repo needs the full ID.
	cid, err := id.NewCheckpointID(target)
	if err != nil {
		return nil, nil, fmt.Errorf("--repo requires a full checkpoint ID (12-char hex or 26-char ULID); a prefix cannot be resolved cross-repo: %w", err)
	}
	url, err := resolveExplainRepoFetchURL(cmd, owner, repoName, clusterFlag)
	if err != nil {
		return nil, nil, err
	}
	return fetchCrossRepoCheckpoint(cmd.Context(), cmd.ErrOrStderr(), url, owner+"/"+repoName, cid)
}

// fetchCrossRepoCheckpoint fetches cid's per-checkpoint ref
// (refs/entire/checkpoints/<shard>/<id>) from url into a fresh temp repo and
// returns an explain lookup over that repo, plus a cleanup that deletes it.
// Separated from URL resolution so tests can exercise the fetch + explain
// flow against a file:// remote without the core API or git-remote-entire.
func fetchCrossRepoCheckpoint(ctx context.Context, errW io.Writer, url, ownerRepo string, cid id.CheckpointID) (*explainCheckpointLookup, func(), error) {
	refName, err := checkpoint.RefName(cid)
	if err != nil {
		// Defensive only: cid is already validated by the caller.
		return nil, nil, err //nolint:wrapcheck // RefName's error is self-describing
	}
	tmpDir, err := os.MkdirTemp("", "entire-explain-repo-")
	if err != nil {
		return nil, nil, fmt.Errorf("create temp repo for cross-repo checkpoint: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }
	lookup, err := fetchCrossRepoCheckpointInto(ctx, errW, tmpDir, url, ownerRepo, cid, refName)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return lookup, cleanup, nil
}

// fetchCrossRepoCheckpointInto initializes a repo in tmpDir, fetches the
// checkpoint ref into it, and builds the explain lookup over it.
func fetchCrossRepoCheckpointInto(ctx context.Context, errW io.Writer, tmpDir, url, ownerRepo string, cid id.CheckpointID, refName plumbing.ReferenceName) (*explainCheckpointLookup, error) {
	if out, initErr := exec.CommandContext(ctx, "git", "init", "-q", tmpDir).CombinedOutput(); initErr != nil {
		return nil, fmt.Errorf("init temp repo for cross-repo checkpoint: %s: %w", strings.TrimSpace(string(out)), initErr)
	}
	stop := startSpinner(errW, "Fetching checkpoint from "+ownerRepo)
	fetchErr := remote.FetchCheckpointRefInto(ctx, tmpDir, url, refName)
	stop(fetchErr == nil)
	if fetchErr != nil {
		if errors.Is(fetchErr, plumbing.ErrReferenceNotFound) {
			return nil, fmt.Errorf("checkpoint %s not found in the mirror for %s; repos using the legacy branch-based checkpoint store (or an external checkpoint_remote) are not supported cross-repo yet", cid, ownerRepo)
		}
		return nil, fmt.Errorf("fetch checkpoint %s from %s: %w", cid, ownerRepo, fetchErr)
	}
	repo, err := gitrepo.OpenPath(tmpDir)
	if err != nil {
		return nil, fmt.Errorf("open temp repo for cross-repo checkpoint: %w", err)
	}
	// No fetchers: the single fetched ref brought every object with it
	// (unfiltered fetch), and an on-demand fetch would dial the cwd repo's
	// checkpoint remote — the wrong repo.
	store := checkpoint.NewRefsReadStore(repo)
	committed, err := store.List(ctx)
	if err != nil {
		_ = repo.Close()
		return nil, fmt.Errorf("list fetched cross-repo checkpoint: %w", err)
	}
	return &explainCheckpointLookup{repo: repo, store: store, committed: committed, noRemoteFallback: true}, nil
}
