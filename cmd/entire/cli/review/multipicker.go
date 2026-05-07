// Package review — see env.go for package-level rationale.
//
// multipicker.go provides spawn-time agent multi-selection and per-run
// prompt collection for multi-agent review runs. When 2+ launchable agents
// are configured AND the user has not passed --agent, the dispatch logic
// in cmd.go calls PickAgents to let the user choose a subset and optionally
// add a one-off prompt without editing settings.
package review

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"charm.land/huh/v2"
)

// PickedAgents is the result of PickAgents: the agents the user selected
// for this run, plus an optional per-run prompt to append to the composed
// review prompt for each agent.
type PickedAgents struct {
	// Names contains the agent registry keys selected by the user,
	// e.g. ["claude-code", "codex"]. Sorted alphabetically.
	Names []string

	// PerRun is optional textarea content; "" when the user skipped or cleared it.
	PerRun string
}

// ErrPickerCancelled is returned when the user aborts the multi-select.
var ErrPickerCancelled = errors.New("agent picker cancelled")

// ErrNoAgentsSelected is returned when the user unchecks all agents.
// Caller should surface a clear error rather than running with zero agents.
var ErrNoAgentsSelected = errors.New("no agents selected for review")

// PickAgents shows a multi-select form populated from eligible (the agents
// that are both configured AND have an AgentReviewer), pre-checks all of
// them, and returns the user's selection plus an optional per-run prompt.
//
// Returns ErrPickerCancelled if the user aborts. An empty selection (user
// unchecked all boxes) returns ErrNoAgentsSelected.
//
// Requires len(eligible) >= 2; returns an error if the caller passes fewer
// than 2 choices — this function is for multi-agent flows only.
func PickAgents(ctx context.Context, eligible []AgentChoice) (PickedAgents, error) {
	if len(eligible) < 2 {
		return PickedAgents{}, fmt.Errorf("PickAgents requires at least 2 eligible agents, got %d", len(eligible))
	}

	// Sort alphabetically for stable display order regardless of how the
	// caller populated the slice.
	sorted := make([]AgentChoice, len(eligible))
	copy(sorted, eligible)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	// Build options pre-selected (all agents checked by default — mirrors
	// PR #1018 behaviour so the user can just press Enter to run all).
	options := make([]huh.Option[string], 0, len(sorted))
	for _, c := range sorted {
		options = append(options, huh.NewOption(c.Label, c.Name).Selected(true))
	}

	var picked []string
	multiForm := newAccessibleForm(huh.NewGroup(
		buildAgentMultiSelect(options, &picked),
	))
	if err := multiForm.RunWithContext(ctx); err != nil {
		return PickedAgents{}, ErrPickerCancelled
	}

	if len(picked) == 0 {
		return PickedAgents{}, ErrNoAgentsSelected
	}

	// Sort the selection alphabetically so the caller receives a stable slice.
	sort.Strings(picked)

	// Per-run prompt: optional textarea presented after agent selection.
	var perRun string
	promptForm := newAccessibleForm(huh.NewGroup(
		huh.NewText().
			Title("Optional per-run prompt").
			Description("e.g. 'focus on auth' — appended to the review prompt for this run only. Leave blank to skip.").
			Value(&perRun),
	))
	if err := promptForm.RunWithContext(ctx); err != nil {
		// Cancellation on the prompt step (Ctrl+C) propagates as picker
		// cancelled — we don't want an empty prompt here; user can retry.
		return PickedAgents{}, ErrPickerCancelled
	}

	return PickedAgents{Names: picked, PerRun: perRun}, nil
}

func buildAgentMultiSelect(options []huh.Option[string], picked *[]string) *huh.MultiSelect[string] {
	return huh.NewMultiSelect[string]().
		Title("Which agents should run this review?").
		Options(options...).
		Height(len(options) + 1).
		Value(picked)
}
