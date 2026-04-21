package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// pushRefIfNeeded pushes a custom ref to the given target if it exists locally.
// Custom refs (under refs/entire/) don't have remote-tracking refs, so there's
// no "has unpushed" optimization — we always attempt the push and let git handle
// the no-op case.
func pushRefIfNeeded(ctx context.Context, target string, refName plumbing.ReferenceName) error {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil //nolint:nilerr // Hook must be silent on failure
	}

	if _, err := repo.Reference(refName, true); err != nil {
		return nil //nolint:nilerr // Ref doesn't exist locally, nothing to push
	}

	return doPushRef(ctx, target, refName)
}

// tryPushRef attempts to push a custom ref using an explicit refspec.
func tryPushRef(ctx context.Context, target string, refName plumbing.ReferenceName) (pushResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	// Use --no-verify to prevent recursive hook calls (this runs inside pre-push).
	// Use --porcelain for machine-readable, locale-independent output.
	refSpec := fmt.Sprintf("%s:%s", refName, refName)
	cmd := CheckpointGitCommand(ctx, target, "push", "--no-verify", "--porcelain", target, refSpec)

	output, err := cmd.CombinedOutput()
	outputStr := string(output)
	if err != nil {
		if strings.Contains(outputStr, "non-fast-forward") ||
			strings.Contains(outputStr, "rejected") {
			return pushResult{}, errors.New("non-fast-forward")
		}
		return pushResult{}, fmt.Errorf("push failed: %s", outputStr)
	}

	return parsePushResult(outputStr), nil
}

// doPushRef pushes a custom ref with fetch+merge recovery on conflict.
func doPushRef(ctx context.Context, target string, refName plumbing.ReferenceName) error {
	displayTarget := target
	if isURL(target) {
		displayTarget = "checkpoint remote"
	}

	shortRef := shortRefName(refName)
	fmt.Fprintf(os.Stderr, "[entire] Pushing %s to %s...", shortRef, displayTarget)
	stop := startProgressDots(os.Stderr)

	if result, err := tryPushRef(ctx, target, refName); err == nil {
		finishPush(ctx, stop, result, target)
		return nil
	}
	stop("")

	fmt.Fprintf(os.Stderr, "[entire] Syncing %s with remote...", shortRef)
	stop = startProgressDots(os.Stderr)

	if err := fetchAndMergeRef(ctx, target, refName); err != nil {
		stop("")
		fmt.Fprintf(os.Stderr, "[entire] Warning: couldn't sync %s: %v\n", shortRef, err)
		printCheckpointRemoteHint(target)
		return nil
	}
	stop(" done")

	fmt.Fprintf(os.Stderr, "[entire] Pushing %s to %s...", shortRef, displayTarget)
	stop = startProgressDots(os.Stderr)

	if result, err := tryPushRef(ctx, target, refName); err != nil {
		stop("")
		fmt.Fprintf(os.Stderr, "[entire] Warning: failed to push %s after sync: %v\n", shortRef, err)
		printCheckpointRemoteHint(target)
	} else {
		finishPush(ctx, stop, result, target)
	}

	return nil
}

// fetchAndMergeRef fetches a remote custom ref and merges it into the local ref.
// Uses the same tree-flattening merge as v1 (sharded paths are unique, so no conflicts).
//
// For /full/current: if the remote has archived generations not present locally,
// another machine rotated. In that case, local data is merged into the latest
// archived generation instead of into /full/current (see handleRotationConflict).
func fetchAndMergeRef(ctx context.Context, target string, refName plumbing.ReferenceName) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	fetchTarget, err := ResolveFetchTarget(ctx, target)
	if err != nil {
		return fmt.Errorf("resolve fetch target: %w", err)
	}

	// Fetch to a temp ref
	tmpRefSuffix := strings.ReplaceAll(string(refName), "/", "-")
	tmpRefName := plumbing.ReferenceName("refs/entire-fetch-tmp/" + tmpRefSuffix)
	refSpec := fmt.Sprintf("+%s:%s", refName, tmpRefName)

	// Use --filter=blob:none for a partial fetch that downloads only commits
	// and trees, skipping blobs. The merge only needs the tree structure to
	// combine entries; blobs are already local or fetched on demand.
	fetchArgs := AppendFetchFilterArgs(ctx, []string{"fetch", "--no-tags", fetchTarget, refSpec})
	fetchCmd := CheckpointGitCommand(ctx, fetchTarget, fetchArgs...)
	fetchCmd.Env = append(fetchCmd.Env, "GIT_TERMINAL_PROMPT=0")
	if output, err := fetchCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("fetch failed: %s", output)
	}

	repo, err := OpenRepository(ctx)
	if err != nil {
		return fmt.Errorf("failed to open repository: %w", err)
	}
	defer func() {
		_ = repo.Storer.RemoveReference(tmpRefName) //nolint:errcheck // cleanup is best-effort
	}()

	// Check for rotation conflict on /full/current
	if refName == plumbing.ReferenceName(paths.V2FullCurrentRefName) {
		remoteOnlyArchives, detectErr := detectRemoteOnlyArchives(ctx, target, repo)
		if detectErr == nil && len(remoteOnlyArchives) > 0 {
			return handleRotationConflict(ctx, target, fetchTarget, repo, refName, tmpRefName, remoteOnlyArchives)
		}
	}

	// Standard tree merge (no rotation detected)
	localRef, err := repo.Reference(refName, true)
	if err != nil {
		return fmt.Errorf("failed to get local ref: %w", err)
	}
	localCommit, err := repo.CommitObject(localRef.Hash())
	if err != nil {
		return fmt.Errorf("failed to get local commit: %w", err)
	}
	localTree, err := localCommit.Tree()
	if err != nil {
		return fmt.Errorf("failed to get local tree: %w", err)
	}

	remoteRef, err := repo.Reference(tmpRefName, true)
	if err != nil {
		return fmt.Errorf("failed to get remote ref: %w", err)
	}
	remoteCommit, err := repo.CommitObject(remoteRef.Hash())
	if err != nil {
		return fmt.Errorf("failed to get remote commit: %w", err)
	}
	remoteTree, err := remoteCommit.Tree()
	if err != nil {
		return fmt.Errorf("failed to get remote tree: %w", err)
	}

	entries := make(map[string]object.TreeEntry)
	if err := checkpoint.FlattenTree(repo, localTree, "", entries); err != nil {
		return fmt.Errorf("failed to flatten local tree: %w", err)
	}
	if err := checkpoint.FlattenTree(repo, remoteTree, "", entries); err != nil {
		return fmt.Errorf("failed to flatten remote tree: %w", err)
	}

	mergedTreeHash, err := checkpoint.BuildTreeFromEntries(ctx, repo, entries)
	if err != nil {
		return fmt.Errorf("failed to build merged tree: %w", err)
	}

	mergeCommitHash, err := createMergeCommitCommon(ctx, repo, mergedTreeHash,
		[]plumbing.Hash{localRef.Hash(), remoteRef.Hash()},
		"Merge remote "+shortRefName(refName))
	if err != nil {
		return fmt.Errorf("failed to create merge commit: %w", err)
	}

	newRef := plumbing.NewHashReference(refName, mergeCommitHash)
	if err := repo.Storer.SetReference(newRef); err != nil {
		return fmt.Errorf("failed to update ref: %w", err)
	}

	return nil
}

// detectRemoteOnlyArchives discovers archived generation refs on the remote
// that don't exist locally. Returns them sorted ascending (oldest first).
func detectRemoteOnlyArchives(ctx context.Context, target string, repo *git.Repository) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := CheckpointGitCommand(ctx, target, "ls-remote", target, paths.V2FullRefPrefix+"*")
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ls-remote failed: %w", err)
	}

	var remoteOnly []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		refName := parts[1]
		suffix := strings.TrimPrefix(refName, paths.V2FullRefPrefix)
		if suffix == "current" || !checkpoint.GenerationRefPattern.MatchString(suffix) {
			continue
		}
		// Only check for existence, not hash equality. A locally-present archive
		// could be stale if another machine updated it via rotation conflict recovery,
		// but that's unlikely and the checkpoints are still on /main regardless.
		if _, err := repo.Reference(plumbing.ReferenceName(refName), true); err != nil {
			remoteOnly = append(remoteOnly, suffix)
		}
	}

	sort.Strings(remoteOnly)
	return remoteOnly, nil
}

// handleRotationConflict handles the case where remote /full/current was rotated.
// Merges local /full/current into the latest remote archived generation to avoid
// duplicating checkpoint data, then adopts remote's /full/current as local.
func handleRotationConflict(ctx context.Context, target, fetchTarget string, repo *git.Repository, refName, tmpRefName plumbing.ReferenceName, remoteOnlyArchives []string) error {
	// Use the latest remote-only archive
	latestArchive := remoteOnlyArchives[len(remoteOnlyArchives)-1]
	archiveRefName := plumbing.ReferenceName(paths.V2FullRefPrefix + latestArchive)

	// Fetch the latest archived generation
	archiveTmpRef := plumbing.ReferenceName("refs/entire-fetch-tmp/archive-" + latestArchive)
	archiveRefSpec := fmt.Sprintf("+%s:%s", archiveRefName, archiveTmpRef)
	fetchArgs := AppendFetchFilterArgs(ctx, []string{"fetch", "--no-tags", fetchTarget, archiveRefSpec})
	fetchCmd := CheckpointGitCommand(ctx, fetchTarget, fetchArgs...)
	fetchCmd.Env = append(fetchCmd.Env, "GIT_TERMINAL_PROMPT=0")
	if output, fetchErr := fetchCmd.CombinedOutput(); fetchErr != nil {
		return fmt.Errorf("fetch archived generation failed: %s", output)
	}
	defer func() {
		_ = repo.Storer.RemoveReference(archiveTmpRef) //nolint:errcheck // cleanup is best-effort
	}()

	// Get archived generation state
	archiveRef, err := repo.Reference(archiveTmpRef, true)
	if err != nil {
		return fmt.Errorf("failed to get archived ref: %w", err)
	}
	archiveCommit, err := repo.CommitObject(archiveRef.Hash())
	if err != nil {
		return fmt.Errorf("failed to get archive commit: %w", err)
	}
	archiveTree, err := archiveCommit.Tree()
	if err != nil {
		return fmt.Errorf("failed to get archive tree: %w", err)
	}

	// Get local /full/current state
	localRef, err := repo.Reference(refName, true)
	if err != nil {
		return fmt.Errorf("failed to get local ref: %w", err)
	}
	localCommit, err := repo.CommitObject(localRef.Hash())
	if err != nil {
		return fmt.Errorf("failed to get local commit: %w", err)
	}
	localTree, err := localCommit.Tree()
	if err != nil {
		return fmt.Errorf("failed to get local tree: %w", err)
	}

	// Tree-merge local /full/current into archived generation.
	// Git content-addressing deduplicates shared shard paths automatically.
	entries := make(map[string]object.TreeEntry)
	if err := checkpoint.FlattenTree(repo, archiveTree, "", entries); err != nil {
		return fmt.Errorf("failed to flatten archive tree: %w", err)
	}
	if err := checkpoint.FlattenTree(repo, localTree, "", entries); err != nil {
		return fmt.Errorf("failed to flatten local tree: %w", err)
	}

	// Update generation.json timestamps if present in the merged tree.
	// Use the local /full/current HEAD commit time as the newest checkpoint time
	// (more accurate than time.Now() for cleanup scheduling).
	if genEntry, exists := entries[paths.GenerationFileName]; exists {
		if updatedEntry, updateErr := updateGenerationTimestamps(repo, genEntry.Hash, localCommit.Committer.When.UTC()); updateErr == nil {
			entries[paths.GenerationFileName] = updatedEntry
		} else {
			logging.Warn(ctx, "rotation recovery: failed to update generation timestamps, using stale values",
				slog.String("error", updateErr.Error()),
			)
		}
	}

	mergedTreeHash, err := checkpoint.BuildTreeFromEntries(ctx, repo, entries)
	if err != nil {
		return fmt.Errorf("failed to build merged tree: %w", err)
	}

	// Create commit parented on archive's commit (fast-forward)
	mergeCommitHash, err := createMergeCommitCommon(ctx, repo, mergedTreeHash,
		[]plumbing.Hash{archiveRef.Hash()},
		"Merge local checkpoints into archived generation")
	if err != nil {
		return fmt.Errorf("failed to create merge commit: %w", err)
	}

	// Update local archived ref and push it
	newArchiveRef := plumbing.NewHashReference(archiveRefName, mergeCommitHash)
	if err := repo.Storer.SetReference(newArchiveRef); err != nil {
		return fmt.Errorf("failed to update archive ref: %w", err)
	}

	if _, pushErr := tryPushRef(ctx, target, archiveRefName); pushErr != nil {
		return fmt.Errorf("failed to push updated archive: %w", pushErr)
	}

	// Adopt remote's /full/current as local
	remoteRef, err := repo.Reference(tmpRefName, true)
	if err != nil {
		return fmt.Errorf("failed to get fetched /full/current: %w", err)
	}
	adoptedRef := plumbing.NewHashReference(refName, remoteRef.Hash())
	if err := repo.Storer.SetReference(adoptedRef); err != nil {
		return fmt.Errorf("failed to adopt remote /full/current: %w", err)
	}

	return nil
}

// updateGenerationTimestamps reads generation.json from a blob, updates
// newest_checkpoint_at if the provided newestFromLocal is newer, and returns
// an updated tree entry. Uses the local commit timestamp rather than
// time.Now() so cleanup scheduling reflects actual checkpoint creation time.
func updateGenerationTimestamps(repo *git.Repository, genBlobHash plumbing.Hash, newestFromLocal time.Time) (object.TreeEntry, error) {
	blob, err := repo.BlobObject(genBlobHash)
	if err != nil {
		return object.TreeEntry{}, fmt.Errorf("failed to read generation blob: %w", err)
	}
	reader, err := blob.Reader()
	if err != nil {
		return object.TreeEntry{}, fmt.Errorf("failed to open generation blob reader: %w", err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return object.TreeEntry{}, fmt.Errorf("failed to read generation blob data: %w", err)
	}

	var gen checkpoint.GenerationMetadata
	if err := json.Unmarshal(data, &gen); err != nil {
		return object.TreeEntry{}, fmt.Errorf("failed to parse generation.json: %w", err)
	}

	if newestFromLocal.After(gen.NewestCheckpointAt) {
		gen.NewestCheckpointAt = newestFromLocal
	}

	updatedData, err := jsonutil.MarshalIndentWithNewline(gen, "", "  ")
	if err != nil {
		return object.TreeEntry{}, fmt.Errorf("failed to marshal generation.json: %w", err)
	}

	newBlobHash, err := checkpoint.CreateBlobFromContent(repo, updatedData)
	if err != nil {
		return object.TreeEntry{}, fmt.Errorf("failed to create generation blob: %w", err)
	}

	return object.TreeEntry{
		Name: paths.GenerationFileName,
		Mode: filemode.Regular,
		Hash: newBlobHash,
	}, nil
}

// pushV2Refs pushes v2 checkpoint refs to the target.
// Pushes /main, /full/current, and the latest archived generation (if any).
// Older archived generations are immutable and were pushed when created.
func pushV2Refs(ctx context.Context, target string) {
	_ = pushRefIfNeeded(ctx, target, plumbing.ReferenceName(paths.V2MainRefName))        //nolint:errcheck // pushRefIfNeeded handles errors internally
	_ = pushRefIfNeeded(ctx, target, plumbing.ReferenceName(paths.V2FullCurrentRefName)) //nolint:errcheck // pushRefIfNeeded handles errors internally

	// Push only the latest archived generation (most likely to be newly created)
	repo, err := OpenRepository(ctx)
	if err != nil {
		return
	}
	v2URL, err := remote.FetchURL(ctx)
	if err != nil {
		logging.Debug(ctx, "push-v2: using origin for archived generation fetch remote",
			slog.String("error", err.Error()),
		)
	}
	store := checkpoint.NewV2GitStore(repo, v2URL)
	archived, err := store.ListArchivedGenerations()
	if err != nil || len(archived) == 0 {
		return
	}
	latest := archived[len(archived)-1]
	_ = pushRefIfNeeded(ctx, target, plumbing.ReferenceName(paths.V2FullRefPrefix+latest)) //nolint:errcheck // pushRefIfNeeded handles errors internally
}

// shortRefName returns a human-readable short form of a ref name for log output.
// e.g., "refs/entire/checkpoints/v2/main" -> "v2/main"
func shortRefName(refName plumbing.ReferenceName) string {
	const prefix = "refs/entire/checkpoints/"
	s := string(refName)
	if strings.HasPrefix(s, prefix) {
		return s[len(prefix):]
	}
	return s
}
