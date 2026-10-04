package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/stretchr/testify/require"
)

func trailStatusTestMergeability(t *testing.T) *api.TrailMergeability {
	t.Helper()
	var mg api.TrailMergeability
	require.NoError(t, json.Unmarshal([]byte(trailMergeabilityWireJSON), &mg))
	return &mg
}

func trailStatusTestMonitors(t *testing.T) []api.TrailMonitor {
	t.Helper()
	var ms []api.TrailMonitor
	require.NoError(t, json.Unmarshal([]byte(trailMonitorsWireJSON), &ms))
	return ms
}

func greenMergeability(head string) *api.TrailMergeability {
	h := head
	return &api.TrailMergeability{
		HeadSHA:   &h,
		Mergeable: true,
		Checks:    api.TrailChecks{Availability: api.TrailChecksNotApplicable},
		Gates: []api.TrailGate{
			{GateKey: "findings", Blocking: true, Status: "passed", EvaluatedAtSHA: &h},
			{GateKey: "approvals", Blocking: true, Status: "failed", EvaluatedAtSHA: &h},
		},
	}
}

func percentMonitor(key string, value float64, polarity, head string) api.TrailMonitor {
	p, h := polarity, head
	return api.TrailMonitor{Key: key, Label: key, ValueType: "percent", PercentValue: &value, Polarity: &p, State: "evaluated", EvaluatedAtSHA: &h, HeadSHA: &h}
}

func boolMonitor(key string, value bool, polarity *string, head string) api.TrailMonitor {
	h := head
	return api.TrailMonitor{Key: key, Label: key, ValueType: "boolean", BooleanValue: &value, Polarity: polarity, State: "evaluated", EvaluatedAtSHA: &h, HeadSHA: &h}
}

func finding(severity string) api.TrailReviewComment {
	title := "finding f1"
	return api.TrailReviewComment{ID: "f1", Title: &title, Severity: &severity, Status: "open"}
}

func TestEvaluateTrailStatus(t *testing.T) {
	t.Parallel()

	const head = "abc123"
	higher := "higher_is_better"
	opts := trailStatusEvalOptions{MinSeverity: trailReviewSeverityLow}

	cases := []struct {
		name     string
		mg       *api.TrailMergeability
		monitors []api.TrailMonitor
		runners  []api.TrailRunner
		findings []api.TrailReviewComment
		opts     trailStatusEvalOptions
		want     string
	}{
		{name: "all green, approvals ignored", mg: greenMergeability(head), monitors: []api.TrailMonitor{percentMonitor("confidence", 90, higher, head)}, opts: opts, want: trailVerdictGreen},
		{name: "approvals counted when asked", mg: greenMergeability(head), opts: trailStatusEvalOptions{MinSeverity: "low", IncludeApprovals: true}, want: trailVerdictRed},
		{name: "null mergeability is unknown", mg: nil, opts: opts, want: trailVerdictUnknown},
		{name: "wire fixture: failed approvals skipped, findings gate pending", mg: trailStatusTestMergeability(t), opts: trailStatusEvalOptions{MinSeverity: "low", NoChecks: true}, want: trailVerdictPending},
		{name: "wire fixture with checks: failed check is red", mg: trailStatusTestMergeability(t), opts: opts, want: trailVerdictRed},
		{name: "danger monitor is red", mg: greenMergeability(head), monitors: []api.TrailMonitor{percentMonitor("confidence", 10, higher, head)}, opts: opts, want: trailVerdictRed},
		{name: "yellow monitor is red by default", mg: greenMergeability(head), monitors: []api.TrailMonitor{percentMonitor("drift", 50, "lower_is_better", head)}, opts: opts, want: trailVerdictRed},
		{name: "yellow monitor passes with allow-warning", mg: greenMergeability(head), monitors: []api.TrailMonitor{percentMonitor("drift", 50, "lower_is_better", head)}, opts: trailStatusEvalOptions{MinSeverity: "low", AllowWarning: true}, want: trailVerdictGreen},
		{name: "plateaued yellow monitor does not block", mg: greenMergeability(head), monitors: []api.TrailMonitor{percentMonitor("drift", 50, "lower_is_better", head)}, opts: trailStatusEvalOptions{MinSeverity: "low", Plateaued: map[string]bool{"drift": true}}, want: trailVerdictGreen},
		{name: "monitor on older head is pending", mg: greenMergeability(head), monitors: []api.TrailMonitor{percentMonitor("confidence", 90, higher, "old")}, opts: opts, want: trailVerdictPending},
		{name: "custom yes/no runner, yes is green", mg: greenMergeability(head), monitors: []api.TrailMonitor{boolMonitor("dipree", true, nil, head)}, opts: opts, want: trailVerdictGreen},
		{name: "custom yes/no runner, no is red", mg: greenMergeability(head), monitors: []api.TrailMonitor{boolMonitor("dipree", false, nil, head)}, opts: opts, want: trailVerdictRed},
		{name: "running runner is pending", mg: greenMergeability(head), runners: []api.TrailRunner{{RunnerID: "trail-review", State: "running"}}, opts: opts, want: trailVerdictPending},
		{name: "open low finding is red", mg: greenMergeability(head), findings: []api.TrailReviewComment{finding("low")}, opts: opts, want: trailVerdictRed},
		{name: "low finding ignored at min-severity high", mg: greenMergeability(head), findings: []api.TrailReviewComment{finding("low")}, opts: trailStatusEvalOptions{MinSeverity: "high"}, want: trailVerdictGreen},
		{name: "skipped finding does not block", mg: greenMergeability(head), findings: []api.TrailReviewComment{finding("high")}, opts: trailStatusEvalOptions{MinSeverity: "low", Skipped: map[string]string{"f1": "product call"}}, want: trailVerdictGreen},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := evaluateTrailStatus(tc.mg, tc.monitors, tc.runners, tc.findings, tc.findings != nil, tc.opts)
			require.Equalf(t, tc.want, r.Verdict, "items: %+v", r.Items)
		})
	}
}

func TestEvaluateTrailStatusWireMonitors(t *testing.T) {
	t.Parallel()

	r := evaluateTrailStatus(greenMergeability("abc123"), trailStatusTestMonitors(t), nil, nil, false, trailStatusEvalOptions{MinSeverity: "low"})
	states := map[string]string{}
	for _, it := range r.Items {
		if it.Kind == "monitor" {
			states[it.Key] = it.State
		}
	}
	require.Equal(t, map[string]string{
		"confidence": trailItemGreen,
		"drift":      trailItemPending, // stale
		"risk":       trailItemRed,     // large risk, lower is better
		"dipree":     trailItemPending, // still evaluating
	}, states)
	require.Equal(t, trailVerdictRed, r.Verdict)
}

func TestTrailVerdictExitError(t *testing.T) {
	t.Parallel()

	require.NoError(t, trailVerdictExitError(trailVerdictGreen))
	for verdict, code := range map[string]int{trailVerdictRed: 1, trailVerdictPending: 2, trailVerdictUnknown: 3} {
		err := trailVerdictExitError(verdict)
		var coded *ExitCodeError
		require.ErrorAs(t, err, &coded)
		require.Equal(t, code, coded.ExitCode())
		var silent *SilentError
		require.ErrorAs(t, err, &silent, "exit-code errors must not print a second error line")
	}
}

func TestPrintTrailStatus(t *testing.T) {
	t.Parallel()

	r := evaluateTrailStatus(greenMergeability("abc1234567"), []api.TrailMonitor{percentMonitor("confidence", 90, "higher_is_better", "abc1234567")},
		nil, []api.TrailReviewComment{finding("high")}, true, trailStatusEvalOptions{MinSeverity: "low"})
	r.Trail = 7
	var out bytes.Buffer
	printTrailStatus(&out, r)
	for _, want := range []string{"Trail #7", "finding  finding f1  red", "monitor  confidence  green", "Verdict: RED"} {
		require.Contains(t, out.String(), want)
	}
}
