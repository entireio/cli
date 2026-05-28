package investigate_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/spawn"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/investigate"
)

// TestRunInvestigateConfigPicker_NoEligibleAgents covers the case where
// none of the installed agents has a Spawner.
func TestRunInvestigateConfigPicker_NoEligibleAgents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	out := &bytes.Buffer{}
	_, err := investigate.RunInvestigateConfigPicker(ctx, out,
		func(_ string) spawn.Spawner { return nil },
		func(_ context.Context) []types.AgentName {
			return []types.AgentName{"some-agent"}
		},
	)
	if err == nil {
		t.Fatal("expected error when no spawnable agents")
	}
	if !strings.Contains(err.Error(), "no launchable agents") {
		t.Errorf("error should mention launchability, got: %v", err)
	}
}

// TestRunInvestigateConfigPicker_FiltersNonInstalled verifies that an
// agent with a spawner but no hooks installed is filtered out.
func TestRunInvestigateConfigPicker_FiltersNonInstalled(t *testing.T) {
	t.Parallel()
	cleanup := investigate.SetPickerFormFnForTest(func(_ context.Context, eligible []investigate.AgentChoice, picks *[]string, maxTurns, quorum *int) error {
		// Capture eligible into picks for assertion via the cfg.Agents.
		names := make([]string, 0, len(eligible))
		for _, c := range eligible {
			names = append(names, c.Name)
		}
		*picks = names
		*maxTurns = 3
		*quorum = 0
		return nil
	})
	defer cleanup()

	ctx := context.Background()
	out := &bytes.Buffer{}
	cfg, err := investigate.RunInvestigateConfigPicker(ctx, out,
		func(name string) spawn.Spawner {
			if name == "spawner-and-hooked" || name == "spawner-only" {
				return stubSpawner{name: name}
			}
			return nil
		},
		func(_ context.Context) []types.AgentName {
			return []types.AgentName{"spawner-and-hooked"} // spawner-only NOT in installed list
		},
	)
	if err != nil {
		t.Fatalf("picker: %v", err)
	}
	if got, want := cfg.Agents, []string{"spawner-and-hooked"}; !equalStringSlices(got, want) {
		t.Errorf("Agents = %v, want %v (spawner-only must be filtered)", got, want)
	}
}

func TestRunInvestigateConfigPicker_NoSpawnerForReturnsError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, err := investigate.RunInvestigateConfigPicker(ctx, &bytes.Buffer{},
		nil, // SpawnerFor missing
		func(_ context.Context) []types.AgentName { return nil },
	)
	if err == nil {
		t.Fatal("expected error when SpawnerFor is nil")
	}
}

func TestRunInvestigateConfigPicker_QuorumExceedsAgents(t *testing.T) {
	t.Parallel()
	cleanup := investigate.SetPickerFormFnForTest(func(_ context.Context, eligible []investigate.AgentChoice, picks *[]string, maxTurns, quorum *int) error {
		_ = eligible
		*picks = []string{"agent-a"}
		*maxTurns = 3
		*quorum = 5 // > 1 picked
		return nil
	})
	defer cleanup()

	ctx := context.Background()
	_, err := investigate.RunInvestigateConfigPicker(ctx, &bytes.Buffer{},
		func(_ string) spawn.Spawner { return stubSpawner{name: "agent-a"} },
		func(_ context.Context) []types.AgentName { return []types.AgentName{"agent-a"} },
	)
	if err == nil {
		t.Fatal("expected error when quorum exceeds agent count")
	}
	if !strings.Contains(err.Error(), "quorum") {
		t.Errorf("error should mention quorum, got: %v", err)
	}
}
