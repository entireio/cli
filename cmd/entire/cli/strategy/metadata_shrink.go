package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"sort"
	"strings"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// OversizedCheckpointMetadataThreshold is the blob size above which a
// per-session metadata.json on entire/checkpoints/v1 is reported by
// `entire doctor`. GitHub warns at 50 MiB and refuses any blob over 100 MiB, and
// a refused blob anywhere in the branch's history makes the whole branch
// unpushable there — including through a forge's push mirror. A healthy
// metadata.json is tens of kilobytes, so anything past this line is the
// pre-v0.10.1 prompt_attributions bloat (see checkpoint.MaxPromptAttributionsBytes),
// not a large session.
const OversizedCheckpointMetadataThreshold int64 = 50 << 20

// promptAttributionsField is the metadata.json key the shrink removes. It is
// the only field whose size scales with the working tree rather than the
// session, and nothing reads it back — the attribution summary it fed is a
// separate field that stays.
const promptAttributionsField = "prompt_attributions"

// OversizedMetadataBlob is one metadata.json blob over the threshold.
type OversizedMetadataBlob struct {
	Path   string        // tree path, e.g. "ab/cdef.../0/metadata.json"
	Size   int64         // blob size in bytes
	Hash   plumbing.Hash // blob hash
	Commit plumbing.Hash // the commit that introduced this blob version
}

// MetadataSizeScan is the read-only half of the oversized-metadata repair: what
// is oversized, where, and what the elected sync remote holds, so the report and
// the fix share one fetch.
type MetadataSizeScan struct {
	Threshold  int64
	LocalTip   plumbing.Hash // zero when the branch does not exist locally
	RemoteName string        // elected checkpoint sync remote; "" when none
	// RemoteTip is the remote-tracking tip of the checkpoint branch on
	// RemoteName after a refresh. Zero when there is no remote, the remote has
	// no such branch, or the refresh failed and no stale tracking ref exists.
	RemoteTip plumbing.Hash
	// RemoteErr records a failed refresh or an unreadable tracking ref. After a
	// failed refresh RemoteTip may still reflect the tracking ref as it was,
	// which the force-with-lease push guards against; after an unreadable ref
	// the remote fields are cleared and the fix does not push.
	RemoteErr error
	// RemoteAhead reports that RemoteTip carries commits LocalTip does not.
	RemoteAhead  bool
	PushDisabled bool
	// DedicatedCheckpointRemote reports that the branch is pushed to a
	// checkpoint_remote URL rather than to RemoteName; the fix is withheld.
	DedicatedCheckpointRemote bool
	// Local are oversized blobs reachable from LocalTip; Remote those reachable
	// from RemoteTip and not already in Local.
	Local  []OversizedMetadataBlob
	Remote []OversizedMetadataBlob
}

// Empty reports whether nothing is oversized on either side.
func (s *MetadataSizeScan) Empty() bool { return len(s.Local) == 0 && len(s.Remote) == 0 }

// All returns every oversized blob, largest first.
func (s *MetadataSizeScan) All() []OversizedMetadataBlob {
	all := make([]OversizedMetadataBlob, 0, len(s.Local)+len(s.Remote))
	all = append(all, s.Local...)
	all = append(all, s.Remote...)
	sort.SliceStable(all, func(i, j int) bool { return all[i].Size > all[j].Size })
	return all
}

// MetadataShrinkResult describes what ShrinkOversizedCheckpointMetadata did.
type MetadataShrinkResult struct {
	OldLocalTip plumbing.Hash
	NewLocalTip plumbing.Hash
	// CommitsRewritten counts commits of the local branch that were rebuilt.
	CommitsRewritten int
	// RemoteCommitsRewritten counts commits of the remote's history that were
	// rebuilt because the remote was ahead of the local branch.
	RemoteCommitsRewritten int
	BlobsShrunk            int
	// StillOversized lists blobs that remain over the threshold after the
	// prompt_attributions field was removed — bloat of a kind this repair does
	// not know about, reported rather than guessed at.
	StillOversized []OversizedMetadataBlob
	Pushed         bool
	// PushSkippedReason explains a Pushed == false when no error occurred.
	PushSkippedReason string
}

// ScanOversizedCheckpointMetadata refreshes the elected sync remote's tracking
// ref for the checkpoint branch (best effort) and reports every metadata.json
// blob over threshold reachable from the local branch or from that tracking ref.
// It is read-only: nothing under refs/heads is touched.
//
// The remote side matters because the repair force-pushes: a blob that is only
// on the remote (an earlier local repair whose push failed) still blocks the
// mirror, and an ordinary pre-push would replay the local branch onto that
// remote history and reintroduce it.
//
// The network is touched only when something is oversized. The local branch
// and the remote-tracking ref as last fetched are scanned first; when both are
// clean the answer is "OK" without a fetch, which keeps the routine doctor run
// offline. When either shows a problem the tracking ref is refreshed and the
// remote side re-scanned, so the fix that follows works from the remote's
// current tip.
func ScanOversizedCheckpointMetadata(ctx context.Context, repo *git.Repository, remoteName string, threshold int64) (*MetadataSizeScan, error) {
	v1 := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	scan := &MetadataSizeScan{Threshold: threshold, RemoteName: remoteName}

	localTip, err := readV1Tip(repo, v1)
	if err != nil {
		return nil, fmt.Errorf("read local %s: %w", v1.Short(), err)
	}
	scan.LocalTip = localTip
	scan.Local, err = findOversizedMetadataBlobs(ctx, repo, localTip, threshold)
	if err != nil {
		return nil, fmt.Errorf("scan local %s: %w", v1.Short(), err)
	}
	// The tracking ref as last fetched. An unreadable one (dangling, partial
	// clone) is reported, not fatal: doctor's other checks must still run, and
	// with a clean local branch there is nothing this check would do anyway.
	var trackingRef plumbing.ReferenceName
	var readErr error
	if remoteName != "" {
		trackingRef = plumbing.NewRemoteReferenceName(remoteName, paths.MetadataBranchName)
		readErr = scan.readRemoteSide(ctx, repo, trackingRef)
	}
	if len(scan.Local) == 0 && (remoteName == "" || readErr != nil || len(scan.Remote) == 0) {
		scan.RemoteErr = readErr
		return scan, nil //nolint:nilerr // fail-soft: the unreadable tracking ref is reported on scan.RemoteErr, not fatal to doctor
	}
	// Something is oversized (or the stale tracking ref could not tell us).
	// Where pre-push actually sends the branch decides what happens next: a
	// dedicated checkpoint_remote URL has no remote-tracking ref to lease
	// against, and pre-push replays local commits onto whatever that URL
	// holds, so a local-only rewrite there would be undone by the next push.
	// Report it and stop rather than repair half of it — and decide that
	// BEFORE the no-remote early return below, so a repository whose only
	// checkpoint destination is the dedicated URL is refused the same way.
	// The settings are read directly rather than through resolvePushSettings,
	// which may fetch and create the local branch as a side effect; this scan
	// must stay read-only. They are read for THIS repository's worktree, not
	// the process working directory: the scan is handed a repo and must not
	// answer for whichever checkout the caller happens to be standing in.
	if s, loadErr := loadSettingsForRepo(ctx, repo); loadErr == nil {
		scan.PushDisabled = s.IsPushSessionsDisabled()
		if s.GetCheckpointRemote() != nil {
			scan.DedicatedCheckpointRemote = true
			scan.clearRemote()
			return scan, nil
		}
	} else {
		// Unknown push policy: do not push. The local rewrite is still useful.
		scan.PushDisabled = true
	}
	if remoteName == "" {
		return scan, nil
	}
	// Refresh from the remote so the fix works from its current tip.
	if fetchErr := refreshCheckpointTrackingRef(ctx, remoteName, v1, trackingRef); fetchErr != nil {
		scan.RemoteErr = fetchErr
	}
	if err := scan.readRemoteSide(ctx, repo, trackingRef); err != nil {
		scan.RemoteErr = errors.Join(scan.RemoteErr, err)
		scan.clearRemote()
	}
	return scan, nil
}

// ErrDedicatedCheckpointRemote reports that the repository pushes its
// checkpoint branch to a dedicated checkpoint_remote URL, which the automatic
// repair does not handle (see ScanOversizedCheckpointMetadata).
var ErrDedicatedCheckpointRemote = errors.New("checkpoint branch is pushed to a dedicated checkpoint remote; automatic repair is not available there")

// loadSettingsForRepo reads Entire settings for the worktree repo was opened on.
func loadSettingsForRepo(ctx context.Context, repo *git.Repository) (*settings.EntireSettings, error) {
	wt, err := repo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("resolve worktree: %w", err)
	}
	return settings.LoadForWorktreeRoot(ctx, wt.Filesystem().Root()) //nolint:wrapcheck // settings errors carry their own context
}

// clearRemote leaves the scan with no knowledge of the remote side.
func (s *MetadataSizeScan) clearRemote() {
	s.RemoteTip = plumbing.ZeroHash
	s.RemoteAhead = false
	s.Remote = nil
}

// readRemoteSide fills RemoteTip, RemoteAhead and Remote from the tracking
// ref as it currently stands. On error the remote fields are cleared.
func (s *MetadataSizeScan) readRemoteSide(ctx context.Context, repo *git.Repository, trackingRef plumbing.ReferenceName) (err error) {
	defer func() {
		if err != nil {
			s.clearRemote()
		}
	}()
	remoteTip, err := readV1Tip(repo, trackingRef)
	if err != nil {
		return fmt.Errorf("read %s: %w", trackingRef.Short(), err)
	}
	s.RemoteTip = remoteTip
	s.RemoteAhead = false
	s.Remote = nil
	switch {
	case remoteTip.IsZero():
		return nil
	case s.LocalTip.IsZero():
		s.RemoteAhead = true
	case !remoteTip.Equal(s.LocalTip):
		base, mbErr := computeMergeBase(repo, s.LocalTip, remoteTip)
		if mbErr != nil {
			return fmt.Errorf("compare local and %s: %w", trackingRef.Short(), mbErr)
		}
		s.RemoteAhead = !base.Equal(remoteTip)
	}
	if remoteTip.Equal(s.LocalTip) {
		return nil
	}
	remoteBlobs, err := findOversizedMetadataBlobs(ctx, repo, remoteTip, s.Threshold)
	if err != nil {
		return fmt.Errorf("scan %s: %w", trackingRef.Short(), err)
	}
	seen := make(map[plumbing.Hash]struct{}, len(s.Local))
	for _, b := range s.Local {
		seen[b.Hash] = struct{}{}
	}
	for _, b := range remoteBlobs {
		if _, dup := seen[b.Hash]; !dup {
			s.Remote = append(s.Remote, b)
		}
	}
	return nil
}

// refreshCheckpointTrackingRef fetches the checkpoint branch from remoteName
// into its remote-tracking ref. A remote that has no such branch is not an
// error: the tracking ref is simply left as it was (normally absent).
func refreshCheckpointTrackingRef(ctx context.Context, remoteName string, branch, trackingRef plumbing.ReferenceName) error {
	fetchCtx, cancel := context.WithTimeout(ctx, checkpointRemoteForegroundFetchTimeout)
	defer cancel()
	output, err := remote.Fetch(fetchCtx, remote.FetchOptions{
		Remote:   remoteName,
		RefSpecs: []string{"+" + branch.String() + ":" + trackingRef.String()},
		NoTags:   true,
		NoFilter: true, // the scan reads blob sizes, which a blob-filtered fetch would not have
	})
	if err == nil {
		return nil
	}
	if strings.Contains(string(output), "couldn't find remote ref") {
		return nil
	}
	if msg := strings.TrimSpace(string(output)); msg != "" {
		return fmt.Errorf("fetch %s from %s: %s: %w", branch.Short(), remoteName, msg, err)
	}
	return fmt.Errorf("fetch %s from %s: %w", branch.Short(), remoteName, err)
}

// findOversizedMetadataBlobs walks the history behind tip and returns every
// metadata.json blob larger than threshold, largest first. Each commit is
// diffed against its first parent, so the walk costs the size of the changes
// rather than the size of the (cumulative) trees; the root commit's tree is
// listed in full.
func findOversizedMetadataBlobs(ctx context.Context, repo *git.Repository, tip plumbing.Hash, threshold int64) ([]OversizedMetadataBlob, error) {
	if tip.IsZero() {
		return nil, nil
	}
	iter, err := repo.Log(&git.LogOptions{From: tip})
	if err != nil {
		return nil, fmt.Errorf("log %s: %w", tip, err)
	}
	defer iter.Close()

	seen := make(map[plumbing.Hash]struct{})
	var found []OversizedMetadataBlob
	walkErr := iter.ForEach(func(c *object.Commit) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr //nolint:wrapcheck // context cancellation propagates as-is
		}
		return forEachMetadataBlobIntroduced(ctx, c, func(blobPath string, hash plumbing.Hash) error {
			if _, dup := seen[hash]; dup {
				return nil
			}
			seen[hash] = struct{}{}
			blob, blobErr := repo.BlobObject(hash)
			if blobErr != nil {
				return fmt.Errorf("blob %s at %s: %w", hash, blobPath, blobErr)
			}
			if blob.Size > threshold {
				found = append(found, OversizedMetadataBlob{Path: blobPath, Size: blob.Size, Hash: hash, Commit: c.Hash})
			}
			return nil
		})
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk history of %s: %w", tip, walkErr)
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].Size > found[j].Size })
	return found, nil
}

// forEachMetadataBlobIntroduced calls fn for every metadata.json blob that
// commit c adds or changes relative to any of its parents. The checkpoint
// branch is linear in practice, but a merge is diffed against every parent so
// a blob that arrived through a side parent is not missed; the caller dedupes
// by hash, so over-reporting costs nothing.
func forEachMetadataBlobIntroduced(ctx context.Context, c *object.Commit, fn func(blobPath string, hash plumbing.Hash) error) error {
	tree, err := c.Tree()
	if err != nil {
		return fmt.Errorf("tree of %s: %w", c.Hash, err)
	}
	if c.NumParents() == 0 {
		files := tree.Files()
		defer files.Close()
		return files.ForEach(func(f *object.File) error { //nolint:wrapcheck // fn's errors carry their own context
			if path.Base(f.Name) != paths.MetadataFileName {
				return nil
			}
			return fn(f.Name, f.Hash)
		})
	}
	for i := range c.NumParents() {
		parent, err := c.Parent(i)
		if err != nil {
			return fmt.Errorf("parent %d of %s: %w", i, c.Hash, err)
		}
		parentTree, err := parent.Tree()
		if err != nil {
			return fmt.Errorf("tree of %s: %w", parent.Hash, err)
		}
		changes, err := object.DiffTreeContext(ctx, parentTree, tree)
		if err != nil {
			return fmt.Errorf("diff %s..%s: %w", parent.Hash, c.Hash, err)
		}
		for _, ch := range changes {
			to := ch.To
			if to.Name == "" || path.Base(to.Name) != paths.MetadataFileName {
				continue
			}
			if !isFileMode(to.TreeEntry.Mode) {
				continue
			}
			if err := fn(to.Name, to.TreeEntry.Hash); err != nil {
				return err
			}
		}
	}
	return nil
}

// isFileMode reports whether a tree entry is a file whose content is a blob.
// filemode.Deprecated (0100664) is a regular file too: go-git documents that it
// "should be treated as Regular", and a metadata.json carrying that mode would
// otherwise be invisible to both the scan and the rewrite.
func isFileMode(mode filemode.FileMode) bool {
	return mode == filemode.Regular || mode == filemode.Executable || mode == filemode.Deprecated
}

// contentReachable reports whether some commit reachable from tip carries
// exactly the tree of commit target. On the cumulative checkpoint branch equal
// trees mean equal checkpoint content, so this answers "does tip's history
// already contain everything target has?" independently of commit hashes —
// which differ across independent rewrites when commit signing is on.
func contentReachable(repo *git.Repository, tip, target plumbing.Hash) (bool, error) {
	if tip.IsZero() || target.IsZero() {
		return false, nil
	}
	tc, err := repo.CommitObject(target)
	if err != nil {
		return false, fmt.Errorf("load commit %s: %w", target, err)
	}
	iter, err := repo.Log(&git.LogOptions{From: tip})
	if err != nil {
		return false, fmt.Errorf("log %s: %w", tip, err)
	}
	defer iter.Close()
	found := false
	walkErr := iter.ForEach(func(c *object.Commit) error {
		if c.TreeHash.Equal(tc.TreeHash) {
			found = true
			return errStop
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, errStop) {
		return false, fmt.Errorf("walk history of %s: %w", tip, walkErr)
	}
	return found, nil
}

// PushFailedError reports that the local rewrite completed but the push of
// the rewritten branch did not. The local branch is left at the rewritten
// tip; re-running doctor pushes it once the remote is reachable.
type PushFailedError struct {
	Remote string
	Err    error
}

func (e *PushFailedError) Error() string {
	return fmt.Sprintf("force-push %s to %s: %v", paths.MetadataBranchName, e.Remote, e.Err)
}

func (e *PushFailedError) Unwrap() error { return e.Err }

// ShrinkOversizedCheckpointMetadata rewrites entire/checkpoints/v1 so that
// every metadata.json blob over scan.Threshold loses its prompt_attributions
// field, then force-pushes the result to the elected sync remote. Commit
// messages, authors, dates and every other blob are preserved; commits whose
// tree and parents are unchanged keep their hash, so history before the first
// oversized blob is untouched.
//
// When the remote carries commits the local branch lacks (RemoteAhead), the
// remote history is rewritten first and the local-only commits are replayed
// onto it through SafelyAdvanceLocalRef — the same reconciliation pre-push
// uses — so nothing either side holds is lost. The push is
// --force-with-lease against the tip the scan observed, so a remote that moved
// in between is refused rather than overwritten; re-running doctor rescans.
//
// On a push failure the returned result still describes the completed local
// rewrite, so the caller can say what state the repository was left in.
func ShrinkOversizedCheckpointMetadata(ctx context.Context, repo *git.Repository, scan *MetadataSizeScan, w io.Writer) (*MetadataShrinkResult, error) {
	v1 := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	logCtx := logging.WithComponent(ctx, "checkpoint")
	res := &MetadataShrinkResult{OldLocalTip: scan.LocalTip, NewLocalTip: scan.LocalTip}
	if scan.DedicatedCheckpointRemote {
		return res, ErrDedicatedCheckpointRemote
	}
	rw := newMetadataRewriter(ctx, repo, scan.Threshold)

	// Local first. When the remote is ahead, its history is rewritten with the
	// same memoized rewriter, so every commit the two sides share maps to the
	// same rewritten commit and the rewritten local tip is an ancestor of the
	// rewritten remote tip (or shares its rewritten merge base). Reconciling
	// the ORIGINAL local tip against a rewritten remote would find no common
	// history past the first oversized blob and replay every commit since as
	// a duplicate.
	newTip, err := rw.rewriteHistory(scan.LocalTip)
	if err != nil {
		return res, fmt.Errorf("rewrite %s: %w", v1.Short(), err)
	}
	res.CommitsRewritten = rw.commitsRewritten
	if !newTip.Equal(scan.LocalTip) {
		if err := atomicSetV1Ref(ctx, repo, scan.LocalTip, newTip); err != nil {
			return res, err
		}
		res.NewLocalTip = newTip
	}

	if !scan.RemoteTip.IsZero() && scan.RemoteAhead {
		before := rw.commitsRewritten
		fixedRemote, err := rw.rewriteHistory(scan.RemoteTip)
		if err != nil {
			return res, fmt.Errorf("rewrite %s/%s: %w", scan.RemoteName, v1.Short(), err)
		}
		res.RemoteCommitsRewritten = rw.commitsRewritten - before
		newTip, err = reconcileRewrittenTips(ctx, repo, w, scan.RemoteName, v1, newTip, fixedRemote)
		if err != nil {
			return res, err
		}
		res.NewLocalTip = newTip
	}
	res.BlobsShrunk = rw.blobsShrunk
	res.StillOversized = rw.stillOversized
	logging.Info(logCtx, "shrank oversized checkpoint metadata",
		slog.String("old_tip", scan.LocalTip.String()),
		slog.String("new_tip", newTip.String()),
		slog.Int("commits_rewritten", res.CommitsRewritten),
		slog.Int("remote_commits_rewritten", res.RemoteCommitsRewritten),
		slog.Int("blobs_shrunk", rw.blobsShrunk))

	switch {
	case scan.RemoteName == "":
		res.PushSkippedReason = "no checkpoint sync remote is configured"
	case scan.RemoteTip.IsZero() && scan.RemoteErr != nil:
		res.PushSkippedReason = fmt.Sprintf("the state of %s could not be determined (%v); re-run entire doctor once it is reachable", scan.RemoteName, scan.RemoteErr)
	case scan.RemoteTip.IsZero():
		res.PushSkippedReason = fmt.Sprintf("%s has no %s branch yet; the next git push creates it", scan.RemoteName, v1.Short())
	case scan.RemoteTip.Equal(newTip):
		res.PushSkippedReason = scan.RemoteName + " already has this history"
	case scan.PushDisabled:
		res.PushSkippedReason = "checkpoint pushing is disabled in settings; push the branch yourself with --force-with-lease"
	default:
		if err := forcePushCheckpointBranch(ctx, scan.RemoteName, v1, scan.RemoteTip, newTip); err != nil {
			return res, &PushFailedError{Remote: scan.RemoteName, Err: err}
		}
		res.Pushed = true
	}
	return res, nil
}

// reconcileRewrittenTips brings the local branch (at rewritten tip local) and
// the rewritten remote history (fixedRemote) together and returns the new
// local tip. Three cases, decided on content rather than hashes because
// independent rewrites of the same history differ in hash when commit signing
// is on:
//
//   - the remote already holds everything local has (local's tree appears in
//     the remote's history): adopt the remote tip, no replay;
//   - local already holds everything the remote has: keep local, which the
//     caller then pushes;
//   - genuine divergence: replay the local-only commits onto the remote via
//     SafelyAdvanceLocalRef, the same reconciliation pre-push uses.
func reconcileRewrittenTips(ctx context.Context, repo *git.Repository, w io.Writer, remoteName string, v1 plumbing.ReferenceName, local, fixedRemote plumbing.Hash) (plumbing.Hash, error) {
	if fixedRemote.Equal(local) {
		return local, nil
	}
	if local.IsZero() {
		if err := SafelyAdvanceLocalRef(ctx, repo, v1, fixedRemote); err != nil {
			return plumbing.ZeroHash, fmt.Errorf("adopt %s/%s: %w", remoteName, v1.Short(), err)
		}
		return fixedRemote, nil
	}
	remoteHasLocal, err := contentReachable(repo, fixedRemote, local)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if remoteHasLocal {
		fmt.Fprintf(w, "  Remote %s already holds every local checkpoint; adopting its history.\n", remoteName)
		if err := atomicSetV1Ref(ctx, repo, local, fixedRemote); err != nil {
			return plumbing.ZeroHash, err
		}
		return fixedRemote, nil
	}
	localHasRemote, err := contentReachable(repo, local, fixedRemote)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if localHasRemote {
		return local, nil
	}
	fmt.Fprintf(w, "  Remote %s has checkpoints not yet local; replaying local checkpoints onto it.\n", remoteName)
	if err := SafelyAdvanceLocalRef(ctx, repo, v1, fixedRemote); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("reconcile local %s with %s: %w", v1.Short(), remoteName, err)
	}
	tip, err := readV1Tip(repo, v1)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("re-read local %s: %w", v1.Short(), err)
	}
	return tip, nil
}

// forcePushCheckpointBranch replaces the remote's checkpoint branch with newTip,
// refusing (via --force-with-lease) if the remote no longer sits at expected.
func forcePushCheckpointBranch(ctx context.Context, remoteName string, branch plumbing.ReferenceName, expected, newTip plumbing.Hash) error {
	pushCtx, cancel := context.WithTimeout(ctx, checkpointPushBudget)
	defer cancel()
	_, err := remote.PushWithOptions(pushCtx, remote.PushOptions{
		Remote:    remoteName,
		RefSpecs:  []string{newTip.String() + ":" + branch.String()},
		ExtraArgs: []string{"--force-with-lease=" + branch.String() + ":" + expected.String()},
	})
	if err != nil {
		return fmt.Errorf("force-push %s to %s: %w", branch.Short(), remoteName, err)
	}
	return nil
}

// metadataRewriter rewrites a commit graph replacing oversized metadata.json
// blobs. Every level is memoized: a blob is stripped once however many trees
// reference it, a tree is rebuilt once however many commits share it, and a
// commit is rewritten once however many children it has.
type metadataRewriter struct {
	ctx       context.Context //nolint:containedctx // scoped to one rewrite; threaded into commit signing
	repo      *git.Repository
	threshold int64

	blobs   map[plumbing.Hash]plumbing.Hash
	trees   map[plumbing.Hash]plumbing.Hash
	commits map[plumbing.Hash]plumbing.Hash

	commitsRewritten int
	blobsShrunk      int
	stillOversized   []OversizedMetadataBlob
}

func newMetadataRewriter(ctx context.Context, repo *git.Repository, threshold int64) *metadataRewriter {
	return &metadataRewriter{
		ctx:       ctx,
		repo:      repo,
		threshold: threshold,
		blobs:     make(map[plumbing.Hash]plumbing.Hash),
		trees:     make(map[plumbing.Hash]plumbing.Hash),
		commits:   make(map[plumbing.Hash]plumbing.Hash),
	}
}

// rewriteHistory returns the rewritten equivalent of tip. Parents are processed
// before children with an explicit stack, so a branch with thousands of
// checkpoints does not recurse thousands deep.
func (r *metadataRewriter) rewriteHistory(tip plumbing.Hash) (plumbing.Hash, error) {
	if tip.IsZero() {
		return tip, nil
	}
	type frame struct {
		hash     plumbing.Hash
		expanded bool
	}
	stack := []frame{{hash: tip}}
	for len(stack) > 0 {
		if err := r.ctx.Err(); err != nil {
			return plumbing.ZeroHash, err //nolint:wrapcheck // context cancellation propagates as-is
		}
		top := len(stack) - 1
		f := stack[top]
		if _, done := r.commits[f.hash]; done {
			stack = stack[:top]
			continue
		}
		c, err := r.repo.CommitObject(f.hash)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("load commit %s: %w", f.hash, err)
		}
		if !f.expanded {
			stack[top].expanded = true
			for _, p := range c.ParentHashes {
				if _, done := r.commits[p]; !done {
					stack = append(stack, frame{hash: p})
				}
			}
			continue
		}
		newHash, err := r.rewriteCommit(c)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		r.commits[f.hash] = newHash
		stack = stack[:top]
	}
	return r.commits[tip], nil
}

func (r *metadataRewriter) rewriteCommit(c *object.Commit) (plumbing.Hash, error) {
	newTree, err := r.rewriteTree(c.TreeHash, "")
	if err != nil {
		return plumbing.ZeroHash, err
	}
	parents := make([]plumbing.Hash, 0, len(c.ParentHashes))
	changed := !newTree.Equal(c.TreeHash)
	for _, p := range c.ParentHashes {
		np, ok := r.commits[p]
		if !ok {
			return plumbing.ZeroHash, fmt.Errorf("parent %s of %s was not rewritten first", p, c.Hash)
		}
		if !np.Equal(p) {
			changed = true
		}
		parents = append(parents, np)
	}
	if !changed {
		return c.Hash, nil
	}
	rewritten := &object.Commit{
		Author:       c.Author,
		Committer:    c.Committer,
		Message:      c.Message,
		TreeHash:     newTree,
		ParentHashes: parents,
		Encoding:     c.Encoding,
		ExtraHeaders: c.ExtraHeaders,
	}
	// Re-sign when signing is configured: the original signature covered the
	// original tree, so it cannot be carried over. Signing makes the rewritten
	// hash non-deterministic across runs, which is why reconciliation below
	// compares trees (contentReachable) rather than hashes.
	checkpoint.SignCommitBestEffort(r.ctx, rewritten)
	obj := r.repo.Storer.NewEncodedObject()
	if err := rewritten.Encode(obj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("encode rewritten commit %s: %w", c.Hash, err)
	}
	hash, err := r.repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("store rewritten commit %s: %w", c.Hash, err)
	}
	r.commitsRewritten++
	return hash, nil
}

func (r *metadataRewriter) rewriteTree(hash plumbing.Hash, prefix string) (plumbing.Hash, error) {
	if done, ok := r.trees[hash]; ok {
		return done, nil
	}
	tree, err := r.repo.TreeObject(hash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("load tree %s: %w", hash, err)
	}
	entries := make([]object.TreeEntry, 0, len(tree.Entries))
	changed := false
	for _, e := range tree.Entries {
		entryPath := e.Name
		if prefix != "" {
			entryPath = prefix + "/" + e.Name
		}
		newEntry := e
		switch e.Mode {
		case filemode.Dir:
			sub, subErr := r.rewriteTree(e.Hash, entryPath)
			if subErr != nil {
				return plumbing.ZeroHash, subErr
			}
			newEntry.Hash = sub
		case filemode.Regular, filemode.Executable, filemode.Deprecated:
			if e.Name == paths.MetadataFileName {
				blob, blobErr := r.rewriteBlob(e.Hash, entryPath)
				if blobErr != nil {
					return plumbing.ZeroHash, blobErr
				}
				newEntry.Hash = blob
			}
		case filemode.Empty, filemode.Symlink, filemode.Submodule:
			// kept verbatim
		}
		if !newEntry.Hash.Equal(e.Hash) {
			changed = true
		}
		entries = append(entries, newEntry)
	}
	if !changed {
		r.trees[hash] = hash
		return hash, nil
	}
	newTree := &object.Tree{Entries: entries}
	obj := r.repo.Storer.NewEncodedObject()
	if err := newTree.Encode(obj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("encode tree %s: %w", prefix, err)
	}
	newHash, err := r.repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("store tree %s: %w", prefix, err)
	}
	r.trees[hash] = newHash
	return newHash, nil
}

// rewriteBlob returns the replacement for a metadata.json blob: itself when it
// is under the threshold or carries no prompt_attributions, else a copy without
// that field.
func (r *metadataRewriter) rewriteBlob(hash plumbing.Hash, blobPath string) (plumbing.Hash, error) {
	if done, ok := r.blobs[hash]; ok {
		return done, nil
	}
	blob, err := r.repo.BlobObject(hash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("load blob %s at %s: %w", hash, blobPath, err)
	}
	if blob.Size <= r.threshold {
		r.blobs[hash] = hash
		return hash, nil
	}
	data, err := readBlob(r.repo, hash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("read blob %s at %s: %w", hash, blobPath, err)
	}
	stripped, changed, err := stripPromptAttributions(data)
	if err != nil || !changed {
		// Either not the JSON this repair understands, or large for a reason
		// other than the field it knows how to remove: leave it and say so
		// rather than guess at what to delete.
		r.stillOversized = append(r.stillOversized, OversizedMetadataBlob{Path: blobPath, Size: blob.Size, Hash: hash})
		r.blobs[hash] = hash
		return hash, nil //nolint:nilerr // deliberate: an unparseable blob is kept verbatim and reported via stillOversized
	}
	newHash, err := checkpoint.CreateBlobFromContent(r.repo, stripped)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("write shrunk blob for %s: %w", blobPath, err)
	}
	r.blobsShrunk++
	if int64(len(stripped)) > r.threshold {
		r.stillOversized = append(r.stillOversized, OversizedMetadataBlob{Path: blobPath, Size: int64(len(stripped)), Hash: newHash})
	}
	r.blobs[hash] = newHash
	return newHash, nil
}

// stripPromptAttributions removes the prompt_attributions key from a
// metadata.json document, preserving every other field verbatim (as raw JSON),
// and reports whether anything changed. Keys come back sorted, which is the one
// cosmetic difference from the writer's field order.
func stripPromptAttributions(data []byte) ([]byte, bool, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, false, fmt.Errorf("parse metadata.json: %w", err)
	}
	if _, ok := doc[promptAttributionsField]; !ok {
		return data, false, nil
	}
	delete(doc, promptAttributionsField)
	out, err := jsonutil.MarshalIndentWithNewline(doc, "", "  ")
	if err != nil {
		return nil, false, fmt.Errorf("re-encode metadata.json: %w", err)
	}
	return out, true, nil
}
