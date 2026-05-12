package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
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

// tryPushRef attempts to push a custom ref using an explicit refspec.
func tryPushRef(ctx context.Context, target string, refName plumbing.ReferenceName) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	refSpec := fmt.Sprintf("%s:%s", refName, refName)
	result, err := remote.Push(ctx, target, refSpec)
	outputStr := result.Output
	if err != nil {
		return classifyPushFailure(ctx, outputStr, err)
	}

	return nil
}

type v2RefPushResult struct {
	refName plumbing.ReferenceName
	result  pushResult
	err     error
}

type pendingV2FullGenerationPublicationResult struct {
	successfulRefs          []plumbing.ReferenceName
	fullCurrentResetHandled bool
}

func tryPushV2Refs(ctx context.Context, target string, refs []plumbing.ReferenceName) []v2RefPushResult {
	if len(refs) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	result, err := remote.PushWithOptions(ctx, remote.PushOptions{
		Remote:   target,
		RefSpecs: refSpecsForRefs(refs),
	})
	return parsePushRefResults(ctx, result.Output, refs, err)
}

func pushV2RefsWithRecovery(ctx context.Context, target string, refs []plumbing.ReferenceName) []v2RefPushResult {
	resultsByRef := make(map[plumbing.ReferenceName]v2RefPushResult, len(refs))
	var retryRefs []plumbing.ReferenceName

	for _, result := range tryPushV2Refs(ctx, target, refs) {
		if result.err == nil {
			resultsByRef[result.refName] = result
			continue
		}
		if !errors.Is(result.err, errNonFastForward) {
			resultsByRef[result.refName] = result
			continue
		}

		shortRef := shortRefName(result.refName)
		if err := fetchAndMergeRef(ctx, target, result.refName); err != nil {
			resultsByRef[result.refName] = v2RefPushResult{
				refName: result.refName,
				err:     fmt.Errorf("couldn't sync %s: %w", shortRef, err),
			}
			continue
		}
		retryRefs = append(retryRefs, result.refName)
	}

	if len(retryRefs) > 0 {
		for _, result := range tryPushV2Refs(ctx, target, retryRefs) {
			if result.err != nil {
				result.err = fmt.Errorf("failed to push %s after sync: %w", shortRefName(result.refName), result.err)
			}
			resultsByRef[result.refName] = result
		}
	}

	results := make([]v2RefPushResult, 0, len(refs))
	for _, refName := range refs {
		result, ok := resultsByRef[refName]
		if !ok {
			result = v2RefPushResult{
				refName: refName,
				err:     errors.New("push result missing"),
			}
		}
		results = append(results, result)
	}
	return results
}

func publishPendingV2FullGenerationPublications(
	ctx context.Context,
	repo *git.Repository,
	store *checkpoint.V2GitStore,
	target string,
	publications []checkpoint.PendingV2FullGenerationPublication,
) (pendingV2FullGenerationPublicationResult, error) {
	var result pendingV2FullGenerationPublicationResult
	if len(publications) == 0 {
		return result, nil
	}

	currentRefName := plumbing.ReferenceName(paths.V2FullCurrentRefName)
	var localCurrentRef *plumbing.Reference
	for {
		latestResetPublicationIndex := latestPendingFullCurrentResetPublicationIndex(publications)
		if latestResetPublicationIndex == -1 {
			break
		}
		var err error
		localCurrentRef, err = repo.Reference(currentRefName, true)
		if err != nil {
			return result, fmt.Errorf("read local %s: %w", shortRefName(currentRefName), err)
		}
		latestResetPublication := publications[latestResetPublicationIndex]
		if pendingResetPublicationMatchesLocalCurrent(ctx, repo, latestResetPublication, localCurrentRef.Hash()) {
			break
		}
		if err := store.RemovePendingFullGenerationPublications(ctx, []checkpoint.PendingV2FullGenerationPublication{latestResetPublication}); err != nil {
			return result, fmt.Errorf("clear stale pending v2 full generation publications: %w", err)
		}
		publications = slices.Delete(publications, latestResetPublicationIndex, latestResetPublicationIndex+1)
	}

	archiveRefs := pendingFullArchiveRefs(publications)
	if len(archiveRefs) > 0 {
		var archivePushErr error
		for _, pushResult := range tryPushV2Refs(ctx, target, archiveRefs) {
			if pushResult.err != nil {
				if archivePushErr == nil {
					archivePushErr = fmt.Errorf("push pending archive %s: %w", shortRefName(pushResult.refName), pushResult.err)
				}
				continue
			}
			result.successfulRefs = append(result.successfulRefs, pushResult.refName)
		}
		if archivePushErr != nil {
			return result, archivePushErr
		}
	}

	resetPublications := pendingFullCurrentResetPublications(publications)
	if len(resetPublications) == 0 {
		if err := store.RemovePendingFullGenerationPublications(ctx, publications); err != nil {
			return result, fmt.Errorf("clear pending v2 full generation publications: %w", err)
		}
		return result, nil
	}

	remoteCurrentHash, remoteCurrentFound, err := lsRemoteRefHash(ctx, target, currentRefName)
	if err != nil {
		return result, fmt.Errorf("read remote %s: %w", shortRefName(currentRefName), err)
	}
	if remoteCurrentFound && remoteCurrentHash == localCurrentRef.Hash() {
		if err := store.RemovePendingFullGenerationPublications(ctx, publications); err != nil {
			return result, fmt.Errorf("clear pending v2 full generation publications: %w", err)
		}
		result.fullCurrentResetHandled = true
		return result, nil
	}

	if remoteCurrentFound && !pendingResetPublicationsContainAncestor(ctx, repo, resetPublications, remoteCurrentHash) {
		return result, fmt.Errorf("remote %s at %s is not covered by pending local archives", shortRefName(currentRefName), remoteCurrentHash)
	}

	expectedRemoteHash := ""
	if remoteCurrentFound {
		expectedRemoteHash = remoteCurrentHash.String()
	}
	currentRefSpec := fmt.Sprintf("%s:%s", currentRefName, currentRefName)
	if err := pushWithLease(ctx, target, currentRefSpec, currentRefName.String(), expectedRemoteHash, "push rotated "+shortRefName(currentRefName)); err != nil {
		return result, fmt.Errorf("push rotated %s: %w", shortRefName(currentRefName), err)
	}
	result.successfulRefs = append(result.successfulRefs, currentRefName)
	result.fullCurrentResetHandled = true

	if err := store.RemovePendingFullGenerationPublications(ctx, publications); err != nil {
		return result, fmt.Errorf("clear pending v2 full generation publications: %w", err)
	}
	return result, nil
}

func pendingFullArchiveRefs(publications []checkpoint.PendingV2FullGenerationPublication) []plumbing.ReferenceName {
	seen := make(map[plumbing.ReferenceName]struct{}, len(publications))
	refs := make([]plumbing.ReferenceName, 0, len(publications))
	for _, publication := range publications {
		suffix, ok := strings.CutPrefix(publication.ArchiveRefName, paths.V2FullRefPrefix)
		if !ok || !checkpoint.GenerationRefPattern.MatchString(suffix) {
			continue
		}
		refName := plumbing.ReferenceName(paths.V2FullRefPrefix + suffix)
		if _, ok := seen[refName]; ok {
			continue
		}
		seen[refName] = struct{}{}
		refs = append(refs, refName)
	}
	return refs
}

func pendingFullCurrentResetPublications(publications []checkpoint.PendingV2FullGenerationPublication) []checkpoint.PendingV2FullGenerationPublication {
	resetPublications := make([]checkpoint.PendingV2FullGenerationPublication, 0, len(publications))
	for _, publication := range publications {
		if publication.PreviousFullCurrentHash == "" && publication.ResetFullCurrentRootHash == "" {
			continue
		}
		resetPublications = append(resetPublications, publication)
	}
	return resetPublications
}

func pendingResetPublicationsContainAncestor(ctx context.Context, repo *git.Repository, publications []checkpoint.PendingV2FullGenerationPublication, hash plumbing.Hash) bool {
	for _, publication := range publications {
		if publication.ArchiveCommitHash == "" {
			continue
		}
		if IsAncestorOf(ctx, repo, hash, plumbing.NewHash(publication.ArchiveCommitHash)) {
			return true
		}
	}
	return false
}

func latestPendingFullCurrentResetPublicationIndex(publications []checkpoint.PendingV2FullGenerationPublication) int {
	for i := len(publications) - 1; i >= 0; i-- {
		if publications[i].PreviousFullCurrentHash == "" && publications[i].ResetFullCurrentRootHash == "" {
			continue
		}
		return i
	}
	return -1
}

func pendingResetPublicationMatchesLocalCurrent(ctx context.Context, repo *git.Repository, publication checkpoint.PendingV2FullGenerationPublication, localCurrentHash plumbing.Hash) bool {
	if len(publication.ResetFullCurrentRootHash) != 40 {
		return false
	}
	return IsAncestorOf(ctx, repo, plumbing.NewHash(publication.ResetFullCurrentRootHash), localCurrentHash)
}

func lsRemoteRefHash(ctx context.Context, target string, refName plumbing.ReferenceName) (plumbing.Hash, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	output, err := remote.LsRemote(ctx, target, refName.String())
	if err != nil {
		return plumbing.ZeroHash, false, fmt.Errorf("ls-remote %s: %w", refName, err)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 || parts[1] != refName.String() {
			continue
		}
		if len(parts[0]) != 40 {
			return plumbing.ZeroHash, false, fmt.Errorf("invalid remote hash %q for %s", parts[0], refName)
		}
		return plumbing.NewHash(parts[0]), true, nil
	}
	return plumbing.ZeroHash, false, nil
}

func refSpecsForRefs(refs []plumbing.ReferenceName) []string {
	refSpecs := make([]string, 0, len(refs))
	for _, refName := range refs {
		refSpecs = append(refSpecs, fmt.Sprintf("%s:%s", refName, refName))
	}
	return refSpecs
}

func parsePushRefResults(ctx context.Context, output string, refs []plumbing.ReferenceName, pushErr error) []v2RefPushResult {
	parsed := make(map[plumbing.ReferenceName]v2RefPushResult, len(refs))
	for line := range strings.SplitSeq(output, "\n") {
		result, ok := parsePushRefStatusLine(line)
		if ok {
			parsed[result.refName] = result
		}
	}

	var fallbackErr error
	if pushErr != nil {
		fallbackErr = classifyPushFailure(ctx, output, pushErr)
		if len(parsed) > 0 && len(parsed) < len(refs) {
			logging.Debug(ctx, "push-v2: incomplete push porcelain output",
				slog.Int("parsed_refs", len(parsed)),
				slog.Int("expected_refs", len(refs)),
				slog.String("error", pushErr.Error()),
				slog.String("output", output),
			)
		}
	}

	results := make([]v2RefPushResult, 0, len(refs))
	for _, refName := range refs {
		if result, ok := parsed[refName]; ok {
			results = append(results, result)
			continue
		}
		if pushErr != nil && len(parsed) > 0 {
			results = append(results, v2RefPushResult{
				refName: refName,
				err:     fmt.Errorf("status missing for %s", shortRefName(refName)),
			})
			continue
		}
		err := fallbackErr
		if err != nil {
			err = fmt.Errorf("failed to push %s: %w", shortRefName(refName), err)
		}
		results = append(results, v2RefPushResult{
			refName: refName,
			err:     err,
		})
	}
	return results
}

func parsePushRefStatusLine(line string) (v2RefPushResult, bool) {
	fields := strings.Split(line, "\t")
	if len(fields) < 2 || fields[0] == "" {
		return v2RefPushResult{}, false
	}

	refName, ok := pushStatusRef(fields[1])
	if !ok {
		return v2RefPushResult{}, false
	}

	switch fields[0][0] {
	case '!':
		err := classifyPushOutput(strings.Join(fields[2:], "\t"))
		return v2RefPushResult{
			refName: refName,
			err:     fmt.Errorf("failed to push %s: %w", shortRefName(refName), err),
		}, true
	case '=':
		return v2RefPushResult{
			refName: refName,
			result:  pushResult{upToDate: true},
		}, true
	default:
		return v2RefPushResult{refName: refName}, true
	}
}

func pushStatusRef(statusRef string) (plumbing.ReferenceName, bool) {
	_, dst, ok := strings.Cut(statusRef, ":")
	if !ok || dst == "" {
		return "", false
	}
	return plumbing.ReferenceName(dst), true
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

	fetchTarget, err := remote.ResolveFetchTarget(ctx, target)
	if err != nil {
		return fmt.Errorf("resolve fetch target: %w", err)
	}

	// Fetch to a temp ref
	tmpRefSuffix := strings.ReplaceAll(string(refName), "/", "-")
	tmpRefName := plumbing.ReferenceName("refs/entire-fetch-tmp/" + tmpRefSuffix)
	refSpec := fmt.Sprintf("+%s:%s", refName, tmpRefName)

	// Recovery flattens fetched trees recursively, so it needs a complete object
	// graph instead of the normal blobless sync fetch.
	if output, err := remote.Fetch(ctx, remote.FetchOptions{
		Remote:    fetchTarget,
		RefSpecs:  []string{refSpec},
		NoTags:    true,
		NoFilter:  true,
		ExtraArgs: []string{"--no-write-fetch-head"},
	}); err != nil {
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
		remoteRotationArchives, detectErr := detectRemoteRotationArchives(ctx, target, repo)
		if detectErr == nil && len(remoteRotationArchives) > 0 {
			return handleRotationConflict(ctx, target, fetchTarget, repo, refName, tmpRefName, remoteRotationArchives)
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

// detectRemoteRotationArchives discovers archived generation refs on the remote
// that are missing locally or whose local ref hash differs from the remote ref
// hash. Returns them sorted ascending (oldest first).
func detectRemoteRotationArchives(ctx context.Context, target string, repo *git.Repository) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	output, err := remote.LsRemote(ctx, target, paths.V2FullRefPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("ls-remote failed: %w", err)
	}

	var remoteRotationArchives []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(output)), "\n") {
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
		if len(parts[0]) != 40 {
			return nil, fmt.Errorf("invalid remote archive hash %q for %s", parts[0], refName)
		}
		remoteHash := plumbing.NewHash(parts[0])
		localRef, err := repo.Reference(plumbing.ReferenceName(refName), true)
		if err != nil || localRef.Hash() != remoteHash {
			remoteRotationArchives = append(remoteRotationArchives, suffix)
		}
	}

	sort.Strings(remoteRotationArchives)
	return remoteRotationArchives, nil
}

type fetchedRemoteRotationArchive struct {
	repo       *git.Repository
	refName    plumbing.ReferenceName
	tmpRefName plumbing.ReferenceName
	ref        *plumbing.Reference
	tree       *object.Tree
}

func readFetchedRemoteRotationArchive(repo *git.Repository, archive string) (fetchedRemoteRotationArchive, error) {
	archiveRefName := plumbing.ReferenceName(paths.V2FullRefPrefix + archive)
	archiveTmpRef := archiveTmpRefName(archive)
	archiveRef, err := repo.Reference(archiveTmpRef, true)
	if err != nil {
		return fetchedRemoteRotationArchive{}, fmt.Errorf("failed to get archived ref: %w", err)
	}
	archiveCommit, err := repo.CommitObject(archiveRef.Hash())
	if err != nil {
		return fetchedRemoteRotationArchive{}, fmt.Errorf("failed to get archive commit: %w", err)
	}
	archiveTree, err := archiveCommit.Tree()
	if err != nil {
		return fetchedRemoteRotationArchive{}, fmt.Errorf("failed to get archive tree: %w", err)
	}

	return fetchedRemoteRotationArchive{
		repo:       repo,
		refName:    archiveRefName,
		tmpRefName: archiveTmpRef,
		ref:        archiveRef,
		tree:       archiveTree,
	}, nil
}

func fetchRelatedRemoteRotationArchive(ctx context.Context, fetchTarget string, archives []string, localCurrentHash plumbing.Hash) (fetchedRemoteRotationArchive, error) {
	refSpecs := make([]string, 0, len(archives))
	archiveTmpRefs := make([]plumbing.ReferenceName, 0, len(archives))

	for _, archive := range archives {
		archiveRefName := plumbing.ReferenceName(paths.V2FullRefPrefix + archive)
		archiveTmpRef := archiveTmpRefName(archive)
		refSpecs = append(refSpecs, fmt.Sprintf("+%s:%s", archiveRefName, archiveTmpRef))
		archiveTmpRefs = append(archiveTmpRefs, archiveTmpRef)
	}

	// These archive commits are read immediately through go-git for tree
	// flattening, so fetch the complete refs rather than blobless packfiles.
	if output, fetchErr := remote.Fetch(ctx, remote.FetchOptions{
		Remote:    fetchTarget,
		RefSpecs:  refSpecs,
		NoTags:    true,
		NoFilter:  true,
		ExtraArgs: []string{"--no-write-fetch-head"},
	}); fetchErr != nil {
		if repo, openErr := OpenRepository(ctx); openErr == nil {
			cleanupFetchedArchiveTmpRefs(repo, archiveTmpRefs)
		}
		return fetchedRemoteRotationArchive{}, fmt.Errorf("fetch archived generations failed: %s", output)
	}

	repo, err := OpenRepository(ctx)
	if err != nil {
		return fetchedRemoteRotationArchive{}, fmt.Errorf("reopen repository after fetching archived generations: %w", err)
	}
	tmpRefsToCleanup := archiveTmpRefs
	defer func() {
		cleanupFetchedArchiveTmpRefs(repo, tmpRefsToCleanup)
	}()

	localCurrentAncestors, ok := currentGenerationAncestors(ctx, repo, localCurrentHash)
	if !ok {
		return fetchedRemoteRotationArchive{}, errors.New("failed to read local /full/current history")
	}
	for _, archive := range archives {
		fetched, err := readFetchedRemoteRotationArchive(repo, archive)
		if err != nil {
			return fetchedRemoteRotationArchive{}, err
		}
		if archiveSharesHistoryWithCurrentGeneration(ctx, repo, localCurrentAncestors, fetched.ref.Hash()) {
			tmpRefsToCleanup = removeRef(tmpRefsToCleanup, fetched.tmpRefName)
			return fetched, nil
		}
	}
	return fetchedRemoteRotationArchive{}, errors.New("no remote archive shares history with local /full/current")
}

func archiveTmpRefName(archive string) plumbing.ReferenceName {
	return plumbing.ReferenceName("refs/entire-fetch-tmp/archive-" + archive)
}

func cleanupFetchedArchiveTmpRefs(repo *git.Repository, tmpRefs []plumbing.ReferenceName) {
	for _, tmpRef := range tmpRefs {
		_ = repo.Storer.RemoveReference(tmpRef) //nolint:errcheck // cleanup is best-effort
	}
}

func currentGenerationAncestors(ctx context.Context, repo *git.Repository, currentHash plumbing.Hash) (map[plumbing.Hash]struct{}, bool) {
	ancestors := make(map[plumbing.Hash]struct{})
	iter, err := repo.Log(&git.LogOptions{From: currentHash})
	if err != nil {
		return nil, false
	}
	defer iter.Close()

	count := 0
	_ = iter.ForEach(func(c *object.Commit) error { //nolint:errcheck // Best-effort search, errors are non-fatal
		if err := ctx.Err(); err != nil {
			return err //nolint:wrapcheck // Propagating context cancellation
		}
		count++
		if count > MaxCommitTraversalDepth {
			return errStop
		}
		ancestors[c.Hash] = struct{}{}
		return nil
	})
	return ancestors, true
}

func archiveSharesHistoryWithCurrentGeneration(ctx context.Context, repo *git.Repository, currentAncestors map[plumbing.Hash]struct{}, archiveHash plumbing.Hash) bool {
	if _, ok := currentAncestors[archiveHash]; ok {
		return true
	}

	iter, err := repo.Log(&git.LogOptions{From: archiveHash})
	if err != nil {
		return false
	}
	defer iter.Close()

	found := false
	count := 0
	_ = iter.ForEach(func(c *object.Commit) error { //nolint:errcheck // Best-effort search, errors are non-fatal
		if err := ctx.Err(); err != nil {
			return err //nolint:wrapcheck // Propagating context cancellation
		}
		count++
		if count > MaxCommitTraversalDepth {
			return errStop
		}
		if _, ok := currentAncestors[c.Hash]; ok {
			found = true
			return errStop
		}
		return nil
	})
	return found
}

// handleRotationConflict handles the case where remote /full/current was rotated.
// Merges local /full/current into the related remote archived generation to avoid
// duplicating checkpoint data, then adopts remote's /full/current as local.
func handleRotationConflict(ctx context.Context, target, fetchTarget string, repo *git.Repository, refName, tmpRefName plumbing.ReferenceName, remoteRotationArchives []string) error {
	localRef, err := repo.Reference(refName, true)
	if err != nil {
		return fmt.Errorf("failed to get local ref: %w", err)
	}

	archive, err := fetchRelatedRemoteRotationArchive(ctx, fetchTarget, remoteRotationArchives, localRef.Hash())
	if err != nil {
		return fmt.Errorf("failed to find related archived generation: %w", err)
	}
	// fetchRelatedRemoteRotationArchive fetches via git CLI, so continue with
	// the fresh go-git handle it used to avoid stale pack indexes.
	repo = archive.repo
	defer func() {
		_ = repo.Storer.RemoveReference(archive.tmpRefName) //nolint:errcheck // cleanup is best-effort
	}()

	localRef, err = repo.Reference(refName, true)
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
	if err := checkpoint.FlattenTree(repo, archive.tree, "", entries); err != nil {
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
		[]plumbing.Hash{archive.ref.Hash()},
		"Merge local checkpoints into archived generation")
	if err != nil {
		return fmt.Errorf("failed to create merge commit: %w", err)
	}

	// Update local archived ref and push it
	newArchiveRef := plumbing.NewHashReference(archive.refName, mergeCommitHash)
	if err := repo.Storer.SetReference(newArchiveRef); err != nil {
		return fmt.Errorf("failed to update archive ref: %w", err)
	}

	if pushErr := tryPushRef(ctx, target, archive.refName); pushErr != nil {
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
// Pushes active refs in one git push. Pending full-generation publications are
// handled separately before /full/current recovery.
func pushV2Refs(ctx context.Context, target string) {
	repo, err := OpenRepository(ctx)
	if err != nil {
		printV2PushFailures(ctx, target, nil, []error{fmt.Errorf("open repository: %w", err)}, false)
		return
	}
	store := checkpoint.NewV2GitStore(repo, target)

	refs := v2RefsToPush(repo)
	pendingPublications, pendingReadErr := readPendingV2FullGenerationPublications(ctx, store)
	if pendingReadErr != nil {
		printV2PushFailures(ctx, target, nil, []error{pendingReadErr}, false)
		return
	}

	if len(refs) == 0 && len(pendingPublications) == 0 {
		return
	}

	fmt.Fprintln(os.Stderr, "[entire] Syncing and pushing v2 checkpoints...")
	pushNames := shortRefNames(refs)
	if len(pushNames) == 0 {
		pushNames = []string{"pending v2/full generations"}
	}
	fmt.Fprintf(os.Stderr, "[entire] Pushing %s...\n", strings.Join(pushNames, ", "))

	var failures []error
	var successfulRefs []plumbing.ReferenceName
	pushedContent := false

	pendingPublicationResult, pendingPublishErr := publishPendingV2FullGenerationPublications(ctx, repo, store, target, pendingPublications)
	successfulRefs = append(successfulRefs, pendingPublicationResult.successfulRefs...)
	if len(pendingPublicationResult.successfulRefs) > 0 {
		pushedContent = true
	}
	if pendingPublishErr != nil {
		failures = append(failures, fmt.Errorf("couldn't publish pending v2 full generation refs: %w", pendingPublishErr))
		printV2PushFailures(ctx, target, successfulRefs, failures, pushedContent)
		return
	}
	if pendingPublicationResult.fullCurrentResetHandled {
		refs = removeRef(refs, plumbing.ReferenceName(paths.V2FullCurrentRefName))
	}

	results := pushV2RefsWithRecovery(ctx, target, refs)
	for _, result := range results {
		if result.err != nil {
			failures = append(failures, result.err)
			continue
		}
		successfulRefs = appendUniqueRef(successfulRefs, result.refName)
		if !result.result.upToDate {
			pushedContent = true
		}
	}

	if len(failures) > 0 {
		printV2PushFailures(ctx, target, successfulRefs, failures, pushedContent)
		return
	}

	fmt.Fprintln(os.Stderr, "[entire] All v2 checkpoints pushed")
	if pushedContent {
		printSettingsCommitHint(ctx, target)
	}
	printCheckpointsV2MigrationHint(ctx)
}

func readPendingV2FullGenerationPublications(ctx context.Context, store *checkpoint.V2GitStore) ([]checkpoint.PendingV2FullGenerationPublication, error) {
	publications, err := store.ReadPendingFullGenerationPublications(ctx)
	if err != nil {
		return nil, fmt.Errorf("read pending v2 full generation publications: %w", err)
	}
	return publications, nil
}

func printV2PartialPushResult(w io.Writer, successfulRefs []plumbing.ReferenceName, failures []error) {
	if len(successfulRefs) > 0 {
		fmt.Fprintf(w, "[entire] Successfully pushed %s\n", strings.Join(shortRefNames(successfulRefs), ", "))
	}
	for _, err := range failures {
		fmt.Fprintf(w, "[entire] Warning: %v\n", err)
	}
}

func printV2PushFailures(ctx context.Context, target string, successfulRefs []plumbing.ReferenceName, failures []error, pushedContent bool) {
	printV2PartialPushResult(os.Stderr, successfulRefs, failures)
	printCheckpointRemoteHint(target)
	if pushedContent {
		printSettingsCommitHint(ctx, target)
	}
	printCheckpointsV2MigrationHint(ctx)
}

func v2RefsToPush(repo *git.Repository) []plumbing.ReferenceName {
	var refs []plumbing.ReferenceName
	for _, refName := range []plumbing.ReferenceName{
		plumbing.ReferenceName(paths.V2MainRefName),
		plumbing.ReferenceName(paths.V2FullCurrentRefName),
	} {
		if _, err := repo.Reference(refName, true); err == nil {
			refs = append(refs, refName)
		}
	}

	return refs
}

func shortRefNames(refs []plumbing.ReferenceName) []string {
	names := make([]string, 0, len(refs))
	for _, refName := range refs {
		names = append(names, shortRefName(refName))
	}
	return names
}

func appendUniqueRef(refs []plumbing.ReferenceName, refName plumbing.ReferenceName) []plumbing.ReferenceName {
	if slices.Contains(refs, refName) {
		return refs
	}
	return append(refs, refName)
}

func removeRef(refs []plumbing.ReferenceName, refToRemove plumbing.ReferenceName) []plumbing.ReferenceName {
	return slices.DeleteFunc(refs, func(refName plumbing.ReferenceName) bool {
		return refName == refToRemove
	})
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
