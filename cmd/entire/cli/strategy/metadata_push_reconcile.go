package strategy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

const metadataCleanupFetchPurpose = "metadata-cleanup-v1"

type shallowMetadataRepairError struct {
	Boundary plumbing.Hash
	Path     string
}

func (e *shallowMetadataRepairError) Error() string {
	return fmt.Sprintf(
		"checkpoint metadata at %s is oversized at shallow boundary %s; fetch the missing checkpoint history before retrying",
		e.Path, e.Boundary,
	)
}

func prepareOversizedV1ForPush(ctx context.Context, repo *git.Repository, target string, threshold int64) error {
	localTip, err := readV1Tip(repo, plumbing.NewBranchReferenceName(paths.MetadataBranchName))
	if err != nil {
		return fmt.Errorf("read local checkpoint history: %w", err)
	}
	if localTip.IsZero() {
		return nil
	}
	hasOversizedHistory, err := hasOversizedMetadataIntroducedSince(
		ctx, repo, localTip, plumbing.ZeroHash, threshold,
	)
	if err != nil {
		return err
	}
	if !hasOversizedHistory {
		return nil
	}
	remoteTip, err := fetchV1TipForMetadataCleanup(ctx, repo, target)
	if err != nil {
		return err
	}
	_, _, err = reconcileOversizedV1ForPush(ctx, repo, localTip, remoteTip, threshold)
	return err
}

func fetchV1TipForMetadataCleanup(ctx context.Context, repo *git.Repository, target string) (plumbing.Hash, error) {
	worktree, err := repo.Worktree()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("resolve worktree for checkpoint metadata probe: %w", err)
	}
	ref := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	probeCtx, cancel := context.WithTimeout(ctx, checkpointRemoteFetchTimeout)
	defer cancel()
	out, err := remote.LsRemoteInDir(probeCtx, worktree.Filesystem().Root(), target, ref.String())
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("probe %s on %s: %w", ref, remote.RedactURLOrPath(target), err)
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return plumbing.ZeroHash, nil
	}

	tmpRef, err := newFetchTmpRef(metadataCleanupFetchPurpose)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	defer func() {
		_ = repo.Storer.RemoveReference(tmpRef) //nolint:errcheck // cleanup is best-effort
	}()
	if err := fetchURLIntoTmpRef(
		ctx,
		worktree.Filesystem().Root(),
		target,
		ref.String(),
		tmpRef.String(),
		"v1 for metadata cleanup",
		true,
		checkpointRemoteFetchTimeout,
	); err != nil {
		return plumbing.ZeroHash, err
	}
	fetched, err := repo.Reference(tmpRef, true)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("read fetched checkpoint history: %w", err)
	}
	return fetched.Hash(), nil
}

// reconcileOversizedV1ForPush rewrites local oversized history. An existing
// remote must prove where its shared checkpoint content ends before replay.
func reconcileOversizedV1ForPush(
	ctx context.Context,
	repo *git.Repository,
	observedLocalTip, remoteTip plumbing.Hash,
	threshold int64,
) (plumbing.Hash, bool, error) {
	if observedLocalTip.IsZero() {
		return observedLocalTip, false, nil
	}
	repoPath, err := getRepoPath(repo)
	if err != nil {
		return plumbing.ZeroHash, false, fmt.Errorf("get repository path: %w", err)
	}

	mergeBase := plumbing.ZeroHash
	if !remoteTip.IsZero() {
		mergeBase, err = computeMergeBaseWithGit(ctx, repoPath, observedLocalTip, remoteTip)
		if err != nil {
			return plumbing.ZeroHash, false, fmt.Errorf("compute original merge-base: %w", err)
		}
		remoteHasOversizedHistory, scanErr := hasOversizedMetadataIntroducedSince(
			ctx, repo, remoteTip, plumbing.ZeroHash, threshold,
		)
		if scanErr != nil {
			return plumbing.ZeroHash, false, fmt.Errorf("scan remote checkpoint metadata: %w", scanErr)
		}
		if remoteHasOversizedHistory {
			return observedLocalTip, false, nil
		}
	}
	hasOversizedLocalHistory, err := hasOversizedMetadataIntroducedSince(
		ctx, repo, observedLocalTip, mergeBase, threshold,
	)
	if err != nil {
		return plumbing.ZeroHash, false, err
	}
	if !hasOversizedLocalHistory {
		return observedLocalTip, false, nil
	}

	shallow, err := loadShallowHashes(ctx, repoPath)
	if err != nil {
		return plumbing.ZeroHash, false, fmt.Errorf("load shallow boundaries: %w", err)
	}
	rewriter := newMetadataRewriter(ctx, repo, threshold)
	rewriter.shallow = shallow
	rewrittenTip, err := rewriter.rewriteHistory(observedLocalTip)
	if err != nil {
		return plumbing.ZeroHash, false, fmt.Errorf("rewrite oversized local metadata: %w", err)
	}
	if rewriter.commitsRewritten == 0 {
		return observedLocalTip, false, nil
	}
	if remoteTip.IsZero() {
		if err := atomicSetV1Ref(ctx, repo, observedLocalTip, rewrittenTip); err != nil {
			return plumbing.ZeroHash, false, err
		}
		return rewrittenTip, true, nil
	}

	rewrittenBoundary, originalBoundary, found, err := findSharedRewrittenTree(
		ctx, repo, repoPath, rewrittenTip, remoteTip, rewriter.commits,
	)
	if err != nil {
		return plumbing.ZeroHash, false, err
	}
	if !found {
		return observedLocalTip, false, nil
	}

	if !mergeBase.IsZero() {
		safe, err := boundaryAtOrAfterMergeBase(ctx, repoPath, mergeBase, originalBoundary)
		if err != nil {
			return plumbing.ZeroHash, false, err
		}
		if !safe {
			return observedLocalTip, false, nil
		}
	}

	localOnly, err := collectFirstParentCommitsSince(ctx, repo, repoPath, rewrittenTip, rewrittenBoundary)
	if err != nil {
		return plumbing.ZeroHash, false, fmt.Errorf("collect local checkpoints after repaired boundary: %w", err)
	}

	newTip := remoteTip
	if len(localOnly) > 0 {
		shallow, shallowErr := loadShallowHashes(ctx, repoPath)
		if shallowErr != nil {
			return plumbing.ZeroHash, false, fmt.Errorf("load shallow boundaries: %w", shallowErr)
		}
		newTip, err = cherryPickOnto(ctx, repo, remoteTip, localOnly, shallow)
		if err != nil {
			return plumbing.ZeroHash, false, fmt.Errorf("replay local checkpoints onto repaired remote: %w", err)
		}
	}

	if err := atomicSetV1Ref(ctx, repo, observedLocalTip, newTip); err != nil {
		return plumbing.ZeroHash, false, err
	}
	return newTip, true, nil
}

func oversizedMetadataPathInTree(repo *git.Repository, root plumbing.Hash, threshold int64) (string, error) {
	type pendingTree struct {
		hash   plumbing.Hash
		prefix string
	}
	stack := []pendingTree{{hash: root}}
	seen := make(map[plumbing.Hash]struct{})
	for len(stack) > 0 {
		last := len(stack) - 1
		pending := stack[last]
		stack = stack[:last]
		if _, ok := seen[pending.hash]; ok {
			continue
		}
		seen[pending.hash] = struct{}{}

		tree, err := repo.TreeObject(pending.hash)
		if err != nil {
			return "", fmt.Errorf("load tree %s: %w", pending.hash, err)
		}
		for _, entry := range tree.Entries {
			entryPath := entry.Name
			if pending.prefix != "" {
				entryPath = pending.prefix + "/" + entry.Name
			}
			if entry.Mode == filemode.Dir {
				stack = append(stack, pendingTree{hash: entry.Hash, prefix: entryPath})
				continue
			}
			if path.Base(entryPath) != paths.MetadataFileName || !isFileMode(entry.Mode) {
				continue
			}
			blob, err := repo.BlobObject(entry.Hash)
			if err != nil {
				return "", fmt.Errorf("load metadata blob %s at %s: %w", entry.Hash, entryPath, err)
			}
			if blob.Size > threshold {
				return entryPath, nil
			}
		}
	}
	return "", nil
}

func hasOversizedMetadataIntroducedSince(
	ctx context.Context,
	repo *git.Repository,
	tip, boundary plumbing.Hash,
	threshold int64,
) (bool, error) {
	repoPath, err := getRepoPath(repo)
	if err != nil {
		return false, fmt.Errorf("get repository path for metadata scan: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	rangeSpec := tip.String()
	if !boundary.IsZero() {
		rangeSpec = boundary.String() + ".." + tip.String()
	}
	// Raw path records keep metadata identity when one blob is also stored under
	// an unrelated path; full merge diffs include side-parent and resolution data.
	objectsCmd := exec.CommandContext(
		ctx,
		"git", "log", "--root", "--full-history", "-m", "--raw", "--no-renames",
		"--diff-filter=AMT", "--format=", "--no-abbrev", "-z", rangeSpec,
		"--", ":(glob)**/metadata.json",
	)
	objectsCmd.Dir = repoPath
	stdout, err := objectsCmd.StdoutPipe()
	if err != nil {
		return false, fmt.Errorf("open checkpoint metadata scan: %w", err)
	}
	var stderr bytes.Buffer
	objectsCmd.Stderr = &stderr
	if err := objectsCmd.Start(); err != nil {
		return false, fmt.Errorf("start checkpoint metadata scan: %w", err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = objectsCmd.Process.Kill() //nolint:errcheck // best-effort after a parse/read failure
			_ = objectsCmd.Wait()         //nolint:errcheck // reap after best-effort termination
		}
	}()

	seen := make(map[plumbing.Hash]struct{})
	found := false
	reader := bufio.NewReader(stdout)
	for {
		rawRecord, readErr := reader.ReadString(0)
		if errors.Is(readErr, io.EOF) && rawRecord == "" {
			break
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return false, fmt.Errorf("read checkpoint metadata scan: %w", readErr)
		}
		record := strings.TrimSpace(strings.TrimSuffix(rawRecord, "\x00"))
		if record == "" {
			if errors.Is(readErr, io.EOF) {
				break
			}
			continue
		}
		if !strings.HasPrefix(record, ":") {
			return false, fmt.Errorf("parse checkpoint metadata scan record %q", record)
		}
		pathField, pathErr := reader.ReadString(0)
		if pathErr != nil && !errors.Is(pathErr, io.EOF) {
			return false, fmt.Errorf("read checkpoint metadata path: %w", pathErr)
		}
		metadataPath := strings.TrimSuffix(pathField, "\x00")
		if path.Base(metadataPath) != paths.MetadataFileName {
			return false, fmt.Errorf("unexpected checkpoint metadata path %q", metadataPath)
		}

		if err := ctx.Err(); err != nil {
			return false, fmt.Errorf("scan local checkpoint metadata: %w", err)
		}
		fields := strings.Fields(record)
		if len(fields) < 5 {
			return false, fmt.Errorf("parse checkpoint metadata scan record %q", record)
		}
		mode, err := strconv.ParseUint(fields[1], 8, 32)
		if err != nil {
			return false, fmt.Errorf("parse checkpoint metadata mode %q: %w", fields[1], err)
		}
		if !isFileMode(filemode.FileMode(mode)) || found {
			continue
		}
		hash := plumbing.NewHash(fields[3])
		if _, exists := seen[hash]; exists {
			continue
		}
		seen[hash] = struct{}{}
		blob, err := repo.BlobObject(hash)
		if err != nil {
			return false, fmt.Errorf("load metadata blob %s at %s: %w", hash, metadataPath, err)
		}
		if blob.Size > threshold {
			found = true
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	if err := objectsCmd.Wait(); err != nil {
		waited = true
		return false, fmt.Errorf("list local checkpoint metadata objects: %s: %w", strings.TrimSpace(stderr.String()), err)
	}
	waited = true
	return found, nil
}

func findSharedRewrittenTree(
	ctx context.Context,
	repo *git.Repository,
	repoPath string,
	rewrittenLocalTip, remoteTip plumbing.Hash,
	originalToRewritten map[plumbing.Hash]plumbing.Hash,
) (rewritten, original plumbing.Hash, found bool, err error) {
	remoteTrees := make(map[plumbing.Hash]struct{})
	remoteHistory, err := reachableHistory(ctx, repo, repoPath, remoteTip)
	if err != nil {
		return plumbing.ZeroHash, plumbing.ZeroHash, false, fmt.Errorf("log remote checkpoint history: %w", err)
	}
	for _, commit := range remoteHistory {
		if err := ctx.Err(); err != nil {
			return plumbing.ZeroHash, plumbing.ZeroHash, false, fmt.Errorf("scan remote checkpoint trees: %w", err)
		}
		remoteTrees[commit.TreeHash] = struct{}{}
	}

	rewrittenToOriginal := make(map[plumbing.Hash]plumbing.Hash, len(originalToRewritten))
	for oldHash, newHash := range originalToRewritten {
		rewrittenToOriginal[newHash] = oldHash
	}
	localHistory, err := firstParentHistory(ctx, repo, repoPath, rewrittenLocalTip)
	if err != nil {
		return plumbing.ZeroHash, plumbing.ZeroHash, false, fmt.Errorf("log rewritten local checkpoint history: %w", err)
	}
	for _, commit := range localHistory {
		if err := ctx.Err(); err != nil {
			return plumbing.ZeroHash, plumbing.ZeroHash, false, fmt.Errorf("scan rewritten local checkpoint trees: %w", err)
		}
		if _, exists := remoteTrees[commit.TreeHash]; !exists {
			continue
		}
		oldHash, exists := rewrittenToOriginal[commit.Hash]
		if !exists {
			return plumbing.ZeroHash, plumbing.ZeroHash, false, fmt.Errorf("rewritten commit %s has no original", commit.Hash)
		}
		return commit.Hash, oldHash, true, nil
	}
	return plumbing.ZeroHash, plumbing.ZeroHash, false, nil
}

func reachableHistory(
	ctx context.Context,
	repo *git.Repository,
	repoPath string,
	tip plumbing.Hash,
) ([]*object.Commit, error) {
	return historyFromRevList(ctx, repo, repoPath, tip.String())
}

func firstParentHistory(
	ctx context.Context,
	repo *git.Repository,
	repoPath string,
	tip plumbing.Hash,
) ([]*object.Commit, error) {
	return historyFromRevList(ctx, repo, repoPath, "--first-parent", tip.String())
}

func historyFromRevList(
	ctx context.Context,
	repo *git.Repository,
	repoPath string,
	args ...string,
) ([]*object.Commit, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"rev-list"}, args...)...)
	cmd.Dir = repoPath
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git rev-list failed: %w", err)
	}
	hashes := strings.Fields(string(output))
	commits := make([]*object.Commit, 0, len(hashes))
	for _, value := range hashes {
		commit, err := repo.CommitObject(plumbing.NewHash(value))
		if err != nil {
			return nil, fmt.Errorf("load history commit %s: %w", value, err)
		}
		commits = append(commits, commit)
	}
	return commits, nil
}

func collectFirstParentCommitsSince(
	ctx context.Context,
	repo *git.Repository,
	repoPath string,
	tip, boundary plumbing.Hash,
) ([]*object.Commit, error) {
	cmd := exec.CommandContext(
		ctx, "git", "rev-list", "--first-parent", "--reverse", boundary.String()+".."+tip.String(),
	)
	cmd.Dir = repoPath
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git rev-list failed: %w", err)
	}
	hashes := strings.Fields(string(output))
	if len(hashes) > MaxCommitTraversalDepth {
		return nil, fmt.Errorf("commit chain exceeded %d commits; aborting rebase", MaxCommitTraversalDepth)
	}
	commits := make([]*object.Commit, 0, len(hashes))
	for _, value := range hashes {
		commit, err := repo.CommitObject(plumbing.NewHash(value))
		if err != nil {
			return nil, fmt.Errorf("load replay commit %s: %w", value, err)
		}
		commits = append(commits, commit)
	}
	return commits, nil
}

func computeMergeBaseWithGit(
	ctx context.Context,
	repoPath string,
	local, remote plumbing.Hash,
) (plumbing.Hash, error) {
	cmd := exec.CommandContext(ctx, "git", "merge-base", "--all", local.String(), remote.String())
	cmd.Dir = repoPath
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return plumbing.ZeroHash, nil
		}
		return plumbing.ZeroHash, fmt.Errorf("git merge-base failed: %w", err)
	}
	bases := strings.Fields(string(output))
	if len(bases) > 1 {
		return plumbing.ZeroHash, errors.New("multiple merge bases prevent safe checkpoint reconciliation")
	}
	if len(bases) == 0 {
		return plumbing.ZeroHash, nil
	}
	return plumbing.NewHash(bases[0]), nil
}

func boundaryAtOrAfterMergeBase(
	ctx context.Context,
	repoPath string,
	mergeBase, boundary plumbing.Hash,
) (bool, error) {
	if mergeBase.Equal(boundary) {
		return true, nil
	}
	cmd := exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", mergeBase.String(), boundary.String())
	cmd.Dir = repoPath
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("compare repaired boundary with original merge-base: %w", err)
	}
	return true, nil
}
