package cli

import (
	"context"
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
	pending            bool // Fresh setup asks after agent selection, before installing hooks.
}

func prepareEnableCheckpointRemoteCommand(cmd *cobra.Command, opts *EnableOptions) error {
	if cmd.Flags().Changed(flagCheckpointPushRemote) && cmd.Flags().Changed(flagCheckpointRemote) {
		return fmt.Errorf("--%s and --%s cannot be combined", flagCheckpointPushRemote, flagCheckpointRemote)
	}
	explicit := cmd.Flags().Changed(flagCheckpointPushRemote)
	canPrompt := !cmd.Flags().Changed(agentFlagName) && interactive.CanPromptInteractively()
	pending := canPrompt && !explicit && !settings.IsSetUpAny(cmd.Context())
	choice, err := prepareEnableCheckpointRemoteSelection(cmd.Context(), *opts, explicit, canPrompt && !pending, nil)
	if err != nil {
		cmd.SilenceUsage = true
		return err
	}
	choice.pending = pending
	opts.checkpointRemoteChoice = choice
	cmd.SetContext(context.WithValue(cmd.Context(), enableCheckpointRemoteKey{}, choice))
	return nil
}

func (c *enableCheckpointRemoteChoice) selectAfterAgents(ctx context.Context, opts EnableOptions, selectFn func(context.Context, []huh.Option[string]) (string, error)) error {
	if c == nil || !c.pending {
		return nil
	}
	choice, err := prepareEnableCheckpointRemoteSelection(ctx, opts, false, true, selectFn)
	if err != nil {
		return err
	}
	// Keep the pointer shared with the command's deferred destination report.
	*c = *choice
	return nil
}

func prepareEnableCheckpointRemoteSelection(ctx context.Context, opts EnableOptions, explicit, canPrompt bool, selectFn func(context.Context, []huh.Option[string]) (string, error)) (*enableCheckpointRemoteChoice, error) {
	choice, err := prepareEnableCheckpointRemote(ctx, opts, explicit)
	if err != nil || explicit || !canPrompt || opts.Yes || opts.SkipPushSessions || opts.CheckpointRemote != "" {
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
	resolved, resolveErr := strategy.ResolveCheckpointSyncRemote(ctx)
	if resolveErr == nil && s.GetCheckpointPushRemote() != "" {
		return choice, nil
	}
	if resolveErr == nil && resolved.Name != "" {
		_, dedicated, err := remote.PushURL(ctx, resolved.Name)
		if err != nil {
			return nil, fmt.Errorf("resolve dedicated checkpoint destination: %w", err)
		}
		if dedicated {
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
	if len(topology.destinations) == 0 || (len(topology.destinations) == 1 && resolveErr == nil) {
		return choice, nil
	}
	current := resolved.Name
	label := fmt.Sprintf("Keep current destination: %s (%s; no settings change)", current, checkpointRemoteSourceLabel(resolved.Source))
	if resolveErr != nil {
		current = s.GetCheckpointPushRemote()
		label = fmt.Sprintf("Keep invalid destination: %s (checkpoint sync remains disabled; no settings change)", current)
	}
	options := []huh.Option[string]{huh.NewOption(label, "")}
	for _, d := range topology.destinations {
		if d.pinned {
			continue
		}
		if err := validateCheckpointPushRemote(ctx, root, d.name); err != nil {
			continue
		}
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
			return selectEnableCheckpointRemote(ctx, options, len(topology.destinations))
		}
	}
	name, err := selectFn(ctx, options)
	if err != nil {
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
	return prepareEnableCheckpointRemote(ctx, opts, true)
}

func selectEnableCheckpointRemote(ctx context.Context, options []huh.Option[string], remoteCount int) (string, error) {
	var selected string
	keys := huh.NewDefaultKeyMap()
	keys.Quit.SetKeys("ctrl+c", "esc")
	form := NewAccessibleForm(huh.NewGroup(huh.NewSelect[string]().
		Title("Where should checkpoints go?").
		Description(fmt.Sprintf("Detected %d Git remotes. Choose where this clone uploads checkpoints. Keeping the current destination changes no destination settings.", remoteCount)).
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
	if explicit && opts.CheckpointRemote != "" {
		return nil, fmt.Errorf("--%s and --%s cannot be combined", flagCheckpointPushRemote, flagCheckpointRemote)
	}
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
	s, err := settings.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot read checkpoint destination settings: %w", err)
	}
	if !explicit {
		return choice, nil
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

func validateCheckpointPushRemote(ctx context.Context, root, name string) error {
	if _, err := gitremote.GetRemoteURLInDir(ctx, root, name); err != nil {
		return fmt.Errorf("read checkpoint remote URL: %w", err)
	}
	// Some Git versions return the remote's name successfully when it has
	// only pushurl entries. Such a remote has no configured fetch destination.
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
	s, err := settings.Load(ctx)
	if err != nil {
		fmt.Fprintf(w, "Cannot determine checkpoint destination: %v\n", err)
		return
	}
	if s.IsPushSessionsDisabled() {
		c.reportUnchangedDestination(w)
		fmt.Fprintln(w, "Checkpoint pushing remains disabled.")
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
		fmt.Fprintln(w, "No Git remotes configured. Checkpoints stay local until a remote is configured.")
		c.reportUnchangedDestination(w)
		return
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
	if !dedicated && c.name == "" && resolved.Source != strategy.SyncRemoteSourceConfig && len(topology.destinations) > 1 {
		fmt.Fprintf(w, "To choose another destination for this clone, run `entire enable --%s <name>`.\n", flagCheckpointPushRemote)
	}
}

func (c *enableCheckpointRemoteChoice) reportUnchangedDestination(w io.Writer) {
	if !c.changed && !c.dedicatedRequested {
		fmt.Fprintln(w, "No checkpoint destination settings changed.")
	}
}

func printSetupCheckpointDestinationNote(ctx context.Context, w io.Writer) {
	if ctx.Value(enableCheckpointRemoteKey{}) == nil {
		printCheckpointDestinationNote(ctx, w, "\nNote: this repo's remotes make the checkpoint destination ambiguous.")
	}
}
