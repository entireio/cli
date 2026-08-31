package telemetry

import "testing"

func TestBuildSearchSelectionPayload(t *testing.T) {
	t.Parallel()
	payload := BuildSearchSelectionPayload(SearchSelection{
		Command:     "entire search",
		Mode:        SearchModeCheckpoint,
		ResultType:  "commit",
		Rank:        3,
		ResultCount: 20,
	}, true, "1.2.3")
	if payload == nil {
		t.Fatal("BuildSearchSelectionPayload returned nil")
	}
	if payload.Event != "cli_search_result_selected" {
		t.Errorf("Event = %q, want %q", payload.Event, "cli_search_result_selected")
	}
	want := map[string]any{
		"command":         "entire search",
		"mode":            "checkpoint",
		"result_type":     "commit",
		"rank":            3,
		"result_count":    20,
		"isEntireEnabled": true,
		"cli_version":     "1.2.3",
	}
	for k, v := range want {
		if got := payload.Properties[k]; got != v {
			t.Errorf("property %s = %v, want %v", k, got, v)
		}
	}
}

// TestBuildSearchSelectionPayload_CodeMode pins the code tab's reported kind:
// code results carry no type from the API, so they report under one fixed kind
// rather than an empty string that would read as missing data.
func TestBuildSearchSelectionPayload_CodeMode(t *testing.T) {
	t.Parallel()
	payload := BuildSearchSelectionPayload(SearchSelection{
		Command:     "entire search",
		Mode:        SearchModeCode,
		ResultType:  SearchSelectionTypeCode,
		Rank:        0,
		ResultCount: 5,
	}, false, "1.2.3")
	if payload == nil {
		t.Fatal("BuildSearchSelectionPayload returned nil")
	}
	if payload.Properties["mode"] != "code" {
		t.Errorf("mode = %v, want code", payload.Properties["mode"])
	}
	if payload.Properties["result_type"] != "code" {
		t.Errorf("result_type = %v, want code", payload.Properties["result_type"])
	}
	if payload.Properties["rank"] != 0 {
		t.Errorf("rank = %v, want 0", payload.Properties["rank"])
	}
}

// TestSearchSelectionPayloadCarriesNoContent is the privacy guard: the payload
// must never carry query text, result titles, repo names, or file paths. It
// asserts on the property key set, so a field added later fails here rather
// than shipping content to PostHog unnoticed.
func TestSearchSelectionPayloadCarriesNoContent(t *testing.T) {
	t.Parallel()
	payload := BuildSearchSelectionPayload(SearchSelection{
		Command:     "entire search",
		Mode:        SearchModeCheckpoint,
		ResultType:  "session",
		Rank:        1,
		ResultCount: 9,
	}, true, "1.2.3")
	if payload == nil {
		t.Fatal("BuildSearchSelectionPayload returned nil")
	}
	allowed := map[string]bool{
		"command": true, "mode": true, "result_type": true, "rank": true,
		"result_count": true, "isEntireEnabled": true, "cli_version": true,
		"os": true, "arch": true,
	}
	for k := range payload.Properties {
		if !allowed[k] {
			t.Errorf("unexpected property %q in selection payload — content-free by construction", k)
		}
	}
}
