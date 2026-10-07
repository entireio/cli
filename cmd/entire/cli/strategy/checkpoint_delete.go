package strategy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// ErrCheckpointDeleteNotFound means no local copy and no reachable remote
// holds the checkpoint.
var ErrCheckpointDeleteNotFound = errors.New("checkpoint not found")

// ErrCheckpointDeleteV1WouldPropagate refuses a delete that would remove the
// local v1 copy while leaving the copy on the remote the next pre-push sends
// the v1 branch to: that push would carry the removal there anyway.
var ErrCheckpointDeleteV1WouldPropagate = errors.New("the local v1 removal would reach an unselected remote on the next push")

// CheckpointRemoteTargetName names the dedicated checkpoint_remote store in
// delete plans and in --remote selection, which otherwise take git remote names.
const CheckpointRemoteTargetName = "checkpoint_remote"

// checkpointDeleteProbeTimeout bounds each target's ls-remote during planning
// and re-checks. An unreachable target is reported, never fatal.
const checkpointDeleteProbeTimeout = 15 * time.Second

// otherCheckpointScanLimit caps the same-session scan over local checkpoints.
const otherCheckpointScanLimit = 2000

// v1BranchRef is the git-branch backend's shared checkpoint branch.
var v1BranchRef = plumbing.NewBranchReferenceName(paths.MetadataBranchName)

// V1CopyState says whether a remote's v1 branch holds the checkpoint.
type V1CopyState string

const (
	V1CopyAbsent  V1CopyState = "absent"
	V1CopyPresent V1CopyState = "present"
	// V1CopyUnknown: the remote has a v1 branch whose tip is not in this
	// clone, so its contents are verified only when the delete runs.
	V1CopyUnknown V1CopyState = "unknown"
)

// CheckpointDeleteTarget is one concrete place a checkpoint can be deleted
// from: a single URL or path, resolved up front so the probe, the lease and the
// push all address the same repository.
type CheckpointDeleteTarget struct {
	// Remotes are the names that resolve to URL: git remote names, or
	// CheckpointRemoteTargetName for the dedicated store.
	Remotes []string
	// URL is passed verbatim to git. Display it through Display only.
	URL string
	// Reachable is false when the probe failed (ProbeError says why) or was
	// skipped (NotChecked).
	Reachable  bool
	ProbeError string
	// NotChecked: --local-only planned the delete without contacting it.
	NotChecked bool
	// RefOID is the checkpoint ref's oid on the remote, zero when absent.
	RefOID plumbing.Hash
	// RefName is the checkpoint ref as the remote spells it (its shard's case
	// can differ from a fresh computation).
	RefName plumbing.ReferenceName
	// V1Tip is the remote's v1 branch tip, zero when it has none.
	V1Tip plumbing.Hash
	V1    V1CopyState
}

// Display returns the target's URL with credentials redacted.
func (t CheckpointDeleteTarget) Display() string { return remote.RedactURLOrPath(t.URL) }

// Name returns the target's primary name for messages and retry commands.
func (t CheckpointDeleteTarget) Name() string {
	if len(t.Remotes) == 0 {
		return t.Display()
	}
	return t.Remotes[0]
}

// HoldsCheckpoint reports whether the target has (or may have) a copy.
func (t CheckpointDeleteTarget) HoldsCheckpoint() bool {
	return t.Reachable && (!t.RefOID.IsZero() || t.V1 == V1CopyPresent || t.V1 == V1CopyUnknown)
}

// CheckpointDeleteSession is one session stored in the checkpoint, with the
// token delta the checkpoint carries for it.
type CheckpointDeleteSession struct {
	SessionID    string
	Agent        string
	InputTokens  int
	OutputTokens int
}

// CheckpointDeleteState is a local session state that references the
// checkpoint and would otherwise write it again.
type CheckpointDeleteState struct {
	SessionID string
	// Active is true for a session that has not ended and whose agent may
	// still be running.
	Active bool
	// Fields names the state fields that will be cleared.
	Fields []string
}

// CheckpointDeleteSibling is another local checkpoint that carries one of the
// deleted checkpoint's sessions.
type CheckpointDeleteSibling struct {
	CheckpointID id.CheckpointID
	SessionIDs   []string
}

// CheckpointDeletePlan is everything a delete would touch, gathered without
// writing anything.
type CheckpointDeletePlan struct {
	CheckpointID id.CheckpointID
	// LocalRef is the local per-checkpoint ref ("" when absent).
	LocalRef    plumbing.ReferenceName
	LocalRefOID plumbing.Hash
	// LocalV1 reports a copy on the local entire/checkpoints/v1 branch.
	LocalV1 bool
	// TrackingV1 lists remote names whose remote-tracking v1 ref holds it.
	// Those refs move only when that remote's copy is deleted.
	TrackingV1 []string
	// V1PushURLs are where the next pre-push sends the local v1 branch: set
	// only when git-branch is the primary backend and pushes are enabled.
	V1PushURLs            []string
	Targets               []CheckpointDeleteTarget
	Sessions              []CheckpointDeleteSession
	SessionStates         []CheckpointDeleteState
	OtherLocalCheckpoints []CheckpointDeleteSibling
	OtherLocalTruncated   bool
	// branchPrimary records a git-branch primary, whose remote v1 branches
	// can hold ULID checkpoints too.
	branchPrimary bool
	// PushSessionsDisabled reports push_sessions=false; the delete still
	// reaches remotes when the user asks for it.
	PushSessionsDisabled bool
	Warnings             []string
}

// HasLocalCopy reports whether this clone holds the checkpoint.
func (p *CheckpointDeletePlan) HasLocalCopy() bool {
	return p.LocalRef != "" || p.LocalV1
}

// HolderTargets returns the reachable targets that hold the checkpoint.
func (p *CheckpointDeletePlan) HolderTargets() []CheckpointDeleteTarget {
	var out []CheckpointDeleteTarget
	for _, t := range p.Targets {
		if t.HoldsCheckpoint() {
			out = append(out, t)
		}
	}
	return out
}

// ActiveStates returns the session states whose session is still running.
func (p *CheckpointDeletePlan) ActiveStates() []CheckpointDeleteState {
	var out []CheckpointDeleteState
	for _, s := range p.SessionStates {
		if s.Active {
			out = append(out, s)
		}
	}
	return out
}

// CheckpointDeletePlanOptions narrows what PlanCheckpointDelete inspects.
type CheckpointDeletePlanOptions struct {
	// LocalOnly resolves the remote targets but never contacts them: each is
	// marked NotChecked.
	LocalOnly bool
}

// UnreachableTargetNames names the targets the probe could not reach.
func (p *CheckpointDeletePlan) UnreachableTargetNames() []string {
	var names []string
	for _, t := range p.Targets {
		if !t.Reachable && !t.NotChecked {
			names = append(names, t.Name())
		}
	}
	return names
}

// CheckpointDeleteSelection says how the remote targets were chosen, so a
// refusal can name a remedy that works for that way of choosing.
type CheckpointDeleteSelection int

const (
	// SelectionAllHolders: every reachable holder (no narrowing).
	SelectionAllHolders CheckpointDeleteSelection = iota
	// SelectionLocalOnly: --local-only, no remote at all.
	SelectionLocalOnly
	// SelectionRemoteFlag: narrowed with --remote.
	SelectionRemoteFlag
	// SelectionPrompt: narrowed in the interactive remote picker.
	SelectionPrompt
)

// CheckLocalV1Propagation refuses a delete whose local v1 removal the next
// pre-push would carry to a remote the delete leaves alone. The removal is a
// commit on the local v1 branch, and pre-push fast-forwards each v1 push
// destination to it, deleting that destination's copy too. A destination
// probed and found without a v1 copy loses nothing; one that could not be
// probed is refused, since its copy cannot be ruled out.
//
// Known limitation: a push to a remote that is not yet the sync remote can
// elect it on the spot (capture) and send v1 there; such a remote is not a
// destination here until it is elected.
func (p *CheckpointDeletePlan) CheckLocalV1Propagation(targets []CheckpointDeleteTarget, selection CheckpointDeleteSelection) error {
	if !p.LocalV1 {
		return nil
	}
	for _, url := range p.V1PushURLs {
		if selection != SelectionLocalOnly && slices.ContainsFunc(targets, func(t CheckpointDeleteTarget) bool { return t.URL == url }) {
			continue
		}
		dest := CheckpointDeleteTarget{URL: url}
		if i := slices.IndexFunc(p.Targets, func(t CheckpointDeleteTarget) bool { return t.URL == url }); i >= 0 {
			dest = p.Targets[i]
		}
		if dest.Reachable && dest.V1 == V1CopyAbsent {
			continue
		}
		return p.v1PropagationError(dest, selection)
	}
	return nil
}

func (p *CheckpointDeletePlan) v1PropagationError(dest CheckpointDeleteTarget, selection CheckpointDeleteSelection) error {
	where := fmt.Sprintf("it is on the local %s branch, which the next git push sends to %s (%s)", paths.MetadataBranchName, dest.Name(), dest.Display())
	if !dest.Reachable {
		return fmt.Errorf("refusing to delete checkpoint %s: %w: %s, but %s could not be reached to check its copy; retry when %s is reachable",
			p.CheckpointID, ErrCheckpointDeleteV1WouldPropagate, where, dest.Name(), dest.Name())
	}
	var remedy string
	switch selection {
	case SelectionLocalOnly:
		remedy = "run without --local-only to delete that copy as well, or switch to the git-refs checkpoint backend"
	case SelectionRemoteFlag:
		remedy = "add --remote " + dest.Name() + " to delete that copy as well"
	case SelectionPrompt:
		remedy = "select " + dest.Name() + " too"
	case SelectionAllHolders:
		remedy = "delete it from " + dest.Name() + " as well"
	}
	return fmt.Errorf("refusing to delete checkpoint %s: %w: %s, removing that copy too; %s",
		p.CheckpointID, ErrCheckpointDeleteV1WouldPropagate, where, remedy)
}

// PlanCheckpointDelete inspects the local repository and every checkpoint
// remote for cid. It writes nothing: no fetch, no ref update, no state save.
func PlanCheckpointDelete(ctx context.Context, cid id.CheckpointID, opts CheckpointDeletePlanOptions) (*CheckpointDeletePlan, error) {
	if cid.Kind() == id.KindUnknown {
		return nil, fmt.Errorf("invalid checkpoint ID %q", cid)
	}
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree root: %w", err)
	}
	ctx = settings.WithWorktreeRoot(ctx, root)
	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil, err
	}
	defer repo.Close()

	plan := &CheckpointDeletePlan{CheckpointID: cid}
	if s, loadErr := settings.Load(ctx); loadErr == nil {
		plan.PushSessionsDisabled = s.IsPushSessionsDisabled()
	}

	plan.LocalRef, plan.LocalRefOID, err = findLocalCheckpointRef(ctx, root, cid)
	if err != nil {
		return nil, err
	}
	// Any ID kind: a git-branch mirror writes ULID checkpoints to v1 too.
	plan.LocalV1 = commitHasCheckpoint(repo, refTip(repo, v1BranchRef), cid)
	plan.TrackingV1 = trackingV1Holders(ctx, repo, cid)

	plan.branchPrimary = !primaryIsGitRefs(ctx)
	targets := resolveDeleteTargets(ctx, root, plan)
	if opts.LocalOnly {
		// No remote is contacted, except a v1 push destination when this
		// clone holds a v1 copy: CheckLocalV1Propagation needs to know
		// whether the next push would remove a copy there.
		var probe, skip []CheckpointDeleteTarget
		for _, t := range targets {
			if plan.LocalV1 && slices.Contains(plan.V1PushURLs, t.URL) {
				probe = append(probe, t)
				continue
			}
			t.NotChecked = true
			skip = append(skip, t)
		}
		plan.Targets = append(probeDeleteTargets(ctx, root, repo, cid, probe, plan.branchPrimary), skip...)
	} else {
		plan.Targets = probeDeleteTargets(ctx, root, repo, cid, targets, plan.branchPrimary)
	}
	for _, t := range plan.Targets {
		if !t.Reachable && !t.NotChecked {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("could not reach %s (%s): %s", t.Name(), t.Display(), t.ProbeError))
		}
	}

	if !plan.HasLocalCopy() && len(plan.HolderTargets()) == 0 {
		return plan, notFoundError(plan, opts.LocalOnly)
	}

	readCheckpointSessions(ctx, repo, plan)
	if err := collectDeleteStates(ctx, plan); err != nil {
		return nil, err
	}
	collectOtherSessionCheckpoints(ctx, repo, plan)
	return plan, nil
}

// notFoundError says where the checkpoint was looked for, naming any remote
// that could not be checked, since one of those may still hold it.
func notFoundError(plan *CheckpointDeletePlan, localOnly bool) error {
	if localOnly {
		return fmt.Errorf("%w: %s is not in this repository (remotes were not checked: --local-only)", ErrCheckpointDeleteNotFound, plan.CheckpointID)
	}
	if unreachable := plan.UnreachableTargetNames(); len(unreachable) > 0 {
		return fmt.Errorf("%w: %s is not in this repository or on any reachable checkpoint remote (could not reach %s)",
			ErrCheckpointDeleteNotFound, plan.CheckpointID, strings.Join(unreachable, ", "))
	}
	return fmt.Errorf("%w: %s is not in this repository or on any reachable checkpoint remote", ErrCheckpointDeleteNotFound, plan.CheckpointID)
}

// findLocalCheckpointRef lists refs/entire/checkpoints/*/<id> and matches with
// ParseRef, so a ref stored under a differently-cased shard is still found.
func findLocalCheckpointRef(ctx context.Context, root string, cid id.CheckpointID) (plumbing.ReferenceName, plumbing.Hash, error) {
	cmd := exec.CommandContext(ctx, "git", "for-each-ref", "--format=%(objectname) %(refname)",
		checkpoint.CheckpointRefPrefix+"*/"+cid.String())
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "", plumbing.ZeroHash, fmt.Errorf("list local checkpoint refs: %w", err)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		oid, name, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		ref := plumbing.ReferenceName(name)
		if parsed, valid := checkpoint.ParseRef(ref); valid && parsed == cid {
			return ref, plumbing.NewHash(oid), nil
		}
	}
	return "", plumbing.ZeroHash, nil
}

func refTip(repo *git.Repository, name plumbing.ReferenceName) plumbing.Hash {
	ref, err := repo.Reference(name, true)
	if err != nil {
		return plumbing.ZeroHash
	}
	return ref.Hash()
}

// commitHasCheckpoint reports whether the v1 commit's tree has cid's subtree.
// A commit not present in this clone reads as false.
func commitHasCheckpoint(repo *git.Repository, commitHash plumbing.Hash, cid id.CheckpointID) bool {
	if commitHash.IsZero() {
		return false
	}
	commit, err := repo.CommitObject(commitHash)
	if err != nil {
		return false
	}
	tree, err := commit.Tree()
	if err != nil {
		return false
	}
	_, err = tree.Tree(cid.Path())
	return err == nil
}

// trackingV1Holders lists remotes whose refs/remotes/<name>/entire/checkpoints/v1
// holds cid: reads fall back to these, so a delete must move them too.
func trackingV1Holders(ctx context.Context, repo *git.Repository, cid id.CheckpointID) []string {
	var out []string
	for _, name := range configuredRemoteNames(repo) {
		tracking := plumbing.NewRemoteReferenceName(name, paths.MetadataBranchName)
		if commitHasCheckpoint(repo, refTip(repo, tracking), cid) {
			out = append(out, name)
		}
	}
	logging.Debug(ctx, "checkpoint delete: tracking v1 holders", slog.Int("count", len(out)))
	return out
}

func configuredRemoteNames(repo *git.Repository) []string {
	remotes, err := repo.Remotes()
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(remotes))
	for _, r := range remotes {
		names = append(names, r.Config().Name)
	}
	slices.Sort(names)
	return names
}

// deleteTargetCandidate is a name → URL pair before deduplication.
type deleteTargetCandidate struct {
	name string
	url  string
}

// resolveDeleteTargets resolves every place this clone's checkpoints can live
// to concrete URLs: the elected sync remote's push destination, the dedicated
// checkpoint_remote (push and fetch URLs), and each read candidate's fetch and
// first push URL (origin is the legacy tier). Names are resolved here, once,
// because git push would otherwise rewrite a name to the checkpoint_remote URL
// and token auth resolves names through the fetch URL.
func resolveDeleteTargets(ctx context.Context, root string, plan *CheckpointDeletePlan) []CheckpointDeleteTarget {
	var candidates []deleteTargetCandidate
	add := func(name, url string) {
		if url != "" {
			candidates = append(candidates, deleteTargetCandidate{name: name, url: url})
		}
	}
	var v1Dests []string
	addV1Dest := func(name, url string) {
		add(name, url)
		if url != "" && !slices.Contains(v1Dests, url) {
			v1Dests = append(v1Dests, url)
		}
	}

	elected, electErr := ResolveCheckpointSyncRemote(ctx)
	if electErr != nil {
		plan.Warnings = append(plan.Warnings, "could not resolve the checkpoint sync remote: "+electErr.Error())
	}
	s, settingsErr := settings.Load(ctx)
	checkpointRemoteConfigured := settingsErr == nil && s.GetCheckpointRemote() != nil
	branchPrimary := !primaryIsGitRefs(ctx)

	// With a dedicated checkpoint_remote, pre-push derives its URL from
	// whichever remote the user pushes to and bypasses the sync-remote gate,
	// so every configured remote can send v1 there, elected or not.
	electedUsesDedicated := false
	if checkpointRemoteConfigured {
		pushNames, _ := configuredRemotesInConfigOrderResult(ctx) //nolint:errcheck // an unreadable remote list adds no destinations
		if elected.Name != "" && !slices.Contains(pushNames, elected.Name) {
			pushNames = append([]string{elected.Name}, pushNames...)
		}
		for _, name := range pushNames {
			if url, enabled, err := remote.PushURL(ctx, name); err == nil && enabled {
				addV1Dest(CheckpointRemoteTargetName, url)
				electedUsesDedicated = electedUsesDedicated || name == elected.Name
			}
		}
		lead := LeadCheckpointReadRemote(ctx)
		if dedicated, err := remote.ReadsDedicatedStore(ctx, lead); err == nil && dedicated {
			if url, urlErr := remote.FetchURL(ctx, remote.FetchURLOptions{WorktreeRoot: root, LeadReadRemote: lead}); urlErr == nil {
				add(CheckpointRemoteTargetName, url)
			}
		}
	}

	names := []string{}
	if elected.Name != "" {
		names = append(names, elected.Name)
	}
	for _, c := range CheckpointReadRemotes(ctx) {
		if !slices.Contains(names, c) {
			names = append(names, c)
		}
	}
	for _, name := range names {
		if urls, err := remote.GetPushURLs(ctx, name); err == nil && len(urls) > 0 {
			switch {
			case name == elected.Name && branchPrimary && !electedUsesDedicated:
				// git pushes the v1 branch to every pushurl of the sync remote.
				for _, url := range urls {
					addV1Dest(name, url)
				}
			default:
				add(name, urls[0])
			}
		}
		if url, err := remote.GetRemoteURL(ctx, name); err == nil {
			add(name, url)
		}
	}
	// pre-push sends the local v1 branch only on a git-branch primary, and
	// not at all while push_sessions is disabled.
	if branchPrimary && !plan.PushSessionsDisabled {
		plan.V1PushURLs = v1Dests
	}
	return dedupeDeleteTargets(candidates)
}

// dedupeDeleteTargets merges candidates that resolved to the same URL, keeping
// first-seen order of both URLs and names.
func dedupeDeleteTargets(candidates []deleteTargetCandidate) []CheckpointDeleteTarget {
	var targets []CheckpointDeleteTarget
	index := map[string]int{}
	for _, c := range candidates {
		i, seen := index[c.url]
		if !seen {
			index[c.url] = len(targets)
			targets = append(targets, CheckpointDeleteTarget{URL: c.url, Remotes: []string{c.name}})
			continue
		}
		if !slices.Contains(targets[i].Remotes, c.name) {
			targets[i].Remotes = append(targets[i].Remotes, c.name)
		}
	}
	return targets
}

// probeDeleteTargets ls-remotes each target for the checkpoint ref and the v1
// branch. A failed probe marks the target unreachable.
func probeDeleteTargets(ctx context.Context, root string, repo *git.Repository, cid id.CheckpointID, targets []CheckpointDeleteTarget, branchPrimary bool) []CheckpointDeleteTarget {
	for i := range targets {
		t := &targets[i]
		listing, err := lsRemoteCheckpoint(ctx, root, t.URL, cid)
		if err != nil {
			t.ProbeError = err.Error()
			continue
		}
		t.Reachable = true
		t.RefName, t.RefOID = listing.ref, listing.refOID
		t.V1Tip = listing.v1Tip
		t.V1 = classifyRemoteV1(repo, cid, listing.v1Tip, branchPrimary)
	}
	return targets
}

func classifyRemoteV1(repo *git.Repository, cid id.CheckpointID, tip plumbing.Hash, branchPrimary bool) V1CopyState {
	switch {
	case tip.IsZero():
		return V1CopyAbsent
	case commitHasCheckpoint(repo, tip, cid):
		return V1CopyPresent
	default:
		if _, err := repo.CommitObject(tip); err == nil {
			return V1CopyAbsent // the tip is local and has no such subtree
		}
		// On a git-refs primary a ULID is never pushed to a remote v1 branch
		// (pre-push does not push a git-branch mirror), so an unfetched tip
		// cannot hold one; treating it as unknown would fetch the whole branch
		// for every ULID delete. A git-branch primary pushes whatever IDs it
		// wrote, so its tip has to be checked at delete time.
		if cid.Kind() != id.KindLegacy && !branchPrimary {
			return V1CopyAbsent
		}
		return V1CopyUnknown
	}
}

type remoteCheckpointListing struct {
	ref    plumbing.ReferenceName
	refOID plumbing.Hash
	v1Tip  plumbing.Hash
}

// lsRemoteCheckpoint lists cid's ref and the v1 branch on target. The ref is
// matched by its id leaf (ls-remote patterns match a ref's tail) and confirmed
// with ParseRef, so a remote that stored it under a differently-cased shard is
// still found.
func lsRemoteCheckpoint(ctx context.Context, root, target string, cid id.CheckpointID) (remoteCheckpointListing, error) {
	probeCtx, cancel := context.WithTimeout(ctx, checkpointDeleteProbeTimeout)
	defer cancel()
	out, err := remote.LsRemoteInDir(probeCtx, root, target, cid.String(), v1BranchRef.String())
	if err != nil {
		return remoteCheckpointListing{}, fmt.Errorf("ls-remote: %w", err)
	}
	return parseCheckpointLsRemote(out, cid), nil
}

func parseCheckpointLsRemote(out []byte, cid id.CheckpointID) remoteCheckpointListing {
	var listing remoteCheckpointListing
	for line := range bytes.SplitSeq(out, []byte("\n")) {
		fields := strings.Fields(string(line))
		if len(fields) != 2 || !plumbing.IsHash(fields[0]) {
			continue
		}
		name := plumbing.ReferenceName(fields[1])
		if name == v1BranchRef {
			listing.v1Tip = plumbing.NewHash(fields[0])
			continue
		}
		if parsed, ok := checkpoint.ParseRef(name); ok && parsed == cid {
			listing.ref, listing.refOID = name, plumbing.NewHash(fields[0])
		}
	}
	return listing
}

// readCheckpointSessions reads the checkpoint's sessions from local data only.
// A remote-only checkpoint has nothing local to read and lists no sessions.
func readCheckpointSessions(ctx context.Context, repo *git.Repository, plan *CheckpointDeletePlan) {
	store, err := openLocalOnlyStore(ctx, repo)
	if err != nil {
		plan.Warnings = append(plan.Warnings, "could not open the local checkpoint store: "+err.Error())
		return
	}
	summary, err := store.Read(ctx, plan.CheckpointID)
	if err != nil || summary == nil {
		return
	}
	for i := range summary.Sessions {
		meta, metaErr := store.ReadSessionMetadata(ctx, plan.CheckpointID, i)
		if metaErr != nil || meta == nil {
			continue
		}
		entry := CheckpointDeleteSession{SessionID: meta.SessionID, Agent: string(meta.Agent)}
		if meta.TokenUsage != nil {
			entry.InputTokens = meta.TokenUsage.InputTokens
			entry.OutputTokens = meta.TokenUsage.OutputTokens
		}
		plan.Sessions = append(plan.Sessions, entry)
	}
}

func openLocalOnlyStore(ctx context.Context, repo *git.Repository) (checkpoint.PersistentStore, error) {
	refs := checkpoint.ResolveRefs(ctx).PrimaryAsLocalRead()
	// ReadRemotes nil on purpose: the plan describes what THIS clone holds.
	stores, err := checkpoint.Open(ctx, repo, checkpoint.OpenOptions{Refs: &refs, ReadRemotes: nil})
	if err != nil {
		return nil, fmt.Errorf("open checkpoint store: %w", err)
	}
	return stores.Persistent, nil
}

// collectDeleteStates finds the session states that reference cid. Read-only:
// it lists without the orphan cleanup the hook-path listing performs.
func collectDeleteStates(ctx context.Context, plan *CheckpointDeletePlan) error {
	store, err := session.NewStateStore(ctx)
	if err != nil {
		return fmt.Errorf("open session state store: %w", err)
	}
	states, err := store.ListReadOnly(ctx)
	if err != nil {
		return fmt.Errorf("list session states: %w", err)
	}
	for _, st := range states {
		fields := deleteStateFields(st, plan.CheckpointID)
		if len(fields) == 0 {
			continue
		}
		plan.SessionStates = append(plan.SessionStates, CheckpointDeleteState{
			SessionID: st.SessionID,
			Active:    !st.IsEnded() && !st.OwnerExited(),
			Fields:    fields,
		})
	}
	return nil
}

// deleteStateFields names the fields of st that hold cid. These are the only
// fields a delete clears: token offsets, usage and baselines are never touched,
// since resetting any of them would make the next checkpoint re-count tokens
// already stored in surviving checkpoints.
func deleteStateFields(st *SessionState, cid id.CheckpointID) []string {
	var fields []string
	if st.LastCheckpointID == cid {
		fields = append(fields, "last_checkpoint_id")
	}
	if st.PendingCondensationID() == cid {
		fields = append(fields, "condensation_attempt")
	}
	if slices.Contains(st.TurnCheckpointIDs, cid.String()) {
		fields = append(fields, "turn_checkpoint_ids")
	}
	return fields
}

// clearDeletedCheckpointFromState removes cid from st, reporting whether
// anything changed.
func clearDeletedCheckpointFromState(st *SessionState, cid id.CheckpointID) bool {
	changed := false
	if st.LastCheckpointID == cid {
		st.LastCheckpointID = id.EmptyCheckpointID
		st.LastCheckpointCommitHash = ""
		changed = true
	}
	if st.PendingCondensationID() == cid {
		st.ClearCondensationAttempt()
		changed = true
	}
	if i := slices.Index(st.TurnCheckpointIDs, cid.String()); i >= 0 {
		st.TurnCheckpointIDs = slices.DeleteFunc(st.TurnCheckpointIDs, func(s string) bool { return s == cid.String() })
		changed = true
	}
	return changed
}

// collectOtherSessionCheckpoints lists other local checkpoints that carry one
// of the deleted checkpoint's sessions. Local only, and capped: phase 1 warns
// about them, it never deletes them.
func collectOtherSessionCheckpoints(ctx context.Context, repo *git.Repository, plan *CheckpointDeletePlan) {
	if len(plan.Sessions) == 0 {
		return
	}
	wanted := make(map[string]bool, len(plan.Sessions))
	for _, s := range plan.Sessions {
		wanted[s.SessionID] = true
	}
	store, err := openLocalOnlyStore(ctx, repo)
	if err != nil {
		return
	}
	infos, err := store.List(ctx)
	if err != nil {
		plan.Warnings = append(plan.Warnings, "could not scan local checkpoints for the same sessions: "+err.Error())
		return
	}
	if len(infos) > otherCheckpointScanLimit {
		infos = infos[:otherCheckpointScanLimit]
		plan.OtherLocalTruncated = true
	}
	for _, info := range infos {
		if info.CheckpointID == plan.CheckpointID {
			continue
		}
		var shared []string
		for _, sid := range sessionIDsOf(info) {
			if wanted[sid] && !slices.Contains(shared, sid) {
				shared = append(shared, sid)
			}
		}
		if len(shared) > 0 {
			plan.OtherLocalCheckpoints = append(plan.OtherLocalCheckpoints, CheckpointDeleteSibling{CheckpointID: info.CheckpointID, SessionIDs: shared})
		}
	}
}

func sessionIDsOf(info checkpoint.CheckpointInfo) []string {
	if len(info.SessionIDs) > 0 {
		return info.SessionIDs
	}
	if info.SessionID != "" {
		return []string{info.SessionID}
	}
	return nil
}
