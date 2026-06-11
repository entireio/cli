package investigate

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"charm.land/huh/v2"
)

// PickedInvestigate is the result of PickInvestigateAgents: the agents the
// user selected for this run, and (when no seed/issue input was supplied)
// the free-form investigation prompt that becomes the topic for this run.
// Prompt is always empty when askPrompt was false.
type PickedInvestigate struct {
	Names  []string
	Prompt string
}

// ErrInvestigatePickerCancelled is returned when the user aborts the
// multi-select.
var ErrInvestigatePickerCancelled = errors.New("investigate agent picker cancelled")

// ErrInvestigateNoAgentsSelected is returned when the user unchecks all
// agents.
var ErrInvestigateNoAgentsSelected = errors.New("no agents selected for investigation")

// PickInvestigateAgents shows a multi-select form populated from eligible
// (the agents that are both configured AND have a launchable Spawner),
// pre-checks all of them, and returns the user's selection.
//
// When askPrompt is true, a second form collects the investigation prompt
// that will become the topic for this run. When false (e.g. a seed doc or
// --issue-link was supplied), the prompt form is skipped and Prompt is
// returned empty.
//
// Requires len(eligible) >= 2.
func PickInvestigateAgents(ctx context.Context, eligible []AgentChoice, askPrompt bool) (PickedInvestigate, error) {
	if len(eligible) < 2 {
		return PickedInvestigate{}, fmt.Errorf("PickInvestigateAgents requires at least 2 eligible agents, got %d", len(eligible))
	}
	if ctx.Err() != nil {
		return PickedInvestigate{}, ErrInvestigatePickerCancelled
	}

	sorted := sortAgentChoices(eligible)

	options := make([]huh.Option[string], 0, len(sorted))
	for _, c := range sorted {
		label := c.Label
		if label == "" {
			label = c.Name
		}
		options = append(options, huh.NewOption(label, c.Name).Selected(true))
	}

	var picked []string
	multiForm := newAccessibleForm(huh.NewGroup(
		huh.NewMultiSelect[string]().
			Title("Which agents should run this investigation?").
			Options(options...).
			Height(len(options) + 1).
			Value(&picked),
	))
	if err := multiForm.RunWithContext(ctx); err != nil {
		return PickedInvestigate{}, ErrInvestigatePickerCancelled
	}

	if len(picked) == 0 {
		return PickedInvestigate{}, ErrInvestigateNoAgentsSelected
	}
	sort.Strings(picked)

	if !askPrompt {
		return PickedInvestigate{Names: picked}, nil
	}

	var prompt string
	promptForm := newAccessibleForm(huh.NewGroup(
		huh.NewText().
			Title("Investigation prompt").
			Description("Describe what you want investigated — this becomes the topic for the run.").
			Value(&prompt),
	))
	if err := promptForm.RunWithContext(ctx); err != nil {
		return PickedInvestigate{}, ErrInvestigatePickerCancelled
	}

	return PickedInvestigate{Names: picked, Prompt: prompt}, nil
}

// sortAgentChoices returns a copy of eligible sorted alphabetically by
// Name.
func sortAgentChoices(eligible []AgentChoice) []AgentChoice {
	sorted := make([]AgentChoice, len(eligible))
	copy(sorted, eligible)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	return sorted
}
