package telemetry

import "testing"

func TestBuildSearchPerformedPayload(t *testing.T) {
	t.Parallel()
	event := SearchPerformedEvent{
		SearchID:    "01JXAMPLE0000000000000000",
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
	for _, key := range []string{"search_id", "mode", "result_count", "total", "page", "limit", "cli_version", "os", "arch"} {
		if _, ok := p[key]; !ok {
			t.Errorf("missing property %q", key)
		}
	}
	// Privacy: the raw query, its hash, impression tuples, session
	// identifiers, and raw checkpoint ids must never be properties.
	for _, key := range []string{"query", "query_hash", "impressions", "results", "session_id", "checkpoint_id"} {
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
		DocRef: "abcd1234abcd1234",
		Source: "prose",
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
	for _, key := range []string{"doc_ref", "source", "cli_version", "os", "arch"} {
		if _, ok := p[key]; !ok {
			t.Errorf("missing property %q", key)
		}
	}
	docRef, ok := p["doc_ref"].(string)
	if !ok {
		t.Fatalf("doc_ref = %v, want string", p["doc_ref"])
	}
	if len(docRef) != 16 {
		t.Errorf("doc_ref len = %d, want 16", len(docRef))
	}
	if got := p["source"]; got != "prose" {
		t.Errorf("source = %v, want prose", got)
	}
	// Privacy: the raw query, its hash, impression tuples, session
	// identifiers, and raw checkpoint ids must never be properties.
	for _, key := range []string{"query", "query_hash", "impressions", "results", "session_id", "checkpoint_id"} {
		if _, ok := p[key]; ok {
			t.Errorf("forbidden property %q present", key)
		}
	}
	// Without a search-hit token, the deterministic-link fields are omitted.
	for _, key := range []string{"search_id", "rank"} {
		if _, ok := p[key]; ok {
			t.Errorf("property %q should be omitted when no search hit is attributed", key)
		}
	}
}

func TestBuildCheckpointExplainedPayload_WithSearchAttribution(t *testing.T) {
	t.Parallel()
	payload := BuildCheckpointExplainedPayload(CheckpointExplainedEvent{
		DocRef:   "abcd1234abcd1234",
		Source:   "export",
		SearchID: "01JXK9RSTQ4B7NW2VYFCH6M3DZ",
		Rank:     3,
	}, "1.2.3")
	if payload == nil {
		t.Fatal("expected payload, got nil")
	}
	p := payload.Properties
	if got := p["search_id"]; got != "01JXK9RSTQ4B7NW2VYFCH6M3DZ" {
		t.Errorf("search_id = %v", got)
	}
	if got := p["rank"]; got != 3 {
		t.Errorf("rank = %v, want 3", got)
	}
	// Privacy: the raw query, its hash, impression tuples, session
	// identifiers, and raw checkpoint ids must never be properties.
	for _, key := range []string{"query", "query_hash", "impressions", "results", "session_id", "checkpoint_id"} {
		if _, ok := p[key]; ok {
			t.Errorf("forbidden property %q present", key)
		}
	}
}

func TestBuildCheckpointExplainedPayload_SearchIDWithoutRank(t *testing.T) {
	t.Parallel()
	payload := BuildCheckpointExplainedPayload(CheckpointExplainedEvent{
		DocRef:   "abcd1234abcd1234",
		Source:   "prose",
		SearchID: "01JXK9RSTQ4B7NW2VYFCH6M3DZ",
	}, "1.2.3")
	if payload == nil {
		t.Fatal("expected payload, got nil")
	}
	if _, ok := payload.Properties["search_id"]; !ok {
		t.Error("search_id should be present")
	}
	if _, ok := payload.Properties["rank"]; ok {
		t.Error("rank should be omitted when unknown")
	}
	// Privacy: the raw query, its hash, impression tuples, session
	// identifiers, and raw checkpoint ids must never be properties.
	for _, key := range []string{"query", "query_hash", "impressions", "results", "session_id", "checkpoint_id"} {
		if _, ok := payload.Properties[key]; ok {
			t.Errorf("forbidden property %q present", key)
		}
	}
}
