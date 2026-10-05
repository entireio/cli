package redact

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// shCmd returns a Cmd that runs the given shell snippet via `sh -c`. Used
// by tests to simulate the `opf` binary's behavior without needing it
// installed.
func shCmd(ctx context.Context, script string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", script)
}

// fakeRuntime is the standard test double for opfRuntime. Records call
// counts so tests can assert "single batched call, not N per-leaf" contracts.
type fakeRuntime struct {
	mu         sync.Mutex
	spans      []Span
	err        error
	calls      int
	batchCalls int
}

func (f *fakeRuntime) Redact(_ context.Context, _ string, _ []string) ([]Span, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.spans, f.err
}

func (f *fakeRuntime) RedactBatch(_ context.Context, inputs []string, _ []string) ([][]Span, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batchCalls++
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]Span, len(inputs))
	for i := range inputs {
		out[i] = f.spans
	}
	return out, nil
}

func TestConfigurePrivacyFilter_StoresConfig(t *testing.T) {
	resetOPFConfig()
	t.Cleanup(resetOPFConfig)

	ConfigurePrivacyFilter(OPFConfig{
		Enabled:    true,
		Categories: map[string]bool{"private_person": true},
		Command:    "/usr/local/bin/opf",
		Timeout:    45,
	})

	got := getOPFConfig()
	if got == nil {
		t.Fatal("getOPFConfig returned nil")
	}
	if !got.Enabled || !got.Categories["private_person"] || got.Command != "/usr/local/bin/opf" || got.Timeout != 45 {
		t.Errorf("config not stored verbatim: %+v", got)
	}
	if got.runtime == nil {
		t.Error("runtime was not constructed")
	}
}

func TestConfigurePrivacyFilter_AppliesDefaults(t *testing.T) {
	resetOPFConfig()
	t.Cleanup(resetOPFConfig)

	ConfigurePrivacyFilter(OPFConfig{Enabled: true})
	got := getOPFConfig()
	if got.Command != "opf" {
		t.Errorf("default Command: want \"opf\", got %q", got.Command)
	}
	if got.Timeout != 30 {
		t.Errorf("default Timeout: want 30, got %d", got.Timeout)
	}
}

func TestIsKnownOPFCategory(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"private_person":  true,
		"private_email":   true,
		"secret":          true,
		"account_number":  true,
		"private_peerson": false,
		"":                false,
		"PII":             false,
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := IsKnownOPFCategory(name); got != want {
				t.Errorf("IsKnownOPFCategory(%q) = %v, want %v", name, got, want)
			}
		})
	}
}

func TestDetectOPF_DisabledReturnsNil(t *testing.T) {
	resetOPFConfig()
	t.Cleanup(resetOPFConfig)

	fake := &fakeRuntime{spans: []Span{{Start: 0, End: 5, Label: "private_person"}}}
	ConfigurePrivacyFilterWithRuntime(OPFConfig{
		Enabled:    false,
		Categories: map[string]bool{"private_person": true},
	}, fake)

	if got := detectOPF(context.Background(), getOPFConfig(), "Alice met Bob"); got != nil {
		t.Errorf("disabled OPF should return nil, got %v", got)
	}
	if fake.calls+fake.batchCalls != 0 {
		t.Errorf("disabled OPF invoked runtime calls=%d batchCalls=%d, want 0", fake.calls, fake.batchCalls)
	}
}

func TestDetectOPF_MapsLabelsCorrectly(t *testing.T) {
	resetOPFConfig()
	t.Cleanup(resetOPFConfig)

	fake := &fakeRuntime{
		spans: []Span{
			{Start: 0, End: 5, Label: "private_person"},
			{Start: 6, End: 9, Label: "private_email"},
		},
	}
	ConfigurePrivacyFilterWithRuntime(OPFConfig{
		Enabled: true,
		Categories: map[string]bool{
			"private_person": true,
			"private_email":  true,
		},
	}, fake)

	regions := detectOPF(context.Background(), getOPFConfig(), "Alice a@b.io test")
	if len(regions) != 2 {
		t.Fatalf("want 2 regions, got %d: %v", len(regions), regions)
	}
	if regions[0].label != "PERSON" {
		t.Errorf("region[0].label: want PERSON, got %q", regions[0].label)
	}
	if regions[1].label != "EMAIL" {
		t.Errorf("region[1].label: want EMAIL, got %q", regions[1].label)
	}
}

func TestDetectOPF_SkipsCategoriesNotEnabled(t *testing.T) {
	resetOPFConfig()
	t.Cleanup(resetOPFConfig)

	fake := &fakeRuntime{
		spans: []Span{
			{Start: 0, End: 5, Label: "private_person"},
			{Start: 6, End: 9, Label: "private_email"}, // not enabled
		},
	}
	ConfigurePrivacyFilterWithRuntime(OPFConfig{
		Enabled:    true,
		Categories: map[string]bool{"private_person": true},
	}, fake)

	regions := detectOPF(context.Background(), getOPFConfig(), "Alice a@b.io test")
	if len(regions) != 1 || regions[0].label != "PERSON" {
		t.Errorf("want only PERSON region, got %v", regions)
	}
}

func TestDetectOPF_SkipsNonProseStrings(t *testing.T) {
	resetOPFConfig()
	t.Cleanup(resetOPFConfig)

	fake := &fakeRuntime{spans: []Span{{Start: 0, End: 5, Label: "private_person"}}}
	ConfigurePrivacyFilterWithRuntime(OPFConfig{
		Enabled:    true,
		Categories: map[string]bool{"private_person": true},
	}, fake)

	cases := []string{
		"src/main.go",
		"user_prompt_submit",
		"tool-use-id",
		"a3b2c4d5e6f7",
		"Alice", // single token, no space
	}
	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			callsBefore := fake.calls + fake.batchCalls
			if got := detectOPF(context.Background(), getOPFConfig(), tc); got != nil {
				t.Errorf("non-prose %q: want nil, got %v", tc, got)
			}
			if fake.calls+fake.batchCalls != callsBefore {
				t.Errorf("non-prose %q: runtime invoked (calls %d → %d)",
					tc, callsBefore, fake.calls+fake.batchCalls)
			}
		})
	}
}

func TestDetectOPF_CircuitBreaker(t *testing.T) {
	resetOPFConfig()
	t.Cleanup(resetOPFConfig)

	fake := &fakeRuntime{err: errors.New("simulated opf failure")}
	ConfigurePrivacyFilterWithRuntime(OPFConfig{
		Enabled:    true,
		Categories: map[string]bool{"private_person": true},
	}, fake)

	// First call hits the runtime and observes the failure.
	if got := detectOPF(context.Background(), getOPFConfig(), "Alice met Bob"); got != nil {
		t.Fatalf("first call: want nil, got %v", got)
	}
	if fake.batchCalls != 1 {
		t.Fatalf("first call: want 1 batch invocation, got %d", fake.batchCalls)
	}

	// Subsequent calls short-circuit before invoking the runtime.
	for i := range 5 {
		if got := detectOPF(context.Background(), getOPFConfig(), "Charlie sat next to Diane"); got != nil {
			t.Errorf("call %d after breaker: want nil, got %v", i+2, got)
		}
	}
	if fake.batchCalls != 1 {
		t.Errorf("breaker did not engage: %d batch invocations, want 1", fake.batchCalls)
	}

	// Reconfiguring resets the breaker.
	ConfigurePrivacyFilterWithRuntime(OPFConfig{
		Enabled:    true,
		Categories: map[string]bool{"private_person": true},
	}, fake)
	if got := detectOPF(context.Background(), getOPFConfig(), "Eve walked home"); got != nil {
		t.Fatal("after reconfigure: want nil (still failing fake), got non-nil")
	}
	if fake.batchCalls != 2 {
		t.Errorf("after reconfigure: want 2 total batch invocations, got %d", fake.batchCalls)
	}
}

type emptyBatchRuntime struct{}

func (r *emptyBatchRuntime) Redact(_ context.Context, _ string, _ []string) ([]Span, error) {
	return nil, nil
}

func (r *emptyBatchRuntime) RedactBatch(_ context.Context, _ []string, _ []string) ([][]Span, error) {
	return nil, nil
}

func TestDetectOPF_SingleInputShortReturnTripsBreaker(t *testing.T) {
	resetOPFConfig()
	t.Cleanup(resetOPFConfig)

	ConfigurePrivacyFilterWithRuntime(OPFConfig{
		Enabled:    true,
		Categories: map[string]bool{"private_person": true},
	}, &emptyBatchRuntime{})

	if got := detectOPF(context.Background(), getOPFConfig(), "Alice met Bob"); got != nil {
		t.Fatalf("short return should fall back to nil regions, got %v", got)
	}
	if !opfBreakerTripped.Load() {
		t.Error("single-input short return must trip the OPF breaker")
	}
}

func TestShellOut_BatchSingleInferencePass(t *testing.T) {
	t.Parallel()
	var calls int
	var mu sync.Mutex
	rt := &shellOut{
		command:        "opf",
		timeoutSeconds: 5,
		commandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			mu.Lock()
			calls++
			mu.Unlock()
			return shCmd(ctx, `printf '{"detected_spans":[]}'`)
		},
	}
	_, err := rt.RedactBatch(context.Background(), []string{"hello world", "foo bar baz"}, []string{"private_person"})
	if err != nil {
		t.Fatalf("RedactBatch: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("want 1 shell-out for 2 inputs, got %d", calls)
	}
}

func TestShellOut_RedactBatchSanitizesSeparatorCollisions(t *testing.T) {
	t.Parallel()
	capture := filepath.Join(t.TempDir(), "stdin")
	rt := &shellOut{
		command:        "opf",
		timeoutSeconds: 5,
		commandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "sh", "-c", `cat > "$1"; printf '{"detected_spans":[]}'`, "sh", capture)
		},
	}

	_, err := rt.RedactBatch(context.Background(),
		[]string{"alpha" + opfBatchSeparator + "beta", "gamma delta"},
		[]string{"private_person"},
	)
	if err != nil {
		t.Fatalf("RedactBatch: %v", err)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read captured stdin: %v", err)
	}
	got := string(data)
	if count := strings.Count(got, opfBatchSeparator); count != 1 {
		t.Fatalf("batched stdin should contain only the inter-input separator, got %d in %q", count, got)
	}
	if !strings.Contains(got, "alpha beta") {
		t.Fatalf("input separator collision should be replaced with a space, got %q", got)
	}
}

func TestShellOut_RedactBatchRejectsOversizedInputBeforeCommand(t *testing.T) {
	t.Parallel()
	called := false
	rt := &shellOut{
		command:        "opf",
		timeoutSeconds: 5,
		commandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			called = true
			return shCmd(ctx, `cat >/dev/null; printf '{"detected_spans":[]}'`)
		},
	}

	_, err := rt.RedactBatch(context.Background(),
		[]string{strings.Repeat("x", 16*1024*1024+1)},
		[]string{"private_person"},
	)
	if err == nil {
		t.Fatal("RedactBatch: want oversized input error, got nil")
	}
	if !strings.Contains(err.Error(), "opf input too large") {
		t.Fatalf("RedactBatch error = %v, want input size message", err)
	}
	if called {
		t.Fatal("oversized input should be rejected before starting opf")
	}
}

func TestShellOut_RedactBatchCapsStdout(t *testing.T) {
	t.Parallel()
	rt := &shellOut{
		command:        "opf",
		timeoutSeconds: 5,
		commandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "sh", "-c", `head -c 1048577 /dev/zero | tr '\0' x`)
		},
	}

	_, err := rt.RedactBatch(context.Background(), []string{"hello world"}, []string{"private_person"})
	if err == nil {
		t.Fatal("RedactBatch: want stdout cap error, got nil")
	}
	if !strings.Contains(err.Error(), "opf stdout exceeded") {
		t.Fatalf("RedactBatch error = %v, want stdout cap message", err)
	}
}

func TestShellOut_RedactBatchCapsStderr(t *testing.T) {
	t.Parallel()
	rt := &shellOut{
		command:        "opf",
		timeoutSeconds: 5,
		commandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "sh", "-c", `head -c 1048577 /dev/zero | tr '\0' x >&2; exit 7`)
		},
	}

	_, err := rt.RedactBatch(context.Background(), []string{"hello world"}, []string{"private_person"})
	if err == nil {
		t.Fatal("RedactBatch: want stderr cap error, got nil")
	}
	if !strings.Contains(err.Error(), "opf stderr exceeded") {
		t.Fatalf("RedactBatch error = %v, want stderr cap message", err)
	}
}

func TestShellOut_ExitError_DoesNotLeakStderr(t *testing.T) {
	t.Parallel()
	const secret = "Alice met Bob at 555-867-5309"
	rt := &shellOut{
		command:        "opf",
		timeoutSeconds: 5,
		commandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			// Echo input to stderr, then exit non-zero — simulates a
			// hostile or misconfigured opf wrapper.
			return shCmd(ctx, "printf '%s' '"+secret+"' >&2; exit 7")
		},
	}
	_, err := rt.Redact(context.Background(), secret, []string{"private_person"})
	if err == nil {
		t.Fatal("Redact: want error, got nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error leaks stderr content: %v", err)
	}
	if !strings.Contains(err.Error(), "bytes on stderr") {
		t.Errorf("error should mention stderr byte count: %v", err)
	}
}

func TestShellOut_ParseError_DoesNotLeakStdout(t *testing.T) {
	t.Parallel()
	const secret = "Alice met Bob"
	rt := &shellOut{
		command:        "opf",
		timeoutSeconds: 5,
		commandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return shCmd(ctx, "printf '%s' 'not json: "+secret+"'")
		},
	}
	_, err := rt.Redact(context.Background(), secret, []string{"private_person"})
	if err == nil {
		t.Fatal("Redact: want parse error, got nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("parse error leaks stdout content: %v", err)
	}
}

func TestCharToByteOffset(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		s       string
		charOff int
		want    int
	}{
		// ASCII: 5 runes, 5 bytes.
		{"ascii_start", "hello", 0, 0},
		{"ascii_mid", "hello", 3, 3},
		{"ascii_end_runecount", "hello", 5, 5},
		{"ascii_past_end", "hello", 6, -1},
		{"ascii_way_past", "hello", 100, -1},
		{"negative", "hello", -1, -1},
		// UTF-8: 3 runes, 9 bytes (3 × 3-byte box-drawing chars).
		{"utf8_start", "───", 0, 0},
		{"utf8_after_first", "───", 1, 3},
		{"utf8_end_runecount", "───", 3, 9},
		{"utf8_past_end", "───", 4, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := charToByteOffset(tc.s, tc.charOff); got != tc.want {
				t.Errorf("charToByteOffset(%q, %d) = %d, want %d", tc.s, tc.charOff, got, tc.want)
			}
		})
	}
}

func TestSplitBatchSpan(t *testing.T) {
	t.Parallel()
	// Three inputs joined by 1-byte separator: starts = [0, 12, 24]
	//   "hello world"   bytes 0..11
	//   "\x1e"          byte  11
	//   "foo bar baz"   bytes 12..23
	//   "\x1e"          byte  23
	//   "the cat sat"   bytes 24..35
	starts := []int{0, 12, 24}
	const batchedLen = 35
	cases := []struct {
		name      string
		spanStart int
		spanEnd   int
		want      []batchSpanPiece
	}{
		{"first_input", 0, 5, []batchSpanPiece{{input: 0, start: 0, end: 5}}},
		{"second_input", 12, 15, []batchSpanPiece{{input: 1, start: 0, end: 3}}},
		{"third_input", 24, 30, []batchSpanPiece{{input: 2, start: 0, end: 6}}},
		{"crosses_first_boundary", 5, 15, []batchSpanPiece{{input: 0, start: 5, end: 11}, {input: 1, start: 0, end: 3}}},
		{"crosses_second_boundary", 15, 25, []batchSpanPiece{{input: 1, start: 3, end: 11}, {input: 2, start: 0, end: 1}}},
		{"crosses_every_boundary", 6, 30, []batchSpanPiece{
			{input: 0, start: 6, end: 11}, {input: 1, start: 0, end: 11}, {input: 2, start: 0, end: 6},
		}},
		{"separator_only", 11, 12, nil},
		{"runs_past_last_input", 30, 40, []batchSpanPiece{{input: 2, start: 6, end: 11}}},
		{"negative_start", -1, 5, nil},
		{"zero_length", 5, 5, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := splitBatchSpan(starts, batchedLen, tc.spanStart, tc.spanEnd, len(opfBatchSeparator))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("splitBatchSpan(%d,%d) = %+v, want %+v", tc.spanStart, tc.spanEnd, got, tc.want)
			}
		})
	}
}

// Regression: the model can join the end of one leaf and the start of the next
// into a single span ("Claude Code" + "Alice Johnson ..." read as the name
// "Code Alice Johnson"). Dropping that span left BOTH leaves unredacted under
// an Entire-OPF-Applied trailer. Each input must receive its own piece.
func TestShellOut_RedactBatchSplitsSpanAcrossSeparator(t *testing.T) {
	t.Parallel()
	// Batched text: "Claude Code\x1eAlice Johnson approved". The span covers
	// "Code\x1eAlice Johnson": runes 7..25.
	rt := &shellOut{
		command:        "opf",
		timeoutSeconds: 5,
		commandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return shCmd(ctx, `cat >/dev/null; printf '{"detected_spans":[{"label":"private_person","start":7,"end":25}]}'`)
		},
	}
	got, err := rt.RedactBatch(context.Background(), []string{"Claude Code", "Alice Johnson approved"}, []string{"private_person"})
	if err != nil {
		t.Fatalf("RedactBatch: %v", err)
	}
	want := [][]Span{
		{{Start: 7, End: 11, Label: "private_person"}},
		{{Start: 0, End: 13, Label: "private_person"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RedactBatch spans = %+v, want %+v", got, want)
	}
}

func TestPlainEntryPointsNeverInvokeOPF(t *testing.T) {
	resetOPFConfig()
	t.Cleanup(resetOPFConfig)

	fake := &fakeRuntime{spans: []Span{{Start: 0, End: 5, Label: "private_person"}}}
	ConfigurePrivacyFilterWithRuntime(OPFConfig{
		Enabled:    true,
		Categories: map[string]bool{"private_person": true},
	}, fake)

	_ = String("Alice met Bob in the lobby")
	_ = Bytes([]byte("Alice met Bob in the lobby"))
	if _, err := JSONLContent(`{"role":"user","content":"Alice met Bob in the lobby"}`); err != nil {
		t.Fatalf("JSONLContent: %v", err)
	}
	if _, err := JSONLBytes([]byte(`{"role":"user","content":"Alice met Bob in the lobby"}`)); err != nil {
		t.Fatalf("JSONLBytes: %v", err)
	}

	if fake.calls+fake.batchCalls != 0 {
		t.Errorf("plain entry points invoked OPF: calls=%d batchCalls=%d", fake.calls, fake.batchCalls)
	}
}

func TestStringWithPrivacyFilter_AugmentsRegexLayers(t *testing.T) {
	resetOPFConfig()
	t.Cleanup(resetOPFConfig)

	fake := &fakeRuntime{spans: []Span{{Start: 0, End: 5, Label: "private_person"}}}
	ConfigurePrivacyFilterWithRuntime(OPFConfig{
		Enabled:    true,
		Categories: map[string]bool{"private_person": true},
	}, fake)

	got := StringWithPrivacyFilter(context.Background(), "Alice met Bob in the lobby")
	if !strings.Contains(got, "[REDACTED_PERSON]") {
		t.Errorf("StringWithPrivacyFilter output missing PERSON tag: %q", got)
	}
}

func TestJSONLContentWithPrivacyFilter_BatchesSingleCall(t *testing.T) {
	resetOPFConfig()
	t.Cleanup(resetOPFConfig)

	fake := &fakeRuntime{spans: []Span{{Start: 0, End: 5, Label: "private_person"}}}
	ConfigurePrivacyFilterWithRuntime(OPFConfig{
		Enabled:    true,
		Categories: map[string]bool{"private_person": true},
	}, fake)

	// Three distinct prose-shaped leaves; should hit OPF in exactly one batch.
	content := `{"a":"Alice met Bob","b":"Charlie sat down","c":"Eve walked home","id":"abc123"}`
	if _, err := JSONLContentWithPrivacyFilter(context.Background(), content); err != nil {
		t.Fatalf("JSONLContentWithPrivacyFilter: %v", err)
	}
	if fake.batchCalls != 1 {
		t.Errorf("want exactly 1 RedactBatch call, got %d", fake.batchCalls)
	}
	if fake.calls != 0 {
		t.Errorf("want 0 single-input Redact calls, got %d", fake.calls)
	}
}

func TestJSONLContentWithPrivacyFilter_FallsBackOnBatchError(t *testing.T) {
	resetOPFConfig()
	t.Cleanup(resetOPFConfig)

	fake := &fakeRuntime{err: errors.New("simulated opf failure")}
	ConfigurePrivacyFilterWithRuntime(OPFConfig{
		Enabled:    true,
		Categories: map[string]bool{"private_person": true},
	}, fake)

	content := `{"a":"Alice met Bob"}`
	got, err := JSONLContentWithPrivacyFilter(context.Background(), content)
	if err != nil {
		t.Fatalf("want graceful fallback, got error: %v", err)
	}
	if got == "" {
		t.Error("fallback should still return non-empty content")
	}
}

// shortReturnRuntime returns FEWER span slices than inputs — a runtime
// contract violation that would silently leave the tail leaves
// un-redacted under the old "log warning + proceed" behavior. The fix
// trips the circuit breaker so the pre-push rewrite aborts before
// commits are tagged Entire-OPF-Applied: true.
type shortReturnRuntime struct{}

func (r *shortReturnRuntime) Redact(_ context.Context, _ string, _ []string) ([]Span, error) {
	return nil, nil
}

func (r *shortReturnRuntime) RedactBatch(_ context.Context, inputs []string, _ []string) ([][]Span, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	// Return exactly ONE span slice regardless of input count — the
	// violation we're testing for.
	return [][]Span{nil}, nil
}

// TestJSONLContentWithPrivacyFilter_ShortReturnTripsBreaker pins the
// privacy contract: if the OPF runtime returns fewer span slices than
// inputs, we treat it as a runtime failure (trip the breaker + regex-only
// fallback) rather than silently produce under-redacted output. The
// per-blob caller in the pre-push rewrite then catches the tripped
// breaker via OPFBreakerTripped() and aborts before CAS.
func TestJSONLContentWithPrivacyFilter_ShortReturnTripsBreaker(t *testing.T) {
	resetOPFConfig()
	t.Cleanup(resetOPFConfig)

	ConfigurePrivacyFilterWithRuntime(OPFConfig{
		Enabled:    true,
		Categories: map[string]bool{"private_person": true},
	}, &shortReturnRuntime{})

	// Three distinct leaves — the fake will return ONE span slice instead
	// of three, triggering the short-return path.
	content := `{"a":"Alice met Bob","b":"Charlie sat down","c":"Eve walked home"}`
	_, err := JSONLContentWithPrivacyFilter(context.Background(), content)
	if err != nil {
		t.Fatalf("short return should fall back to regex-only (no error), got %v", err)
	}
	if !opfBreakerTripped.Load() {
		t.Error("short return must trip the OPF breaker so the rewrite's post-loop check aborts the push")
	}
}

// FuzzSplitBatchSpan checks splitBatchSpan against a byte-by-byte reference:
// for any batch layout and span, every byte of the span that lies inside an
// input is covered by exactly one piece of that input, and no piece covers a
// separator byte, a byte outside the span, or a byte past the batch. A span
// with a negative start is malformed (RedactBatch discards those before
// splitting) and must yield nothing.
func FuzzSplitBatchSpan(f *testing.F) {
	f.Add([]byte{11, 11, 11}, 5, 15)
	f.Add([]byte{11, 11, 11}, 6, 30)
	f.Add([]byte{0, 3, 0}, 0, 10)
	f.Add([]byte{4}, -2, 100)

	f.Fuzz(func(t *testing.T, lens []byte, spanStart, spanEnd int) {
		if len(lens) == 0 || len(lens) > 16 {
			return
		}
		sepLen := len(opfBatchSeparator)
		// owner[b] is the input byte b belongs to, or -1 for a separator.
		var owner []int
		starts := make([]int, len(lens))
		for i, l := range lens {
			if i > 0 {
				for range sepLen {
					owner = append(owner, -1)
				}
			}
			starts[i] = len(owner)
			for range int(l % 32) {
				owner = append(owner, i)
			}
		}
		batchedLen := len(owner)
		spanStart %= batchedLen + 8
		spanEnd %= batchedLen + 8

		covered := make([]int, batchedLen)
		for _, p := range splitBatchSpan(starts, batchedLen, spanStart, spanEnd, sepLen) {
			if p.input < 0 || p.input >= len(starts) || p.start < 0 || p.end <= p.start {
				t.Fatalf("invalid piece %+v", p)
			}
			for off := p.start; off < p.end; off++ {
				b := starts[p.input] + off
				if b >= batchedLen || owner[b] != p.input {
					t.Fatalf("piece %+v covers byte %d outside input %d", p, b, p.input)
				}
				if b < spanStart || b >= spanEnd {
					t.Fatalf("piece %+v covers byte %d outside span [%d,%d)", p, b, spanStart, spanEnd)
				}
				covered[b]++
			}
		}
		for b := range batchedLen {
			want := 0
			if spanStart >= 0 && owner[b] >= 0 && b >= spanStart && b < spanEnd {
				want = 1
			}
			if covered[b] != want {
				t.Fatalf("byte %d (input %d) covered %d times, want %d; span [%d,%d)",
					b, owner[b], covered[b], want, spanStart, spanEnd)
			}
		}
	})
}
