package telemetry

import "testing"

func TestBuildSearchOutcomePayload_Success(t *testing.T) {
	t.Parallel()
	payload := BuildSearchOutcomePayload(SearchOutcome{
		Command:            "entire search",
		Mode:               SearchModeCheckpoint,
		ResultCount:        7,
		CoverageIncomplete: true,
		DurationMS:         340,
	}, true, "1.2.3")
	if payload == nil {
		t.Fatal("BuildSearchOutcomePayload returned nil")
	}
	if payload.Event != "cli_search_completed" {
		t.Errorf("Event = %q, want %q", payload.Event, "cli_search_completed")
	}
	want := map[string]any{
		"command":             "entire search",
		"mode":                "checkpoint",
		"success":             true,
		"result_count":        7,
		"coverage_incomplete": true,
		"duration_ms":         int64(340),
		"isEntireEnabled":     true,
		"cli_version":         "1.2.3",
	}
	for k, v := range want {
		if got := payload.Properties[k]; got != v {
			t.Errorf("property %s = %v, want %v", k, got, v)
		}
	}
	if _, ok := payload.Properties["error_class"]; ok {
		t.Error("success payload must not include error_class")
	}
}

func TestBuildSearchOutcomePayload_Failure(t *testing.T) {
	t.Parallel()
	payload := BuildSearchOutcomePayload(SearchOutcome{
		Command:    "entire checkpoint search",
		Mode:       SearchModeCode,
		ErrorClass: SearchErrClassAuth,
		DurationMS: 12,
	}, false, "1.2.3")
	if payload == nil {
		t.Fatal("BuildSearchOutcomePayload returned nil")
	}
	if got := payload.Properties["success"]; got != false {
		t.Errorf("success property = %v, want false", got)
	}
	if got := payload.Properties["error_class"]; got != "auth" {
		t.Errorf("error_class property = %v, want %q", got, "auth")
	}
	// A failed search has no meaningful result count or coverage flag; omit
	// them so PostHog zero-result queries never count failures as empty
	// result sets.
	if _, ok := payload.Properties["result_count"]; ok {
		t.Error("failure payload must not include result_count")
	}
	if _, ok := payload.Properties["coverage_incomplete"]; ok {
		t.Error("failure payload must not include coverage_incomplete")
	}
}
