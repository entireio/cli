package api

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestSnakeWireKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"createdAt", "created_at"},
		{"originalBranch", "original_branch"},
		{"headSha", "head_sha"},
		{"ghPrId", "gh_pr_id"},
		{"already_snake", "already_snake"},
		{"id", "id"},
		{"", ""},
	} {
		if got := snakeWireKey(tc.in); got != tc.want {
			t.Errorf("snakeWireKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A pre-RFD-026 cell's camelCase body must decode into the current
// snake_case-tagged wire struct.
func TestDecodesLegacyCamelCaseIntoCurrentStruct(t *testing.T) {
	t.Parallel()
	legacy := []byte(`{
		"items": [{
			"id": "01ABC", "number": 7, "branch": "feat/x", "originalBranch": "feat/old",
			"base": "main", "title": "T", "status": "open",
			"createdAt": "2026-09-21T00:00:00Z", "updatedAt": "2026-09-21T01:00:00Z",
			"commentCount": 3, "checkpointCount": 4
		}],
		"totalCount": 1,
		"nextPageToken": "tok-1"
	}`)

	var page TrailListResponse
	if err := UnmarshalTrailWire(legacy, &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.Total != 1 {
		t.Errorf("Total = %d, want 1 (totalCount -> total_count)", page.Total)
	}
	if page.NextCursor == nil || *page.NextCursor != "tok-1" {
		t.Errorf("NextCursor = %v, want tok-1 (nextPageToken -> next_cursor)", page.NextCursor)
	}
	if len(page.Trails) != 1 {
		t.Fatalf("got %d trails, want 1", len(page.Trails))
	}
	got := page.Trails[0]
	if got.OriginalBranch != "feat/old" {
		t.Errorf("OriginalBranch = %q, want feat/old", got.OriginalBranch)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("timestamps did not decode: created=%v updated=%v", got.CreatedAt, got.UpdatedAt)
	}
	if got.CommentCount != 3 || got.CheckpointCount != 4 {
		t.Errorf("counts = %d/%d, want 3/4", got.CommentCount, got.CheckpointCount)
	}
}

// A current cell's body is returned byte-identical: the scan finds no
// lower-to-upper run, so no decode/re-encode happens at all.
func TestCurrentWireIsUntouched(t *testing.T) {
	t.Parallel()
	// The RFC 3339 timestamp is the point: its "1T"/"0Z" runs must not be
	// mistaken for a camelCase key and disarm the fast path.
	current := []byte(`{"items":[{"id":"01ABC","original_branch":"feat/old","created_at":"2026-09-21T00:00:00Z"}],"total_count":1,"next_cursor":"c1"}`)
	if got := reconcileWireKeys(current); string(got) != string(current) {
		t.Errorf("current wire was rewritten:\n got %s\nwant %s", got, current)
	}
}

// The discussion vocabulary renames are not pure case changes.
func TestDiscussionVocabularyRenames(t *testing.T) {
	t.Parallel()
	legacy := []byte(`{"thread":{"id":"t1","threadId":"t1","threadMessageCount":2,"repositoryId":"r1"}}`)
	var out map[string]any
	if err := UnmarshalTrailWire(legacy, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	discussion, ok := out["discussion"].(map[string]any)
	if !ok {
		t.Fatalf("thread -> discussion missing; got keys %v", keysOf(out))
	}
	for _, want := range []string{"discussion_id", "discussion_message_count", "repo_id"} {
		if _, ok := discussion[want]; !ok {
			t.Errorf("missing %q; got keys %v", want, keysOf(discussion))
		}
	}
	// Renamed, not duplicated — see TestNestedCamelCaseDoesNotInflate.
	if _, ok := out["thread"]; ok {
		t.Error("legacy \"thread\" key survived; the rewrite must rename, not duplicate")
	}
}

// A document that already carries the current spelling keeps its value: the
// legacy twin must not overwrite it.
func TestExistingCurrentKeyWins(t *testing.T) {
	t.Parallel()
	mixed := []byte(`{"createdAt":"legacy","created_at":"current"}`)
	var out map[string]any
	if err := UnmarshalTrailWire(mixed, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["created_at"] != "current" {
		t.Errorf("created_at = %v, want current (legacy twin must not clobber)", out["created_at"])
	}
}

// Scope check: the reconciliation renames, so a camelCase-tagged destination
// would NOT survive it. That is why it is confined to DecodeTrailJSON and the
// camelCase checkpoint/session routes keep using DecodeJSON.
func TestRenameIsScopedAwayFromCamelCaseConsumers(t *testing.T) {
	t.Parallel()
	var viaTrail struct {
		SessionID string `json:"sessionId"`
	}
	if err := UnmarshalTrailWire([]byte(`{"sessionId":"s1"}`), &viaTrail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if viaTrail.SessionID != "" {
		t.Error("expected the trail decoder to rename sessionId away; if this ever passes, the scoping rationale is stale")
	}
	var direct struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal([]byte(`{"sessionId":"s1"}`), &direct); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if direct.SessionID != "s1" {
		t.Error("plain DecodeJSON path must be untouched")
	}
}

// The branch deleted importedTrailEventPayloadAliases on the grounds that the
// cell projects payload keys server-side. A pre-RFD-026 cell does not, so the
// watch command's payload lookups need the twins too.
func TestEventPayloadKeysGainTwins(t *testing.T) {
	t.Parallel()
	frame := []byte(`{"eventType":"comment.created","payload":{"filePath":"a/b.go","headSha":"abc123"}}`)
	var ev struct {
		EventType string         `json:"event_type"`
		Payload   map[string]any `json:"payload"`
	}
	if err := UnmarshalTrailWire(frame, &ev); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ev.EventType != "comment.created" {
		t.Errorf("EventType = %q", ev.EventType)
	}
	if ev.Payload["file_path"] != "a/b.go" || ev.Payload["head_sha"] != "abc123" {
		t.Errorf("payload twins missing: %v", ev.Payload)
	}
}

// Malformed input is handed to the caller's own decoder unchanged, so this
// never turns one decode error into a different one.
func TestMalformedInputPassesThrough(t *testing.T) {
	t.Parallel()
	bad := []byte(`{"createdAt": `)
	if got := reconcileWireKeys(bad); string(got) != string(bad) {
		t.Errorf("malformed input was rewritten: %s", got)
	}
	var out map[string]any
	if err := UnmarshalTrailWire(bad, &out); err == nil {
		t.Error("want a decode error for malformed input")
	}
}

func TestDeepNestingIsBounded(t *testing.T) {
	t.Parallel()
	deep := []byte(`{"aB":` + nest(compatWalkDepthLimit+10) + `}`)
	var out map[string]any
	if err := UnmarshalTrailWire(deep, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := out["a_b"]; !ok {
		t.Error("top-level key should still gain its twin")
	}
}

func nest(depth int) string {
	if depth == 0 {
		return `1`
	}
	return `{"xY":` + nest(depth-1) + `}`
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestReconcileIsIdempotent(t *testing.T) {
	t.Parallel()
	once := reconcileWireKeys([]byte(`{"createdAt":"x","nested":{"headSha":"y"}}`))
	twice := reconcileWireKeys(once)
	var a, b map[string]any
	if err := json.Unmarshal(once, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(twice, &b); err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Errorf("not idempotent: %d keys then %d", len(a), len(b))
	}
}

// A camelCase run inside a string VALUE is not a key and must not trigger the
// rewrite.
func TestCamelCaseInValueDoesNotTrigger(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"branch":"feat/addFooBar","title":"Fix parseJSON crash"}`)
	if got := reconcileWireKeys(raw); string(got) != string(raw) {
		t.Errorf("value-only camelCase triggered a rewrite:\n got %s\nwant %s", got, raw)
	}
}

// Re-encoding must not round-trip numbers through float64.
func TestLargeNumbersSurviveReconciliation(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"someKey":1,"bigId":9007199254740993,"exp":1e3}`)
	out := reconcileWireKeys(raw)
	for _, want := range []string{"9007199254740993", "1e3"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("number %s did not round-trip; got %s", want, out)
		}
	}
}

// The encoded document must stay proportional to the input: aliasing a key to
// its twin would share the child pointer and serialize each subtree once per
// alias, doubling per level of nesting.
func TestNestedCamelCaseDoesNotInflate(t *testing.T) {
	t.Parallel()
	raw := []byte(nest(16))
	out := reconcileWireKeys(raw)
	if len(out) > 4*len(raw) {
		t.Errorf("encoded %d bytes from %d: output should stay proportional", len(out), len(raw))
	}
}
