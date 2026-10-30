package strategy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

// checkpointRefSelection says which queued refs one upload may send. The zero
// value selects every queued ref, which is automatic delivery.
type checkpointRefSelection struct {
	// refs, when non-nil, limits the upload to these refs. A non-zero hash
	// pins the commit the caller looked at: the ref is sent only while that
	// commit is still its tip.
	refs map[plumbing.ReferenceName]plumbing.Hash
}

// pin reports whether ref is selected, and the commit it is pinned to.
func (s checkpointRefSelection) pin(ref plumbing.ReferenceName) (plumbing.Hash, bool) {
	if s.refs == nil {
		return plumbing.ZeroHash, true
	}
	hash, ok := s.refs[ref]
	return hash, ok
}

// automaticCheckpointDelivery reports whether paths that upload without being
// told which checkpoints to send may run. An unreadable policy is an error,
// never automatic.
func automaticCheckpointDelivery(repo *git.Repository) (bool, error) {
	commonDir, err := checkpoint.RepositoryCommonDir(repo)
	if err != nil {
		return false, fmt.Errorf("read checkpoint push policy: %w", err)
	}
	policy, err := settings.LoadCheckpointPushPolicyAt(commonDir)
	if err != nil {
		return false, err //nolint:wrapcheck // already names the file and the problem
	}
	return policy == settings.CheckpointPushPolicyAutomatic, nil
}

// CheckpointsHeldForManualPush reports whether new checkpoints wait for
// `entire checkpoint push` instead of uploading now: the git-refs backend under
// any policy but automatic. The policy does not apply to the git-branch
// backend. An unreadable policy or checkpoint backend config is an error.
func CheckpointsHeldForManualPush(ctx context.Context) (bool, error) {
	isRefs, err := primaryBackendIsGitRefs(ctx)
	if err != nil {
		return false, err
	}
	if !isRefs {
		return false, nil
	}
	repo, err := OpenRepository(ctx)
	if err != nil {
		return false, fmt.Errorf("open repository: %w", err)
	}
	defer repo.Close()
	automatic, err := automaticCheckpointDelivery(repo)
	if err != nil {
		return false, err
	}
	return !automatic, nil
}

// ErrOuterPushCarriesHeldCheckpoints aborts a user push that sends checkpoint
// content itself while the clone's policy holds checkpoints back.
var ErrOuterPushCarriesHeldCheckpoints = errors.New("refusing to push checkpoint content")

// ErrOuterPushUncheckable aborts a user push that cannot be checked for held
// checkpoints because the hook could not save git's ref list.
var ErrOuterPushUncheckable = errors.New("refusing a push that could not be checked")

// CheckOuterPushHeldCheckpoints refuses the user's own push when it sends
// checkpoint content that this clone's policy holds back: `--mirror`, a
// refs/entire refspec, the v1 branch, or a waiting checkpoint's commit under
// another name. Git has already chosen what to send when the hook runs, so the
// whole push is refused. A deletion sends nothing and passes.
//
// The policy, the queue and the checkpoint refs belong to the whole clone, so
// this decides from the policy file and the ref list git passed, never from the
// checked-out branch's settings, and pre-push runs it whether or not Entire's
// hooks are on for this branch. Checks run cheapest first: a push that sends
// nothing, or a clone whose policy is automatic, costs no repository I/O.
// An old hook line passes no ref list, so there is nothing to refuse, and it
// warns instead.
func CheckOuterPushHeldCheckpoints(ctx context.Context) error {
	if prePushRefsUnsaved(ctx) {
		return checkOuterPushUnchecked(ctx)
	}
	if !prePushRefsKnown(ctx) {
		warnOuterPushUncheckable(ctx)
		return nil
	}
	lines := outerPushSendingLines(ctx)
	if len(lines) == 0 {
		return nil
	}
	why, held := checkpointsHeldBecause(ctx)
	if !held {
		return nil
	}
	return checkOuterPushLines(ctx, lines, why)
}

// refuseOuterPushAfterManualAnswer re-checks the user's push once manual was
// chosen for it at the first-push question. It takes the answer rather than
// reading the policy, which may not have been saved.
func refuseOuterPushAfterManualAnswer(ctx context.Context) error {
	const why = "you just chose the manual checkpoint push policy"
	if prePushRefsUnsaved(ctx) {
		return refuseUncheckedPushWhileWaiting(ctx, why)
	}
	return checkOuterPushLines(ctx, outerPushSendingLines(ctx), why)
}

// checkOuterPushLines refuses lines that send held checkpoint content; why
// says what holds it back.
func checkOuterPushLines(ctx context.Context, lines []PrePushRef, why string) error {
	for _, line := range lines {
		name := line.checkpointRefName()
		if strings.HasPrefix(name, checkpoint.CheckpointRefPrefix) {
			return outerPushHeldError(name, why)
		}
		// The v1 branch is the git-branch store, which the policy covers only
		// on git-refs. Per-checkpoint refs need no such check: only git-refs
		// writes them.
		if isCheckpointRefName(name) {
			if isRefs, err := primaryBackendIsGitRefs(ctx); err != nil || isRefs {
				return outerPushHeldError(name, why)
			}
		}
	}
	return checkOuterPushOfHeldCommits(ctx, lines, why)
}

// checkpointsHeldBecause reports whether the clone's policy holds checkpoints
// back, and why, for the refusal. An unreadable policy holds them.
func checkpointsHeldBecause(ctx context.Context) (string, bool) {
	policy, err := settings.LoadCheckpointPushPolicy(ctx)
	if err != nil {
		return fmt.Sprintf("the checkpoint push policy could not be read (%v)", err), true
	}
	if policy == settings.CheckpointPushPolicyAutomatic {
		return "", false
	}
	return "this clone's checkpoint push policy is manual", true
}

func outerPushHeldError(name, why string) error {
	return fmt.Errorf("%w: this push includes %s, and %s. %s", ErrOuterPushCarriesHeldCheckpoints, name, why, outerPushHowTo)
}

// checkOuterPushOfHeldCommits refuses the push when any line sends the
// current version of a waiting checkpoint under another name: a commit or an
// expression (`git push origin <commit>:refs/heads/copy`), or a branch or tag,
// annotated or not, created at that commit. A commit built on top of it is not
// caught: that would need an ancestry check per pushed line. When the repository or the queue cannot be read
// there is nothing to compare against, and the push is not blocked over it.
func checkOuterPushOfHeldCommits(ctx context.Context, lines []PrePushRef, why string) error {
	repo, err := OpenRepository(ctx)
	if err != nil {
		logging.Warn(ctx, "pre-push: cannot open repository to check pushed commits", slog.String("error", err.Error()))
		return nil
	}
	defer repo.Close()
	waiting, err := waitingCheckpointRefs(ctx, repo)
	if err != nil {
		logging.Warn(ctx, "pre-push: cannot read the checkpoint queue to check pushed commits", slog.String("error", err.Error()))
		return nil
	}
	tips := make(map[plumbing.Hash]plumbing.ReferenceName, len(waiting))
	for ref := range waiting {
		if r, err := repo.Reference(ref, true); err == nil {
			tips[r.Hash()] = ref
		}
	}
	if len(tips) == 0 {
		return nil
	}
	for _, line := range lines {
		hash := plumbing.NewHash(line.LocalSHA)
		ref, held := tips[hash]
		// Only a tag can name a tag object, and peeling reads an object, which
		// adds up on a first `git push --tags`.
		if !held && line.sendsTag() {
			ref, held = tips[peelToCommit(repo, hash)]
		}
		if held {
			return fmt.Errorf("%w: this push sends %s, the current version of %s, and %s. %s",
				ErrOuterPushCarriesHeldCheckpoints, line.LocalRef, ref, why, outerPushHowTo)
		}
	}
	return nil
}

// maxTagPeel bounds how many tag objects peelToCommit follows, so a cycle in a
// corrupt repository cannot hold the push.
const maxTagPeel = 16

// peelToCommit follows annotated tags to the object they name, so a tag made
// at a waiting checkpoint's commit compares as that commit. Anything that is
// not a readable tag is returned unchanged.
func peelToCommit(repo *git.Repository, hash plumbing.Hash) plumbing.Hash {
	for range maxTagPeel {
		tag, err := repo.TagObject(hash)
		if err != nil {
			return hash
		}
		hash = tag.Target
	}
	return hash
}

// checkOuterPushUnchecked refuses a push whose ref list the hook could not
// save, while checkpoints are held and waiting: it might be a `--mirror`, and
// there is no way to tell.
func checkOuterPushUnchecked(ctx context.Context) error {
	why, held := checkpointsHeldBecause(ctx)
	if !held {
		return nil
	}
	return refuseUncheckedPushWhileWaiting(ctx, why)
}

// refuseUncheckedPushWhileWaiting refuses a push whose ref list the hook could
// not save, if any checkpoint waits; why says what holds them back.
func refuseUncheckedPushWhileWaiting(ctx context.Context, why string) error {
	repo, err := OpenRepository(ctx)
	if err != nil {
		return fmt.Errorf("%w: the hook could not save the list of refs this push sends, and %s. %s",
			ErrOuterPushUncheckable, why, outerPushUncheckedHowTo)
	}
	defer repo.Close()
	if waitingCheckpointCount(ctx, repo) == 0 {
		return nil
	}
	return fmt.Errorf("%w: the hook could not save the list of refs this push sends, so it cannot tell whether the push "+
		"includes held checkpoints, and %s. %s", ErrOuterPushUncheckable, why, outerPushUncheckedHowTo)
}

const outerPushHowTo = "Leave checkpoint refs out of this push (avoid --mirror) and upload checkpoints with 'entire checkpoint push --all'. " +
	"To push them anyway, run git push with --no-verify"

const outerPushUncheckedHowTo = "Check that TMPDIR exists and is writable, then push again. To push without the check, run git push with --no-verify"

// warnOuterPushUncheckable warns, while checkpoints are held and waiting, that
// an old hook line cannot show what the user's own push sends. It runs before
// the push_sessions and sync-remote gates, and with Entire's hooks off: those
// decide what Entire sends, but the user's push can carry checkpoint refs to
// any remote.
func warnOuterPushUncheckable(ctx context.Context) {
	if _, held := checkpointsHeldBecause(ctx); !held {
		return
	}
	repo, err := OpenRepository(ctx)
	if err != nil {
		return
	}
	defer repo.Close()
	if waitingCheckpointCount(ctx, repo) > 0 {
		warnPrePushRefsUnknown(ctx, stderrWriter)
	}
}

// CheckpointRefExists reports whether checkpointID has its own checkpoint ref
// locally, whether or not it is queued.
func CheckpointRefExists(ctx context.Context, checkpointID id.CheckpointID) (bool, error) {
	ref, err := checkpoint.RefName(checkpointID)
	if err != nil {
		return false, fmt.Errorf("checkpoint ref for %s: %w", checkpointID, err)
	}
	repo, err := OpenRepository(ctx)
	if err != nil {
		return false, fmt.Errorf("open repository: %w", err)
	}
	defer repo.Close()
	if _, err := repo.Reference(ref, true); err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("read %s: %w", ref, err)
	}
	return true, nil
}

// CheckpointWaiting reports whether checkpointID has a queued ref present
// locally, the set 'entire checkpoint push' can upload.
func CheckpointWaiting(ctx context.Context, checkpointID id.CheckpointID) (bool, error) {
	ref, err := checkpoint.RefName(checkpointID)
	if err != nil {
		return false, fmt.Errorf("checkpoint ref for %s: %w", checkpointID, err)
	}
	repo, err := OpenRepository(ctx)
	if err != nil {
		return false, fmt.Errorf("open repository: %w", err)
	}
	defer repo.Close()
	waiting, err := waitingCheckpointRefs(ctx, repo)
	if err != nil {
		return false, err
	}
	_, ok := waiting[ref]
	return ok, nil
}

// announceCheckpointPushPolicy is the pre-push hook's view of the policy. It
// reports whether automatic delivery may run, and otherwise tells the user why
// their checkpoints stayed behind. It never fails the user's push.
func announceCheckpointPushPolicy(ctx context.Context, repo *git.Repository) bool {
	automatic, err := automaticCheckpointDelivery(repo)
	if err != nil {
		logging.Warn(ctx, "git-refs pre-push: checkpoint push policy unreadable; no checkpoints pushed",
			slog.String("error", err.Error()))
		fmt.Fprintf(stderrWriter, "[entire] No checkpoints were pushed: %v\n", err)
		return false
	}
	if automatic {
		return true
	}
	if waiting := waitingCheckpointCount(ctx, repo); waiting > 0 {
		fmt.Fprintf(stderrWriter, "[entire] %s waiting, upload with: entire checkpoint push --all\n", pluralCheckpoints(waiting))
	}
	return false
}

// waitingCheckpointCount counts queued refs that exist locally, the same set
// `entire checkpoint list --outgoing` lists. A queue that cannot be read
// counts as zero: the line it feeds is a reminder, not a report.
func waitingCheckpointCount(ctx context.Context, repo *git.Repository) int {
	waiting, err := waitingCheckpointRefs(ctx, repo)
	if err != nil {
		return 0
	}
	return len(waiting)
}

func pluralCheckpoints(n int) string {
	if n == 1 {
		return "1 checkpoint"
	}
	return fmt.Sprintf("%d checkpoints", n)
}

// ErrCheckpointPushDisabled is returned by PushCheckpoints when settings turn
// checkpoint pushing off.
var ErrCheckpointPushDisabled = errors.New("checkpoint pushing is disabled in settings (push_sessions=false)")

// CheckpointPushRequest names one checkpoint ref to upload now. A non-zero
// Commit pins the version the caller looked at.
type CheckpointPushRequest struct {
	Ref    plumbing.ReferenceName
	Commit plumbing.Hash
}

// CheckpointPushStatus is what happened to one CheckpointPushRequest.
type CheckpointPushStatus string

const (
	CheckpointPushUploaded   CheckpointPushStatus = "uploaded"
	CheckpointPushChanged    CheckpointPushStatus = "changed"
	CheckpointPushFailed     CheckpointPushStatus = "failed"
	CheckpointPushNotWaiting CheckpointPushStatus = "not_waiting"
	// CheckpointPushNotFound is a named checkpoint with no ref here at all,
	// such as a mistyped ID.
	CheckpointPushNotFound CheckpointPushStatus = "not_found"
)

// CheckpointPushOutcome reports one target. Commit is the version sent, or
// for a changed target the version that is now waiting instead.
type CheckpointPushOutcome struct {
	Target CheckpointPushRequest
	Commit plumbing.Hash
	Status CheckpointPushStatus
	Err    error
}

// CheckpointPushReport is the result of PushCheckpoints, one outcome per
// target in the order given.
type CheckpointPushReport struct {
	Remote   string
	Outcomes []CheckpointPushOutcome
}

// PushCheckpoints uploads the named queued checkpoint refs to the checkpoint
// sync remote now, through the same delivery as git push, under either
// policy. It does not read the policy: naming the refs is the user's decision,
// and the policy only governs what uploads without being named. It returns
// ErrOutgoingNeedsGitRefs on the git-branch backend.
func PushCheckpoints(ctx context.Context, targets []CheckpointPushRequest) (CheckpointPushReport, error) {
	isRefs, err := primaryBackendIsGitRefs(ctx)
	if err != nil {
		return CheckpointPushReport{}, err
	}
	if !isRefs {
		return CheckpointPushReport{}, ErrOutgoingNeedsGitRefs
	}
	syncRemote, err := ResolveCheckpointSyncRemote(ctx)
	if err != nil {
		return CheckpointPushReport{}, err
	}
	if syncRemote.Name == "" {
		return CheckpointPushReport{}, errors.New("no git remote to push checkpoints to")
	}
	ps := resolvePushSettings(ctx, syncRemote.Name)
	if ps.pushDisabled {
		return CheckpointPushReport{}, ErrCheckpointPushDisabled
	}
	// OPF reads redaction config that only this sets; see opfGateForCheckpointRefs.
	if err := EnsureRedactionConfigured(ctx); err != nil {
		return CheckpointPushReport{}, fmt.Errorf("configure redaction before pushing: %w", err)
	}

	repo, err := OpenRepository(ctx)
	if err != nil {
		return CheckpointPushReport{}, fmt.Errorf("open repository: %w", err)
	}
	defer repo.Close()

	waiting, err := waitingCheckpointRefs(ctx, repo)
	if err != nil {
		return CheckpointPushReport{}, err
	}
	selection, err := waitingTargetSelection(targets, waiting)
	if err != nil {
		return CheckpointPushReport{}, err
	}

	missing := missingCheckpointRefs(repo, targets, waiting)
	report := CheckpointPushReport{Remote: syncRemote.Name}
	if len(selection.refs) == 0 {
		report.Outcomes = checkpointPushOutcomes(targets, waiting, missing, checkpointFlushResult{}, nil)
		return report, nil
	}
	if opfErr := opfGateForCheckpointRefs(ctx, repo); opfErr != nil {
		return report, fmt.Errorf("checkpoint refs stay queued: %w", opfErr)
	}
	result, flushErr := flushCheckpointRefs(ctx, repo, ps, selection)
	report.Outcomes = checkpointPushOutcomes(targets, waiting, missing, result, flushErr)
	if flushErr != nil {
		logging.Warn(ctx, "checkpoint push: delivery failed",
			slog.String("error", flushErr.Error()))
	}
	return report, nil
}

// waitingCheckpointRefs is the set of queued refs present locally.
func waitingCheckpointRefs(ctx context.Context, repo *git.Repository) (map[plumbing.ReferenceName]struct{}, error) {
	queued, err := peekCheckpointQueue(ctx, repo)
	if err != nil {
		return nil, err
	}
	existing, _ := partitionLocalRefs(repo, queued)
	set := make(map[plumbing.ReferenceName]struct{}, len(existing))
	for _, ref := range existing {
		set[ref] = struct{}{}
	}
	return set, nil
}

// waitingTargetSelection selects the named refs that were waiting when the
// queue was read. The OPF gate runs only for those, so a named ref queued
// after that read is left for the next upload rather than sent unscanned.
func waitingTargetSelection(targets []CheckpointPushRequest, waiting map[plumbing.ReferenceName]struct{}) (checkpointRefSelection, error) {
	named := make(map[plumbing.ReferenceName]plumbing.Hash, len(targets))
	for _, target := range targets {
		if pinned, ok := named[target.Ref]; ok && pinned != target.Commit {
			return checkpointRefSelection{}, fmt.Errorf("%s is named with two different commits; name each checkpoint once", target.Ref)
		}
		named[target.Ref] = target.Commit
	}
	selection := checkpointRefSelection{refs: make(map[plumbing.ReferenceName]plumbing.Hash, len(named))}
	for ref, commit := range named {
		if _, ok := waiting[ref]; ok {
			selection.refs[ref] = commit
		}
	}
	return selection, nil
}

// checkpointPushOutcomes maps the flush back onto each target. A flush-wide
// error (one with no per-ref report, such as a failed queue drain) fails every
// waiting target the flush did not report on, rather than reading as absence.
// missingCheckpointRefs is the named refs that are not waiting and do not
// exist here, as opposed to ones already uploaded.
func missingCheckpointRefs(repo *git.Repository, targets []CheckpointPushRequest, waiting map[plumbing.ReferenceName]struct{}) map[plumbing.ReferenceName]struct{} {
	missing := make(map[plumbing.ReferenceName]struct{})
	for _, target := range targets {
		if _, ok := waiting[target.Ref]; ok {
			continue
		}
		if _, err := repo.Reference(target.Ref, true); errors.Is(err, plumbing.ErrReferenceNotFound) {
			missing[target.Ref] = struct{}{}
		}
	}
	return missing
}

func checkpointPushOutcomes(
	targets []CheckpointPushRequest,
	waiting map[plumbing.ReferenceName]struct{},
	missing map[plumbing.ReferenceName]struct{},
	result checkpointFlushResult,
	flushErr error,
) []CheckpointPushOutcome {
	pushed := make(map[plumbing.ReferenceName]plumbing.Hash, len(result.pushed))
	for _, ref := range result.pushed {
		pushed[ref.name] = ref.hash
	}
	changed := make(map[plumbing.ReferenceName]plumbing.Hash, len(result.changed))
	for _, ref := range result.changed {
		changed[ref.name] = ref.hash
	}

	outcomes := make([]CheckpointPushOutcome, 0, len(targets))
	for _, target := range targets {
		outcome := CheckpointPushOutcome{Target: target}
		if hash, ok := pushed[target.Ref]; ok {
			outcome.Status, outcome.Commit = CheckpointPushUploaded, hash
		} else if hash, ok := changed[target.Ref]; ok {
			outcome.Status, outcome.Commit = CheckpointPushChanged, hash
		} else if err, ok := result.failed[target.Ref]; ok {
			outcome.Status, outcome.Err = CheckpointPushFailed, err
			if errors.Is(err, errCheckpointRefSuperseded) {
				outcome.Status = CheckpointPushChanged
			}
		} else if _, queued := waiting[target.Ref]; queued && flushErr != nil {
			outcome.Status, outcome.Err = CheckpointPushFailed, flushErr
		} else if _, absent := missing[target.Ref]; absent {
			outcome.Status = CheckpointPushNotFound
		} else {
			// Not queued, or it left the queue while this command ran.
			outcome.Status = CheckpointPushNotWaiting
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes
}
