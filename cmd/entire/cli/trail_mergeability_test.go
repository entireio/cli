package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/stretchr/testify/require"
)

// trailMergeabilityWireJSON is a detail-resource mergeability object in the
// backend's wire shape, trimmed from a live TrailDetailResponse. Keeping it as
// raw JSON (not an encoded api struct) checks decoding against the real wire
// format rather than against our own types round-tripped.
const trailMergeabilityWireJSON = `{
  "bypass_policy": "nobody",
  "mergeable": false,
  "approval_gate_passed": false,
  "behind_by": 0,
  "comparison_status": "unknown",
  "conflict_status": "clean",
  "head_sha": "b1205a52b53b2206cf168a62c0c08b4e37e4449f",
  "gates": [
    {
      "reviewers": [{"login": "rev1", "state": "pending", "reviewed_head_sha": null, "reason": null}],
      "runner_ids": [],
      "id": "",
      "gate_definition_id": "gd_approvals",
      "gate_key": "approvals",
      "gate_type": "approvals",
      "blocking": true,
      "status": "failed",
      "head_sha": "b1205a52b53b2206cf168a62c0c08b4e37e4449f",
      "value": {"approved": 0, "required": 1, "config": {"version": 1, "min_reviewers": 1}},
      "rationale": "No reviewers have approved this trail",
      "created_at": "2026-09-24T14:38:31.646908047Z",
      "completed_at": "2026-09-24T14:38:31.646908047Z",
      "state": "evaluated",
      "outcome": "failed",
      "stale_reason": null,
      "evaluated_at_sha": "b1205a52b53b2206cf168a62c0c08b4e37e4449f",
      "finding_count": null
    },
    {
      "reviewers": [],
      "runner_ids": ["trail-review"],
      "id": "gate_findings",
      "gate_definition_id": "gd_findings",
      "gate_key": "findings",
      "gate_type": "findings",
      "blocking": true,
      "status": "pending",
      "head_sha": "b1205a52b53b2206cf168a62c0c08b4e37e4449f",
      "value": {"review_in_flight": true, "agent_blocking_findings": 0},
      "rationale": "Waiting for the agent review of the current head",
      "created_at": "2026-09-24T14:37:50.995491Z",
      "completed_at": null,
      "state": "running",
      "outcome": null,
      "stale_reason": null,
      "evaluated_at_sha": "b1205a52b53b2206cf168a62c0c08b4e37e4449f",
      "finding_count": 0
    },
    {
      "reviewers": [],
      "runner_ids": [],
      "id": "",
      "gate_definition_id": "gd_up_to_date",
      "gate_key": "up_to_date",
      "gate_type": "up_to_date",
      "blocking": false,
      "status": "skipped",
      "head_sha": "b1205a52b53b2206cf168a62c0c08b4e37e4449f",
      "value": null,
      "rationale": null,
      "created_at": "2026-06-29T15:25:52.779Z",
      "completed_at": null,
      "state": "disabled",
      "outcome": null,
      "stale_reason": null,
      "evaluated_at_sha": null,
      "finding_count": null
    }
  ],
  "checks": {
    "availability": "available",
    "runs": [
      {"name": "lint", "status": "completed", "conclusion": "success", "details_url": "https://ci.example/lint", "started_at": "2026-09-24T14:37:47Z", "completed_at": "2026-09-24T14:38:07Z", "app_name": "GitHub Actions"},
      {"name": "CodeQL", "status": "completed", "conclusion": "neutral", "details_url": null, "started_at": null, "completed_at": null, "app_name": null},
      {"name": "test-windows", "status": "completed", "conclusion": "failure", "details_url": "https://ci.example/windows", "started_at": "2026-09-24T14:37:48Z", "completed_at": "2026-09-24T14:45:00Z", "app_name": "GitHub Actions"},
      {"name": "test-core", "status": "in_progress", "conclusion": null, "details_url": "https://ci.example/core", "started_at": "2026-09-24T14:37:47Z", "completed_at": null, "app_name": "GitHub Actions"}
    ]
  }
}`

const trailMergeabilityDetailJSON = `{"id": "trl_1", "number": 7, "branch": "feature/x", "base": "main", "title": "T", "status": "open",
  "body_document": {"text_snapshot": "detail body"},
  "mergeability": ` + trailMergeabilityWireJSON + `}`

// trailMergeabilityTestServer serves a list that omits mergeability (as the
// real list endpoint does) and a detail that carries it. detailStatus > 0
// makes the detail fail.
func trailMergeabilityTestServer(t *testing.T, detailStatus int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case trailTestBasePath:
			if _, err := w.Write([]byte(`{"items": [{"id": "trl_1", "number": 7, "branch": "feature/x", "base": "main", "title": "T", "status": "open"}], "total_count": 1}`)); err != nil {
				t.Errorf("write list response: %v", err)
			}
		case trailTestBasePath + "/7":
			if detailStatus > 0 {
				w.WriteHeader(detailStatus)
				if _, err := w.Write([]byte(`{"error": "boom"}`)); err != nil {
					t.Errorf("write detail error: %v", err)
				}
				return
			}
			if _, err := w.Write([]byte(trailMergeabilityDetailJSON)); err != nil {
				t.Errorf("write detail response: %v", err)
			}
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runTrailShowForMergeabilityTest(t *testing.T, srv *httptest.Server, selector string, jsonOut bool) (string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := runTrailShowWithClientAtPath(t.Context(), &out, &errOut, api.NewClientWithBaseURL("tok", srv.URL),
		trailTestBasePath, "gh", "acme", "repo", trailShowOptions{Selector: selector, JSON: jsonOut})
	require.NoError(t, err)
	return out.String(), errOut.String()
}

// Both selector paths must surface the snapshot: a number resolves through the
// detail route directly, a branch resolves through the list (which omits
// mergeability) and then fetches the detail.
func TestRunTrailShowJSONIncludesMergeability(t *testing.T) {
	t.Parallel()

	for _, selector := range []string{"7", "feature/x"} {
		t.Run(selector, func(t *testing.T) {
			t.Parallel()

			out, errOut := runTrailShowForMergeabilityTest(t, trailMergeabilityTestServer(t, 0), selector, true)
			require.Empty(t, errOut)

			var got struct {
				Number       int                    `json:"number"`
				Body         string                 `json:"body"`
				Mergeability *trailMergeabilityJSON `json:"mergeability"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &got), "output must be one JSON object: %s", out)
			require.Equal(t, 7, got.Number, "existing trail fields must stay at the top level")
			require.Equal(t, "detail body", got.Body)

			mg := got.Mergeability
			require.NotNil(t, mg)
			require.NotNil(t, mg.HeadSHA)
			require.Equal(t, "b1205a52b53b2206cf168a62c0c08b4e37e4449f", *mg.HeadSHA)
			require.False(t, mg.Mergeable)
			require.Equal(t, "clean", mg.ConflictStatus)

			require.Equal(t, "available", mg.Checks.Availability)
			require.Len(t, mg.Checks.Runs, 4, "--json must carry every run, not just failures")
			require.Equal(t, "test-windows", mg.Checks.Runs[2].Name)
			require.NotNil(t, mg.Checks.Runs[2].AppName)
			require.Equal(t, "GitHub Actions", *mg.Checks.Runs[2].AppName)
			require.Nil(t, mg.Checks.Runs[3].Conclusion)

			require.Len(t, mg.Gates, 3)
			approvals := mg.Gates[0]
			require.Equal(t, "approvals", approvals.GateKey)
			require.True(t, approvals.Blocking)
			require.Equal(t, "failed", approvals.Status)
			require.Equal(t, "evaluated", approvals.State)
			require.Equal(t, []trailGateReviewerJSON{{Login: "rev1", State: "pending"}}, approvals.Reviewers)
			require.JSONEq(t, `{"approved": 0, "required": 1, "config": {"version": 1, "min_reviewers": 1}}`, string(approvals.Value),
				"gate value must pass through untouched")
			require.Equal(t, []string{"trail-review"}, mg.Gates[1].RunnerIDs)
			require.Equal(t, "null", string(mg.Gates[2].Value))
		})
	}
}

// Fields the CLI does not expose stay out of the output: the snapshot is
// passed through, not re-derived, but only the agreed fields are surfaced.
func TestRunTrailShowJSONMergeabilityOmitsUnexposedFields(t *testing.T) {
	t.Parallel()

	out, _ := runTrailShowForMergeabilityTest(t, trailMergeabilityTestServer(t, 0), "7", true)
	var got struct {
		Mergeability map[string]json.RawMessage `json:"mergeability"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	keys := make([]string, 0, len(got.Mergeability))
	for k := range got.Mergeability {
		keys = append(keys, k)
	}
	require.ElementsMatch(t, []string{"head_sha", "mergeable", "conflict_status", "checks", "gates"}, keys)
}

// A numeric selector's detail response is recognized as the detail even
// without body_document (an empty description), so it is neither refetched
// nor stripped of its mergeability.
func TestRunTrailShowNumericDetailWithoutBodyIsNotRefetched(t *testing.T) {
	t.Parallel()

	const detail = `{"id": "trl_1", "number": 7, "branch": "feature/x", "status": "open", "mergeability": ` + trailMergeabilityWireJSON + `}`
	var detailHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != trailTestBasePath+"/7" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// Any refetch fails, so a second request would also lose the snapshot.
		if detailHits.Add(1) > 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(detail)); err != nil {
			t.Errorf("write detail response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	for _, jsonOut := range []bool{true, false} {
		detailHits.Store(0)
		out, errOut := runTrailShowForMergeabilityTest(t, srv, "7", jsonOut)
		require.Empty(t, errOut)
		require.EqualValues(t, 1, detailHits.Load(), "json=%v: the detail route must be requested exactly once", jsonOut)
		if jsonOut {
			var got struct {
				Mergeability *trailMergeabilityJSON `json:"mergeability"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &got))
			require.NotNil(t, got.Mergeability, "the snapshot from the first response must survive")
		} else {
			require.Contains(t, out, "Mergeable: no")
		}
	}
}

// A failed detail fetch must leave the verdict unknown — an explicit null —
// rather than a zero-valued "mergeable": false nobody computed.
func TestRunTrailShowJSONMergeabilityNullWhenDetailFails(t *testing.T) {
	t.Parallel()

	out, errOut := runTrailShowForMergeabilityTest(t, trailMergeabilityTestServer(t, http.StatusInternalServerError), "feature/x", true)
	require.Contains(t, errOut, "could not load trail detail")

	var top map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(out), &top), "stdout must stay valid JSON: %s", out)
	raw, ok := top["mergeability"]
	require.True(t, ok, "mergeability key must be present even when unknown")
	require.Equal(t, "null", string(raw))
}

func TestRunTrailShowTextRendersMergeability(t *testing.T) {
	t.Parallel()

	out, errOut := runTrailShowForMergeabilityTest(t, trailMergeabilityTestServer(t, 0), "7", false)
	require.Empty(t, errOut)

	for _, want := range []string{
		"Mergeable: no",
		"Head:      b1205a52",
		"Conflicts: clean",
		"Gates:",
		"✗ approvals   failed   No reviewers have approved this trail",
		"… findings    pending  Waiting for the agent review of the current head",
		"- up_to_date  skipped  (non-blocking)",
		"Checks:    2 passed · 1 running · 1 failed (4 total)",
		"✗ test-windows  failure  https://ci.example/windows",
	} {
		require.Containsf(t, out, want, "text output missing %q:\n%s", want, out)
	}
	// Only failed runs are listed; passing and running ones are counted only.
	for _, absent := range []string{"https://ci.example/lint", "https://ci.example/core", "CodeQL"} {
		require.NotContainsf(t, out, absent, "text output must not list non-failed run %q:\n%s", absent, out)
	}
	// The snapshot renders before the description, not after it.
	require.Less(t, strings.Index(out, "Mergeable:"), strings.Index(out, "Description:"))
}

func TestPrintTrailMergeabilityUnknownWhenNil(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	printTrailMergeability(&out, newStatusStyles(&out), func(s string) string { return s }, nil)
	require.Equal(t, "  Mergeable: unknown\n", out.String())
}

func TestPrintTrailMergeabilityChecksAvailability(t *testing.T) {
	t.Parallel()

	tests := []struct {
		availability string
		runs         []api.TrailCheckRun
		want         string
	}{
		{availability: "unavailable", want: "Checks:    unavailable"},
		{availability: "not_applicable", want: "Checks:    not applicable"},
		{availability: "available", want: "Checks:    none reported"},
		// Runs are meaningful only when available: an unavailable snapshot
		// must not summarize stale runs.
		{availability: "unavailable", runs: []api.TrailCheckRun{{Name: "lint", Status: "completed"}}, want: "Checks:    unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.availability, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer
			printTrailMergeability(&out, newStatusStyles(&out), func(s string) string { return s },
				&api.TrailMergeability{Mergeable: true, ConflictStatus: "clean", Checks: api.TrailChecks{Availability: tt.availability, Runs: tt.runs}})
			text := out.String()
			require.Contains(t, text, "Mergeable: yes")
			require.Contains(t, text, "Gates:     none")
			require.Contains(t, text, tt.want)
			require.NotContains(t, text, "passed ·")
		})
	}
}

// Gate and check strings come from the server and are printed to a terminal,
// so escape sequences and newlines must not survive.
func TestPrintTrailMergeabilitySanitizesServerText(t *testing.T) {
	t.Parallel()

	rationale := "evil\x1b[31m\nsecond line"
	conclusion := "failure"
	url := "https://ci.example/\x1b]8;;x\x07"
	var out bytes.Buffer
	printTrailMergeability(&out, newStatusStyles(&out), func(s string) string { return s }, &api.TrailMergeability{
		ConflictStatus: "conflicting\x1b[0m",
		Gates:          []api.TrailGate{{GateKey: "approvals\r", Status: "failed", Blocking: true, Rationale: &rationale}},
		Checks: api.TrailChecks{Availability: "available", Runs: []api.TrailCheckRun{
			{Name: "lint\n", Status: "completed", Conclusion: &conclusion, DetailsURL: &url},
		}},
	})
	text := out.String()
	require.NotContains(t, text, "\x1b")
	require.NotContains(t, text, "\r")
	require.NotContains(t, text, "\x07")
	require.Contains(t, text, "evil second line")
	require.Equal(t, 6, strings.Count(text, "\n"), "each rendered row must stay on one line:\n%s", text)
}
