package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/stretchr/testify/require"
)

// trailMonitorsWireJSON and trailRunnersWireJSON are detail-resource monitors
// and runners in the backend's wire shape (entire.io trail-detail-schema.ts),
// including a custom yes/no runner and a stale one.
const trailMonitorsWireJSON = `[
  {"key": "confidence", "label": "Confidence", "value_type": "percent", "percent_value": 82, "size_value": null, "boolean_value": null,
   "rationale": "Well covered", "polarity": "higher_is_better", "trend_delta": 4, "evaluating": false, "history": [], "last_failed_attempt": null,
   "state": "evaluated", "outcome": "passed", "evaluated_at_sha": "abc123", "stale_reason": null, "run_id": "run_1", "head_sha": "abc123", "updated_at": "2026-09-28T10:00:00Z"},
  {"key": "drift", "label": "Drift", "value_type": "percent", "percent_value": 45, "size_value": null, "boolean_value": null,
   "rationale": "Some scope creep", "polarity": "lower_is_better", "evaluating": false, "history": [],
   "state": "stale", "outcome": "passed", "evaluated_at_sha": "old999", "stale_reason": "head_moved", "run_id": "run_2", "head_sha": "old999", "updated_at": "2026-09-28T09:00:00Z"},
  {"key": "risk", "label": "Risk", "value_type": "size", "percent_value": null, "size_value": "large", "boolean_value": null,
   "rationale": "Touches auth", "polarity": "lower_is_better", "evaluating": false, "history": [],
   "state": "evaluated", "outcome": "passed", "evaluated_at_sha": "abc123", "run_id": "run_3", "head_sha": "abc123", "updated_at": "2026-09-28T10:00:00Z"},
  {"key": "dipree", "label": "Dipree", "value_type": "boolean", "percent_value": null, "size_value": null, "boolean_value": true,
   "rationale": null, "polarity": "higher_is_better", "evaluating": true, "history": [],
   "state": "running", "outcome": null, "evaluated_at_sha": null, "run_id": "run_4", "head_sha": null, "updated_at": "2026-09-28T10:01:00Z"}
]`

const trailRunnersWireJSON = `[
  {"runner_id": "trail-review", "state": "evaluated", "outcome": "passed", "run_id": "run_9", "evaluated_at_sha": "abc123", "started_at": "2026-09-28T09:59:00Z", "stale_reason": null},
  {"runner_id": "dipree", "state": "running", "outcome": null, "run_id": "run_4", "evaluated_at_sha": null, "started_at": "2026-09-28T10:01:00Z", "stale_reason": null}
]`

func trailMonitorsTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	body := `{"id": "trl_1", "number": 7, "branch": "feature/x", "base": "main", "title": "T", "status": "open",
  "body_document": {"text_snapshot": "detail body"},
  "mergeability": ` + trailMergeabilityWireJSON + `,
  "monitors": ` + trailMonitorsWireJSON + `,
  "runners": ` + trailRunnersWireJSON + `}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != trailTestBasePath+"/7" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("write detail response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRunTrailShowJSONIncludesMonitorsAndRunners(t *testing.T) {
	t.Parallel()

	out, errOut := runTrailShowForMergeabilityTest(t, trailMonitorsTestServer(t), "7", true)
	require.Empty(t, errOut)

	var got struct {
		Monitors []struct {
			Key            string  `json:"key"`
			Quality        string  `json:"quality"`
			State          string  `json:"state"`
			EvaluatedAtSHA *string `json:"evaluated_at_sha"`
		} `json:"monitors"`
		Runners []api.TrailRunner `json:"runners"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Len(t, got.Monitors, 4)
	qualities := map[string]string{}
	for _, m := range got.Monitors {
		qualities[m.Key] = m.Quality
	}
	require.Equal(t, map[string]string{
		"confidence": "success",
		"drift":      "warning",
		"risk":       "danger",
		"dipree":     "success",
	}, qualities)
	require.Equal(t, "stale", got.Monitors[1].State)
	require.Len(t, got.Runners, 2)
	require.Equal(t, "dipree", got.Runners[1].RunnerID)
	require.Equal(t, "running", got.Runners[1].State)
}

// A detail that serves no monitors or runners emits null for both, so a
// script can tell "not served" from "served, empty".
func TestRunTrailShowJSONMonitorsNullWhenAbsent(t *testing.T) {
	t.Parallel()

	out, _ := runTrailShowForMergeabilityTest(t, trailMergeabilityTestServer(t, 0), "7", true)
	var got map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.JSONEq(t, "null", string(got["monitors"]))
	require.JSONEq(t, "null", string(got["runners"]))
}

func TestRunTrailShowDegradesOnMalformedMonitors(t *testing.T) {
	t.Parallel()

	body := `{"id": "trl_1", "number": 7, "branch": "feature/x", "title": "T", "status": "open", "monitors": {"not": "an array"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body)) //nolint:errcheck // test server
	}))
	t.Cleanup(srv.Close)

	out, errOut := runTrailShowForMergeabilityTest(t, srv, "7", false)
	require.Contains(t, errOut, "could not read trail monitors")
	require.Contains(t, out, "Trail: T")
	require.NotContains(t, out, "Monitors:")
}

func TestRunTrailShowTextRendersMonitors(t *testing.T) {
	t.Parallel()

	out, errOut := runTrailShowForMergeabilityTest(t, trailMonitorsTestServer(t), "7", false)
	require.Empty(t, errOut)
	for _, want := range []string{
		"Monitors:",
		"✓ Confidence  82%    success",
		"! Drift       45%    warning (stale)",
		"✗ Risk        large  danger",
		"✓ Dipree      yes    success (evaluating)",
	} {
		require.Containsf(t, out, want, "text output missing %q:\n%s", want, out)
	}
}

func TestPrintTrailMonitorsEmptyPrintsNothing(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	printTrailMonitors(&out, newStatusStyles(&out), func(s string) string { return s }, nil)
	require.Empty(t, out.String())
}

func TestTrailMonitorQuality(t *testing.T) {
	t.Parallel()

	f := func(v float64) *float64 { return &v }
	s := func(v string) *string { return &v }
	b := func(v bool) *bool { return &v }
	cases := map[string]struct {
		m    api.TrailMonitor
		want string
	}{
		"percent higher 67":        {api.TrailMonitor{ValueType: "percent", PercentValue: f(67), Polarity: s("higher_is_better")}, "success"},
		"percent higher 66":        {api.TrailMonitor{ValueType: "percent", PercentValue: f(66), Polarity: s("higher_is_better")}, "warning"},
		"percent higher 34":        {api.TrailMonitor{ValueType: "percent", PercentValue: f(34), Polarity: s("higher_is_better")}, "warning"},
		"percent higher 33":        {api.TrailMonitor{ValueType: "percent", PercentValue: f(33), Polarity: s("higher_is_better")}, "danger"},
		"percent lower 20":         {api.TrailMonitor{ValueType: "percent", PercentValue: f(20), Polarity: s("lower_is_better")}, "success"},
		"percent lower 80":         {api.TrailMonitor{ValueType: "percent", PercentValue: f(80), Polarity: s("lower_is_better")}, "danger"},
		"percent null":             {api.TrailMonitor{ValueType: "percent", Polarity: s("higher_is_better")}, "neutral"},
		"percent neutral polarity": {api.TrailMonitor{ValueType: "percent", PercentValue: f(90), Polarity: s("neutral")}, "neutral"},
		"percent no polarity":      {api.TrailMonitor{ValueType: "percent", PercentValue: f(90)}, "neutral"},
		"size lower small":         {api.TrailMonitor{ValueType: "size", SizeValue: s("small"), Polarity: s("lower_is_better")}, "success"},
		"size lower medium":        {api.TrailMonitor{ValueType: "size", SizeValue: s("medium"), Polarity: s("lower_is_better")}, "warning"},
		"size higher small":        {api.TrailMonitor{ValueType: "size", SizeValue: s("small"), Polarity: s("higher_is_better")}, "danger"},
		"size unknown":             {api.TrailMonitor{ValueType: "size", SizeValue: s("huge"), Polarity: s("higher_is_better")}, "neutral"},
		"boolean higher true":      {api.TrailMonitor{ValueType: "boolean", BooleanValue: b(true), Polarity: s("higher_is_better")}, "success"},
		"boolean higher false":     {api.TrailMonitor{ValueType: "boolean", BooleanValue: b(false), Polarity: s("higher_is_better")}, "danger"},
		"boolean lower true":       {api.TrailMonitor{ValueType: "boolean", BooleanValue: b(true), Polarity: s("lower_is_better")}, "danger"},
		"boolean neutral true":     {api.TrailMonitor{ValueType: "boolean", BooleanValue: b(true), Polarity: s("neutral")}, "success"},
		"boolean null":             {api.TrailMonitor{ValueType: "boolean", Polarity: s("higher_is_better")}, "neutral"},
	}
	for name, tc := range cases {
		require.Equalf(t, tc.want, tc.m.Quality(), "case %s", name)
	}
}
