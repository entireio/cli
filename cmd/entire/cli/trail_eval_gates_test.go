package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/stretchr/testify/require"
)

// trailEvalGatesWireJSON is a mergeability snapshot in entire-api's wire
// shape with one eval gate in each state, beside two other gates.
const trailEvalGatesWireJSON = `{
  "mergeable": false,
  "conflict_status": "clean",
  "head_sha": "b1205a52b53b2206cf168a62c0c08b4e37e4449f",
  "checks": {"availability": "not_applicable", "runs": []},
  "gates": [
    {"gate_key": "approvals", "gate_type": "approvals", "blocking": true, "status": "passed", "rationale": null, "value": null, "created_at": "2026-10-30T10:00:00Z"},
    {"gate_key": "findings", "gate_type": "findings", "blocking": false, "status": "failed", "rationale": "advisory findings open", "value": null, "created_at": "2026-10-30T10:00:00Z"},
    {"gate_key": "01K8EVALPASSED0000000000AA", "gate_type": "eval", "blocking": true, "status": "passed",
     "rationale": "Code quality is 85%; the gate needs 70% or higher.", "created_at": "2026-10-30T10:00:00Z",
     "value": {"agent_id": "01K8EVALPASSED0000000000AA", "agent_name": "Code quality", "agent_version": 3, "value_type": "percent",
       "polarity": "higher_is_better", "threshold": 70, "value": 85, "run_id": "01K8RUN0000000000000000001", "configuration_issue": null}},
    {"gate_key": "01K8EVALBLOCKS0000000000AA", "gate_type": "eval", "blocking": true, "status": "failed",
     "rationale": "Risk review is 72%; the gate needs 40% or lower.", "created_at": "2026-10-30T10:00:00Z",
     "value": {"agent_id": "01K8EVALBLOCKS0000000000AA", "agent_name": "Risk review", "agent_version": 1, "value_type": "percent",
       "polarity": "lower_is_better", "threshold": 40, "value": 72, "run_id": "01K8RUN0000000000000000002", "configuration_issue": null}},
    {"gate_key": "01K8EVALATTENTION000000AAA", "gate_type": "eval", "blocking": false, "status": "failed",
     "rationale": "Change size is large; the gate needs medium or smaller.", "created_at": "2026-10-30T10:00:00Z",
     "value": {"agent_id": "01K8EVALATTENTION000000AAA", "agent_name": "Change size", "agent_version": 2, "value_type": "size",
       "polarity": "lower_is_better", "threshold": "medium", "value": "large", "run_id": "01K8RUN0000000000000000003", "configuration_issue": null}},
    {"gate_key": "01K8EVALYESNO00000000000AA", "gate_type": "eval", "blocking": true, "status": "failed",
     "rationale": "Docs present is no; the gate needs yes.", "created_at": "2026-10-30T10:00:00Z",
     "value": {"agent_id": "01K8EVALYESNO00000000000AA", "agent_name": "Docs present", "agent_version": 1, "value_type": "boolean",
       "polarity": "higher_is_better", "threshold": null, "value": false, "run_id": "01K8RUN0000000000000000004", "configuration_issue": null}},
    {"gate_key": "01K8EVALPENDING000000000AA", "gate_type": "eval", "blocking": true, "status": "pending",
     "rationale": "Waiting for Security review to finish on this commit.", "created_at": "2026-10-30T10:00:00Z",
     "value": {"agent_id": "01K8EVALPENDING000000000AA", "agent_name": "Security review", "agent_version": 1, "value_type": "percent",
       "polarity": "lower_is_better", "threshold": 20, "value": null, "run_id": "01K8RUN0000000000000000005", "configuration_issue": null}},
    {"gate_key": "01K8EVALERROR0000000000AAA", "gate_type": "eval", "blocking": false, "status": "error",
     "rationale": "Test plan's run on this commit failed.", "created_at": "2026-10-30T10:00:00Z",
     "value": {"agent_id": "01K8EVALERROR0000000000AAA", "agent_name": "Test plan", "agent_version": 1, "value_type": "boolean",
       "polarity": "lower_is_better", "threshold": null, "value": null, "run_id": "01K8RUN0000000000000000006", "configuration_issue": null}},
    {"gate_key": "01K8EVALPAUSED0000000000AA", "gate_type": "eval", "blocking": true, "status": "skipped",
     "rationale": "Not enforced: Style review is paused.", "created_at": "2026-10-30T10:00:00Z",
     "value": {"agent_id": "01K8EVALPAUSED0000000000AA", "agent_name": "Style review", "agent_version": 4, "value_type": "percent",
       "polarity": "lower_is_better", "threshold": 30, "value": null, "run_id": null,
       "configuration_issue": {"code": "agent_paused", "message": "Not enforced: Style review is paused."}}}
  ]
}`

func TestPrintTrailMergeabilityEvalGates(t *testing.T) {
	t.Parallel()

	var mg api.TrailMergeability
	require.NoError(t, json.Unmarshal([]byte(trailEvalGatesWireJSON), &mg))
	var out bytes.Buffer
	printTrailMergeability(&out, newStatusStyles(&out), func(s string) string { return s }, &mg)

	require.Equal(t, `  Mergeable: no
  Head:      b1205a52
  Conflicts: clean
  Gates:
    ✓ approvals  passed
    ✗ findings   failed  advisory findings open (non-blocking)
    Evals:
      ✓ Code quality     85%      must be ≥ 70%     passed
      ✗ Risk review      72%      must be ≤ 40%     blocks merge
      ✗ Change size      large    must be ≤ medium  needs attention
      ✗ Docs present     no       must be yes       blocks merge
      … Security review  pending  Waiting for Security review to finish on this commit.
      ✗ Test plan        error    Test plan's run on this commit failed. (non-blocking)
      - Style review     not enforced: Style review is paused.
  Needs attention:
    - findings: advisory findings open
    - Change size is large; the gate needs medium or smaller.
    - Test plan's run on this commit failed.
  Checks:    not applicable
`, out.String())
}

// Only eval gates make an Evals group, and only non-blocking failures make a
// Needs attention list.
func TestPrintTrailMergeabilityWithoutEvalGates(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	printTrailMergeability(&out, newStatusStyles(&out), func(s string) string { return s }, &api.TrailMergeability{
		Mergeable: true, ConflictStatus: "clean", Checks: api.TrailChecks{Availability: "not_applicable"},
		Gates: []api.TrailGate{{GateKey: "approvals", GateType: "approvals", Status: "passed", Blocking: true}},
	})
	require.NotContains(t, out.String(), "Evals:")
	require.NotContains(t, out.String(), "Needs attention:")
}

func TestTrailEvalGateFallbacks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		gate api.TrailGate
		want string
	}{
		{
			name: "no value names the gate by its key",
			gate: api.TrailGate{GateKey: "01K8AGENT", GateType: "eval", Blocking: true, Status: "pending"},
			want: "      … 01K8AGENT  pending",
		},
		{
			name: "unknown polarity leaves out the threshold",
			gate: api.TrailGate{GateKey: "01K8AGENT", GateType: "eval", Blocking: true, Status: "failed",
				Value: json.RawMessage(`{"agent_name": "Tone", "value_type": "percent", "polarity": "neutral", "threshold": 40, "value": 72}`)},
			want: "      ✗ Tone  72%  blocks merge",
		},
		{
			name: "a reading of the wrong type prints like other gates",
			gate: api.TrailGate{GateKey: "01K8AGENT", GateType: "eval", Blocking: true, Status: "failed", Rationale: new("Tone failed."),
				Value: json.RawMessage(`{"agent_name": "Tone", "value_type": "percent", "polarity": "lower_is_better", "threshold": 40, "value": "high"}`)},
			want: "      ✗ Tone  failed  Tone failed.",
		},
		{
			name: "a fractional percent keeps its digits",
			gate: api.TrailGate{GateKey: "01K8AGENT", GateType: "eval", Blocking: false, Status: "passed",
				Value: json.RawMessage(`{"agent_name": "Tone", "value_type": "percent", "polarity": "lower_is_better", "threshold": 40, "value": 12.5}`)},
			want: "      ✓ Tone  12.5%  must be ≤ 40%  passed",
		},
		{
			name: "an issue message without the lead-in prints whole",
			gate: api.TrailGate{GateKey: "01K8AGENT", GateType: "eval", Blocking: true, Status: "skipped",
				Value: json.RawMessage(`{"agent_name": "Tone", "configuration_issue": {"code": "repo_not_selected", "message": "Tone no longer runs on this repository."}}`)},
			want: "      - Tone  not enforced: Tone no longer runs on this repository.",
		},
		{
			name: "an issue without a message falls back to its code",
			gate: api.TrailGate{GateKey: "01K8AGENT", GateType: "eval", Blocking: true, Status: "skipped",
				Value: json.RawMessage(`{"agent_name": "Tone", "configuration_issue": {"code": "signal_changed", "message": ""}}`)},
			want: "      - Tone  not enforced: signal_changed",
		},
		{
			name: "an issue on a gate that is not skipped is not shown as not enforced",
			gate: api.TrailGate{GateKey: "01K8AGENT", GateType: "eval", Blocking: true, Status: "pending",
				Value: json.RawMessage(`{"agent_name": "Tone", "configuration_issue": {"code": "agent_paused", "message": "Tone is paused."}}`)},
			want: "      … Tone  pending",
		},
		{
			name: "server text is sanitized",
			gate: api.TrailGate{GateKey: "01K8AGENT", GateType: "eval", Blocking: true, Status: "failed",
				Value: json.RawMessage(`{"agent_name": "Tone\u001b[31m\nx", "value_type": "size", "polarity": "higher_is_better", "threshold": "medium\r", "value": "small\u0007"}`)},
			want: "      ✗ Tone x  small  must be ≥ medium  blocks merge",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer
			printTrailEvalGates(&out, newStatusStyles(&out), []api.TrailGate{tt.gate})
			require.Equal(t, tt.want+"\n", out.String())
		})
	}
}

// `trail show --json` passes an eval gate's value through untouched, so the
// agent, threshold, reading, run and configuration issue all reach the caller.
func TestRunTrailShowJSONPassesEvalGateValueThrough(t *testing.T) {
	t.Parallel()

	srv := trailDetailServerWithMergeability(t, trailEvalGatesWireJSON)
	out, errOut := runTrailShowForMergeabilityTest(t, srv, "7", true)
	require.Empty(t, errOut)

	var got struct {
		Mergeability *trailMergeabilityJSON `json:"mergeability"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.NotNil(t, got.Mergeability)

	var wire struct {
		Gates []struct {
			Value json.RawMessage `json:"value"`
		} `json:"gates"`
	}
	require.NoError(t, json.Unmarshal([]byte(trailEvalGatesWireJSON), &wire))
	require.Len(t, got.Mergeability.Gates, len(wire.Gates))
	for i, g := range got.Mergeability.Gates {
		require.JSONEq(t, string(wire.Gates[i].Value), string(g.Value), "gate %d (%s) value must pass through untouched", i, g.GateKey)
	}
	paused := got.Mergeability.Gates[len(got.Mergeability.Gates)-1]
	require.Equal(t, "eval", paused.GateType)
	require.Equal(t, "skipped", paused.Status)
	var value api.TrailEvalGateValue
	require.NoError(t, json.Unmarshal(paused.Value, &value))
	require.NotNil(t, value.ConfigurationIssue)
	require.Equal(t, "agent_paused", value.ConfigurationIssue.Code)
}

func TestRunTrailShowTextRendersEvalGates(t *testing.T) {
	t.Parallel()

	srv := trailDetailServerWithMergeability(t, trailEvalGatesWireJSON)
	out, errOut := runTrailShowForMergeabilityTest(t, srv, "7", false)
	require.Empty(t, errOut)
	for _, want := range []string{
		"    Evals:\n",
		"      ✗ Risk review      72%      must be ≤ 40%     blocks merge\n",
		"      - Style review     not enforced: Style review is paused.\n",
		"  Needs attention:\n    - findings: advisory findings open\n",
	} {
		require.Contains(t, out, want)
	}
	require.NotContains(t, out, "01K8EVAL", "an eval gate is named by its agent, never its id")
	require.Less(t, strings.Index(out, "Needs attention:"), strings.Index(out, "Description:"))
}
