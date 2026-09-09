package cli

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

	"charm.land/huh/v2"
	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	checkpointremote "github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// newCheckpointMigrateCmd builds `entire checkpoint migrate`: choose the one remote
// that carries this repo's checkpoints (the Entire remote when there is one)
// and bring the backlog over to it. The decision logic is the pure planner in
// checkpoint_migrate_plan.go; this file gathers its inputs, prompts, and drives
// the strategy engine step by step.
func newCheckpointMigrateCmd() *cobra.Command {
	var opts checkpointMigrateOptions

	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Move checkpoints to a single remote and bring older ones with them",
		Long: `Choose the one git remote that carries this repository's checkpoints and bring
the backlog over to it.

When one of your remotes is an Entire remote (entire://…), it is the destination:
Entire keeps checkpoints alongside your code, and pushes to an Entire mirror stay
on Entire rather than being forwarded to the upstream forge. Otherwise pass --to
to pick a remote; the choice is recorded in .entire/settings.local.json as
strategy_options.checkpoint_push_remote, because a remote name is a fact about
this clone, not about the project.

What migrate does, in order, skipping steps that are already done:
  1. Fetch checkpoint artifacts that exist only on the old remote.
  2. Convert git-branch checkpoints (entire/checkpoints/v1) to per-checkpoint
     refs and make git-refs the primary store.
  3. Push every checkpoint ref to the destination and verify it arrived.
  4. With your consent (--remove, or a prompt), delete the checkpoint refs and
     the entire/checkpoints/v1 branch from the old remote. Your code is never
     touched.

Without a terminal, migrate reports the plan and the exact command to run; it
writes nothing unless --yes is passed, and deletes nothing unless --remove is.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if checkDisabledGuard(cmd.Context(), cmd.OutOrStdout()) {
				return nil
			}
			return runCheckpointMigrate(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), opts)
		},
	}
	cmd.Flags().StringVar(&opts.To, "to", "", "Remote that should carry checkpoints (default: your Entire remote, else the elected remote)")
	cmd.Flags().StringArrayVar(&opts.From, "from", nil, "Old remote to migrate from (repeatable; default: every remote holding checkpoint data)")
	cmd.Flags().BoolVar(&opts.Remove, "remove", false, "After verification, delete checkpoint refs and the entire/checkpoints/v1 branch from the old remote")
	cmd.Flags().BoolVar(&opts.Yes, "yes", false, "Run the non-destructive steps without prompting (never implies --remove)")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "Report the plan without writing or pushing anything")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "Emit the plan (and result, with --yes) as JSON")
	cmd.MarkFlagsMutuallyExclusive("dry-run", "remove")
	cmd.MarkFlagsMutuallyExclusive("dry-run", "yes")
	return cmd
}

// syncEngine is the strategy surface the executor drives. An interface so the
// executor is unit-testable against a fake; the real implementation is
// strategySyncEngine.
type syncEngine interface {
	Inventory(ctx context.Context, remote string) (strategy.CheckpointInventory, error)
	Hydrate(ctx context.Context, repo *git.Repository, from string, inv strategy.CheckpointInventory, progress strategy.ProgressFunc) (strategy.HydrateResult, error)
	Convert(ctx context.Context, repo *git.Repository, dryRun bool) (checkpoint.MigrateResult, error)
	Requeue(ctx context.Context, repo *git.Repository) (int, error)
	Publish(ctx context.Context, repo *git.Repository, to string) (pushed int, pushDisabled bool, err error)
	Verify(ctx context.Context, repo *git.Repository, to string, expected []plumbing.ReferenceName) (verified, missing []plumbing.ReferenceName, err error)
	Remove(ctx context.Context, from string, refs []plumbing.ReferenceName, deleteV1 bool, progress strategy.ProgressFunc) (strategy.RemoveResult, error)
	LoadState(ctx context.Context) (strategy.EntireSyncState, bool)
	MarkMigration(ctx context.Context, status strategy.EntireSyncMigration, from string) error
}

type strategySyncEngine struct{}

func (strategySyncEngine) Inventory(ctx context.Context, remote string) (strategy.CheckpointInventory, error) {
	inv, err := strategy.InventoryCheckpointArtifacts(ctx, remote)
	if err != nil {
		return inv, fmt.Errorf("list checkpoints on %s: %w", remote, err)
	}
	return inv, nil
}

func (strategySyncEngine) Hydrate(ctx context.Context, repo *git.Repository, from string, inv strategy.CheckpointInventory, progress strategy.ProgressFunc) (strategy.HydrateResult, error) {
	res, err := strategy.HydrateCheckpointArtifacts(ctx, repo, from, inv, progress)
	if err != nil {
		return res, fmt.Errorf("fetch checkpoints from %s: %w", from, err)
	}
	return res, nil
}

func (strategySyncEngine) Convert(ctx context.Context, repo *git.Repository, dryRun bool) (checkpoint.MigrateResult, error) {
	res, err := checkpoint.MigrateBranchToRefs(ctx, repo, dryRun)
	if err != nil {
		return res, fmt.Errorf("convert v1 branch checkpoints to refs: %w", err)
	}
	return res, nil
}

func (strategySyncEngine) Requeue(ctx context.Context, repo *git.Repository) (int, error) {
	n, err := strategy.RequeueAllCheckpointRefs(ctx, repo)
	if err != nil {
		return n, fmt.Errorf("queue checkpoint refs for push: %w", err)
	}
	return n, nil
}

// Publish flushes the queue through the OPF- and policy-gated path. The
// redaction precondition is the same one doctor migrate-checkpoints documents:
// the OPF gate reads process-global config that only EnsureRedactionConfigured
// sets, and an unconfigured one reads as "OPF off".
func (strategySyncEngine) Publish(ctx context.Context, repo *git.Repository, to string) (int, bool, error) {
	if err := strategy.EnsureRedactionConfigured(ctx); err != nil {
		return 0, false, fmt.Errorf("configure redaction before pushing: %w", err)
	}
	pushed, disabled, err := strategy.PushQueuedCheckpointRefs(ctx, repo, to)
	if err != nil {
		// Not re-wrapped with prose: callers match ErrOPFAbortedByUser and
		// context.Canceled through errors.Is, which %w preserves.
		return pushed, disabled, fmt.Errorf("push checkpoint refs to %s: %w", to, err)
	}
	return pushed, disabled, nil
}

func (strategySyncEngine) Verify(ctx context.Context, repo *git.Repository, to string, expected []plumbing.ReferenceName) ([]plumbing.ReferenceName, []plumbing.ReferenceName, error) {
	verified, missing, err := strategy.VerifyCheckpointArtifacts(ctx, repo, to, expected)
	if err != nil {
		return verified, missing, fmt.Errorf("list checkpoints on %s: %w", to, err)
	}
	return verified, missing, nil
}

func (strategySyncEngine) Remove(ctx context.Context, from string, refs []plumbing.ReferenceName, deleteV1 bool, progress strategy.ProgressFunc) (strategy.RemoveResult, error) {
	res, err := strategy.RemoveCheckpointArtifacts(ctx, from, refs, deleteV1, progress)
	if err != nil {
		return res, fmt.Errorf("delete checkpoints from %s: %w", from, err)
	}
	return res, nil
}

func (strategySyncEngine) LoadState(ctx context.Context) (strategy.EntireSyncState, bool) {
	return strategy.LoadEntireSyncState(ctx)
}

func (strategySyncEngine) MarkMigration(ctx context.Context, status strategy.EntireSyncMigration, from string) error {
	if err := strategy.MarkEntireSyncMigration(ctx, status, from); err != nil {
		return fmt.Errorf("record checkpoint sync state: %w", err)
	}
	return nil
}

// newSyncEngine is a package var so tests can substitute a fake engine.
var newSyncEngine = func() syncEngine { return strategySyncEngine{} }

// syncCanPrompt decides whether the command may open prompts; a package var so
// the interactive paths are reachable from tests without a TTY.
var syncCanPrompt = interactive.CanPromptInteractively

// syncConfirmFn is set by tests to answer confirmations without a TTY.
// nil means "ask through confirmDoctorFix".
var syncConfirmFn func(ctx context.Context, w io.Writer, title string) (bool, error)

func confirmSyncStep(ctx context.Context, w io.Writer, title string) (bool, error) {
	if syncConfirmFn != nil {
		return syncConfirmFn(ctx, w, title)
	}
	return confirmDoctorFix(ctx, w, title)
}

// checkpointMigrateJSON is the --json shape: the plan, and the result when the
// command executed.
type checkpointMigrateJSON struct {
	State             string                   `json:"state"`
	Destination       string                   `json:"destination,omitempty"`
	DestinationSource string                   `json:"destination_source,omitempty"`
	Steps             []syncStep               `json:"steps"`
	Sources           []string                 `json:"sources,omitempty"`
	UnknownSources    []string                 `json:"unknown_sources,omitempty"`
	LocalCheckpoints  int                      `json:"local_checkpoints"`
	QueuedRefs        int                      `json:"queued_refs"`
	Remotes           []syncRemoteJSON         `json:"remotes"`
	Reason            string                   `json:"reason"`
	NextCommand       string                   `json:"next_command,omitempty"`
	Error             string                   `json:"error,omitempty"`
	Result            *checkpointMigrateResult `json:"result,omitempty"`
}

type syncRemoteJSON struct {
	Name     string `json:"name"`
	IsEntire bool   `json:"is_entire"`
	Refs     int    `json:"checkpoint_refs"`
	V1Branch bool   `json:"v1_branch"`
	Error    string `json:"error,omitempty"`
}

// checkpointMigrateResult is what a run actually did.
type checkpointMigrateResult struct {
	DestinationRecorded bool           `json:"destination_recorded,omitempty"`
	Converted           int            `json:"converted,omitempty"`
	BackendSet          bool           `json:"backend_set,omitempty"`
	Fetched             map[string]int `json:"fetched,omitempty"`
	Pushed              int            `json:"pushed"`
	Verified            int            `json:"verified"`
	Missing             int            `json:"missing,omitempty"`
	Removed             map[string]int `json:"removed,omitempty"`
	V1Deleted           []string       `json:"v1_deleted,omitempty"`
	RemovalDeclined     bool           `json:"removal_declined,omitempty"`
	Marker              string         `json:"marker,omitempty"`
}

// runCheckpointMigrate is the command body.
//
// Two planning passes: the first uses only local facts and stops early for the
// states that need no network (no remotes, dedicated store, fail-closed without
// --to, a choice to make); the second adds an ls-remote inventory of every
// remote and drives the engine.
func runCheckpointMigrate(ctx context.Context, w, errW io.Writer, opts checkpointMigrateOptions) error {
	eng := newSyncEngine()
	sty := newStatusStyles(w)

	repo, err := strategy.OpenRepository(ctx)
	if err != nil {
		return fmt.Errorf("open repository: %w", err)
	}
	defer repo.Close()

	in, err := gatherLocalSyncInputs(ctx, eng, repo, opts)
	if err != nil {
		return err
	}
	plan := planCheckpointSync(in)

	// States that end here, or that ask the user to choose before anything
	// else happens. A choice re-enters as --to.
	switch plan.State {
	case syncStateNoRemotes, syncStateDedicatedRemote:
		return finishReportOnly(w, sty, in, plan)
	case syncStateFailClosed, syncStateChooseDestination, syncStatePinnedElsewhere:
		chosen, chooseErr := chooseSyncDestination(ctx, w, sty, in, plan)
		if chooseErr != nil {
			return chooseErr
		}
		if chosen == "" {
			if len(plan.Candidates) > 0 && in.Interactive {
				return finishAfterDeclinedChoice(w, sty, in, plan)
			}
			return finishReportOnly(w, sty, in, plan)
		}
		in.Opts.To = chosen
	case syncStateAlreadyHome, syncStateSetDestination, syncStatePublishOnly, syncStateMigrate:
		// Continue to the inventoried pass.
	}
	if plan.ExitError != nil {
		return finishReportOnly(w, sty, in, plan)
	}

	// Second pass: inventory the remotes the plan can involve, then plan for
	// real. With --from, only the destination and the named remotes are
	// listed; the user excluded the rest.
	inventorySyncRemotes(ctx, eng, &in, plan.Destination)
	plan = planCheckpointSync(in)

	if in.Opts.JSON && !in.Opts.Yes {
		return writeSyncJSON(w, in, plan, nil)
	}
	if !in.Opts.JSON {
		renderSyncHeader(w, sty, in, plan)
		renderSyncReason(w, sty, plan)
		if in.Opts.To != "" && !plan.WriteDestinationSetting && plan.Destination == in.Opts.To {
			fmt.Fprintln(w, "  "+formatDestinationUnchanged(plan.Destination, plan.DestinationSource))
		}
	}
	if plan.ExitError != nil {
		return finishReportOnly(w, sty, in, plan)
	}
	if in.Opts.DryRun {
		renderDryRunSteps(w, in, plan)
		return nil
	}

	// Nothing to execute beyond bookkeeping.
	if plan.State == syncStateAlreadyHome {
		res := &checkpointMigrateResult{}
		if plan.hasStep(stepRecordMarker) {
			res.Marker = recordSyncMarker(ctx, w, eng, strategy.EntireSyncMigrationDone, "")
		}
		if in.Opts.JSON {
			return writeSyncJSON(w, in, plan, res)
		}
		renderAlreadyHome(w, sty, describeDestination(plan.Destination, plan.DestinationSource), valueOrEmpty(ctx, repo))
		return nil
	}

	// Consent for the non-destructive part: --yes, or one combined question.
	if !in.Opts.Yes {
		if !in.Interactive {
			renderSyncNext(w, sty, plan)
			return nil
		}
		question := formatCombinedConfirm(in, plan)
		if question != "" {
			fmt.Fprintln(w)
			ok, cerr := confirmSyncStep(ctx, w, question)
			if cerr != nil {
				return cerr
			}
			if !ok {
				fmt.Fprintln(w, "  "+syncNothingChanged)
				return nil
			}
		}
	}

	res, execErr := executeSyncPlan(ctx, w, errW, sty, eng, repo, in, plan)
	if in.Opts.JSON {
		if jerr := writeSyncJSON(w, in, plan, res); jerr != nil {
			return jerr
		}
		return execErr
	}
	return execErr
}

// finishReportOnly renders a plan that will not be executed and returns its
// exit error (nil for advisory states). An ExitError with no Reason is a plain
// flag error (unknown --to, --from naming the destination) and is returned
// as-is so main prints it; every other exit error was already rendered.
func finishReportOnly(w io.Writer, sty statusStyles, in syncInputs, plan syncPlan) error {
	if in.Opts.JSON {
		return errors.Join(writeSyncJSON(w, in, plan, nil), silentIfSet(plan.ExitError))
	}
	if plan.ExitError != nil && plan.Reason == "" {
		return plan.ExitError
	}
	if plan.State == syncStateNoRemotes || plan.State == syncStateDedicatedRemote {
		fmt.Fprintln(w, sty.render(sty.bold, "Checkpoint migration"))
		renderSyncReason(w, sty, plan)
	} else {
		renderSyncHeader(w, sty, in, plan)
		renderSyncReason(w, sty, plan)
		renderSyncNext(w, sty, plan)
	}
	return silentIfSet(plan.ExitError)
}

// finishAfterDeclinedChoice prints only the footer: the header and reason were
// already rendered above the picker the user just declined.
func finishAfterDeclinedChoice(w io.Writer, sty statusStyles, in syncInputs, plan syncPlan) error {
	if in.Opts.JSON {
		return finishReportOnly(w, sty, in, plan)
	}
	renderSyncNext(w, sty, plan)
	return silentIfSet(plan.ExitError)
}

// silentIfSet wraps a plan's exit error so main exits 1 without repeating a
// message the plan already rendered.
func silentIfSet(err error) error {
	if err == nil {
		return nil
	}
	return NewSilentError(err)
}

// chooseSyncDestination runs the picker for states that need a choice. It
// returns the chosen remote, or "" when the user kept things as they are (or
// there is nobody to ask). The first option is always the no-change default,
// because accessible mode answers an unreadable prompt with the first option.
func chooseSyncDestination(ctx context.Context, w io.Writer, sty statusStyles, in syncInputs, plan syncPlan) (string, error) {
	if len(plan.Candidates) == 0 || !in.Interactive {
		return "", nil
	}
	if !in.Opts.JSON {
		renderSyncHeader(w, sty, in, plan)
		renderSyncReason(w, sty, plan)
		fmt.Fprintln(w)
	}

	if plan.State == syncStatePinnedElsewhere {
		entire := plan.Candidates[0]
		ok, err := confirmSyncStep(ctx, w, formatSwitchToEntirePrompt(entire))
		if err != nil || !ok {
			return "", err
		}
		return entire, nil
	}

	const keep = ""
	noChange := "Decide later (no change)"
	if plan.State == syncStateFailClosed {
		noChange = "Leave as is"
	}
	options := []huh.Option[string]{huh.NewOption(noChange, keep)}
	if plan.State == syncStateChooseDestination && len(in.EntireRemotes) == 0 && plan.Destination != "" {
		// (b): the current election is what already happens, so it is the
		// safe first option; keeping it writes nothing.
		options = []huh.Option[string]{huh.NewOption(plan.Destination+" (current)", keep)}
	}
	for _, name := range plan.Candidates {
		if name == plan.Destination && len(in.EntireRemotes) == 0 {
			continue
		}
		options = append(options, huh.NewOption(name, name))
	}

	var selected string
	form := NewAccessibleForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Which remote should carry this repo's checkpoints?").
			Description("Checkpoints go to exactly one remote. A push to any other remote carries your code but no session history.").
			Options(options...).
			Value(&selected),
	))
	if err := form.RunWithContext(ctx); err != nil {
		if errors.Is(err, huh.ErrUserAborted) || errors.Is(err, context.Canceled) {
			return "", nil
		}
		return "", fmt.Errorf("prompt failed: %w", err)
	}
	return selected, nil
}

// gatherLocalSyncInputs collects everything the planner needs that costs no
// network: settings, the election, remotes and their raw classification, the
// backend, local checkpoint counts, the queue, and the migration ledger.
func gatherLocalSyncInputs(ctx context.Context, eng syncEngine, repo *git.Repository, opts checkpointMigrateOptions) (syncInputs, error) {
	in := syncInputs{Opts: opts, Interactive: syncCanPrompt() && !opts.JSON}

	s, err := settings.Load(ctx)
	if err != nil {
		return in, fmt.Errorf("load settings: %w", err)
	}
	in.PushSessionsDisabled = s.IsPushSessionsDisabled()

	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return in, fmt.Errorf("resolve worktree root: %w", err)
	}
	byRemote, err := pushURLsByRemote(ctx, root)
	if err != nil {
		return in, err
	}
	entire := strategy.EntireRemotes(ctx)
	in.EntireRemotes = entire
	names := make([]string, 0, len(byRemote))
	for name := range byRemote {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		in.Remotes = append(in.Remotes, syncRemoteInfo{
			Name:     name,
			PushURLs: byRemote[name],
			IsEntire: slices.Contains(entire, name),
		})
	}

	elected, electErr := strategy.ResolveCheckpointSyncRemote(ctx)
	if electErr != nil {
		in.ElectionErr = electErr
	} else {
		in.Elected = syncElection{Name: elected.Name, Source: string(elected.Source)}
	}

	if cr := s.GetCheckpointRemote(); cr != nil && elected.Name != "" {
		if _, enabled, purlErr := checkpointremote.PushURL(ctx, elected.Name); purlErr == nil && enabled {
			in.DedicatedRemote = true
			in.DedicatedRepo = cr.Repo
		}
	}

	cpCfg, _ := settings.LoadCheckpointsConfig(ctx) //nolint:errcheck // fail-soft: a bad checkpoints block surfaces via Open; default is git-branch
	in.PrimaryIsRefs = checkpoint.PrimaryIsRefs(cpCfg)
	in.BackendEnvOverride = strings.TrimSpace(os.Getenv(settings.EnvCheckpointsPrimary)) != ""

	refs, err := listLocalCheckpointRefs(repo)
	if err != nil {
		return in, fmt.Errorf("list local checkpoint refs: %w", err)
	}
	in.LocalRefs = refs
	in.LocalCheckpoints = len(refs)
	in.HasLocalV1 = hasLocalV1Branch(ctx, repo)
	if !in.PrimaryIsRefs {
		// On git-branch the checkpoints live on the v1 branch; a dry-run
		// conversion counts them without writing anything.
		if res, convErr := eng.Convert(ctx, repo, true); convErr == nil && res.Total > in.LocalCheckpoints {
			in.LocalCheckpoints = res.Total
		}
	}

	if queue, qerr := checkpoint.PushQueueForRepo(ctx, repo); qerr == nil {
		if queued, perr := queue.Peek(); perr == nil {
			in.QueuedRefs = len(queued)
		}
	}

	if st, ok := eng.LoadState(ctx); ok {
		in.Marker = syncMarkerState(st.Migration)
	}
	return in, nil
}

// inventorySyncRemotes lists the remotes the plan can involve (one ls-remote
// each, bounded by the engine's foreground budget) and keeps the advertised
// hashes, so execution acts on the listing the user consented to rather than
// dialing again. With --from, remotes the user excluded are not listed at
// all. A failed listing is recorded on the remote so the planner reports it
// and never removes from it.
func inventorySyncRemotes(ctx context.Context, eng syncEngine, in *syncInputs, destination string) {
	for i := range in.Remotes {
		name := in.Remotes[i].Name
		if len(in.Opts.From) > 0 && name != destination && !slices.Contains(in.Opts.From, name) {
			continue
		}
		inv, err := eng.Inventory(ctx, name)
		if err != nil {
			in.Remotes[i].Inventory = &syncInventory{Err: err}
			continue
		}
		in.Remotes[i].Inventory = &syncInventory{
			Refs:        inv.RefNames(),
			HasV1Branch: !inv.V1Branch.IsZero(),
			Hashes:      inv.Refs,
			V1Tip:       inv.V1Branch,
		}
	}
}

// storedInventory rebuilds the engine's inventory for a remote from the
// listing the planner used.
func storedInventory(in syncInputs, name string) (strategy.CheckpointInventory, bool) {
	r, ok := in.remote(name)
	if !ok || r.Inventory == nil || r.Inventory.Err != nil {
		return strategy.CheckpointInventory{}, false
	}
	return strategy.CheckpointInventory{Remote: name, Refs: r.Inventory.Hashes, V1Branch: r.Inventory.V1Tip}, true
}

// syncRun carries the state one execution threads through its steps.
type syncRun struct {
	out  io.Writer // step output; io.Discard under --json
	errW io.Writer
	sty  statusStyles
	eng  syncEngine
	repo *git.Repository
	in   syncInputs
	plan syncPlan
	res  *checkpointMigrateResult
	// sources are the old remotes still eligible for removal: a source whose
	// listing or fetch failed drops out, because nothing may be deleted from a
	// remote that was not read in full.
	sources     []string
	inventories map[string]strategy.CheckpointInventory
	// v1Converted reports that a local v1 branch was converted to refs in this
	// run; storeIsRefs that git-refs is (now) the primary store. Deleting an
	// old remote's v1 branch needs both: refs on the destination are what
	// readers of a git-refs store see, and a git-branch reader would look for
	// the branch this run never pushed.
	v1Converted bool
	storeIsRefs bool
	verified    []plumbing.ReferenceName
}

func (r *syncRun) warn(lines ...string) {
	for _, line := range lines {
		fmt.Fprintln(r.out, r.sty.render(r.sty.yellow, "  ! "+line))
	}
}

// executeSyncPlan runs the plan's steps in engine order: record destination →
// hydrate → convert (+ backend flip) → requeue → publish → verify → remove →
// marker. Hydration precedes conversion because the v1 branch may exist only
// on the old remote. It returns what happened and, when a step failed in a way
// the user must act on, a SilentError (the message was already printed).
func executeSyncPlan(ctx context.Context, w, errW io.Writer, sty statusStyles, eng syncEngine, repo *git.Repository, in syncInputs, plan syncPlan) (*checkpointMigrateResult, error) {
	r := &syncRun{
		out: w, errW: errW, sty: sty, eng: eng, repo: repo, in: in, plan: plan,
		res:         &checkpointMigrateResult{Fetched: map[string]int{}, Removed: map[string]int{}},
		sources:     plan.Sources,
		inventories: map[string]strategy.CheckpointInventory{},
		storeIsRefs: in.PrimaryIsRefs,
	}
	quiet := in.Opts.JSON
	if quiet {
		r.out = io.Discard
	}
	fmt.Fprintln(r.out)

	if plan.hasStep(stepWriteDestination) {
		r.writeDestination(ctx)
	}
	if plan.hasStep(stepHydrate) {
		r.hydrateSources(ctx)
	}
	if err := r.convert(ctx); err != nil {
		return r.res, err
	}
	if plan.hasStep(stepRequeue) {
		n, err := eng.Requeue(ctx, repo)
		if err != nil {
			return r.res, printedError(errW, fmt.Errorf("queue checkpoint refs: %w", err))
		}
		fmt.Fprintln(r.out, "  "+formatQueued(n))
	}
	if plan.hasStep(stepPublish) {
		if err := r.publish(ctx); err != nil {
			return r.res, err
		}
	}
	if plan.hasStep(stepVerify) {
		if err := r.verify(ctx); err != nil {
			return r.res, err
		}
	}

	// The data is where it belongs from here on; the ledger says so even if
	// cleanup is declined or fails, so status stops nudging.
	r.res.Marker = recordSyncMarker(ctx, r.out, eng, strategy.EntireSyncMigrationDone, strings.Join(r.sources, ","))

	removalNote := ""
	var removeErr error
	if plan.hasStep(stepRemove) && len(r.sources) > 0 {
		removalNote, removeErr = r.removeFromSources(ctx)
	}
	if !quiet {
		renderSyncCelebration(w, sty, plan.Destination, plan.DestinationSource == syncSourceEntire, valueOrEmpty(ctx, repo), removalNote)
	}
	return r.res, removeErr
}

// writeDestination records --to in the per-clone settings file.
func (r *syncRun) writeDestination(ctx context.Context) {
	grant := setCheckpointPushRemoteLocally(ctx, r.plan.Destination)
	if grant.Effective {
		r.res.DestinationRecorded = true
		fmt.Fprintln(r.out, "  "+formatDestinationRecorded(r.plan.Destination))
		return
	}
	r.warn(formatDestinationWriteFailed(grant.Reason)...)
}

// hydrateSources fetches each old remote's artifacts into this clone. A source
// that cannot be listed or fetched is reported and dropped from removal.
func (r *syncRun) hydrateSources(ctx context.Context) {
	var kept []string
	for _, name := range r.plan.Sources {
		inv, ok := storedInventory(r.in, name)
		if !ok {
			// The planner only makes a source of a remote it listed; reaching
			// here means the listing failed after all, so leave it alone.
			r.warn(formatHydrateFailed(name, errors.New("remote was not listed")))
			continue
		}
		r.inventories[name] = inv
		update, stop := startUpdatableSpinner(r.out, formatFetchStart(name))
		hres, err := r.eng.Hydrate(ctx, r.repo, name, inv, func(format string, args ...any) { update(fmt.Sprintf(format, args...)) })
		if err != nil {
			stop(false)
			r.warn(formatHydrateFailed(name, err))
			continue
		}
		if hres.RefsFetched == 0 && hres.V1Advanced {
			update("Fetched " + syncV1BranchName + " from " + name)
		} else {
			update(formatFetchDone(hres.RefsFetched, name))
		}
		stop(true)
		r.res.Fetched[name] = hres.RefsFetched
		kept = append(kept, name)
	}
	r.sources = kept
}

// convert turns a local v1 branch (any backend) into refs, then flips the
// primary store when the plan asks for it.
func (r *syncRun) convert(ctx context.Context) error {
	if r.plan.hasStep(stepConvertV1) && hasLocalV1Branch(ctx, r.repo) {
		update, stop := startUpdatableSpinner(r.out, formatConvertStart(r.in.LocalCheckpoints))
		mres, err := r.eng.Convert(ctx, r.repo, false)
		if err != nil {
			stop(false)
			return printedError(r.errW, fmt.Errorf("convert checkpoints to refs: %w", err))
		}
		update(formatConvertDone(len(mres.Migrated), mres.Skipped))
		stop(true)
		r.res.Converted = len(mres.Migrated)
		r.v1Converted = true
	}
	if !r.plan.hasStep(stepConvertBackend) {
		return nil
	}
	if r.in.BackendEnvOverride {
		fmt.Fprintln(r.out, r.sty.render(r.sty.dim, "  "+formatBackendEnvOverride()))
		return nil
	}
	if err := updateCheckpointBackend(ctx, r.out, EnableOptions{CheckpointBackend: checkpointBackendRefsAlias}); err != nil {
		return printedError(r.errW, fmt.Errorf("set checkpoint backend: %w", err))
	}
	r.res.BackendSet = true
	r.storeIsRefs = true
	if target, _ := settingsTargetFile(ctx, false, false); target == settings.EntireSettingsFile {
		fmt.Fprintln(r.out, r.sty.render(r.sty.dim, "  "+formatCommitSettingsHint()))
	}
	return nil
}

// publish flushes the queue to the destination through the gated push path.
func (r *syncRun) publish(ctx context.Context) error {
	dest := r.plan.Destination
	update, stop := startUpdatableSpinner(r.out, formatPushStart(r.in.LocalCheckpoints, dest))
	restore := strategy.SetRefsPushProgressWriter(io.Discard)
	pushed, pushDisabled, err := r.eng.Publish(ctx, r.repo, dest)
	restore()
	r.res.Pushed = pushed
	switch {
	case errors.Is(err, context.Canceled):
		stop(false)
		return NewSilentError(err)
	case errors.Is(err, strategy.ErrOPFAbortedByUser):
		stop(false)
		fmt.Fprintln(r.out, "  "+formatOPFCancelled())
		return NewSilentError(err)
	case err != nil:
		stop(false)
		r.warn(formatPublishFailed(dest, err))
		return NewSilentError(err)
	case pushDisabled:
		stop(false)
		r.warn(formatPushSessionsDisabled()...)
		return NewSilentError(errors.New("push_sessions is disabled"))
	}
	update(formatPushDone(pushed, dest))
	stop(true)
	return nil
}

// verify lists the destination and refuses to go on while any local ref is
// missing there at its local hash.
func (r *syncRun) verify(ctx context.Context) error {
	dest := r.plan.Destination
	expected, err := listLocalCheckpointRefs(r.repo)
	if err != nil {
		return printedError(r.errW, fmt.Errorf("list local checkpoint refs: %w", err))
	}
	update, stop := startUpdatableSpinner(r.out, formatVerifyStart(dest))
	verified, missing, err := r.eng.Verify(ctx, r.repo, dest, expected)
	if err != nil {
		stop(false)
		return printedError(r.errW, fmt.Errorf("verify checkpoints on %s: %w", dest, err))
	}
	update(formatVerifyDone(len(verified), dest))
	stop(true)
	r.verified = verified
	r.res.Verified = len(verified)
	r.res.Missing = len(missing)
	if len(missing) > 0 {
		r.warn(formatVerifyMissing(len(missing), len(expected), dest, strings.Join(r.sources, ", "))...)
		return NewSilentError(fmt.Errorf("%d checkpoint refs missing on %s", len(missing), dest))
	}
	return nil
}

// removeFromSources deletes verified artifacts from each old remote after its
// own consent line. It returns the one-line note about copies that remain (or
// "" when every source was cleaned) and an error when a deletion failed.
func (r *syncRun) removeFromSources(ctx context.Context) (string, error) {
	out, sty, eng, in, sources, inventories, verified, res := r.out, r.sty, r.eng, r.in, r.sources, r.inventories, r.verified, r.res
	// The old v1 branch goes only when this run converted it to refs AND
	// git-refs is the store readers will use; otherwise (ENTIRE_CHECKPOINTS_PRIMARY
	// pinning git-branch, or no conversion) a git-branch reader would look for
	// a branch the destination never received.
	v1Deletable := r.v1Converted && r.storeIsRefs
	v1Kept := false
	isVerified := make(map[plumbing.ReferenceName]bool, len(verified))
	for _, r := range verified {
		isVerified[r] = true
	}
	var remaining []string
	var firstErr error
	for _, name := range sources {
		inv := inventories[name]
		var refs []plumbing.ReferenceName
		for _, r := range inv.RefNames() {
			if isVerified[r] {
				refs = append(refs, r)
			}
		}
		hasV1 := !inv.V1Branch.IsZero()
		deleteV1 := hasV1 && v1Deletable
		if hasV1 && !deleteV1 {
			v1Kept = true
		}
		if len(refs) == 0 && !deleteV1 {
			remaining = append(remaining, name)
			continue
		}

		info, _ := in.remote(name)
		if !in.Opts.Remove {
			if !in.Interactive {
				remaining = append(remaining, name)
				continue
			}
			ok, err := confirmSyncStep(ctx, out, formatRemovalPrompt(len(refs), deleteV1, info))
			if err != nil {
				return "", err
			}
			if !ok {
				res.RemovalDeclined = true
				fmt.Fprintln(out, sty.render(sty.dim, "  "+formatRemovalDeclined(name)))
				res.Marker = recordSyncMarker(ctx, out, eng, strategy.EntireSyncMigrationDeclined, name)
				remaining = append(remaining, name)
				continue
			}
		}

		update, stop := startUpdatableSpinner(out, formatRemoveProgress(0, len(refs), name))
		rres, err := eng.Remove(ctx, name, refs, deleteV1, func(format string, args ...any) { update(fmt.Sprintf(format, args...)) })
		if err != nil {
			stop(false)
			for _, line := range formatRemoveFailed(name, err, in.Elected.Name) {
				fmt.Fprintln(out, sty.render(sty.yellow, "  ! "+line))
			}
			if firstErr == nil {
				firstErr = NewSilentError(fmt.Errorf("remove checkpoint artifacts from %s: %w", name, err))
			}
			remaining = append(remaining, name)
			continue
		}
		update(formatRemoveDone(rres.RefsDeleted, rres.V1Deleted, name))
		stop(true)
		res.Removed[name] = rres.RefsDeleted
		if rres.V1Deleted {
			res.V1Deleted = append(res.V1Deleted, name)
		}
	}
	note := ""
	if len(remaining) > 0 && firstErr == nil {
		note = formatRemovalDeclined(strings.Join(remaining, ", "))
	}
	if v1Kept && firstErr == nil {
		fmt.Fprintln(out, sty.render(sty.dim, "  The "+syncV1BranchName+" branch stays on the old remote: this repo still reads the git-branch store."))
	}
	return note, firstErr
}

// recordSyncMarker persists the migration ledger, reporting (not failing) when
// the write does not land — status would merely keep nudging.
func recordSyncMarker(ctx context.Context, out io.Writer, eng syncEngine, status strategy.EntireSyncMigration, from string) string {
	if err := eng.MarkMigration(ctx, status, from); err != nil {
		fmt.Fprintln(out, "  ! "+formatMarkerWriteFailed(err))
		logging.Debug(ctx, "checkpoint migrate: could not record migration marker", slog.String("error", err.Error()))
		return ""
	}
	return string(status)
}

// hasLocalV1Branch reports whether this clone has the git-branch checkpoint
// branch, whichever backend is primary.
func hasLocalV1Branch(ctx context.Context, repo *git.Repository) bool {
	_, err := repo.Reference(checkpoint.ResolveRefs(ctx).Primary, true)
	return err == nil
}

// valueOrEmpty computes the celebration metrics, degrading to the zero value:
// the celebration must never fail the command.
func valueOrEmpty(ctx context.Context, repo *git.Repository) checkpointValue {
	v, err := computeCheckpointValue(ctx, repo)
	if err != nil {
		logging.Debug(ctx, "checkpoint migrate: value summary unavailable", slog.String("error", err.Error()))
		return checkpointValue{}
	}
	return v
}

// printedError prints err to errW and returns it wrapped as silent, for
// failures mid-execution whose message belongs next to the step output.
func printedError(errW io.Writer, err error) error {
	fmt.Fprintln(errW, "  ! "+err.Error())
	return NewSilentError(err)
}

// checkpointPushRemoteGrant is what writing checkpoint_push_remote achieved.
type checkpointPushRemoteGrant struct {
	Effective bool
	Reason    string
}

// setCheckpointPushRemoteLocally records strategy_options.checkpoint_push_remote
// in .entire/settings.local.json — always the local file, whatever the project
// settings say: a remote name is a per-clone fact, and committing it would
// fail-close checkpoint sync for every teammate whose clone lacks the name.
//
// Raw read-modify-write, not a struct save (the merged struct carries the
// project file's fields; see enableExternalAgentsLocally). Verified by
// re-resolving the election: a tracked settings.local.json makes the loader
// drop the whole local layer, so the key can land where it is never read.
func setCheckpointPushRemoteLocally(ctx context.Context, name string) checkpointPushRemoteGrant {
	path, raw, _, err := settings.LoadLocalRaw(ctx)
	if err != nil {
		return checkpointPushRemoteGrant{Reason: fmt.Sprintf("local settings could not be read (%v)", err)}
	}
	var so map[string]json.RawMessage
	if v, ok := raw["strategy_options"]; ok {
		if uerr := json.Unmarshal(v, &so); uerr != nil {
			return checkpointPushRemoteGrant{Reason: fmt.Sprintf("strategy_options in %s is not an object (%v)", syncLocalFile, uerr)}
		}
	}
	if so == nil {
		so = map[string]json.RawMessage{}
	}
	nameJSON, err := json.Marshal(name)
	if err != nil {
		return checkpointPushRemoteGrant{Reason: fmt.Sprintf("could not encode the remote name (%v)", err)}
	}
	so["checkpoint_push_remote"] = nameJSON
	soJSON, err := json.Marshal(so)
	if err != nil {
		return checkpointPushRemoteGrant{Reason: fmt.Sprintf("could not encode strategy_options (%v)", err)}
	}
	raw["strategy_options"] = soJSON
	if err := settings.SaveLocalRaw(path, raw); err != nil {
		return checkpointPushRemoteGrant{Reason: fmt.Sprintf("could not save %s (%v)", syncLocalFile, err)}
	}
	strategy.InvalidateGitRemoteCache(ctx)
	return verifyCheckpointPushRemote(ctx, name)
}

// verifyCheckpointPushRemote re-resolves the election and reports whether the
// just-written setting is what decides it.
func verifyCheckpointPushRemote(ctx context.Context, name string) checkpointPushRemoteGrant {
	elected, err := strategy.ResolveCheckpointSyncRemote(ctx)
	if err != nil {
		return checkpointPushRemoteGrant{Reason: err.Error()}
	}
	if elected.Name == name && elected.Source == strategy.SyncRemoteSourceConfig {
		return checkpointPushRemoteGrant{Effective: true}
	}
	if s, lerr := settings.Load(ctx); lerr == nil {
		if reason := s.LocalLayerRejection(); reason != "" {
			return checkpointPushRemoteGrant{Reason: reason}
		}
	}
	return checkpointPushRemoteGrant{Reason: "the setting was written but a fresh settings load does not honor it"}
}

// writeSyncJSON emits the plan (and result) as JSON.
func writeSyncJSON(w io.Writer, in syncInputs, plan syncPlan, res *checkpointMigrateResult) error {
	outJSON := checkpointMigrateJSON{
		State:             string(plan.State),
		Destination:       plan.Destination,
		DestinationSource: plan.DestinationSource,
		Steps:             plan.Steps,
		Sources:           plan.Sources,
		UnknownSources:    plan.UnknownSources,
		LocalCheckpoints:  in.LocalCheckpoints,
		QueuedRefs:        in.QueuedRefs,
		Remotes:           []syncRemoteJSON{},
		Reason:            plan.Reason,
		NextCommand:       plan.NextCommand,
		Result:            res,
	}
	if outJSON.Steps == nil {
		outJSON.Steps = []syncStep{}
	}
	if plan.ExitError != nil {
		outJSON.Error = plan.ExitError.Error()
	}
	for _, r := range in.Remotes {
		rj := syncRemoteJSON{Name: r.Name, IsEntire: r.IsEntire}
		if r.Inventory != nil {
			rj.Refs = len(r.Inventory.Refs)
			rj.V1Branch = r.Inventory.HasV1Branch
			if r.Inventory.Err != nil {
				rj.Error = r.Inventory.Err.Error()
			}
		}
		outJSON.Remotes = append(outJSON.Remotes, rj)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(outJSON); err != nil {
		return fmt.Errorf("write checkpoint migrate JSON: %w", err)
	}
	return nil
}
