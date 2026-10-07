package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// checkpointDeleteFlags are the parsed flags of `checkpoint delete`.
type checkpointDeleteFlags struct {
	dryRun    bool
	force     bool
	localOnly bool
	remotes   []string
	json      bool
}

func newCheckpointDeleteCmd() *cobra.Command {
	return newCheckpointDeleteCmdWithPrompt(interactive.CanPromptInteractively)
}

// newCheckpointDeleteCmdWithPrompt takes the "can we prompt?" probe so tests
// can exercise both the interactive and the non-interactive paths.
func newCheckpointDeleteCmdWithPrompt(canPrompt func() bool) *cobra.Command {
	var flags checkpointDeleteFlags

	cmd := &cobra.Command{
		Use:   "delete <checkpoint-id>",
		Short: "Delete a checkpoint locally and from checkpoint remotes",
		Long: `Delete one checkpoint from this repository and from every checkpoint remote
that holds it.

The full checkpoint ID is required; prefixes are not accepted. Deleting from a
remote cannot be undone. A checkpoint on the entire/checkpoints/v1 branch is
removed from the branch tip only: the branch's history still contains it.

Other checkpoints of the same sessions are listed but not deleted. Each one
carries the session's full transcript, so the session stays visible on
entire.io while any of them remain.

Without --force the command asks for confirmation, and refuses when it cannot
prompt. --dry-run prints what would be deleted and changes nothing.

Examples:
  entire checkpoint delete <id> --dry-run
  entire checkpoint delete <id>
  entire checkpoint delete <id> --remote origin --force
  entire checkpoint delete <id> --local-only --force`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runCheckpointDelete(cmd, args[0], flags, canPrompt())
		},
	}

	cmd.Flags().BoolVar(&flags.dryRun, "dry-run", false, "Print what would be deleted without changing anything")
	cmd.Flags().BoolVarP(&flags.force, "force", "f", false, "Delete without asking for confirmation")
	cmd.Flags().BoolVar(&flags.localOnly, "local-only", false, "Delete only the local copy; leave every remote untouched")
	cmd.Flags().StringArrayVar(&flags.remotes, "remote", nil, "Limit remote deletion to this remote; the local copy is still deleted (repeatable; \""+strategy.CheckpointRemoteTargetName+"\" names the dedicated checkpoint remote)")
	cmd.Flags().BoolVar(&flags.json, "json", false, "Output as JSON")
	return cmd
}

func validateCheckpointDeleteFlags(flags checkpointDeleteFlags) error {
	if flags.localOnly && len(flags.remotes) > 0 {
		return errors.New("--local-only cannot be combined with --remote")
	}
	if flags.json && !flags.dryRun && !flags.force {
		return errors.New("--json without --dry-run requires --force, since it cannot prompt")
	}
	return nil
}

func runCheckpointDelete(cmd *cobra.Command, rawID string, flags checkpointDeleteFlags, canPrompt bool) error {
	ctx := cmd.Context()
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	if err := validateCheckpointDeleteFlags(flags); err != nil {
		return err
	}
	cid, err := id.NewCheckpointID(rawID)
	if err != nil {
		return fmt.Errorf("invalid checkpoint ID %q: pass the full ID (prefixes are not accepted)", rawID)
	}
	if checkDisabledGuard(ctx, errOut) {
		return NewSilentError(errors.New("entire is disabled in this repository"))
	}

	plan, err := strategy.PlanCheckpointDelete(ctx, cid)
	if err != nil {
		return fmt.Errorf("delete checkpoint: %w", err)
	}
	targets, err := selectCheckpointDeleteTargets(plan, flags)
	if err != nil {
		return err
	}

	if flags.dryRun {
		if flags.json {
			return writeCheckpointDeleteJSON(out, plan, targets, nil, true)
		}
		printCheckpointDeletePlan(out, plan, targets, flags.localOnly)
		fmt.Fprintln(out, "\nDry run: nothing was changed.")
		return nil
	}

	if active := plan.ActiveStates(); len(active) > 0 && !flags.force {
		return fmt.Errorf("refusing to delete checkpoint %s: session %s is still active; end the session first or pass --force", cid, active[0].SessionID)
	}
	if !plan.HasLocalCopy() && len(targets) == 0 {
		return fmt.Errorf("checkpoint %s has no local copy and no selected remote holds it; nothing to delete", cid)
	}

	if !flags.json {
		printCheckpointDeletePlan(out, plan, targets, flags.localOnly)
		fmt.Fprintln(out)
	}
	if !flags.force && canPrompt && len(targets) > 1 {
		targets, err = promptCheckpointDeleteTargets(ctx, targets)
		if err != nil {
			return err
		}
	}
	confirmed, err := confirmControlPlaneDeletion(ctx, out, "checkpoint "+cid.String(), flags.force, canPrompt)
	if err != nil || !confirmed {
		return err
	}

	result, execErr := strategy.ExecuteCheckpointDelete(ctx, plan, strategy.CheckpointDeleteOptions{Targets: targets, LocalOnly: flags.localOnly})
	if flags.json && result != nil {
		if err := writeCheckpointDeleteJSON(out, plan, targets, result, false); err != nil {
			return err
		}
	} else if result != nil {
		printCheckpointDeleteResult(out, plan, result, flags)
	}
	if execErr != nil {
		return fmt.Errorf("delete checkpoint %s: %w", cid, execErr)
	}
	if result.Failed() {
		for _, t := range result.Targets {
			if t.Failed() {
				fmt.Fprintf(errOut, "Failed to delete checkpoint %s from %s: %s\n  Retry: %s\n", cid, t.Target.Name(), t.Error, checkpointDeleteRetryCommand(cid, t.Target))
			}
		}
		return NewSilentError(fmt.Errorf("checkpoint %s was not deleted from every remote", cid))
	}
	return nil
}

// selectCheckpointDeleteTargets picks the remote targets a delete acts on:
// none with --local-only, the named ones with --remote, else every reachable
// target that holds the checkpoint. A --remote name that matches no known
// checkpoint remote is an error rather than a silent no-op.
func selectCheckpointDeleteTargets(plan *strategy.CheckpointDeletePlan, flags checkpointDeleteFlags) ([]strategy.CheckpointDeleteTarget, error) {
	if flags.localOnly {
		return nil, nil
	}
	holders := plan.HolderTargets()
	if len(flags.remotes) == 0 {
		return holders, nil
	}
	var known []string
	for _, t := range plan.Targets {
		known = append(known, t.Remotes...)
	}
	for _, name := range flags.remotes {
		if !slices.Contains(known, name) {
			slices.Sort(known)
			return nil, fmt.Errorf("--remote %q is not a checkpoint remote of this repository (known: %s)", name, strings.Join(slices.Compact(known), ", "))
		}
	}
	var selected []strategy.CheckpointDeleteTarget
	for _, t := range holders {
		for _, name := range t.Remotes {
			if slices.Contains(flags.remotes, name) {
				selected = append(selected, t)
				break
			}
		}
	}
	return selected, nil
}

func promptCheckpointDeleteTargets(ctx context.Context, targets []strategy.CheckpointDeleteTarget) ([]strategy.CheckpointDeleteTarget, error) {
	options := make([]huh.Option[int], len(targets))
	selected := make([]int, len(targets))
	for i, t := range targets {
		options[i] = huh.NewOption(fmt.Sprintf("%s (%s)", strings.Join(t.Remotes, ", "), t.Display()), i).Selected(true)
		selected[i] = i
	}
	form := NewAccessibleForm(huh.NewGroup(
		huh.NewMultiSelect[int]().
			Title("Delete the checkpoint from these remotes").
			Description("Use space to toggle, enter to confirm.").
			Options(options...).
			Value(&selected),
	))
	if err := form.RunWithContext(ctx); err != nil {
		return nil, fmt.Errorf("remote selection: %w", err)
	}
	chosen := make([]strategy.CheckpointDeleteTarget, 0, len(selected))
	for _, i := range selected {
		chosen = append(chosen, targets[i])
	}
	return chosen, nil
}

func checkpointDeleteRetryCommand(cid id.CheckpointID, target strategy.CheckpointDeleteTarget) string {
	return fmt.Sprintf("entire checkpoint delete %s --remote %s", cid, target.Name())
}

func printCheckpointDeletePlan(w io.Writer, plan *strategy.CheckpointDeletePlan, targets []strategy.CheckpointDeleteTarget, localOnly bool) {
	fmt.Fprintf(w, "Checkpoint %s\n", plan.CheckpointID)

	fmt.Fprintln(w, "\nLocal:")
	if !plan.HasLocalCopy() {
		fmt.Fprintln(w, "  not in this clone")
	}
	if plan.LocalRef != "" {
		fmt.Fprintf(w, "  ref %s\n", plan.LocalRef)
	}
	if plan.LocalV1 {
		fmt.Fprintf(w, "  %s branch\n", paths.MetadataBranchName)
	}

	fmt.Fprintln(w, "\nRemotes:")
	if len(plan.Targets) == 0 {
		fmt.Fprintln(w, "  none configured")
	}
	for _, t := range plan.Targets {
		fmt.Fprintf(w, "  %s (%s): %s%s\n", strings.Join(t.Remotes, ", "), t.Display(), describeDeleteTarget(t), selectionNote(t, targets, localOnly))
	}

	if len(plan.Sessions) > 0 {
		fmt.Fprintln(w, "\nSessions in this checkpoint (their token counts are removed with it):")
		for _, s := range plan.Sessions {
			fmt.Fprintf(w, "  %s  %s  input %d, output %d tokens\n", s.SessionID, s.Agent, s.InputTokens, s.OutputTokens)
		}
	}
	if len(plan.SessionStates) > 0 {
		fmt.Fprintln(w, "\nLocal session state to clear:")
		for _, s := range plan.SessionStates {
			status := ""
			if s.Active {
				status = " (session still active)"
			}
			fmt.Fprintf(w, "  %s: %s%s\n", s.SessionID, strings.Join(s.Fields, ", "), status)
		}
	}
	if len(plan.OtherLocalCheckpoints) > 0 {
		fmt.Fprintln(w, "\nOther local checkpoints of these sessions (not deleted; this list is local only,")
		fmt.Fprintln(w, "and the sessions stay visible on entire.io while these remain):")
		for _, o := range plan.OtherLocalCheckpoints {
			fmt.Fprintf(w, "  %s  %s\n", o.CheckpointID, strings.Join(o.SessionIDs, ", "))
		}
		if plan.OtherLocalTruncated {
			fmt.Fprintln(w, "  (scan truncated; there may be more)")
		}
	}
	if checkpointDeleteTouchesV1(plan) {
		fmt.Fprintf(w, "\nNote: on the %s branch the checkpoint is removed from the tip only; its content stays in the branch history.\n", paths.MetadataBranchName)
	}
	if plan.PushSessionsDisabled && !localOnly {
		fmt.Fprintln(w, "Note: push_sessions is disabled, but the delete still removes the copies on the selected remotes.")
	}
	for _, warning := range plan.Warnings {
		fmt.Fprintf(w, "Warning: %s\n", warning)
	}
}

func describeDeleteTarget(t strategy.CheckpointDeleteTarget) string {
	if !t.Reachable {
		return "unreachable"
	}
	var parts []string
	if !t.RefOID.IsZero() {
		parts = append(parts, "ref")
	}
	switch t.V1 {
	case strategy.V1CopyPresent:
		parts = append(parts, paths.MetadataBranchName)
	case strategy.V1CopyUnknown:
		parts = append(parts, paths.MetadataBranchName+" (verified when deleting)")
	case strategy.V1CopyAbsent:
	}
	if len(parts) == 0 {
		return "does not hold it"
	}
	return "holds " + strings.Join(parts, ", ")
}

func selectionNote(t strategy.CheckpointDeleteTarget, selected []strategy.CheckpointDeleteTarget, localOnly bool) string {
	if !t.HoldsCheckpoint() {
		return ""
	}
	if localOnly || !slices.ContainsFunc(selected, func(s strategy.CheckpointDeleteTarget) bool { return s.URL == t.URL }) {
		return " (not selected)"
	}
	return ""
}

func checkpointDeleteTouchesV1(plan *strategy.CheckpointDeletePlan) bool {
	if plan.LocalV1 || len(plan.TrackingV1) > 0 {
		return true
	}
	return slices.ContainsFunc(plan.Targets, func(t strategy.CheckpointDeleteTarget) bool {
		return t.V1 == strategy.V1CopyPresent || t.V1 == strategy.V1CopyUnknown
	})
}

func printCheckpointDeleteResult(w io.Writer, plan *strategy.CheckpointDeletePlan, result *strategy.CheckpointDeleteResult, flags checkpointDeleteFlags) {
	fmt.Fprintf(w, "Local ref: %s\n", result.LocalRef)
	if plan.LocalV1 {
		fmt.Fprintf(w, "Local %s: %s\n", paths.MetadataBranchName, result.LocalV1)
	}
	for _, t := range result.Targets {
		line := fmt.Sprintf("%s: ref %s", t.Target.Name(), t.Ref)
		if t.V1 != strategy.DeleteOutcomeAbsent {
			line += fmt.Sprintf(", %s %s", paths.MetadataBranchName, t.V1)
		}
		fmt.Fprintln(w, line)
	}
	if result.Failed() {
		return
	}
	unchecked := unreachableTargetNames(plan, flags)
	if len(unchecked) == 0 {
		fmt.Fprintf(w, "Deleted checkpoint %s.\n", plan.CheckpointID)
		return
	}
	fmt.Fprintf(w, "Deleted checkpoint %s from every remote that could be reached. Could not check %s, which may still hold it; retry with:\n",
		plan.CheckpointID, strings.Join(unchecked, ", "))
	for _, name := range unchecked {
		fmt.Fprintf(w, "  entire checkpoint delete %s --remote %s\n", plan.CheckpointID, name)
	}
}

// unreachableTargetNames names the targets the probe could not reach. An
// unreachable remote is a warning, never a failure, but the summary must not
// claim the checkpoint is gone everywhere while one of them may still hold it.
func unreachableTargetNames(plan *strategy.CheckpointDeletePlan, flags checkpointDeleteFlags) []string {
	if flags.localOnly {
		return nil
	}
	var names []string
	for _, t := range plan.Targets {
		if t.Reachable {
			continue
		}
		if len(flags.remotes) > 0 && !slices.ContainsFunc(t.Remotes, func(n string) bool { return slices.Contains(flags.remotes, n) }) {
			continue
		}
		names = append(names, t.Name())
	}
	return names
}

type checkpointDeleteJSON struct {
	CheckpointID         string                        `json:"checkpoint_id"`
	DryRun               bool                          `json:"dry_run"`
	Local                checkpointDeleteLocalJSON     `json:"local"`
	Remotes              []checkpointDeleteRemoteJSON  `json:"remotes"`
	Sessions             []checkpointDeleteSessionJSON `json:"sessions"`
	SessionStates        []checkpointDeleteStateJSON   `json:"session_states"`
	OtherLocal           []checkpointDeleteSiblingJSON `json:"other_local_session_checkpoints"`
	OtherLocalTruncated  bool                          `json:"other_local_session_checkpoints_truncated,omitempty"`
	V1HistoryRetained    bool                          `json:"v1_history_retained"`
	PushSessionsDisabled bool                          `json:"push_sessions_disabled,omitempty"`
	Warnings             []string                      `json:"warnings,omitempty"`
}

type checkpointDeleteLocalJSON struct {
	Ref       string `json:"ref,omitempty"`
	RefOID    string `json:"ref_oid,omitempty"`
	V1        bool   `json:"v1"`
	RefResult string `json:"ref_result,omitempty"`
	V1Result  string `json:"v1_result,omitempty"`
}

type checkpointDeleteRemoteJSON struct {
	Name     string                            `json:"name"`
	Names    []string                          `json:"names"`
	URL      string                            `json:"url"`
	Status   string                            `json:"status"`
	Selected bool                              `json:"selected"`
	RefOID   string                            `json:"ref_oid,omitempty"`
	V1       string                            `json:"v1,omitempty"`
	Result   *checkpointDeleteRemoteResultJSON `json:"result,omitempty"`
}

type checkpointDeleteRemoteResultJSON struct {
	Ref   string `json:"ref"`
	V1    string `json:"v1"`
	Error string `json:"error,omitempty"`
	Retry string `json:"retry,omitempty"`
}

type checkpointDeleteSessionJSON struct {
	SessionID    string `json:"session_id"`
	Agent        string `json:"agent,omitempty"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
}

type checkpointDeleteStateJSON struct {
	SessionID string   `json:"session_id"`
	Active    bool     `json:"active"`
	Fields    []string `json:"fields"`
}

type checkpointDeleteSiblingJSON struct {
	CheckpointID string   `json:"checkpoint_id"`
	SessionIDs   []string `json:"session_ids"`
}

func buildCheckpointDeleteJSON(plan *strategy.CheckpointDeletePlan, targets []strategy.CheckpointDeleteTarget, result *strategy.CheckpointDeleteResult, dryRun bool) checkpointDeleteJSON {
	doc := checkpointDeleteJSON{
		CheckpointID:         plan.CheckpointID.String(),
		DryRun:               dryRun,
		Local:                checkpointDeleteLocalJSON{Ref: plan.LocalRef.String(), V1: plan.LocalV1},
		Remotes:              []checkpointDeleteRemoteJSON{},
		Sessions:             []checkpointDeleteSessionJSON{},
		SessionStates:        []checkpointDeleteStateJSON{},
		OtherLocal:           []checkpointDeleteSiblingJSON{},
		OtherLocalTruncated:  plan.OtherLocalTruncated,
		V1HistoryRetained:    checkpointDeleteTouchesV1(plan),
		PushSessionsDisabled: plan.PushSessionsDisabled,
		Warnings:             plan.Warnings,
	}
	if !plan.LocalRefOID.IsZero() {
		doc.Local.RefOID = plan.LocalRefOID.String()
	}
	if result != nil {
		doc.Local.RefResult = string(result.LocalRef)
		doc.Local.V1Result = string(result.LocalV1)
	}
	for _, t := range plan.Targets {
		entry := checkpointDeleteRemoteJSON{
			Name:     t.Name(),
			Names:    t.Remotes,
			URL:      t.Display(),
			Status:   checkpointDeleteTargetStatus(t),
			Selected: slices.ContainsFunc(targets, func(s strategy.CheckpointDeleteTarget) bool { return s.URL == t.URL }),
		}
		if !t.RefOID.IsZero() {
			entry.RefOID = t.RefOID.String()
		}
		if t.Reachable {
			entry.V1 = string(t.V1)
		}
		if result != nil {
			for _, r := range result.Targets {
				if r.Target.URL != t.URL {
					continue
				}
				res := &checkpointDeleteRemoteResultJSON{Ref: string(r.Ref), V1: string(r.V1), Error: r.Error}
				if r.Failed() {
					res.Retry = checkpointDeleteRetryCommand(plan.CheckpointID, t)
				}
				entry.Result = res
			}
		}
		doc.Remotes = append(doc.Remotes, entry)
	}
	for _, s := range plan.Sessions {
		doc.Sessions = append(doc.Sessions, checkpointDeleteSessionJSON{SessionID: s.SessionID, Agent: s.Agent, InputTokens: s.InputTokens, OutputTokens: s.OutputTokens})
	}
	for _, s := range plan.SessionStates {
		doc.SessionStates = append(doc.SessionStates, checkpointDeleteStateJSON{SessionID: s.SessionID, Active: s.Active, Fields: s.Fields})
	}
	for _, o := range plan.OtherLocalCheckpoints {
		doc.OtherLocal = append(doc.OtherLocal, checkpointDeleteSiblingJSON{CheckpointID: o.CheckpointID.String(), SessionIDs: o.SessionIDs})
	}
	return doc
}

func checkpointDeleteTargetStatus(t strategy.CheckpointDeleteTarget) string {
	switch {
	case !t.Reachable:
		return "unreachable"
	case t.HoldsCheckpoint():
		return "holds"
	default:
		return "absent"
	}
}

func writeCheckpointDeleteJSON(w io.Writer, plan *strategy.CheckpointDeletePlan, targets []strategy.CheckpointDeleteTarget, result *strategy.CheckpointDeleteResult, dryRun bool) error {
	data, err := json.MarshalIndent(buildCheckpointDeleteJSON(plan, targets, result, dryRun), "", "  ")
	if err != nil {
		return fmt.Errorf("encode JSON: %w", err)
	}
	fmt.Fprintln(w, string(data))
	return nil
}
