package telemetry

import "testing"

func TestBuildSearchPerformedPayload(t *testing.T) {
	t.Parallel()
	event := SearchPerformedEvent{
		SearchID:    "01JXAMPLE0000000000000000",
		QueryHash:   "abcd1234abcd1234",
		Mode:        "compact",
		ResultCount: 5,
		Total:       42,
		Page:        1,
		Limit:       5,
	}
	payload := BuildSearchPerformedPayload(event, "1.2.3")
	if payload == nil {
		t.Fatal("expected payload, got nil")
	}
	if payload.Event != "cli_search_performed" {
		t.Errorf("event = %q, want cli_search_performed", payload.Event)
	}
	if payload.DistinctID == "" {
		t.Error("distinct_id must be set")
	}
	p := payload.Properties
	for _, key := range []string{"search_id", "query_hash", "mode", "result_count", "total", "page", "limit", "cli_version", "os", "arch"} {
		if _, ok := p[key]; !ok {
			t.Errorf("missing property %q", key)
		}
	}
	// Privacy: the raw query, impression tuples, and session identifiers must
	// never be properties.
	for _, key := range []string{"query", "impressions", "results", "session_id"} {
		if _, ok := p[key]; ok {
			t.Errorf("forbidden property %q present", key)
		}
	}
	if got := p["result_count"]; got != 5 {
		t.Errorf("result_count = %v, want 5", got)
	}
}

func TestBuildCheckpointExplainedPayload(t *testing.T) {
	t.Parallel()
	payload := BuildCheckpointExplainedPayload(CheckpointExplainedEvent{
		CheckpointID: "01JRESULT0000000000000000",
		Source:       "prose",
	}, "1.2.3")
	if payload == nil {
		t.Fatal("expected payload, got nil")
	}
	if payload.Event != "cli_checkpoint_explained" {
		t.Errorf("event = %q, want cli_checkpoint_explained", payload.Event)
	}
	if payload.DistinctID == "" {
		t.Error("distinct_id must be set")
	}
	p := payload.Properties
	for _, key := range []string{"checkpoint_id", "source", "cli_version", "os", "arch"} {
		if _, ok := p[key]; !ok {
			t.Errorf("missing property %q", key)
		}
	}
	if got := p["checkpoint_id"]; got != "01JRESULT0000000000000000" {
		t.Errorf("checkpoint_id = %v", got)
	}
	if got := p["source"]; got != "prose" {
		t.Errorf("source = %v, want prose", got)
	}
}
