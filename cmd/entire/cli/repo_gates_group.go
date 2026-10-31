package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// Gate modes: off is a disabled gate, attention a gate that marks a trail as
// needing attention when it fails, block a gate that stops the merge.
const (
	repoGateModeOff       = "off"
	repoGateModeAttention = "attention"
	repoGateModeBlock     = "block"

	repoGateEvalArg = "eval"
)

// repoGatesOpener opens a client for the repository's cell and returns the
// repository id. newTrailAPIClient in production; tests pass a stub server.
type repoGatesOpener func(ctx context.Context, insecureHTTP bool, forge, owner, repo string) (*api.Client, string, error)

func newRepoGatesCmd() *cobra.Command {
	return newRepoGatesCmdWith(func(ctx context.Context, insecureHTTP bool, forge, owner, repo string) (*api.Client, string, error) {
		return newTrailAPIClient(ctx, insecureHTTP, forge, owner, repo)
	})
}

// newRepoGatesCmdWith is `entire repo gates`: the checks a trail must pass
// before it can merge.
func newRepoGatesCmdWith(open repoGatesOpener) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gates",
		Short: "List and set the gates a trail must pass to merge",
		Long: `List and set a repository's gates: the checks Entire runs before a trail
can merge.

Each gate has a mode. "block" stops the merge when the gate fails, "attention"
marks the trail as needing attention but lets it merge, and "off" skips the
gate.

An eval gate checks one agent's eval (its risk, confidence, size, or yes/no
answer) on the trail's latest commit against a threshold. A repository has at
most one eval gate per agent. Changing gates requires admin access to the
repository.

The repository is the current one unless --repo names another.`,
	}
	cmd.PersistentFlags().String("repo", "", "Repository as forge/owner/repo (e.g. gh/acme/app) or a clone URL (default: the current repository)")
	cmd.AddCommand(newRepoGatesListCmd(open))
	cmd.AddCommand(newRepoGatesAddCmd(open))
	cmd.AddCommand(newRepoGatesSetCmd(open))
	cmd.AddCommand(newRepoGatesRemoveCmd(open))
	return requireSubcommand(cmd)
}

type repoGatesTarget struct {
	client *api.Client
	repoID string
	label  string
}

func resolveRepoGatesTarget(cmd *cobra.Command, open repoGatesOpener) (*repoGatesTarget, error) {
	ctx := cmd.Context()
	repoFlag, _ := cmd.Flags().GetString("repo") //nolint:errcheck // registered on the gates group
	forge, owner, repo, err := resolveTrailRepoOrRemote(ctx, strings.TrimSpace(repoFlag))
	if err != nil {
		return nil, err
	}
	client, repoID, err := open(ctx, insecureHTTPRequested(cmd), forge, owner, repo)
	if err != nil {
		return nil, renderDataAPIAuthError(ctx, cmd.ErrOrStderr(), owner+"/"+repo, err)
	}
	label := forge + "/" + owner + "/" + repo
	if repoID == "" {
		return nil, fmt.Errorf("repository %s has no id in Entire", label)
	}
	return &repoGatesTarget{client: client, repoID: repoID, label: label}, nil
}

func repoGateMode(enabled, blocking bool) string {
	switch {
	case !enabled:
		return repoGateModeOff
	case blocking:
		return repoGateModeBlock
	default:
		return repoGateModeAttention
	}
}

func parseRepoGateMode(s string) (enabled, blocking bool, err error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case repoGateModeOff:
		return false, false, nil
	case repoGateModeAttention:
		return true, false, nil
	case repoGateModeBlock:
		return true, true, nil
	}
	return false, false, fmt.Errorf("invalid --mode %q: use off, attention or block", s)
}

// requireEvalArg refuses any gate type but eval for add and remove: the
// other gates always exist and only change mode.
func requireEvalArg(verb, arg string) error {
	if arg == repoGateEvalArg {
		return nil
	}
	return fmt.Errorf("only eval gates can be %s; the other gates always exist, so change their mode with `entire repo gates set <gate> --mode off|attention|block`", verb)
}

// findRepoAgent finds the repository's agent whose id is ref, or whose name
// is ref, ignoring case.
func findRepoAgent(agents []api.RepoAgent, ref string) (api.RepoAgent, error) {
	ref = strings.TrimSpace(ref)
	var byName []api.RepoAgent
	for _, a := range agents {
		if a.ID == ref {
			return a, nil
		}
		if strings.EqualFold(strings.TrimSpace(a.Name), ref) {
			byName = append(byName, a)
		}
	}
	switch len(byName) {
	case 1:
		return byName[0], nil
	case 0:
		return api.RepoAgent{}, fmt.Errorf("no agent named %q runs on this repository; `entire project agent list` lists the project's agents", ref)
	}
	ids := make([]string, len(byName))
	for i, a := range byName {
		ids[i] = a.ID
	}
	return api.RepoAgent{}, fmt.Errorf("%d agents are named %q; pass one id with --agent: %s", len(byName), ref, strings.Join(ids, ", "))
}

// findEvalGate finds the eval gate of the agent whose id is ref, or whose
// name is ref, ignoring case. It matches on the gate's own agent name, so a
// gate whose agent was archived can still be found.
func findEvalGate(gates []api.RepoEvalGate, ref string) (int, error) {
	ref = strings.TrimSpace(ref)
	var byName []int
	for i, g := range gates {
		if g.AgentID == ref {
			return i, nil
		}
		if strings.EqualFold(strings.TrimSpace(evalGateName(g)), ref) {
			byName = append(byName, i)
		}
	}
	switch len(byName) {
	case 1:
		return byName[0], nil
	case 0:
		return -1, fmt.Errorf("no eval gate for agent %q; add one with `entire repo gates add eval --agent <name|id> --threshold <value> --mode attention|block`", ref)
	}
	ids := make([]string, len(byName))
	for i, idx := range byName {
		ids[i] = gates[idx].AgentID
	}
	return -1, fmt.Errorf("%d eval gates are for agents named %q; pass one agent id with --agent: %s", len(byName), ref, strings.Join(ids, ", "))
}

// evalGateName names an eval gate by its agent.
func evalGateName(g api.RepoEvalGate) string {
	if name := strings.TrimSpace(g.AgentName); name != "" {
		return name
	}
	if g.Agent != nil && strings.TrimSpace(g.Agent.Name) != "" {
		return strings.TrimSpace(g.Agent.Name)
	}
	return "agent " + g.AgentID
}

// evalGateThreshold checks that an agent's eval can gate a merge and that
// raw fits it, and returns the threshold to send: a whole percent from 0 to
// 100, small, medium or large, or yes or no.
func evalGateThreshold(agent string, sig api.EvalSignal, raw string) (json.RawMessage, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	var v any
	switch sig.ValueType {
	case trailEvalBoolean:
		if s != trailEvalYes && s != trailEvalNo {
			return nil, fmt.Errorf("%s gives a yes/no answer: --threshold must be yes or no, the answer the gate requires", agent)
		}
		v = s
	case trailEvalPercent, trailEvalSize:
		if sig.Polarity != trailEvalHigherIsBetter && sig.Polarity != trailEvalLowerIsBetter {
			return nil, fmt.Errorf("%s's eval is neutral: it has no better direction, so it can't gate a merge", agent)
		}
		if sig.ValueType == trailEvalSize {
			if s != "small" && s != "medium" && s != "large" {
				return nil, fmt.Errorf("%s reports a size: --threshold must be small, medium or large", agent)
			}
			v = s
			break
		}
		n, err := strconv.Atoi(strings.TrimSuffix(s, "%"))
		if err != nil || n < 0 || n > 100 {
			return nil, fmt.Errorf("%s reports a percent: --threshold must be a whole number from 0 to 100", agent)
		}
		v = n
	default:
		return nil, fmt.Errorf("%s reports an eval of an unknown type %q, so it can't gate a merge", agent, sig.ValueType)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode threshold: %w", err)
	}
	return out, nil
}

// keepEvalGate is the replace entry that leaves a gate as it is: the same
// threshold and no signal, so it keeps the eval it was set against.
func keepEvalGate(g api.RepoEvalGate) api.RepoEvalGateInput {
	return api.RepoEvalGateInput{AgentID: g.AgentID, Threshold: g.Threshold, Enabled: g.Enabled, Blocking: g.Blocking}
}

// editRepoEvalGates reads the eval gate set, applies change, and saves the
// result under the read's ETag. When the set changed in between (412, or 409
// for a gate created meanwhile), it says so, reads again and applies change
// to the new set once more.
func editRepoEvalGates(ctx context.Context, errW io.Writer, t *repoGatesTarget, change func([]api.RepoEvalGate) ([]api.RepoEvalGateInput, error)) (*api.RepoEvalGates, error) {
	for attempt := 0; ; attempt++ {
		current, err := t.client.RepoEvalGates(ctx, t.repoID)
		if err != nil {
			return nil, fmt.Errorf("read eval gates: %w", err)
		}
		items, err := change(current.Items)
		if err != nil {
			return nil, err
		}
		saved, err := t.client.ReplaceRepoEvalGates(ctx, t.repoID, current.ETag, items)
		if err == nil {
			return saved, nil
		}
		if !api.IsHTTPErrorStatus(err, http.StatusPreconditionFailed) && !api.IsHTTPErrorStatus(err, http.StatusConflict) {
			return nil, fmt.Errorf("save eval gates: %w", err)
		}
		if attempt > 0 {
			return nil, fmt.Errorf("the eval gates changed again while saving; run the command again: %w", err)
		}
		fmt.Fprintln(errW, "The eval gates changed while saving; reading them again.")
	}
}

// repoEvalGatesJSON is the --json view of an eval gate write: the set as
// saved, each gate as the server sent it.
type repoEvalGatesJSON struct {
	Repo      string            `json:"repo"`
	EvalGates []json.RawMessage `json:"eval_gates"`
}

// printEvalGateResult prints line, or the whole saved set for --json.
func printEvalGateResult(cmd *cobra.Command, t *repoGatesTarget, saved *api.RepoEvalGates, line string) error {
	if jsonRequested(cmd) {
		return printJSON(cmd.OutOrStdout(), repoEvalGatesJSON{Repo: t.label, EvalGates: saved.Raw})
	}
	fmt.Fprintln(cmd.OutOrStdout(), line)
	return nil
}

// savedEvalGateSummary describes agentID's gate as saved, or only names the
// agent when the response leaves the gate out.
func savedEvalGateSummary(saved *api.RepoEvalGates, agentID, name string) string {
	for _, g := range saved.Items {
		if g.AgentID == agentID {
			return evalGateSummary(g)
		}
	}
	return tuiutil.SanitizeDisplayText(name)
}

// evalGateSummary is "Risk review, block, must be ≤ 40%".
func evalGateSummary(g api.RepoEvalGate) string {
	parts := []string{tuiutil.SanitizeDisplayText(evalGateName(g)), repoGateMode(g.Enabled, g.Blocking)}
	if req := evalGateRequirement(g); req != "" {
		parts = append(parts, "must be "+tuiutil.SanitizeDisplayText(req))
	}
	return strings.Join(parts, ", ")
}

// evalGateRequirement is what the agent's reading must be for the gate to
// pass, read against the eval the threshold was set against.
func evalGateRequirement(g api.RepoEvalGate) string {
	sig := g.Signal
	if sig == nil && g.Agent != nil {
		sig = &g.Agent.Signal
	}
	if sig == nil {
		return ""
	}
	return trailEvalRequirement(api.TrailEvalGateValue{ValueType: sig.ValueType, Polarity: sig.Polarity, Threshold: g.Threshold})
}
