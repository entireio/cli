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

// tryPushRef attempts to push a custom ref using an explicit refspec.
func tryPushRef(ctx context.Context, target string, refName plumbing.ReferenceName) (pushResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	refSpec := fmt.Sprintf("%s:%s", refName, refName)
	result, err := remote.Push(ctx, target, refSpec)
	outputStr := result.Output
	if err != nil {
		return pushResult{}, classifyPushFailure(ctx, outputStr, err)
	}

	return parsePushResult(outputStr), nil
}

type v2RefPushResult struct {
	refName plumbing.ReferenceName
	result  pushResult
	err     error
}

func tryPushV2Refs(ctx context.Context, target string, refs []plumbing.ReferenceName) []v2RefPushResult {
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

func refSpecsForRefs(refs []plumbing.ReferenceName) []string {
	refSpecs := make([]string, 0, len(refs))
	for _, refName := range refs {
		refSpecs = append(refSpecs, fmt.Sprintf("%s:%s", refName, refName))
	}
	return refSpecs
}

func parsePushRefResults(ctx context.Context, output string, refs []plumbing.ReferenceName, pushErr error) []v2RefPushResult {
	parsed := make(map[plumbing.ReferenceName]v2RefPushResult, len(refs))
	for _, line := range strings.Split(output, "\n") {
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

	if output, err := remote.Fetch(ctx, remote.FetchOptions{
		Remote:    fetchTarget,
		RefSpecs:  []string{refSpec},
		NoTags:    true,
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

	output, err := remote.LsRemote(ctx, target, paths.V2FullRefPrefix+"*")
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
	if output, fetchErr := remote.Fetch(ctx, remote.FetchOptions{
		Remote:    fetchTarget,
		RefSpecs:  []string{archiveRefSpec},
		NoTags:    true,
		ExtraArgs: []string{"--no-write-fetch-head"},
	}); fetchErr != nil {
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
// Pushes /main, /full/current, and the latest archived generation (if any) in
// one git push. Older archived generations are immutable and were pushed when created.
func pushV2Refs(ctx context.Context, target string) {
	refs := v2RefsToPush(ctx)
	if len(refs) == 0 {
		return
	}

	fmt.Fprintln(os.Stderr, "[entire] Syncing and pushing v2 checkpoints...")
	fmt.Fprintf(os.Stderr, "[entire] Pushing %s...\n", strings.Join(shortRefNames(refs), ", "))

	results := pushV2RefsWithRecovery(ctx, target, refs)
	var failures []error
	var successfulRefs []plumbing.ReferenceName
	pushedContent := false
	for _, result := range results {
		if result.err != nil {
			failures = append(failures, result.err)
			continue
		}
		successfulRefs = append(successfulRefs, result.refName)
		if !result.result.upToDate {
			pushedContent = true
		}
	}

	if len(failures) > 0 {
		printV2PartialPushResult(os.Stderr, successfulRefs, failures)
		printCheckpointRemoteHint(target)
		if pushedContent {
			printSettingsCommitHint(ctx, target)
		}
		printCheckpointsV2MigrationHint(ctx)
		return
	}

	fmt.Fprintln(os.Stderr, "[entire] All v2 checkpoints pushed")
	if pushedContent {
		printSettingsCommitHint(ctx, target)
	}
	printCheckpointsV2MigrationHint(ctx)
}

func printV2PartialPushResult(w io.Writer, successfulRefs []plumbing.ReferenceName, failures []error) {
	if len(successfulRefs) > 0 {
		fmt.Fprintf(w, "[entire] Successfully pushed %s\n", strings.Join(shortRefNames(successfulRefs), ", "))
	}
	for _, err := range failures {
		fmt.Fprintf(w, "[entire] Warning: %v\n", err)
	}
}

func v2RefsToPush(ctx context.Context) []plumbing.ReferenceName {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil
	}

	var refs []plumbing.ReferenceName
	for _, refName := range []plumbing.ReferenceName{
		plumbing.ReferenceName(paths.V2MainRefName),
		plumbing.ReferenceName(paths.V2FullCurrentRefName),
	} {
		if _, err := repo.Reference(refName, true); err == nil {
			refs = append(refs, refName)
		}
	}

	// Push only the latest archived generation (most likely to be newly created).
	store := checkpoint.NewV2GitStore(repo, "")
	archived, err := store.ListArchivedGenerations()
	if err != nil || len(archived) == 0 {
		return refs
	}
	latest := archived[len(archived)-1]
	refs = append(refs, plumbing.ReferenceName(paths.V2FullRefPrefix+latest))

	return refs
}

func shortRefNames(refs []plumbing.ReferenceName) []string {
	names := make([]string, 0, len(refs))
	for _, refName := range refs {
		names = append(names, shortRefName(refName))
	}
	return names
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
