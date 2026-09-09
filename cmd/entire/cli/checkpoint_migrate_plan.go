package cli

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
)

// This file is the pure half of `entire checkpoint migrate`: the planner that turns
// a snapshot of the repo (remotes, election, inventories, flags) into an ordered
// plan, and every user-facing string the command prints. Nothing here touches
// git, settings, or the network, so the whole state table is unit-testable with
// literals. The executor in checkpoint_migrate.go gathers the inputs, runs the
// steps, and phrases outcomes through the format helpers below.

// checkpointMigrateOptions are the command's flags.
type checkpointMigrateOptions struct {
	To     string
	From   []string
	Remove bool
	Yes    bool
	DryRun bool
	JSON   bool
}

// syncInventory is what one remote holds of checkpoint data, from ls-remote.
// Err records that the listing failed; such a remote is reported and left
// untouched, never treated as empty.
type syncInventory struct {
	Refs        []plumbing.ReferenceName
	HasV1Branch bool
	Err         error
	// Hashes and V1Tip carry the listing's advertised hashes so the executor
	// acts on the exact listing the user consented to, without a second
	// ls-remote.
	Hashes map[plumbing.ReferenceName]plumbing.Hash
	V1Tip  plumbing.Hash
}

// holdsArtifacts reports whether the remote is known to hold checkpoint data.
func (i *syncInventory) holdsArtifacts() bool {
	return i != nil && i.Err == nil && (len(i.Refs) > 0 || i.HasV1Branch)
}

// syncRemoteInfo is one configured remote as the planner sees it.
type syncRemoteInfo struct {
	Name string
	// PushURLs are for display only (git remote -v order, possibly insteadOf
	// rewritten); classification comes from IsEntire, which reads raw config.
	PushURLs []string
	IsEntire bool
	// Inventory is nil when the remote was not listed (the planner
	// short-circuited before needing network).
	Inventory *syncInventory
}

// displayURL renders the remote's first push URL for prose: credentials
// removed, the https:// scheme and a trailing .git dropped so "origin
// (github.com/o/r)" reads as a place rather than a link. Other schemes
// (entire://, ssh, file paths) keep their spelling because it is the
// information. "" when no URL is known.
func (r syncRemoteInfo) displayURL() string {
	if len(r.PushURLs) == 0 {
		return ""
	}
	return compactRemoteURL(r.PushURLs[0])
}

func compactRemoteURL(raw string) string {
	u := gitremote.RedactURLOrPath(raw)
	for _, scheme := range []string{"https://", "http://"} {
		if rest, ok := strings.CutPrefix(u, scheme); ok {
			return strings.TrimSuffix(rest, ".git")
		}
	}
	return u
}

// nameWithURL renders "origin (github.com/o/r)", or just the name when no URL
// is known.
func (r syncRemoteInfo) nameWithURL() string {
	if u := r.displayURL(); u != "" {
		return r.Name + " (" + u + ")"
	}
	return r.Name
}

// syncElection is the resolved checkpoint sync election, decoupled from the
// strategy type so the planner stays free of that package.
type syncElection struct {
	Name   string
	Source string
}

// Election sources the planner recognises. They mirror the strategy package's
// CheckpointSyncRemoteSource values plus the planner-only "flag" (chosen by
// --to).
const (
	syncSourceConfig   = "config"
	syncSourceObserved = "observed"
	syncSourceEntire   = gitremote.ProtocolEntire // the source is named after the URL scheme
	syncSourceDefault  = "default"
	syncSourceSole     = "sole"
	syncSourceFirst    = "first"
	syncSourceFlag     = "flag"
)

// syncMarkerState is the persisted migration ledger read from the git common
// dir: "" (pending), "done", or "declined".
type syncMarkerState string

const (
	syncMarkerPending  syncMarkerState = ""
	syncMarkerDone     syncMarkerState = "done"
	syncMarkerDeclined syncMarkerState = "declined"
)

// syncInputs is everything the planner needs, gathered by the executor.
type syncInputs struct {
	Remotes            []syncRemoteInfo // .git/config order
	Elected            syncElection
	ElectionErr        error
	EntireRemotes      []string
	DedicatedRemote    bool   // strategy_options.checkpoint_remote is in effect
	DedicatedRepo      string // its org/repo slug, for the message
	PrimaryIsRefs      bool
	BackendEnvOverride bool // ENTIRE_CHECKPOINTS_PRIMARY is set
	// LocalCheckpoints counts checkpoints in this clone: refs on git-refs, v1
	// checkpoints on git-branch.
	LocalCheckpoints int
	// LocalRefs are the local refs/entire/checkpoints/* names (empty on
	// git-branch), used to decide whether the destination already has them all.
	LocalRefs []plumbing.ReferenceName
	// HasLocalV1 reports a local entire/checkpoints/v1 branch, whichever store
	// is primary; it is what the conversion step converts.
	HasLocalV1           bool
	QueuedRefs           int
	PushSessionsDisabled bool
	Marker               syncMarkerState
	Opts                 checkpointMigrateOptions
	Interactive          bool
}

func (in syncInputs) remote(name string) (syncRemoteInfo, bool) {
	for _, r := range in.Remotes {
		if r.Name == name {
			return r, true
		}
	}
	return syncRemoteInfo{}, false
}

func (in syncInputs) remoteNames() []string {
	names := make([]string, 0, len(in.Remotes))
	for _, r := range in.Remotes {
		names = append(names, r.Name)
	}
	return names
}

// syncState names the situation the planner found. The letters in comments are
// the rows of the state table in the design plan.
type syncState string

const (
	syncStateNoRemotes         syncState = "no_remotes"
	syncStateDedicatedRemote   syncState = "dedicated_remote"   // (h)
	syncStateFailClosed        syncState = "fail_closed"        // (g)
	syncStateChooseDestination syncState = "choose_destination" // (b), (f)
	syncStatePinnedElsewhere   syncState = "pinned_elsewhere"   // (o)
	syncStateAlreadyHome       syncState = "already_home"       // (a), (n)
	syncStateSetDestination    syncState = "set_destination"    // (e)
	syncStatePublishOnly       syncState = "publish_only"       // (d)
	syncStateMigrate           syncState = "migrate"            // (c), (i)
)

// syncStep is one unit of work the executor performs, in plan order.
type syncStep string

const (
	stepWriteDestination syncStep = "write_destination"
	stepConvertV1        syncStep = "convert_v1"
	stepConvertBackend   syncStep = "convert_backend"
	stepHydrate          syncStep = "hydrate"
	stepRequeue          syncStep = "requeue"
	stepPublish          syncStep = "publish"
	stepVerify           syncStep = "verify"
	stepRemove           syncStep = "remove"
	stepRecordMarker     syncStep = "record_marker"
)

// syncAdvice is one "label: command" pair printed when the command reports
// rather than acts.
type syncAdvice struct {
	Label   string
	Command string
}

// syncPlan is the planner's output.
type syncPlan struct {
	State             syncState
	Destination       string
	DestinationSource string
	// WriteDestinationSetting records that --to names a remote the election
	// would not pick on its own, so checkpoint_push_remote must be written.
	WriteDestinationSetting bool
	// Sources are the old remotes holding checkpoint artifacts, config order.
	Sources []string
	// UnknownSources could not be listed; they are reported, never removed.
	UnknownSources []string
	// Candidates is the picker list for states that need a choice, first
	// entry being the safe no-change default.
	Candidates []string
	Steps      []syncStep
	// Reason is the one-paragraph explanation of the state, already phrased.
	Reason string
	// Warn marks Reason as a warning (rendered with the "  ! " prefix).
	Warn bool
	// Notes are extra plain lines printed after Reason.
	Notes []string
	// Next is what to run, printed when the command only reports.
	Next []syncAdvice
	// NextCommand is the primary command from Next, "" when nothing to do.
	NextCommand string
	// ExitError makes the command exit 1 after printing.
	ExitError error
}

func (p syncPlan) hasStep(s syncStep) bool { return slices.Contains(p.Steps, s) }

// originRemoteName is git's conventional default remote, which the election
// falls back to and which prose therefore names as "the old remote".
const originRemoteName = "origin"

// Copy fragments shared by several states.
const (
	syncCmd          = "entire checkpoint migrate"
	syncSettingKey   = "strategy_options.checkpoint_push_remote"
	syncLocalFile    = ".entire/settings.local.json"
	syncProjectFile  = ".entire/settings.json"
	syncV1BranchName = "entire/checkpoints/v1"
	entireRemoteNote = "your Entire remote"

	syncReasonNoRemotes = "This repo has no git remotes, so there is nowhere to sync checkpoints to. Add one with git remote add, then rerun."
	syncNoteLocalFile   = "This records " + syncSettingKey + " in " + syncLocalFile + " (a per-clone choice; it is not committed)."
	syncNoteRemoveKey   = "or remove " + syncSettingKey + " from " + syncLocalFile + " (or " + syncProjectFile + " if it lives there)."
	syncNothingChanged  = "Nothing changed. Run " + syncCmd + " when you are ready."
)

// planCheckpointSync decides what `entire checkpoint migrate` should do from a
// snapshot of the repo. Pure: no I/O.
//
// Vocabulary: the `sync*` names in this file (syncPlan, syncStep, syncInputs)
// refer to the checkpoint sync remote — the destination `entire status` reports
// as "Checkpoints sync to" — not to the command, which is `migrate`.
func planCheckpointSync(in syncInputs) syncPlan {
	var plan syncPlan

	if len(in.Remotes) == 0 {
		plan.State = syncStateNoRemotes
		plan.Reason = syncReasonNoRemotes
		return plan
	}
	if in.DedicatedRemote {
		plan.State = syncStateDedicatedRemote
		plan.Reason = fmt.Sprintf("This repo stores checkpoints in a dedicated checkpoint repository (%s, strategy_options.checkpoint_remote). "+
			"checkpoint migrate manages the single-remote setup and does not move data out of a dedicated checkpoint repository. "+
			"Remove checkpoint_remote from your settings first if you want checkpoints to live on one of this repo's remotes instead.",
			in.DedicatedRepo)
		return plan
	}
	if in.ElectionErr != nil && in.Opts.To == "" {
		return planFailClosed(in)
	}

	if done := planDestination(in, &plan); done {
		return plan
	}
	if err := validateFromFlags(in, plan.Destination); err != nil {
		plan.ExitError = err
		return plan
	}
	plan.Sources, plan.UnknownSources = planSources(in, plan.Destination)
	planSteps(in, &plan)
	planState(in, &plan)
	return plan
}

// planFailClosed handles state (g): checkpoint_push_remote names a remote that
// does not exist, so nothing syncs until it is fixed.
func planFailClosed(in syncInputs) syncPlan {
	plan := syncPlan{
		State:  syncStateFailClosed,
		Reason: "Checkpoints are NOT syncing: " + in.ElectionErr.Error(),
		Warn:   true,
		Notes:  []string{syncNoteRemoveKey},
		Next:   []syncAdvice{{Label: "Fix", Command: syncCmd + " --to <remote>   (rewrites the setting in " + syncLocalFile + ")"}},
	}
	plan.NextCommand = syncCmd + " --to <remote>"
	if in.Interactive && !in.Opts.JSON {
		plan.Candidates = in.remoteNames()
	} else {
		plan.ExitError = in.ElectionErr
	}
	return plan
}

// planDestination resolves where checkpoints should go. It returns true when
// the plan is complete (a choice is needed, or --to is invalid).
func planDestination(in syncInputs, plan *syncPlan) bool {
	automatic := in.Elected.Source == syncSourceDefault || in.Elected.Source == syncSourceSole || in.Elected.Source == syncSourceFirst

	switch {
	case in.Opts.To != "":
		if _, ok := in.remote(in.Opts.To); !ok {
			plan.ExitError = fmt.Errorf("--to %q is not a configured git remote", in.Opts.To)
			return true
		}
		plan.Destination = in.Opts.To
		plan.DestinationSource = in.Elected.Source
		if in.ElectionErr != nil || in.Elected.Name != in.Opts.To {
			plan.WriteDestinationSetting = true
			plan.DestinationSource = syncSourceFlag
		}
		return false

	case len(in.EntireRemotes) >= 2 && in.Elected.Source != syncSourceConfig && in.Elected.Source != syncSourceObserved:
		// (f) Several Entire remotes: the tier does not apply and nobody can
		// guess which one is meant.
		plan.State = syncStateChooseDestination
		plan.Reason = fmt.Sprintf("This repo has %d Entire remotes (%s). Checkpoints can go to only one.",
			len(in.EntireRemotes), strings.Join(in.EntireRemotes, ", "))
		plan.NextCommand = syncCmd + " --to " + in.EntireRemotes[0]
		plan.Next = []syncAdvice{{Label: "Pass --to <remote> to choose", Command: plan.NextCommand}}
		if in.Interactive && !in.Opts.JSON {
			plan.Candidates = in.EntireRemotes
		} else {
			plan.ExitError = fmt.Errorf("several Entire remotes (%s); pass --to <remote>", strings.Join(in.EntireRemotes, ", "))
		}
		return true

	case in.Elected.Source == syncSourceEntire:
		plan.Destination = in.Elected.Name
		plan.DestinationSource = syncSourceEntire
		return false

	case len(in.EntireRemotes) == 1 && in.Elected.Name != in.EntireRemotes[0]:
		// (o) An explicit setting or a captured election pins another remote
		// while an Entire remote is available.
		entire := in.EntireRemotes[0]
		plan.State = syncStatePinnedElsewhere
		plan.Destination = in.Elected.Name
		plan.DestinationSource = in.Elected.Source
		plan.Reason = fmt.Sprintf("Checkpoints sync to %s, but this repo also has an Entire remote (%s). Entire is built to hold them.",
			describeDestination(in.Elected.Name, in.Elected.Source), entire)
		plan.NextCommand = syncCmd + " --to " + entire
		plan.Next = []syncAdvice{{Label: "To switch", Command: plan.NextCommand}}
		plan.Candidates = []string{entire}
		return true

	case len(in.EntireRemotes) == 0 && len(in.Remotes) > 1 && automatic:
		// (b) Several plain remotes and an automatic election: advisory only.
		plan.State = syncStateChooseDestination
		plan.Destination = in.Elected.Name
		plan.DestinationSource = in.Elected.Source
		plan.Reason = fmt.Sprintf("This repo has %d remotes (%s) and none is an Entire remote. Checkpoints go to exactly one; today that is %s.",
			len(in.Remotes), strings.Join(in.remoteNames(), ", "), describeDestination(in.Elected.Name, in.Elected.Source))
		plan.NextCommand = syncCmd + " --to <remote>"
		plan.Next = []syncAdvice{{Label: "To choose one", Command: plan.NextCommand}}
		plan.Notes = []string{syncNoteLocalFile}
		if in.Interactive && !in.Opts.JSON {
			// Current election first: it is what already happens, so the
			// accessible-mode default of "first option" changes nothing.
			plan.Candidates = append([]string{in.Elected.Name}, slices.DeleteFunc(in.remoteNames(), func(n string) bool { return n == in.Elected.Name })...)
		}
		return true

	default:
		plan.Destination = in.Elected.Name
		plan.DestinationSource = in.Elected.Source
		return false
	}
}

func validateFromFlags(in syncInputs, destination string) error {
	for _, from := range in.Opts.From {
		if _, ok := in.remote(from); !ok {
			return fmt.Errorf("--from %q is not a configured git remote", from)
		}
		if from == destination {
			return fmt.Errorf("--from %q is the destination; pass a different remote", from)
		}
	}
	return nil
}

// planSources picks the old remotes to migrate from: --from when given, else
// every non-destination remote known to hold artifacts. Remotes whose listing
// failed are reported separately and never removed from.
func planSources(in syncInputs, destination string) (sources, unknown []string) {
	for _, r := range in.Remotes {
		if r.Name == destination {
			continue
		}
		if r.Inventory != nil && r.Inventory.Err != nil {
			unknown = append(unknown, r.Name)
			continue
		}
		if len(in.Opts.From) > 0 {
			if slices.Contains(in.Opts.From, r.Name) {
				sources = append(sources, r.Name)
			}
			continue
		}
		if r.Inventory.holdsArtifacts() {
			sources = append(sources, r.Name)
		}
	}
	return sources, unknown
}

func planSteps(in syncInputs, plan *syncPlan) {
	if plan.WriteDestinationSetting {
		plan.Steps = append(plan.Steps, stepWriteDestination)
	}
	// Hydrate before converting: the v1 branch a conversion reads may exist
	// only on the old remote, and MigrateBranchToRefs falls back to origin's
	// tracking ref, which is wrong when the old remote is not origin.
	if len(plan.Sources) > 0 {
		plan.Steps = append(plan.Steps, stepHydrate)
	}
	// A v1 branch is converted whenever one will exist locally after
	// hydration — this clone's, or one an old remote holds — on either store,
	// so the step is planned (and so listed by --dry-run and the confirmation)
	// rather than discovered at run time.
	if in.HasLocalV1 || anySourceHasV1(in, plan.Sources) {
		plan.Steps = append(plan.Steps, stepConvertV1)
	}
	if !in.PrimaryIsRefs {
		plan.Steps = append(plan.Steps, stepConvertBackend)
	}
	movesData := in.LocalCheckpoints > 0 || len(plan.Sources) > 0
	if movesData {
		// Requeue whenever data moves: refs already pushed elsewhere have left
		// the queue, and refs hydration is about to fetch are not in it yet.
		// In a fresh clone nothing is local before hydration, so gating this
		// on local checkpoints skipped the whole publish. Dedup is free.
		plan.Steps = append(plan.Steps, stepRequeue)
		plan.Steps = append(plan.Steps, stepPublish, stepVerify)
	}
	if len(plan.Sources) > 0 {
		plan.Steps = append(plan.Steps, stepRemove)
	}
	plan.Steps = append(plan.Steps, stepRecordMarker)
}

// anySourceHasV1 reports whether one of the old remotes advertises a v1 branch.
func anySourceHasV1(in syncInputs, sources []string) bool {
	for _, name := range sources {
		if r, ok := in.remote(name); ok && r.Inventory != nil && r.Inventory.HasV1Branch {
			return true
		}
	}
	return false
}

// destinationHoldsAllLocalRefs reports whether the destination's listing
// contains every local checkpoint ref.
func destinationHoldsAllLocalRefs(in syncInputs, destination string) bool {
	dest, ok := in.remote(destination)
	if !ok || dest.Inventory == nil || dest.Inventory.Err != nil {
		return false
	}
	if len(in.LocalRefs) == 0 {
		return in.LocalCheckpoints == 0
	}
	have := make(map[plumbing.ReferenceName]bool, len(dest.Inventory.Refs))
	for _, r := range dest.Inventory.Refs {
		have[r] = true
	}
	for _, r := range in.LocalRefs {
		if !have[r] {
			return false
		}
	}
	return true
}

func planState(in syncInputs, plan *syncPlan) {
	dest := describeDestination(plan.Destination, plan.DestinationSource)
	switch {
	case len(plan.Sources) == 0 && in.LocalCheckpoints == 0:
		// (e) Nothing anywhere yet: at most the destination is recorded.
		plan.State = syncStateSetDestination
		plan.Steps = slices.DeleteFunc(plan.Steps, func(s syncStep) bool {
			return s == stepConvertV1 || s == stepConvertBackend || s == stepRequeue || s == stepPublish || s == stepVerify
		})
		if plan.WriteDestinationSetting {
			plan.Reason = fmt.Sprintf("Checkpoints will sync to %s once recorded as %s in %s.", plan.Destination, syncSettingKey, syncLocalFile)
		} else {
			plan.Reason = fmt.Sprintf("✓ Checkpoints will sync to %s from your first commit.", dest)
		}
	case len(plan.Sources) == 0 && in.PrimaryIsRefs && in.QueuedRefs == 0 && !plan.WriteDestinationSetting && destinationHoldsAllLocalRefs(in, plan.Destination):
		// (a)/(n) Everything is already where it belongs.
		plan.State = syncStateAlreadyHome
		plan.Steps = nil
		plan.Reason = fmt.Sprintf("✓ Checkpoints live on %s. Nothing to do.", dest)
		if in.Marker == syncMarkerPending {
			// Recording the verdict is a write, so it needs the same consent as
			// any other: --yes, or a human at the terminal who ran the command.
			if in.Opts.Yes || in.Interactive {
				plan.Steps = []syncStep{stepRecordMarker}
			} else {
				plan.Notes = append(plan.Notes, "Run "+syncCmd+" --yes to record this, so entire status stops suggesting it.")
			}
		}
	case len(plan.Sources) == 0:
		// (d) Local checkpoints that never left this clone.
		plan.State = syncStatePublishOnly
		plan.Reason = fmt.Sprintf("%s. %s will be pushed to %s.",
			countNoun(in.LocalCheckpoints, "checkpoint exists", "checkpoints exist")+" only in this clone",
			pluralWord(in.LocalCheckpoints, "It", "They"), dest)
	default:
		// (c) Another remote holds artifacts to bring over.
		plan.State = syncStateMigrate
		plan.Reason = describeMigration(in, plan.Destination, plan.Sources)
	}

	if plan.hasStep(stepConvertBackend) {
		plan.Reason = describeConversion(in.BackendEnvOverride) + " " + plan.Reason // (i)
	} else if plan.hasStep(stepConvertV1) {
		plan.Reason = "The " + syncV1BranchName + " branch is converted to refs first. " + plan.Reason
	}
	if in.PushSessionsDisabled && (plan.hasStep(stepPublish)) {
		plan.Warn = true
		plan.Notes = append(plan.Notes, formatPushSessionsDisabled()...)
	}
	for _, name := range plan.UnknownSources {
		r, _ := in.remote(name)
		plan.Notes = append(plan.Notes, formatUnknownSource(r, r.Inventory.Err))
	}

	plan.NextCommand = nextSyncCommand(*plan, in)
	if plan.NextCommand != "" {
		plan.Next = []syncAdvice{{Label: "Nothing was changed. To run this plan", Command: plan.NextCommand}}
		if plan.hasStep(stepRemove) && !in.Opts.Remove {
			plan.Next = append(plan.Next, syncAdvice{
				Label:   "To also delete the old copies from " + strings.Join(plan.Sources, ", ") + " after verification",
				Command: plan.NextCommand + " --remove",
			})
		}
	}
}

// describeConversion is the (i) prefix: the repo still writes the shared v1
// branch and will be converted first.
func describeConversion(envOverride bool) string {
	if envOverride {
		return "This repo still uses the shared " + syncV1BranchName + " branch. Each checkpoint will be converted to its own ref. " +
			"ENTIRE_CHECKPOINTS_PRIMARY is set, so the backend is not written to settings and the old " + syncV1BranchName + " branch is left in place."
	}
	return "This repo still uses the shared " + syncV1BranchName + " branch. Each checkpoint will be converted to its own ref and git-refs made the primary store (" + syncProjectFile + ")."
}

// describeMigration is the (c) reason: what each old remote holds and what
// will happen to it.
func describeMigration(in syncInputs, destination string, sources []string) string {
	var b strings.Builder
	for _, name := range sources {
		r, _ := in.remote(name)
		fmt.Fprintf(&b, "%s holds %s. ", r.nameWithURL(), describeArtifacts(r.Inventory))
	}
	fmt.Fprintf(&b, "They will be copied to %s, verified, and — if you agree — removed from %s.", destination, strings.Join(sources, ", "))
	return b.String()
}

// describeArtifacts renders "128 checkpoint refs and the entire/checkpoints/v1
// branch" for a listing; "checkpoint data" when --from named a remote that was
// not listed.
func describeArtifacts(inv *syncInventory) string {
	if inv == nil {
		return "checkpoint data"
	}
	var parts []string
	if n := len(inv.Refs); n > 0 {
		parts = append(parts, countNoun(n, "checkpoint ref", "checkpoint refs"))
	}
	if inv.HasV1Branch {
		parts = append(parts, "the "+syncV1BranchName+" branch")
	}
	if len(parts) == 0 {
		return "no checkpoint data"
	}
	return strings.Join(parts, " and ")
}

// describeDestination renders a remote name with the annotation entire status
// uses for its election source.
func describeDestination(name, source string) string {
	switch source {
	case syncSourceEntire:
		return name + " (" + entireRemoteNote + ")"
	case syncSourceConfig:
		return name + " (set by checkpoint_push_remote)"
	case syncSourceObserved:
		return name + " (follows your branch's push destination)"
	case syncSourceDefault:
		return name + " (the default)"
	case syncSourceFlag:
		return name + " (--to)"
	default:
		return name
	}
}

// nextSyncCommand builds the exact command that would execute the plan
// non-interactively, carrying --to/--from through. Empty when there is nothing
// to run.
func nextSyncCommand(plan syncPlan, in syncInputs) string {
	if len(plan.Steps) == 0 || (len(plan.Steps) == 1 && plan.Steps[0] == stepRecordMarker) {
		return ""
	}
	parts := []string{syncCmd}
	if in.Opts.To != "" {
		parts = append(parts, "--to", in.Opts.To)
	}
	for _, from := range in.Opts.From {
		parts = append(parts, "--from", from)
	}
	parts = append(parts, "--yes")
	if in.Opts.Remove && plan.hasStep(stepRemove) {
		parts = append(parts, "--remove")
	}
	return strings.Join(parts, " ")
}

// --- Runtime copy (executor outcomes) ---

// formatVerifyMissing is row (j): some refs did not arrive on the destination.
// old is "" when nothing was going to be removed.
func formatVerifyMissing(missing, total int, dest, old string) []string {
	first := fmt.Sprintf("%d of %d checkpoint refs did not arrive on %s.", missing, total, dest)
	if old != "" {
		first += fmt.Sprintf(" Nothing was removed from %s.", old)
	}
	return []string{first, "Run again to retry:  " + syncCmd}
}

// formatRemoveFailed is row (k): the old remote could not be cleaned.
func formatRemoveFailed(old string, err error, dest string) []string {
	return []string{
		fmt.Sprintf("Could not remove old copies from %s: %v. Your checkpoints are safe on %s.", old, err, dest),
		"Retry later with:  " + syncCmd + " --remove",
	}
}

// formatPushSessionsDisabled is row (l).
func formatPushSessionsDisabled() []string {
	return []string{
		"strategy_options.push_sessions is false, so nothing can be pushed. Refs stay queued.",
		"Set push_sessions to true (or remove it) and rerun.",
	}
}

// formatRemovalDeclined is row (m), printed after confirmDoctorFix's "-> Skipped".
func formatRemovalDeclined(old string) string {
	return fmt.Sprintf("Older copies stay on %s. Remove them any time with:  %s --remove", old, syncCmd)
}

// formatPublishFailed phrases a failed push to the destination.
func formatPublishFailed(dest string, err error) string {
	return fmt.Sprintf("Could not push to %s: %v. Refs stay queued and will go with your next git push to %s.", dest, err, dest)
}

// formatHydrateFailed phrases a failed fetch from an old remote; that remote is
// then never removed from.
func formatHydrateFailed(old string, err error) string {
	return fmt.Sprintf("Could not fetch checkpoints from %s: %v", old, err)
}

// formatUnknownSource phrases a remote whose listing failed.
func formatUnknownSource(r syncRemoteInfo, err error) string {
	return fmt.Sprintf("Could not check %s: %v. It is left untouched.", r.nameWithURL(), err)
}

// formatRemovalPrompt is the per-remote deletion question. It names the count,
// the branch, and the redacted URL, and says what is not touched.
func formatRemovalPrompt(refs int, hasV1 bool, r syncRemoteInfo) string {
	what := countNoun(refs, "checkpoint ref", "checkpoint refs")
	switch {
	case refs == 0 && hasV1:
		what = "the " + syncV1BranchName + " branch"
	case hasV1:
		what += " and the " + syncV1BranchName + " branch"
	}
	return fmt.Sprintf("Delete %s from %s? Your code is untouched.", what, r.nameWithURL())
}

// formatCombinedConfirm is the single question for every non-destructive step,
// so the user is not asked once per step.
func formatCombinedConfirm(in syncInputs, plan syncPlan) string {
	var verbs []string
	if plan.hasStep(stepConvertV1) {
		verbs = append(verbs, "Convert to refs")
	}
	if plan.hasStep(stepHydrate) {
		total := 0
		for _, name := range plan.Sources {
			if r, ok := in.remote(name); ok && r.Inventory != nil {
				total += len(r.Inventory.Refs)
			}
		}
		verbs = append(verbs, fmt.Sprintf("copy %s from %s to %s", countNoun(total, "checkpoint", "checkpoints"), strings.Join(plan.Sources, ", "), plan.Destination))
	} else if plan.hasStep(stepPublish) {
		verbs = append(verbs, fmt.Sprintf("push %s to %s", countNoun(in.LocalCheckpoints, "checkpoint", "checkpoints"), plan.Destination))
	}
	if plan.hasStep(stepVerify) {
		verbs = append(verbs, "verify")
	}
	if len(verbs) == 0 && plan.hasStep(stepWriteDestination) {
		verbs = append(verbs, "Record "+plan.Destination+" as the checkpoint sync remote")
	}
	q := joinClauses(verbs)
	if q == "" {
		return ""
	}
	q = strings.ToUpper(q[:1]) + q[1:] + "?"

	var writes []string
	if plan.hasStep(stepConvertBackend) && !in.BackendEnvOverride {
		writes = append(writes, syncProjectFile)
	}
	if plan.hasStep(stepWriteDestination) {
		writes = append(writes, syncLocalFile)
	}
	if len(writes) > 0 {
		q += " (writes " + strings.Join(writes, " and ") + ")"
	}
	return q
}

// formatSwitchToEntirePrompt is the (o) question.
func formatSwitchToEntirePrompt(entire string) string {
	return fmt.Sprintf("Switch checkpoint sync to %s?", entire)
}

// formatDestinationRecorded is printed after --to wrote the setting.
func formatDestinationRecorded(dest string) string {
	return fmt.Sprintf("✓ Checkpoints will sync to %s. Recorded %s in %s.", dest, syncSettingKey, syncLocalFile)
}

// formatDestinationUnchanged is printed when --to names the remote the
// election already picks.
func formatDestinationUnchanged(dest, source string) string {
	return fmt.Sprintf("✓ Checkpoints already sync to %s. Nothing to record.", describeDestination(dest, source))
}

// formatDestinationWriteFailed follows verifyExternalAgentsGrant's phrasing for
// a setting that did not take effect.
func formatDestinationWriteFailed(reason string) []string {
	return []string{
		"Warning: checkpoint_push_remote could not be set: " + reason + ".",
		"If " + syncLocalFile + " is tracked by git, run `git rm --cached " + syncLocalFile + "` and keep it out of version control.",
	}
}

// formatCommitSettingsHint follows the backend flip when it landed in the
// project file.
func formatCommitSettingsHint() string {
	return "Commit " + syncProjectFile + " so everyone writes the same store."
}

// formatBackendEnvOverride explains a skipped backend write.
func formatBackendEnvOverride() string {
	return "ENTIRE_CHECKPOINTS_PRIMARY is set, so the backend was not written to settings."
}

// formatMarkerWriteFailed is a soft warning: the command worked, the ledger did not.
func formatMarkerWriteFailed(err error) string {
	return fmt.Sprintf("Could not record the sync result in .git (%v); entire status may keep suggesting this command.", err)
}

// formatOPFCancelled mirrors doctor migrate-checkpoints: Ctrl-C at the OPF
// prompt is a decline, not a failure.
func formatOPFCancelled() string {
	return "OPF cancelled; refs stay queued for the next push."
}

// Spinner messages. Each reads well both while running and after the "✓ "
// prefix startUpdatableSpinner adds on success.
func formatConvertStart(n int) string {
	return fmt.Sprintf("Converting %s to refs", countNoun(n, "checkpoint", "checkpoints"))
}
func formatConvertDone(migrated, skipped int) string {
	msg := fmt.Sprintf("Converted %s to refs", countNoun(migrated, "checkpoint", "checkpoints"))
	if skipped > 0 {
		msg += fmt.Sprintf(" (%d already converted)", skipped)
	}
	return msg
}
func formatFetchStart(old string) string { return "Fetching checkpoints from " + old }
func formatFetchDone(n int, old string) string {
	return fmt.Sprintf("Fetched %s from %s", countNoun(n, "checkpoint ref", "checkpoint refs"), old)
}
func formatQueued(n int) string {
	return fmt.Sprintf("Queued %s for push", countNoun(n, "checkpoint ref", "checkpoint refs"))
}
func formatPushStart(n int, dest string) string {
	return fmt.Sprintf("Pushing %s to %s", countNoun(n, "checkpoint ref", "checkpoint refs"), dest)
}
func formatPushDone(n int, dest string) string {
	return fmt.Sprintf("Pushed %s to %s", countNoun(n, "checkpoint ref", "checkpoint refs"), dest)
}
func formatVerifyStart(dest string) string { return "Verifying on " + dest }
func formatVerifyDone(n int, dest string) string {
	return fmt.Sprintf("Verified %s on %s", countNoun(n, "checkpoint ref", "checkpoint refs"), dest)
}
func formatRemoveProgress(done, total int, old string) string {
	return fmt.Sprintf("Removing %s from %s (%d/%d)", countNoun(total, "checkpoint ref", "checkpoint refs"), old, done, total)
}
func formatRemoveDone(refs int, v1 bool, old string) string {
	what := countNoun(refs, "checkpoint ref", "checkpoint refs")
	if v1 {
		what += " and " + syncV1BranchName
	}
	return fmt.Sprintf("Removed %s from %s", what, old)
}

// --- Rendering ---

// renderSyncHeader prints the "Checkpoint migration" block: destination, store,
// local counts, and what each other remote holds.
func renderSyncHeader(w io.Writer, sty statusStyles, in syncInputs, plan syncPlan) {
	fmt.Fprintln(w, sty.render(sty.bold, "Checkpoint migration"))
	fmt.Fprintln(w)

	dest := plan.Destination
	if dest == "" {
		dest = in.Elected.Name
	}
	rows := []explainRow{}
	if dest != "" {
		rows = append(rows, explainRow{Label: "Destination", Value: describeDestination(dest, plan.DestinationSource)})
	}
	store := "git-refs"
	if !in.PrimaryIsRefs {
		store = "git-branch"
		if plan.hasStep(stepConvertBackend) {
			store = "git-branch → git-refs"
		}
	}
	rows = append(rows, explainRow{Label: "Store", Value: store})

	local := countNoun(in.LocalCheckpoints, "checkpoint", "checkpoints")
	if in.QueuedRefs > 0 {
		local += fmt.Sprintf(", %d not yet pushed", in.QueuedRefs)
	}
	rows = append(rows, explainRow{Label: "Local", Value: local})

	label := "Elsewhere"
	for _, r := range in.Remotes {
		if r.Name == dest || r.Inventory == nil {
			continue
		}
		value := r.nameWithURL() + ": "
		switch {
		case r.Inventory.Err != nil:
			value += "could not check"
		case !r.Inventory.holdsArtifacts():
			value += "none"
		default:
			value += describeInventoryShort(r.Inventory)
		}
		rows = append(rows, explainRow{Label: label, Value: value})
		label = ""
	}
	fmt.Fprint(w, sty.metadataRows(rows))
}

// describeInventoryShort renders "128 refs + entire/checkpoints/v1".
func describeInventoryShort(inv *syncInventory) string {
	var parts []string
	if n := len(inv.Refs); n > 0 {
		parts = append(parts, countNoun(n, "ref", "refs"))
	}
	if inv.HasV1Branch {
		parts = append(parts, syncV1BranchName)
	}
	return strings.Join(parts, " + ")
}

// renderSyncReason prints the state explanation, warning-styled when Warn.
func renderSyncReason(w io.Writer, sty statusStyles, plan syncPlan) {
	fmt.Fprintln(w)
	if plan.Warn {
		fmt.Fprintln(w, sty.render(sty.yellow, "  ! "+plan.Reason))
	} else {
		fmt.Fprintln(w, "  "+plan.Reason)
	}
	for _, note := range plan.Notes {
		fmt.Fprintln(w, sty.render(sty.dim, "    "+note))
	}
}

// renderSyncNext prints the "what to run" footer when the command only reports.
func renderSyncNext(w io.Writer, sty statusStyles, plan syncPlan) {
	if len(plan.Next) == 0 {
		return
	}
	fmt.Fprintln(w)
	for _, adv := range plan.Next {
		fmt.Fprintln(w, "  "+adv.Label+":")
		fmt.Fprintln(w, sty.render(sty.cyan, "    "+adv.Command))
	}
}

// renderDryRunSteps prints one "Would …" line per step.
func renderDryRunSteps(w io.Writer, in syncInputs, plan syncPlan) {
	fmt.Fprintln(w)
	for _, step := range plan.Steps {
		switch step {
		case stepWriteDestination:
			fmt.Fprintf(w, "  Would record %s as %s in %s\n", plan.Destination, syncSettingKey, syncLocalFile)
		case stepConvertV1:
			fmt.Fprintf(w, "  Would convert %s to refs\n", countNoun(in.LocalCheckpoints, "checkpoint", "checkpoints"))
		case stepConvertBackend:
			if in.BackendEnvOverride {
				fmt.Fprintln(w, "  Would leave the store on git-branch (ENTIRE_CHECKPOINTS_PRIMARY is set) and keep the old "+syncV1BranchName+" branch")
			} else {
				fmt.Fprintf(w, "  Would set the checkpoint store to git-refs (%s)\n", syncProjectFile)
			}
		case stepHydrate:
			for _, name := range plan.Sources {
				r, _ := in.remote(name)
				n := 0
				if r.Inventory != nil {
					n = len(r.Inventory.Refs)
				}
				fmt.Fprintf(w, "  Would fetch %s from %s\n", countNoun(n, "checkpoint ref", "checkpoint refs"), name)
			}
		case stepRequeue:
			fmt.Fprintln(w, "  Would queue every local checkpoint ref for push")
		case stepPublish:
			fmt.Fprintf(w, "  Would push %s to %s\n", countNoun(in.LocalCheckpoints, "checkpoint ref", "checkpoint refs"), plan.Destination)
		case stepVerify:
			fmt.Fprintf(w, "  Would verify them on %s\n", plan.Destination)
		case stepRemove:
			for _, name := range plan.Sources {
				r, _ := in.remote(name)
				fmt.Fprintf(w, "  Would delete %s from %s only with --remove\n", describeArtifacts(r.Inventory), r.nameWithURL())
			}
		case stepRecordMarker:
			// Bookkeeping, not worth a line.
		}
	}
}

// renderSyncCelebration is the closing block after a successful sync.
// removalNote, when non-empty, is the one line about old copies that remain.
func renderSyncCelebration(w io.Writer, sty statusStyles, dest string, destIsEntire bool, v checkpointValue, removalNote string) {
	fmt.Fprintln(w)
	if destIsEntire {
		fmt.Fprintln(w, sty.render(sty.green, "✓")+" "+sty.render(sty.bold, "Your checkpoints now live on Entire."))
	} else {
		fmt.Fprintln(w, sty.render(sty.green, "✓")+" "+sty.render(sty.bold, "Your checkpoints now live on "+dest+"."))
	}
	fmt.Fprintln(w)
	renderCheckpointValue(w, sty, v)
	fmt.Fprintln(w)
	if destIsEntire {
		fmt.Fprintf(w, "  From now on, every commit's session history syncs to %s automatically,\n", dest)
		fmt.Fprintln(w, "  whichever remote you push your code to.")
	} else {
		fmt.Fprintf(w, "  From now on, every commit's session history syncs to %s with your pushes to it.\n", dest)
		fmt.Fprintln(w, "  A push to any other remote carries your code but no session history.")
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  "+sty.render(sty.bold, "Next"))
	fmt.Fprint(w, sty.metadataRowsWithWidth([]explainRow{
		{Label: "  entire checkpoint list", Value: "what is on this branch"},
		{Label: "  entire search \"<what you did>\"", Value: "find work by what it did"},
		{Label: "  entire status", Value: "where checkpoints sync"},
	}, 34))
	if removalNote != "" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, sty.render(sty.dim, "  "+removalNote))
	}
}

// renderAlreadyHome is the closing block for states (a)/(n): the metrics
// without the "from now on" story.
func renderAlreadyHome(w io.Writer, sty statusStyles, destDescribed string, v checkpointValue) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, sty.render(sty.green, "✓")+" Checkpoints live on "+destDescribed+".")
	if v.Checkpoints == 0 {
		return
	}
	fmt.Fprintln(w)
	renderCheckpointValue(w, sty, v)
}

// countNoun renders "1 checkpoint" / "3 checkpoints".
func countNoun(n int, singular, plural string) string {
	return fmt.Sprintf("%d %s", n, pluralWord(n, singular, plural))
}

func pluralWord(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

// joinClauses joins "a", "b", "c" as "a, b, and c" (Oxford comma), "a and b",
// or "a".
func joinClauses(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	case 2:
		return strings.Join(parts, " and ")
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + ", and " + parts[len(parts)-1]
	}
}
