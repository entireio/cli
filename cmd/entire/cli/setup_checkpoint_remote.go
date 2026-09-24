package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

const flagCheckpointPushRemote = "checkpoint-push-remote"

type enableCheckpointRemoteKey struct{}

// A choice is prepared before setup touches the repository, then saved after
// ordinary setup settings so those writes cannot overwrite the local choice.
type enableCheckpointRemoteChoice struct {
	name               string
	changed            bool
	saved              bool
	dedicatedRequested bool
	suppressClaim      bool
	// pending marks a fresh setup, the only path that opens the picker: it
	// asks after agent selection, before installing hooks. A bare re-enable in
	// a configured repo never prompts — "Keep current destination" writes
	// nothing, so a picker there would return on every run with no way to say
	// "stop asking" short of pinning a remote. Changing the destination later
	// is --checkpoint-push-remote.
	pending bool
	// offered records that the picker was actually shown. The ambiguity note
	// printed at the end of setup is suppressed only then, because on every
	// other path — non-interactive, --agent, a repo with nothing to choose
	// between — nobody has been told which remote checkpoints go to.
	offered bool
}

func prepareEnableCheckpointRemoteCommand(cmd *cobra.Command, opts *EnableOptions) error {
	if cmd.Flags().Changed(flagCheckpointPushRemote) && cmd.Flags().Changed(flagCheckpointRemote) {
		return fmt.Errorf("--%s and --%s cannot be combined", flagCheckpointPushRemote, flagCheckpointRemote)
	}
	explicit := cmd.Flags().Changed(flagCheckpointPushRemote)
	canPrompt := !cmd.Flags().Changed(agentFlagName) && interactive.CanPromptInteractively()
	pending := canPrompt && !explicit && !settings.IsSetUpAny(cmd.Context())
	choice, err := prepareEnableCheckpointRemote(cmd.Context(), *opts, explicit)
	if err != nil {
		cmd.SilenceUsage = true
		return err
	}
	choice.pending = pending
	choice.suppressClaim = opts.Yes || cmd.Flags().Changed(agentFlagName)
	opts.checkpointRemoteChoice = choice
	cmd.SetContext(context.WithValue(cmd.Context(), enableCheckpointRemoteKey{}, choice))
	return nil
}

func (c *enableCheckpointRemoteChoice) selectAfterAgents(ctx context.Context, opts EnableOptions, selectFn func(context.Context, []huh.Option[string]) (string, error)) error {
	if c == nil || !c.pending {
		return nil
	}
	choice, err := prepareEnableCheckpointRemoteSelection(ctx, opts, false, selectFn)
	if err != nil {
		return err
	}
	// Keep the pointer shared with the command's deferred destination report.
	choice.suppressClaim = c.suppressClaim
	*c = *choice
	return nil
}

func prepareEnableCheckpointRemoteSelection(ctx context.Context, opts EnableOptions, explicit bool, selectFn func(context.Context, []huh.Option[string]) (string, error)) (*enableCheckpointRemoteChoice, error) {
	choice, err := prepareEnableCheckpointRemote(ctx, opts, explicit)
	if err != nil || explicit || opts.Yes || opts.SkipPushSessions || opts.CheckpointRemote != "" {
		return choice, err
	}
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return choice, nil //nolint:nilerr // Repository bootstrap has not run yet; there is no existing destination to select.
	}
	s, err := settings.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load checkpoint destination settings: %w", err)
	}
	if s.IsPushSessionsDisabled() {
		return choice, nil
	}
	// The picked remote is saved to .entire/settings.local.json, and a rejected
	// local layer makes that write fail. Asking first and failing after the
	// answer costs the user their agent selection on the fresh-setup path, so
	// do not offer a choice that cannot be honoured. An explicit
	// --checkpoint-push-remote still reports the rejection, in
	// prepareEnableCheckpointRemote.
	if s.LocalLayerRejection() != "" {
		return choice, nil
	}
	resolved, resolveErr := strategy.ResolveCheckpointSyncRemote(ctx)
	if resolveErr == nil && resolved.Name != "" {
		_, dedicated, err := remote.PushURL(ctx, resolved.Name)
		if err != nil {
			return nil, fmt.Errorf("resolve dedicated checkpoint destination: %w", err)
		}
		if dedicated {
			return choice, nil
		}
	}
	var eligibilityErr error
	if resolveErr == nil && resolved.Name != "" {
		eligibilityErr = validateCheckpointPushRemote(ctx, root, resolved.Name)
		if eligibilityErr == nil && s.GetCheckpointPushRemote() != "" {
			return choice, nil
		}
	}
	topology := inspectRemoteTopology(ctx)
	if resolveErr != nil {
		for _, d := range topology.destinations {
			if d.pinned {
				return choice, nil
			}
		}
	}
	if len(topology.destinations) == 0 || (len(topology.destinations) == 1 && resolveErr == nil && eligibilityErr == nil) {
		return choice, nil
	}
	current := resolved.Name
	label := fmt.Sprintf("Keep current destination: %s (%s; no settings change)", current, checkpointRemoteSourceLabel(resolved.Source))
	if resolveErr != nil {
		current = s.GetCheckpointPushRemote()
		label = fmt.Sprintf("Keep invalid destination: %s (checkpoint sync remains disabled; no settings change)", current)
	} else if eligibilityErr != nil {
		label = fmt.Sprintf("Keep unsupported destination: %s (no configured fetch URL; no settings change)", current)
	}
	options := []huh.Option[string]{huh.NewOption(label, "")}
	eligible := 0 // remotes the picker can actually name, current one included
	for _, d := range topology.destinations {
		if d.pinned {
			continue
		}
		if err := validateCheckpointPushRemote(ctx, root, d.name); err != nil {
			continue
		}
		eligible++
		urls := make([]string, 0, len(d.pushURLs))
		for _, u := range d.pushURLs {
			urls = append(urls, gitremote.RedactURLOrPath(u))
		}
		if d.name == current {
			options[0].Key += " — " + strings.Join(urls, ", ")
			continue
		}
		options = append(options, huh.NewOption(fmt.Sprintf("%s — %s", d.name, strings.Join(urls, ", ")), d.name))
	}
	if len(options) == 1 {
		return choice, nil
	}
	if selectFn == nil {
		selectFn = func(ctx context.Context, options []huh.Option[string]) (string, error) {
			return selectEnableCheckpointRemote(ctx, options, eligible)
		}
	}
	name, err := selectFn(ctx, options)
	choice.offered = true
	if err != nil {
		// esc (and ctrl+c, which is bound to the same Quit) at an optional
		// question means "skip this", which this picker already offers as
		// "Keep current destination". A cancelled context is the command
		// itself going away and still aborts.
		if errors.Is(err, huh.ErrUserAborted) && ctx.Err() == nil {
			return choice, nil
		}
		return nil, fmt.Errorf("checkpoint destination selection cancelled: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("checkpoint destination selection cancelled: %w", err)
	}
	if name == "" {
		return choice, nil
	}
	// Apply exactly the same strict validation as the explicit flag, including
	// dedicated-URL precedence. Preparing a selection never writes settings.
	opts.CheckpointPushRemote = name
	picked, err := prepareEnableCheckpointRemote(ctx, opts, true)
	if err != nil {
		return nil, err
	}
	picked.offered = true
	return picked, nil
}

func selectEnableCheckpointRemote(ctx context.Context, options []huh.Option[string], eligible int) (string, error) {
	var selected string
	keys := huh.NewDefaultKeyMap()
	keys.Quit.SetKeys("ctrl+c", "esc")
	form := NewAccessibleForm(huh.NewGroup(huh.NewSelect[string]().
		Title("Where should checkpoints go?").
		Description(fmt.Sprintf("%d Git remotes can receive checkpoints. Choose where this clone uploads them. Keeping the current destination changes no destination settings.", eligible)).
		Options(options...).Value(&selected))).WithKeyMap(keys)
	if err := form.RunWithContext(ctx); err != nil {
		return "", fmt.Errorf("checkpoint destination form: %w", err)
	}
	return selected, nil
}

func checkpointRemoteSourceLabel(source strategy.CheckpointSyncRemoteSource) string {
	if source == strategy.SyncRemoteSourceConfig {
		return "configured"
	}
	if source == strategy.SyncRemoteSourceObserved {
		return "previously selected"
	}
	return "automatic"
}

func prepareEnableCheckpointRemote(ctx context.Context, opts EnableOptions, explicit bool) (*enableCheckpointRemoteChoice, error) {
	choice := &enableCheckpointRemoteChoice{dedicatedRequested: opts.CheckpointRemote != ""}
	if explicit && opts.CheckpointPushRemote == "" {
		return nil, fmt.Errorf("--%s must not be empty", flagCheckpointPushRemote)
	}
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		if explicit {
			return nil, fmt.Errorf("--%s requires an existing git repository: %w", flagCheckpointPushRemote, err)
		}
		return choice, nil
	}
	if !explicit {
		return choice, nil
	}
	s, err := settings.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot read checkpoint destination settings: %w", err)
	}
	remotes, err := pushURLsByRemote(ctx, root)
	if err != nil {
		return nil, err
	}
	if _, ok := remotes[opts.CheckpointPushRemote]; !ok {
		return nil, fmt.Errorf("--%s must name a configured Git remote", flagCheckpointPushRemote)
	}
	if err := validateCheckpointPushRemote(ctx, root, opts.CheckpointPushRemote); err != nil {
		return nil, fmt.Errorf("--%s must name a configured Git remote: %w", flagCheckpointPushRemote, err)
	}
	if reason := s.LocalLayerRejection(); reason != "" {
		return nil, fmt.Errorf("cannot save checkpoint destination: local settings ignored: %s", reason)
	}
	if _, dedicated, err := remote.PushURL(ctx, opts.CheckpointPushRemote); err != nil {
		return nil, fmt.Errorf("resolve checkpoint destination: %w", err)
	} else if dedicated {
		return nil, fmt.Errorf("--%s cannot replace the effective dedicated checkpoint_remote destination", flagCheckpointPushRemote)
	}
	choice.name = opts.CheckpointPushRemote
	return choice, nil
}

// validateCheckpointPushRemote reads remote.<name>.url directly rather than
// asking `git remote get-url`: some Git versions answer that with the remote's
// name for a pushurl-only remote, which has no fetch destination, and an unset
// url is also what an unknown remote looks like — one read covers both.
func validateCheckpointPushRemote(ctx context.Context, root, name string) error {
	url, err := gitRunner(ctx, root, "config", "--get", "remote."+name+".url")
	if err != nil || strings.TrimSpace(url) == "" {
		return fmt.Errorf("remote %q has no configured fetch URL", name)
	}
	return nil
}

func (c *enableCheckpointRemoteChoice) persist(ctx context.Context) error {
	if c == nil || c.name == "" || c.saved {
		return nil
	}
	changed, err := settings.SetCheckpointPushRemoteLocal(ctx, c.name)
	c.changed = changed
	c.saved = changed || err == nil
	if err != nil {
		return fmt.Errorf("save checkpoint destination: %w", err)
	}
	return nil
}

func (c *enableCheckpointRemoteChoice) report(ctx context.Context, w io.Writer, setupErr error) {
	if setupErr != nil {
		if c.changed {
			fmt.Fprintf(w, "Checkpoint destination saved to %s in .entire/settings.local.json; enable did not complete.\n", c.name)
		}
		return
	}
	if c.changed {
		fmt.Fprintf(w, "Checkpoint destination set to: %s\n", c.name)
		fmt.Fprintln(w, "Saved to .entire/settings.local.json for this clone.")
	}
	// A healthy destination nobody asked about is not reported: the command
	// ends at "Ready." and the multi-remote note covers the ambiguous repos. A
	// broken destination is reported on every run, because the user cannot
	// otherwise learn that checkpoint sync is off.
	touched := c.name != "" || c.offered || c.dedicatedRequested
	s, err := settings.Load(ctx)
	if err != nil {
		fmt.Fprintf(w, "Cannot determine checkpoint destination: %v\n", err)
		return
	}
	if s.IsPushSessionsDisabled() {
		if touched {
			c.reportUnchangedDestination(w)
			fmt.Fprintln(w, "Checkpoint pushing remains disabled.")
		}
		return
	}
	resolved, err := strategy.ResolveCheckpointSyncRemote(ctx)
	if err != nil {
		dedicated := false
		for _, d := range inspectRemoteTopology(ctx).destinations {
			if !d.pinned {
				continue
			}
			url, effective, urlErr := remote.PushURL(ctx, d.name)
			if urlErr == nil && effective {
				fmt.Fprintf(w, "Dedicated checkpoint destination for pushes to %s: %s\n", d.name, gitremote.RedactURLOrPath(url))
				dedicated = true
			}
		}
		if dedicated {
			fmt.Fprintf(w, "Cannot resolve checkpoint remote selection: %v. Dedicated checkpoint uploads can still proceed.\n", err)
			c.reportUnchangedDestination(w)
			return
		}
		fmt.Fprintf(w, "Checkpoint sync remains disabled: %v\n", err)
		c.reportUnchangedDestination(w)
		fmt.Fprintf(w, "Choose a configured remote with `entire enable --%s <name>`.\n", flagCheckpointPushRemote)
		return
	}
	if resolved.Name == "" {
		if touched {
			fmt.Fprintln(w, "No Git remotes configured. Checkpoints stay local until a remote is configured.")
			c.reportUnchangedDestination(w)
		}
		return
	}
	// Resolve a possible local claim before computing the destination summary.
	if reportIgnoredCheckpointRemote(ctx, w, s, resolved.Name, !c.suppressClaim) {
		c.changed = true
		touched = true
	}
	destination := resolved.Name
	url, dedicated, err := remote.PushURL(ctx, resolved.Name)
	if err != nil {
		fmt.Fprintf(w, "Cannot determine checkpoint destination: %v\n", err)
		return
	}
	if dedicated {
		destination = gitremote.RedactURLOrPath(url)
	}
	root, rootErr := paths.WorktreeRoot(ctx)
	if !dedicated {
		eligibilityErr := rootErr
		if eligibilityErr == nil {
			eligibilityErr = validateCheckpointPushRemote(ctx, root, resolved.Name)
		}
		if eligibilityErr != nil {
			fmt.Fprintf(w, "Cannot confirm checkpoint uploads to %s: %v\n", resolved.Name, eligibilityErr)
			c.reportUnchangedDestination(w)
			fmt.Fprintf(w, "Choose a configured remote with `entire enable --%s <name>`.\n", flagCheckpointPushRemote)
			return
		}
	}

	if !touched {
		return
	}
	if !c.changed && !c.dedicatedRequested {
		fmt.Fprintf(w, "Keeping checkpoint destination: %s (%s)\n", destination, checkpointRemoteSourceLabel(resolved.Source))
	}
	c.reportUnchangedDestination(w)
	if dedicated {
		fmt.Fprintf(w, "Checkpoint uploads use the dedicated destination: %s.\n", destination)
	} else {
		fmt.Fprintf(w, "Checkpoints will be uploaded when you push to %s.\n", resolved.Name)
	}
	topology := inspectRemoteTopology(ctx)
	for _, d := range topology.destinations {
		if !dedicated && d.pinned {
			url, effective, err := remote.PushURL(ctx, d.name)
			if err == nil && effective {
				fmt.Fprintf(w, "Pushes to %s still upload checkpoints to the dedicated destination: %s\n", d.name, gitremote.RedactURLOrPath(url))
			}
		}
		if d.name != resolved.Name || !d.fansOut() {
			continue
		}
		remoteTopology{destinations: []remoteDestination{d}, primaryIsRefs: topology.primaryIsRefs}.describeFanout(w)
	}
	if !dedicated && c.name == "" && resolved.Source != strategy.SyncRemoteSourceConfig {
		for _, d := range topology.destinations {
			if d.name != resolved.Name && !d.pinned && validateCheckpointPushRemote(ctx, root, d.name) == nil {
				fmt.Fprintf(w, "To choose another destination for this clone, run `entire enable --%s <name>`.\n", flagCheckpointPushRemote)
				break
			}
		}
	}
}

// reportUnchangedDestination announces the absence of a change only when one
// was asked for: an explicit or picked remote that was already in effect.
func (c *enableCheckpointRemoteChoice) reportUnchangedDestination(w io.Writer) {
	if c.name != "" && !c.changed {
		fmt.Fprintln(w, "No checkpoint destination settings changed.")
	}
}

// printSetupCheckpointDestinationNote drops the multi-remote ambiguity note
// only when `entire enable` has already put the same question to the user. The
// gate is "a picker was shown", not "the prepare step ran": the latter runs on
// every invocation, including the non-interactive ones where no picker can
// appear and the note is the only thing that describes the ambiguity.
func printSetupCheckpointDestinationNote(ctx context.Context, w io.Writer) {
	if c, ok := ctx.Value(enableCheckpointRemoteKey{}).(*enableCheckpointRemoteChoice); ok && c != nil && c.offered {
		return
	}
	printCheckpointDestinationNote(ctx, w, "\nNote: this repo's remotes make the checkpoint destination ambiguous.")
}

// reportIgnoredCheckpointRemote explains a refused checkpoint_remote and offers
// a local claim only when it can safely confirm ownership. Reports whether it
// saved a claim, so the caller can describe the resulting destination.
//
// The rejection is deliberate — checkpointRemoteIsInherited refuses a committed
// checkpoint_remote whose owner does not match every remote identifying this
// repo, so a fork contributor's transcripts never land in the upstream's
// checkpoint store. What was missing is the other half: when the store really
// is the developer's own, nothing told them how to say so, and the symptom
// (checkpoints arriving in the code repository) looks like a working setup.
func reportIgnoredCheckpointRemote(ctx context.Context, w io.Writer, s *settings.EntireSettings, electedRemote string, allowPrompt bool) bool {
	cr := s.GetCheckpointRemote()
	if cr == nil {
		return false
	}
	verdict, reason := remote.InheritedCheckpointRemoteVerdict(ctx, s, electedRemote)
	if !verdict.Refused() {
		return false
	}
	repo := cr.Repo
	// The verdict above votes with ONE remote — the elected one — and a repo can
	// have several. A remote whose owner matches still resolves the store, so
	// "not in use" would be flatly false for a repo where pushing to that remote
	// uploads checkpoints exactly as configured. `pinned` is the resolver's own
	// answer to "does this remote reach the checkpoint_remote", so any pinned
	// destination means the store is in use somewhere and the user is not in the
	// broken state this message describes.
	for _, d := range inspectRemoteTopology(ctx).destinations {
		if d.pinned {
			return false
		}
	}
	explanation, command := remote.IgnoredCheckpointRemoteGuidance(ctx, cr, verdict, reason)
	fmt.Fprintf(w, "checkpoint_remote %s is not in use: %s.\n", repo, explanation)

	// Ownership merely UNPROVABLE is the one case a human can settle that local
	// git config cannot: a single-segment or non-forge origin
	// (git@host:repo.git, https://host/repo.git, a filesystem path) yields no
	// owner to compare, which is ordinary on self-hosted git. Refusing stays
	// right non-interactively — absence of evidence is not proof the store is
	// ours — but the person running `entire enable` knows.
	//
	// Never offered for OwnershipDisproved: a remote named an owner and it was
	// somebody else, which is the fork case the check exists for. Offering to
	// adopt there would walk a contributor into publishing their transcripts to
	// the upstream's store, one keystroke deep.
	//
	// This is not a new trust boundary. `entire enable --local
	// --checkpoint-remote <provider>:<owner>/<repo>` already performs exactly
	// this write in one command; the prompt makes the existing remedy
	// discoverable to someone who does not know it exists.
	if allowPrompt && verdict == remote.OwnershipUnprovable &&
		settings.CheckpointRemoteLocalClaimRejection(ctx) == "" &&
		offerToClaimCheckpointRemote(ctx, w, remote.ClaimCheckpointRemoteFlagValue(cr), repo) {
		return true
	}

	if command != "" {
		fmt.Fprintf(w, "If %s is yours, confirm it for this clone: `%s`.\n", repo, command)
	}
	return false
}

// offerToClaimCheckpointRemote asks whether the configured store belongs to this
// developer and, on yes, declares it in .entire/settings.local.json — the
// per-clone, gitignored layer whose presence CheckpointRemoteIsLocalOnly takes
// as proof the developer chose it. Reports whether it wrote.
//
// Interactive only. A non-TTY run gets the printed command instead, because a
// prompt nobody can answer must not become an implicit yes — and because an
// agent reading the output should be handed the command rather than have the
// decision made for the human it works for.
// Takes the already-validated claimValue rather than the config it came from,
// so the value written can never diverge from the one validated and printed —
// reconstructing it here from raw fields let surrounding whitespace pass the
// check and fail the write.
func offerToClaimCheckpointRemote(ctx context.Context, w io.Writer, claimValue, repo string) bool {
	if !interactive.CanPromptInteractively() {
		return false
	}
	// A provider the flag cannot express has no value and no write path; fall
	// through to the settings-file message rather than prompting for something
	// that cannot be carried out.
	if claimValue == "" {
		return false
	}

	var claim bool
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title(fmt.Sprintf("Use %s for this clone's checkpoints?", repo)).
				Description("This repo's remotes cannot prove who owns that store, so it is being ignored.\nAnswer yes only if it is yours.").
				Affirmative("Yes, it's mine").
				Negative("No, leave it").
				Value(&claim),
		),
	)
	if err := form.Run(); err != nil || !claim {
		return false
	}

	if err := updateStrategyOptions(ctx, w, EnableOptions{
		UseLocalSettings: true,
		CheckpointRemote: claimValue,
	}); err != nil {
		fmt.Fprintf(w, "Could not save the checkpoint destination: %v\n", err)
		return false
	}
	if !settings.CheckpointRemoteIsLocalOnly(ctx) {
		fmt.Fprintln(w, "The saved checkpoint destination could not be confirmed as your own untracked local setting; check `entire status` before pushing.")
		return false
	}
	fmt.Fprintf(w, "Confirmed %s for this clone (saved to .entire/settings.local.json).\n", repo)
	return true
}
