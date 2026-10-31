package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

const (
	gatesTestRepoID = "01JREPO0000000000000000000"
	gatesTestPath   = "/api/v1/repos/" + gatesTestRepoID + "/trails/gates"
	gatesTestAgents = "/api/v1/repos/" + gatesTestRepoID + "/agents"

	gatesTestConfig = `{"gatesEnabled":true,"gates":[
		{"id":"d1","gateKey":"approvals","gateType":"approvals","blocking":true,"enabled":true,"config":{}},
		{"id":"d2","gateKey":"findings","gateType":"findings","blocking":false,"enabled":true,"config":{}},
		{"id":"d3","gateKey":"up_to_date","gateType":"up_to_date","blocking":false,"enabled":false,"config":{}},
		{"id":"d4","gateKey":"A1","gateType":"eval","blocking":true,"enabled":true,"config":{"agent_id":"A1"}}]}`

	gatesTestRisk = `{"id":"d4","agent_id":"A1","agent_name":"Risk review","threshold":40,"enabled":true,"blocking":true,
		"signal":{"value_type":"percent","polarity":"lower_is_better"},
		"agent":{"name":"Risk review","is_enabled":true,"signal":{"value_type":"percent","polarity":"lower_is_better"}},
		"configuration_issue":null}`
	gatesTestConfidence = `{"id":"d5","agent_id":"A2","agent_name":"Confidence","threshold":70,"enabled":true,"blocking":false,
		"signal":{"value_type":"percent","polarity":"higher_is_better"},
		"agent":{"name":"Confidence","is_enabled":false,"signal":{"value_type":"percent","polarity":"higher_is_better"}},
		"configuration_issue":{"code":"agent_paused","message":"Not enforced: Confidence is paused."}}`
	gatesTestArchived = `{"id":"d6","agent_id":"A3","agent_name":"Old review","threshold":"medium","enabled":true,"blocking":true,
		"signal":{"value_type":"size","polarity":"lower_is_better"},"agent":null,
		"configuration_issue":{"code":"agent_archived","message":"Not enforced: Old review was archived."}}`

	gatesTestAgentList = `{"items":[
		{"id":"A1","name":"Risk review","is_enabled":true,"signal":{"value_type":"percent","polarity":"lower_is_better"}},
		{"id":"A4","name":"Size","is_enabled":true,"signal":{"value_type":"size","polarity":"lower_is_better"}},
		{"id":"A5","name":"Tests added","is_enabled":true,"signal":{"value_type":"boolean","polarity":"neutral"}},
		{"id":"A6","name":"Mood","is_enabled":true,"signal":{"value_type":"percent","polarity":"neutral"}}]}`
)

type gatesTestPut struct {
	ifMatch string
	items   []map[string]any
}

// gatesStub serves the gate routes. Each GET of the eval gates returns the
// next of evalSets (the last one repeats) with ETag "e<n>"; each PUT answers
// the next of putStatuses (200 once they run out) with the set it was sent.
type gatesStub struct {
	t           *testing.T
	config      string
	evalSets    [][]string
	putStatuses []int

	mu        sync.Mutex
	evalReads int
	puts      []gatesTestPut
	patches   []string
	requests  []string
}

func (s *gatesStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.t.Error(err)
	}
	switch r.Method + " " + r.URL.Path {
	case "GET " + gatesTestPath:
		fmt.Fprint(w, s.config)
	case "GET " + gatesTestAgents:
		fmt.Fprint(w, gatesTestAgentList)
	case "GET " + gatesTestPath + "/evals":
		set := s.evalSets[min(s.evalReads, len(s.evalSets)-1)]
		s.evalReads++
		w.Header().Set("ETag", fmt.Sprintf(`W/"e%d"`, s.evalReads))
		fmt.Fprintf(w, `{"items":[%s]}`, strings.Join(set, ","))
	case "PUT " + gatesTestPath + "/evals":
		var sent struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(body, &sent); err != nil {
			s.t.Error(err)
		}
		s.puts = append(s.puts, gatesTestPut{ifMatch: r.Header.Get("If-Match"), items: sent.Items})
		if n := len(s.puts) - 1; n < len(s.putStatuses) && s.putStatuses[n] != 0 {
			w.WriteHeader(s.putStatuses[n])
			fmt.Fprint(w, `{"detail":"If-Match does not match the repository's current eval gates"}`)
			return
		}
		var echo struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(body, &echo); err != nil {
			s.t.Error(err)
		}
		saved := make([]string, len(echo.Items))
		for i, item := range echo.Items {
			item["agent_name"] = "Agent " + fmt.Sprint(item["agent_id"])
			if item["agent_id"] == "A1" {
				item["agent_name"] = "Risk review"
				item["signal"] = map[string]any{"value_type": "percent", "polarity": "lower_is_better"}
			}
			encoded, err := json.Marshal(item)
			if err != nil {
				s.t.Error(err)
			}
			saved[i] = string(encoded)
		}
		w.Header().Set("ETag", `W/"saved"`)
		fmt.Fprintf(w, `{"items":[%s]}`, strings.Join(saved, ","))
	default:
		if r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, gatesTestPath+"/") {
			s.patches = append(s.patches, strings.TrimPrefix(r.URL.Path, gatesTestPath+"/")+" "+string(body))
			fmt.Fprint(w, `{"gate":{"id":"d1","gateKey":"approvals","gateType":"approvals","blocking":false,"enabled":true}}`)
			return
		}
		s.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *gatesStub) record() (puts []gatesTestPut, patches, requests []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]gatesTestPut(nil), s.puts...), append([]string(nil), s.patches...), append([]string(nil), s.requests...)
}

func runRepoGates(t *testing.T, stub *gatesStub, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	cmd := newRepoGatesCmdWith(func(context.Context, bool, string, string, string) (*api.Client, string, error) {
		return api.NewClientWithBaseURL("token", srv.URL), gatesTestRepoID, nil
	})
	cmd.SetContext(t.Context())
	cmd.SetArgs(append(args, "--repo", "gh/acme/app"))
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	err = cmd.Execute()
	return out.String(), errOut.String(), err
}

func TestRepoGatesList_ShowsModesAndEvalGates(t *testing.T) {
	t.Parallel()
	stub := &gatesStub{t: t, config: gatesTestConfig, evalSets: [][]string{{gatesTestRisk, gatesTestConfidence, gatesTestArchived}}}
	out, _, err := runRepoGates(t, stub, "list")
	require.NoError(t, err)

	for _, want := range []string{
		"Gates for gh/acme/app",
		"approvals   approvals   block",
		"findings    findings    attention",
		"up_to_date  up_to_date  off",
		"Risk review  block      ≤ 40%",
		"Confidence   attention  ≥ 70%",
		"not enforced: Confidence is paused.",
		"Old review   block      ≤ medium",
		"not enforced: Old review was archived.",
	} {
		require.Contains(t, out, want)
	}
	require.NotContains(t, out, "A1", "an eval gate is named by its agent, not listed as a gate key")
	require.NotContains(t, out, "turned off")
}

func TestRepoGatesList_SaysWhenGatesAreOff(t *testing.T) {
	t.Parallel()
	stub := &gatesStub{t: t, config: `{"gatesEnabled":false,"gates":[{"id":"d1","gateKey":"approvals","gateType":"approvals","blocking":true,"enabled":true}]}`}
	out, _, err := runRepoGates(t, stub, "list")
	require.NoError(t, err)
	require.Contains(t, out, "Gates are turned off for this repository")
	require.Contains(t, out, "No eval gates.")
	_, _, requests := stub.record()
	require.NotContains(t, requests, "GET "+gatesTestPath+"/evals", "without an eval gate the evals route is never read")
}

func TestRepoGatesList_JSONPassesGatesThrough(t *testing.T) {
	t.Parallel()
	stub := &gatesStub{t: t, config: gatesTestConfig, evalSets: [][]string{{gatesTestRisk}}}
	out, _, err := runRepoGates(t, stub, "list", "--json")
	require.NoError(t, err)

	var got struct {
		Repo         string           `json:"repo"`
		GatesEnabled bool             `json:"gates_enabled"`
		Gates        []map[string]any `json:"gates"`
		EvalGates    []map[string]any `json:"eval_gates"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Equal(t, "gh/acme/app", got.Repo)
	require.True(t, got.GatesEnabled)
	require.Len(t, got.Gates, 4)
	require.Equal(t, map[string]any{}, got.Gates[0]["config"], "each gate is passed through as sent")
	require.Len(t, got.EvalGates, 1)
	require.InDelta(t, 40, got.EvalGates[0]["threshold"], 0)
	require.Equal(t, map[string]any{"name": "Risk review", "is_enabled": true,
		"signal": map[string]any{"value_type": "percent", "polarity": "lower_is_better"}}, got.EvalGates[0]["agent"])
}

func TestRepoGatesAddEval_SendsWholeSetWithNewEntry(t *testing.T) {
	t.Parallel()
	stub := &gatesStub{t: t, evalSets: [][]string{{gatesTestRisk}}}
	out, _, err := runRepoGates(t, stub, "add", "eval", "--agent", "size", "--threshold", "Medium", "--mode", "attention")
	require.NoError(t, err)

	puts, _, _ := stub.record()
	require.Len(t, puts, 1)
	require.Equal(t, `W/"e1"`, puts[0].ifMatch)
	require.Equal(t, []map[string]any{
		{"agent_id": "A1", "threshold": float64(40), "enabled": true, "blocking": true},
		{"agent_id": "A4", "threshold": "medium", "enabled": true, "blocking": false,
			"signal": map[string]any{"value_type": "size", "polarity": "lower_is_better"}},
	}, puts[0].items, "the other gate is sent unchanged and without a signal; the new one confirms the agent's eval")
	require.Contains(t, out, "Added eval gate on gh/acme/app: Agent A4, attention")
}

func TestRepoGatesAddEval_RefusesBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, agent, threshold, want string
	}{
		{"percent takes no size", "Risk review", "medium", "must be a whole number from 0 to 100"},
		{"percent out of range", "A1", "101", "must be a whole number from 0 to 100"},
		{"size takes no percent", "Size", "40", "must be small, medium or large"},
		{"yes/no takes no percent", "Tests added", "40", "must be yes or no"},
		{"neutral percent", "Mood", "40", "neutral"},
		{"unknown agent", "Nobody", "40", `no agent named "Nobody"`},
		{"agent already gated", "Risk review", "30", "already has an eval gate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := &gatesStub{t: t, evalSets: [][]string{{gatesTestRisk}}}
			_, _, err := runRepoGates(t, stub, "add", "eval", "--agent", tc.agent, "--threshold", tc.threshold, "--mode", "block")
			require.ErrorContains(t, err, tc.want)
			puts, _, _ := stub.record()
			require.Empty(t, puts)
		})
	}
}

func TestRepoGatesAddEval_YesNoThreshold(t *testing.T) {
	t.Parallel()
	stub := &gatesStub{t: t, evalSets: [][]string{{}}}
	_, _, err := runRepoGates(t, stub, "add", "eval", "--agent", "A5", "--threshold", "yes", "--mode", "block")
	require.NoError(t, err)
	puts, _, _ := stub.record()
	require.Len(t, puts, 1)
	require.Equal(t, "yes", puts[0].items[0]["threshold"])
}

func TestRepoGatesAddEval_RereadsOnceWhenTheSetChanged(t *testing.T) {
	t.Parallel()
	stub := &gatesStub{
		t:           t,
		evalSets:    [][]string{{}, {gatesTestConfidence}},
		putStatuses: []int{http.StatusPreconditionFailed},
	}
	_, errOut, err := runRepoGates(t, stub, "add", "eval", "--agent", "Risk review", "--threshold", "40%", "--mode", "block")
	require.NoError(t, err)
	require.Contains(t, errOut, "The eval gates changed while saving; reading them again.")

	puts, _, _ := stub.record()
	require.Len(t, puts, 2)
	require.Equal(t, `W/"e2"`, puts[1].ifMatch, "the retry is guarded by the new read")
	require.Len(t, puts[1].items, 2, "the gate added meanwhile is kept")
	require.Equal(t, "A2", puts[1].items[0]["agent_id"])
}

func TestRepoGatesAddEval_GivesUpAfterASecondConflict(t *testing.T) {
	t.Parallel()
	stub := &gatesStub{
		t:           t,
		evalSets:    [][]string{{}},
		putStatuses: []int{http.StatusPreconditionFailed, http.StatusConflict},
	}
	_, _, err := runRepoGates(t, stub, "add", "eval", "--agent", "A1", "--threshold", "40", "--mode", "block")
	require.ErrorContains(t, err, "changed again while saving")
	puts, _, _ := stub.record()
	require.Len(t, puts, 2)
}

func TestRepoGatesSetEval_Threshold(t *testing.T) {
	t.Parallel()
	stub := &gatesStub{t: t, evalSets: [][]string{{gatesTestRisk, gatesTestConfidence}}}
	_, _, err := runRepoGates(t, stub, "set", "eval", "--agent", "risk review", "--threshold", "30")
	require.NoError(t, err)

	puts, _, _ := stub.record()
	require.Len(t, puts, 1)
	require.Equal(t, []map[string]any{
		{"agent_id": "A1", "threshold": float64(30), "enabled": true, "blocking": true,
			"signal": map[string]any{"value_type": "percent", "polarity": "lower_is_better"}},
		{"agent_id": "A2", "threshold": float64(70), "enabled": true, "blocking": false},
	}, puts[0].items)
}

func TestRepoGatesSetEval_ModeKeepsThreshold(t *testing.T) {
	t.Parallel()
	stub := &gatesStub{t: t, evalSets: [][]string{{gatesTestRisk}}}
	out, _, err := runRepoGates(t, stub, "set", "eval", "--agent", "A1", "--mode", "attention")
	require.NoError(t, err)

	puts, _, _ := stub.record()
	require.Equal(t, []map[string]any{{"agent_id": "A1", "threshold": float64(40), "enabled": true, "blocking": false}}, puts[0].items,
		"a mode change sends no signal, so the gate keeps the eval it was set against")
	require.Contains(t, out, "Set eval gate on gh/acme/app: Risk review, attention, must be ≤ 40%")
}

func TestRepoGatesSetEval_ThresholdNeedsAnActiveAgent(t *testing.T) {
	t.Parallel()
	stub := &gatesStub{t: t, evalSets: [][]string{{gatesTestArchived}}}
	_, _, err := runRepoGates(t, stub, "set", "eval", "--agent", "Old review", "--threshold", "small")
	require.ErrorContains(t, err, "no longer runs on this repository")
	puts, _, _ := stub.record()
	require.Empty(t, puts)
}

func TestRepoGatesRemoveEval_DropsTheEntry(t *testing.T) {
	t.Parallel()
	stub := &gatesStub{t: t, evalSets: [][]string{{gatesTestRisk, gatesTestArchived}}}
	out, _, err := runRepoGates(t, stub, "remove", "eval", "--agent", "Old review")
	require.NoError(t, err)

	puts, _, _ := stub.record()
	require.Len(t, puts, 1)
	require.Len(t, puts[0].items, 1)
	require.Equal(t, "A1", puts[0].items[0]["agent_id"])
	require.Contains(t, out, "Removed the eval gate for Old review from gh/acme/app")
}

func TestRepoGatesSetMode_PatchesOnlyTheMode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ mode, body string }{
		{"attention", `{"enabled":true,"blocking":false}`},
		{"block", `{"enabled":true,"blocking":true}`},
		{"off", `{"enabled":false}`},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			t.Parallel()
			stub := &gatesStub{t: t, config: gatesTestConfig}
			_, _, err := runRepoGates(t, stub, "set", "approvals", "--mode", tc.mode)
			require.NoError(t, err)
			_, patches, _ := stub.record()
			require.Equal(t, []string{"d1 " + tc.body}, patches)
		})
	}
}

func TestRepoGatesSet_Refusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"unknown gate", []string{"set", "reviews", "--mode", "block"}, `no gate "reviews"`},
		{"eval gate by key", []string{"set", "A1", "--mode", "block"}, `no gate "A1"`},
		{"bad mode", []string{"set", "approvals", "--mode", "loud"}, "use off, attention or block"},
		{"threshold on a plain gate", []string{"set", "approvals", "--threshold", "3"}, "eval gates only"},
		{"eval without a change", []string{"set", "eval", "--agent", "A1"}, "pass --threshold, --mode, or both"},
		{"add a plain gate", []string{"add", "approvals"}, "only eval gates can be added"},
		{"remove a plain gate", []string{"remove", "findings"}, "only eval gates can be removed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := &gatesStub{t: t, config: gatesTestConfig}
			_, _, err := runRepoGates(t, stub, tc.args...)
			require.ErrorContains(t, err, tc.want)
			puts, patches, _ := stub.record()
			require.Empty(t, puts)
			require.Empty(t, patches)
		})
	}
}

func TestFindRepoAgent_RefusesAnAmbiguousName(t *testing.T) {
	t.Parallel()
	agents := []api.RepoAgent{{ID: "A1", Name: "Risk"}, {ID: "A2", Name: "risk"}}
	_, err := findRepoAgent(agents, "RISK")
	require.ErrorContains(t, err, "2 agents are named")
	require.ErrorContains(t, err, "A1, A2")

	got, err := findRepoAgent(agents, "A2")
	require.NoError(t, err)
	require.Equal(t, "A2", got.ID)
}
