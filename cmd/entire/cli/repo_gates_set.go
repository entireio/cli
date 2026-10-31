package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

func newRepoGatesSetCmd(open repoGatesOpener) *cobra.Command {
	var agentRef, threshold, mode string
	cmd := &cobra.Command{
		Use:   "set <gate|eval> [--agent <name|id>] [--threshold <value>] [--mode off|attention|block]",
		Short: "Change a gate's mode, or an eval gate's threshold",
		Long: `Change a gate's mode: entire repo gates set approvals --mode block.

For an eval gate, name its agent: entire repo gates set eval --agent "Risk
review" --threshold 30. --threshold takes the same values as for add. Setting
the threshold again also makes a gate whose agent's eval changed enforced
again.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key := strings.TrimSpace(args[0])
			if key == repoGateEvalArg {
				return runRepoGatesSetEval(cmd, open, agentRef, threshold, mode)
			}
			if agentRef != "" || threshold != "" {
				return errors.New("--agent and --threshold apply to eval gates only: entire repo gates set eval --agent <name|id>")
			}
			if strings.TrimSpace(mode) == "" {
				return errors.New("--mode is required")
			}
			return runRepoGatesSetMode(cmd, open, key, mode)
		},
	}
	cmd.Flags().StringVar(&agentRef, "agent", "", "For an eval gate: its agent, by name or id")
	cmd.Flags().StringVar(&threshold, "threshold", "", "For an eval gate: a percent (0-100), small/medium/large, or yes/no")
	cmd.Flags().StringVar(&mode, "mode", "", "off, attention, or block")
	addJSONFlag(cmd)
	return cmd
}

func runRepoGatesSetEval(cmd *cobra.Command, open repoGatesOpener, agentRef, threshold, mode string) error {
	if strings.TrimSpace(agentRef) == "" {
		return errors.New("--agent is required for an eval gate")
	}
	if strings.TrimSpace(threshold) == "" && strings.TrimSpace(mode) == "" {
		return errors.New("pass --threshold, --mode, or both")
	}
	var enabled, blocking bool
	if mode != "" {
		var err error
		if enabled, blocking, err = parseRepoGateMode(mode); err != nil {
			return err
		}
	}
	t, err := resolveRepoGatesTarget(cmd, open)
	if err != nil {
		return err
	}
	var agentID, name string
	saved, err := editRepoEvalGates(cmd.Context(), cmd.ErrOrStderr(), t, func(gates []api.RepoEvalGate) ([]api.RepoEvalGateInput, error) {
		i, err := findEvalGate(gates, agentRef)
		if err != nil {
			return nil, err
		}
		g := gates[i]
		agentID, name = g.AgentID, evalGateName(g)
		next := keepEvalGate(g)
		if mode != "" {
			next.Enabled = enabled
			if enabled {
				next.Blocking = blocking
			}
		}
		if threshold != "" {
			if g.Agent == nil {
				return nil, fmt.Errorf("%s no longer runs on this repository, so its threshold can't be set; remove the gate with `entire repo gates remove eval --agent %s`", name, g.AgentID)
			}
			signal := g.Agent.Signal
			if next.Threshold, err = evalGateThreshold(name, signal, threshold); err != nil {
				return nil, err
			}
			next.Signal = &signal
		}
		items := make([]api.RepoEvalGateInput, len(gates))
		for j, other := range gates {
			items[j] = keepEvalGate(other)
		}
		items[i] = next
		return items, nil
	})
	if err != nil {
		return err
	}
	return printEvalGateResult(cmd, t, saved,
		fmt.Sprintf("Set eval gate on %s: %s", t.label, savedEvalGateSummary(saved, agentID, name)))
}

// runRepoGatesSetMode changes one gate's mode. The PATCH sends only enabled
// and blocking, so it writes back nothing read here and needs no If-Match.
func runRepoGatesSetMode(cmd *cobra.Command, open repoGatesOpener, key, mode string) error {
	enabled, blocking, err := parseRepoGateMode(mode)
	if err != nil {
		return err
	}
	t, err := resolveRepoGatesTarget(cmd, open)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	cfg, err := t.client.RepoGateConfig(ctx, t.repoID)
	if err != nil {
		return fmt.Errorf("read gates: %w", err)
	}
	var gate *api.RepoGateDefinition
	var keys []string
	for i, g := range cfg.Gates {
		if g.GateType == api.TrailGateTypeEval {
			continue
		}
		keys = append(keys, g.GateKey)
		if g.GateKey == key {
			gate = &cfg.Gates[i]
		}
	}
	if gate == nil {
		return fmt.Errorf("no gate %q on %s; its gates are: %s (eval gates: entire repo gates set eval --agent <name|id>)",
			tuiutil.SanitizeDisplayText(key), t.label, tuiutil.SanitizeDisplayText(strings.Join(keys, ", ")))
	}
	patch := api.RepoGatePatch{Enabled: &enabled}
	if enabled {
		patch.Blocking = &blocking
	}
	raw, err := t.client.PatchRepoGate(ctx, t.repoID, gate.ID, patch)
	if err != nil {
		return fmt.Errorf("set gate %s: %w", tuiutil.SanitizeDisplayText(key), err)
	}
	if jsonRequested(cmd) {
		return printJSON(cmd.OutOrStdout(), struct {
			Repo string          `json:"repo"`
			Gate json.RawMessage `json:"gate"`
		}{t.label, raw})
	}
	var updated api.RepoGateDefinition
	if json.Unmarshal(raw, &updated) != nil || updated.GateKey == "" {
		updated = api.RepoGateDefinition{GateKey: key, Enabled: enabled, Blocking: blocking}
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Set gate %s on %s: %s\n",
		tuiutil.SanitizeDisplayText(updated.GateKey), t.label, repoGateMode(updated.Enabled, updated.Blocking))
	return nil
}
