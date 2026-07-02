package redact

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// configureFakeOPF wires up the runtime and registers cleanup so each
// test starts and ends with a clean config. Returns the fake so
// individual tests can assert call counts. (opfStderr is silenced
// process-wide in TestMain; see global_test.go.)
func configureFakeOPF(t *testing.T, fake *fakeRuntime, cats map[string]bool) {
	t.Helper()
	resetOPFConfig()
	t.Cleanup(resetOPFConfig)
	ConfigurePrivacyFilterWithRuntime(OPFConfig{
		Enabled:    true,
		Categories: cats,
	}, fake)
}

// TestBatchBytesWithPrivacyFilter_BatchesSingleCall pins the headline
// contract: N blobs across multiple shapes (JSONL, plain JSON, raw text)
// produce exactly ONE RedactBatch invocation. This is what the pre-push
// rewrite walker depends on for its ~6×–9× wall-clock win.
func TestBatchBytesWithPrivacyFilter_BatchesSingleCall(t *testing.T) {
	fake := &fakeRuntime{spans: []Span{{Start: 0, End: 5, Label: "private_person"}}}
	configureFakeOPF(t, fake, map[string]bool{"private_person": true})

	inputs := []NamedBlob{
		{Name: "full.jsonl", Content: []byte(`{"content":"Alice met Bob"}` + "\n" + `{"content":"Charlie sat down"}`)},
		{Name: "metadata.json", Content: []byte(`{"summary":"Eve walked home"}`)},
		{Name: "prompt.txt", Content: []byte("Frank reviewed the diff")},
	}

	_, err := BatchBytesWithPrivacyFilter(context.Background(), inputs)
	if err != nil {
		t.Fatalf("BatchBytesWithPrivacyFilter: %v", err)
	}
	if fake.batchCalls != 1 {
		t.Errorf("want exactly 1 RedactBatch call across %d blobs, got %d", len(inputs), fake.batchCalls)
	}
	if fake.calls != 0 {
		t.Errorf("want 0 single-input Redact calls, got %d", fake.calls)
	}
}

// TestBatchBytesWithPrivacyFilter_PreservesInputOrder confirms that
// output[i] corresponds to inputs[i]. Without this, a multi-blob walker
// couldn't safely use a parallel slice to look up redacted bytes by index.
func TestBatchBytesWithPrivacyFilter_PreservesInputOrder(t *testing.T) {
	fake := &fakeRuntime{} // empty spans; we're checking order, not content
	configureFakeOPF(t, fake, map[string]bool{"private_person": true})

	inputs := []NamedBlob{
		{Name: "a.txt", Content: []byte("alpha content here")},
		{Name: "b.txt", Content: []byte("beta content here")},
		{Name: "c.txt", Content: []byte("gamma content here")},
	}

	got, err := BatchBytesWithPrivacyFilter(context.Background(), inputs)
	if err != nil {
		t.Fatalf("BatchBytesWithPrivacyFilter: %v", err)
	}
	if len(got) != len(inputs) {
		t.Fatalf("want %d outputs, got %d", len(inputs), len(got))
	}
	wantPrefixes := []string{"alpha", "beta", "gamma"}
	for i, prefix := range wantPrefixes {
		if !strings.HasPrefix(string(got[i]), prefix) {
			t.Errorf("output[%d] = %q, want prefix %q", i, string(got[i]), prefix)
		}
	}
}

// TestBatchBytesWithPrivacyFilter_AppliesSpansToJSON verifies that OPF
// spans returned by the batch call are applied back to the right leaves
// inside a JSON-shaped blob, and that the surrounding JSON structure
// survives. This is the load-bearing assertion for the metadata.json
// redaction path that PR 1236 specifically extended.
func TestBatchBytesWithPrivacyFilter_AppliesSpansToJSON(t *testing.T) {
	// "Alice" is bytes 0..5 of "Alice met Bob"; the fake returns that span
	// for every input, so all leaves get redacted at the same offset.
	fake := &fakeRuntime{spans: []Span{{Start: 0, End: 5, Label: "private_person"}}}
	configureFakeOPF(t, fake, map[string]bool{"private_person": true})

	inputs := []NamedBlob{
		{Name: "metadata.json", Content: []byte(`{"summary":"Alice met Bob","id":"keep-this"}`)},
	}
	got, err := BatchBytesWithPrivacyFilter(context.Background(), inputs)
	if err != nil {
		t.Fatalf("BatchBytesWithPrivacyFilter: %v", err)
	}
	out := string(got[0])
	if !strings.Contains(out, "[REDACTED_PERSON]") {
		t.Errorf("expected [REDACTED_PERSON] tag, got %q", out)
	}
	// The id field has no space, so the has-space gate excludes it; OPF
	// never sees it. The regex layers also leave it alone for this input.
	if !strings.Contains(out, `"keep-this"`) {
		t.Errorf("non-prose id field should survive, got %q", out)
	}
}

// TestBatchBytesWithPrivacyFilter_AppliesSpansToRawText pins the
// raw-text path (.txt blobs, prompt files): the whole content is
// treated as one leaf and gets the cached spans applied. Without this,
// the walker would silently skip OPF redaction on prompt files.
func TestBatchBytesWithPrivacyFilter_AppliesSpansToRawText(t *testing.T) {
	fake := &fakeRuntime{spans: []Span{{Start: 0, End: 5, Label: "private_person"}}}
	configureFakeOPF(t, fake, map[string]bool{"private_person": true})

	inputs := []NamedBlob{
		{Name: "prompt.txt", Content: []byte("Alice met Bob in the lobby")},
	}
	got, err := BatchBytesWithPrivacyFilter(context.Background(), inputs)
	if err != nil {
		t.Fatalf("BatchBytesWithPrivacyFilter: %v", err)
	}
	if !strings.Contains(string(got[0]), "[REDACTED_PERSON]") {
		t.Errorf("expected [REDACTED_PERSON] in raw-text output, got %q", string(got[0]))
	}
}

// recordingRuntime captures the slice passed to RedactBatch so a test
// can assert exact dedup behavior (not just call count). Independent
// of fakeRuntime so its presence doesn't change unrelated tests.
type recordingRuntime struct {
	spans      []Span
	lastInputs []string
	calls      int
}

func (r *recordingRuntime) Redact(_ context.Context, _ string, _ []string) ([]Span, error) {
	return r.spans, nil
}

func (r *recordingRuntime) RedactBatch(_ context.Context, inputs []string, _ []string) ([][]Span, error) {
	r.calls++
	r.lastInputs = append([]string(nil), inputs...)
	out := make([][]Span, len(inputs))
	for i := range inputs {
		out[i] = r.spans
	}
	return out, nil
}

// TestBatchBytesWithPrivacyFilter_DedupsLeavesAcrossBlobs confirms that
// the same leaf string appearing in N blobs is sent to OPF as ONE
// input, not N. Without this, a transcript that quotes the same prompt
// in 10 places would inflate the batch unnecessarily.
func TestBatchBytesWithPrivacyFilter_DedupsLeavesAcrossBlobs(t *testing.T) {
	rt := &recordingRuntime{spans: []Span{{Start: 0, End: 5, Label: "private_person"}}}
	resetOPFConfig()
	t.Cleanup(resetOPFConfig)
	ConfigurePrivacyFilterWithRuntime(OPFConfig{
		Enabled:    true,
		Categories: map[string]bool{"private_person": true},
	}, rt)

	shared := "Alice met Bob in the lobby"
	inputs := []NamedBlob{
		{Name: "a.jsonl", Content: []byte(`{"text":"` + shared + `"}`)},
		{Name: "b.jsonl", Content: []byte(`{"text":"` + shared + `"}`)},
		{Name: "c.txt", Content: []byte(shared)},
	}
	_, err := BatchBytesWithPrivacyFilter(context.Background(), inputs)
	if err != nil {
		t.Fatalf("BatchBytesWithPrivacyFilter: %v", err)
	}
	if rt.calls != 1 {
		t.Errorf("want 1 batch call, got %d", rt.calls)
	}
	if got := len(rt.lastInputs); got != 1 {
		t.Errorf("want dedup to collapse 3 identical leaves into 1 batch input, got %d (inputs: %v)", got, rt.lastInputs)
	}
	if len(rt.lastInputs) == 1 && rt.lastInputs[0] != shared {
		t.Errorf("want dedup'd input = %q, got %q", shared, rt.lastInputs[0])
	}
}

// TestBatchBytesWithPrivacyFilter_EmptyInputs is a degenerate case the
// walker hits when a push has no unpushed commits or every commit is
// already OPF-applied. Must not crash and must not invoke OPF.
func TestBatchBytesWithPrivacyFilter_EmptyInputs(t *testing.T) {
	fake := &fakeRuntime{}
	configureFakeOPF(t, fake, map[string]bool{"private_person": true})

	got, err := BatchBytesWithPrivacyFilter(context.Background(), nil)
	if err != nil {
		t.Fatalf("nil inputs: %v", err)
	}
	if got != nil {
		t.Errorf("want nil output for nil input, got %v", got)
	}
	if fake.batchCalls != 0 {
		t.Errorf("want 0 OPF calls for nil input, got %d", fake.batchCalls)
	}
}

// TestBatchBytesWithPrivacyFilter_FailsClosedOnBatchError is the
// fail-closed contract: when the OPF runtime errors, callers must see
// the error rather than silently get 7-layer-only output tagged as if
// OPF ran. This is the privacy-critical difference vs
// JSONLContentWithPrivacyFilter (which silently falls back).
func TestBatchBytesWithPrivacyFilter_FailsClosedOnBatchError(t *testing.T) {
	fake := &fakeRuntime{err: errors.New("simulated opf runtime failure")}
	configureFakeOPF(t, fake, map[string]bool{"private_person": true})

	inputs := []NamedBlob{
		{Name: "x.jsonl", Content: []byte(`{"text":"Alice met Bob"}`)},
	}
	got, err := BatchBytesWithPrivacyFilter(context.Background(), inputs)
	if err == nil {
		t.Fatal("want non-nil error on OPF batch failure, got nil")
	}
	if got != nil {
		t.Errorf("want nil output on batch failure, got %v", got)
	}
	if !strings.Contains(err.Error(), "opf batch failed") {
		t.Errorf("want error mentioning 'opf batch failed', got %q", err.Error())
	}
}

// shortReturnBatchRuntime returns FEWER span slices than inputs — a
// runtime contract violation that, if silently accepted, would leave
// the tail leaves un-redacted while the rewrite ships the commits
// tagged Entire-OPF-Applied: true.
type shortReturnBatchRuntime struct{ batchCalls int }

func (r *shortReturnBatchRuntime) Redact(_ context.Context, _ string, _ []string) ([]Span, error) {
	return nil, nil
}

func (r *shortReturnBatchRuntime) RedactBatch(_ context.Context, inputs []string, _ []string) ([][]Span, error) {
	r.batchCalls++
	if len(inputs) == 0 {
		return nil, nil
	}
	// Return exactly ONE span slice regardless of input count — the
	// violation we're testing for.
	return [][]Span{nil}, nil
}

// TestBatchBytesWithPrivacyFilter_ShortReturnFailsClosed pins the
// fail-closed contract for the runtime short-return case. Production
// shell-outs always return len(inputs); this guards against any future
// runtime (daemon, gRPC, mocked partial-failure scenario) producing
// fewer slices and the caller silently treating the missing tail as
// "no PII found." The fix trips the breaker AND returns an error so
// the orchestrator's two-layer abort fires either way.
func TestBatchBytesWithPrivacyFilter_ShortReturnFailsClosed(t *testing.T) {
	fake := &shortReturnBatchRuntime{}
	resetOPFConfig()
	t.Cleanup(resetOPFConfig)
	ConfigurePrivacyFilterWithRuntime(OPFConfig{
		Enabled:    true,
		Categories: map[string]bool{"private_person": true},
	}, fake)

	inputs := []NamedBlob{
		{Name: "a.jsonl", Content: []byte(`{"text":"Alice met Bob"}`)},
		{Name: "b.jsonl", Content: []byte(`{"text":"Charlie sat down"}`)},
		{Name: "c.jsonl", Content: []byte(`{"text":"Eve walked home"}`)},
	}
	_, err := BatchBytesWithPrivacyFilter(context.Background(), inputs)
	if err == nil {
		t.Fatal("short return must produce a non-nil error, got nil")
	}
	if !strings.Contains(err.Error(), "short return") {
		t.Errorf("want error mentioning 'short return', got %q", err.Error())
	}
	if !opfBreakerTripped.Load() {
		t.Error("short return must trip the breaker so future calls in this process skip OPF")
	}
}

// TestBatchBytesWithPrivacyFilter_OPFDisabledReturns7Layer covers the
// "OPF turned off in settings" path: every blob gets regex-only
// redaction, no shell-out happens, no error. Without this, a user with
// OPF disabled would get a hard error from the new API instead of the
// fast 7-layer path they expect.
func TestBatchBytesWithPrivacyFilter_OPFDisabledReturns7Layer(t *testing.T) {
	resetOPFConfig()
	t.Cleanup(resetOPFConfig)
	// No ConfigurePrivacyFilter call → cfg == nil

	inputs := []NamedBlob{
		{Name: "x.jsonl", Content: []byte(`{"text":"key=AKIAYRWQG5EJLPZLBYNP"}`)},
	}
	got, err := BatchBytesWithPrivacyFilter(context.Background(), inputs)
	if err != nil {
		t.Fatalf("OPF-disabled path should not error: %v", err)
	}
	if !strings.Contains(string(got[0]), "REDACTED") {
		t.Errorf("7-layer fallback should still redact AWS key, got %q", string(got[0]))
	}
}

// TestBatchBytesWithPrivacyFilter_BreakerTrippedReturns7Layer ensures
// that once the circuit breaker has tripped (e.g. an earlier batch
// failed and the strategy aborted), subsequent calls in the same
// process don't pay another shell-out cost. They short-circuit to
// regex-only with no error.
func TestBatchBytesWithPrivacyFilter_BreakerTrippedReturns7Layer(t *testing.T) {
	fake := &fakeRuntime{spans: []Span{{Start: 0, End: 5, Label: "private_person"}}}
	configureFakeOPF(t, fake, map[string]bool{"private_person": true})
	opfBreakerTripped.Store(true)
	t.Cleanup(func() { opfBreakerTripped.Store(false) })

	inputs := []NamedBlob{
		{Name: "x.jsonl", Content: []byte(`{"text":"Alice met Bob"}`)},
	}
	_, err := BatchBytesWithPrivacyFilter(context.Background(), inputs)
	if err != nil {
		t.Fatalf("breaker-tripped path should not error: %v", err)
	}
	if fake.batchCalls != 0 {
		t.Errorf("want 0 OPF calls when breaker tripped, got %d", fake.batchCalls)
	}
}

// TestBatchBytesWithPrivacyFilter_MatchesPerBlobOutput is the most
// important correctness assertion at this layer. The pre-push rewrite
// will swap from N calls to JSONLBytesWithPrivacyFilter/BytesWithPrivacyFilter
// (one per blob) to a single BatchBytesWithPrivacyFilter call. If the
// batched output differs even slightly from the per-blob output, the
// rewrite would write subtly different blobs to the v1 ref — invisible
// in unit tests, visible only as "redacted content shifted" in production.
//
// This test runs both paths against the same inputs with the same fake
// runtime and asserts byte-identical output per blob. The fake returns
// the same span for every input, so any divergence here is purely from
// span application or JSONL walking, not from runtime non-determinism.
func TestBatchBytesWithPrivacyFilter_MatchesPerBlobOutput(t *testing.T) {
	cats := map[string]bool{"private_person": true}
	inputs := []NamedBlob{
		{Name: "full.jsonl", Content: []byte(`{"content":"Alice met Bob in the lobby"}` + "\n" + `{"content":"Charlie sat at the table"}`)},
		{Name: "metadata.json", Content: []byte(`{"summary":"Eve walked home tonight","id":"keep-this"}`)},
		{Name: "prompt.txt", Content: []byte("Frank reviewed the diff this morning")},
		{Name: "duplicate.jsonl", Content: []byte(`{"content":"Alice met Bob in the lobby"}`)}, // same leaf as full.jsonl
	}

	// Per-blob baseline: route each blob through the existing API the
	// same way RedactBlobBytes does in cmd/entire/cli/checkpoint/.
	fake1 := &fakeRuntime{spans: []Span{{Start: 0, End: 5, Label: "private_person"}}}
	configureFakeOPF(t, fake1, cats)
	wantPerBlob := make([][]byte, len(inputs))
	for i, in := range inputs {
		if isJSONLikeName(in.Name) {
			redacted, err := JSONLBytesWithPrivacyFilter(context.Background(), in.Content)
			if err != nil {
				t.Fatalf("per-blob JSONL[%d]: %v", i, err)
			}
			wantPerBlob[i] = redacted.Bytes()
		} else {
			wantPerBlob[i] = BytesWithPrivacyFilter(context.Background(), in.Content)
		}
	}

	// Batched run with a fresh fake (configureFakeOPF resets config).
	fake2 := &fakeRuntime{spans: []Span{{Start: 0, End: 5, Label: "private_person"}}}
	configureFakeOPF(t, fake2, cats)
	gotBatched, err := BatchBytesWithPrivacyFilter(context.Background(), inputs)
	if err != nil {
		t.Fatalf("batched: %v", err)
	}

	if len(gotBatched) != len(wantPerBlob) {
		t.Fatalf("output length mismatch: batched=%d per-blob=%d", len(gotBatched), len(wantPerBlob))
	}
	for i := range inputs {
		if string(gotBatched[i]) != string(wantPerBlob[i]) {
			t.Errorf("blob[%d] (%s) output differs:\n  batched:  %q\n  per-blob: %q",
				i, inputs[i].Name, string(gotBatched[i]), string(wantPerBlob[i]))
		}
	}

	// Bonus assertion: batching collapsed 4 blobs into 1 call, while
	// per-blob made exactly 4 batch calls — one per JSON-shaped blob
	// (full.jsonl, metadata.json, duplicate.jsonl), plus one for the
	// .txt blob's single-string path. The singular path deliberately
	// routes through RedactBatch too so short returns trip the breaker.
	// Within a JSON blob, leaves are deduped, but across blobs in the
	// per-blob path they aren't — that's the inefficiency the batched
	// API addresses.
	if fake2.batchCalls != 1 {
		t.Errorf("want exactly 1 batched call, got %d", fake2.batchCalls)
	}
	if fake1.batchCalls != 4 {
		t.Errorf("want exactly 4 per-blob batch calls, got %d", fake1.batchCalls)
	}
}

// TestSumProseLeafBytes_CountsAcrossBlobShapes covers the helper the
// strategy uses to enforce ENTIRE_OPF_BATCH_LIMIT. The number must
// reflect what would actually go to OPF — so the has-space gate
// excludes non-prose, JSON-parsed leaves are counted individually,
// and raw text blobs are counted whole.
func TestSumProseLeafBytes_CountsAcrossBlobShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		inputs []NamedBlob
		want   int
	}{
		{
			name: "empty_input_zero",
			want: 0,
		},
		{
			name: "single_jsonl_prose_leaves_only",
			inputs: []NamedBlob{
				{Name: "a.jsonl", Content: []byte(`{"id":"non-prose","content":"Alice met Bob"}`)},
			},
			// "Alice met Bob" = 13 bytes (has space → counted). "non-prose" has no
			// space → excluded.
			want: 13,
		},
		{
			name: "json_metadata_with_id_field_skips_id",
			inputs: []NamedBlob{
				{Name: "metadata.json", Content: []byte(`{"summary":"Eve walked home","id":"keep-this"}`)},
			},
			want: 15, // "Eve walked home"
		},
		{
			name: "raw_text_blob_counted_whole",
			inputs: []NamedBlob{
				{Name: "prompt.txt", Content: []byte("Find Frank Smith here")}, // 21 bytes
			},
			want: 21,
		},
		{
			name: "raw_text_blob_no_space_excluded",
			inputs: []NamedBlob{
				{Name: "id.txt", Content: []byte("abc123-no-spaces-here")},
			},
			want: 0,
		},
		{
			name: "multi_blob_sums_each",
			inputs: []NamedBlob{
				{Name: "a.jsonl", Content: []byte(`{"content":"Alice met Bob"}`)},
				{Name: "b.txt", Content: []byte("Charlie sat down")}, // 16 bytes
			},
			want: 13 + 16,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := SumProseLeafBytes(tc.inputs)
			if got != tc.want {
				t.Errorf("SumProseLeafBytes(%s) = %d, want %d", tc.name, got, tc.want)
			}
		})
	}
}

// TestBatchBytesWithPrivacyFilter_NoEnabledCategoriesReturns7Layer
// covers the configuration edge case where OPF is enabled but every
// category is turned off — the model has nothing to look for, so we
// skip the shell-out entirely and return regex-only output.
func TestBatchBytesWithPrivacyFilter_NoEnabledCategoriesReturns7Layer(t *testing.T) {
	fake := &fakeRuntime{}
	// Empty categories — Configure call still wires the runtime but
	// enabledCategories returns nothing.
	configureFakeOPF(t, fake, map[string]bool{})

	inputs := []NamedBlob{
		{Name: "x.jsonl", Content: []byte(`{"text":"Alice met Bob"}`)},
	}
	_, err := BatchBytesWithPrivacyFilter(context.Background(), inputs)
	if err != nil {
		t.Fatalf("no-categories path should not error: %v", err)
	}
	if fake.batchCalls != 0 {
		t.Errorf("want 0 OPF calls when no categories enabled, got %d", fake.batchCalls)
	}
}
