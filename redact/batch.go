package redact

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"
)

// NamedBlob is one input to BatchBytesWithPrivacyFilter. Name drives
// redaction shape: a ".jsonl" or ".json" suffix triggers JSON-aware
// leaf extraction (string values inside the parsed structure); any
// other suffix treats the whole content as a single leaf.
//
// Content is the raw blob bytes. The blob's redacted output appears at
// the same index in the function's return slice.
type NamedBlob struct {
	Name    string
	Content []byte
}

// BatchBytesWithPrivacyFilter redacts N blobs with a single OPF
// inference call instead of N. Returns redacted bytes in input order
// (output[i] is the redaction of inputs[i]).
//
// Failure semantics — fail-closed: any error from the OPF runtime
// returns a non-nil error. Callers running this for privacy-critical
// operations (e.g. the pre-push rewrite) must abort rather than
// proceed with partially-redacted content. The per-blob
// JSONLContentWithPrivacyFilter falls back to 7-layer on batch
// failure; this batched variant intentionally does not, because the
// only caller (cross-blob walker) needs an explicit signal that OPF
// did not finish.
//
// When OPF is unconfigured, disabled, has no enabled categories, or
// the per-process circuit breaker has tripped, returns 7-layer-only
// output for every blob with no error. This matches the existing
// non-batched paths and keeps the caller's hot-path code clean.
func BatchBytesWithPrivacyFilter(ctx context.Context, inputs []NamedBlob) ([][]byte, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	cfg := getOPFConfig()
	if cfg == nil || !cfg.Enabled || cfg.runtime == nil || opfBreakerTripped.Load() {
		return apply7LayerToBlobs(inputs), nil
	}
	cats := enabledCategories(cfg)
	if len(cats) == 0 {
		return apply7LayerToBlobs(inputs), nil
	}

	// Pass 1: collect unique prose-shaped leaves across every blob.
	// The has-space gate excludes structural strings (paths, IDs,
	// snake_case keys) that would pay model-load cost for no benefit.
	// Dedup keys by leaf text, mirroring JSONLContentWithPrivacyFilter:
	// OPF is a pure function of input text, so identical leaves in
	// different blobs share a single inference result.
	seen := make(map[string]struct{})
	var batchInputs []string
	addLeaf := func(v string) {
		if !strings.ContainsRune(v, ' ') {
			return
		}
		if _, ok := seen[v]; ok {
			return
		}
		seen[v] = struct{}{}
		batchInputs = append(batchInputs, v)
	}
	for _, in := range inputs {
		collectLeaves(in, addLeaf)
	}

	// Pass 2: single batched OPF call covering every unique leaf.
	spansByInput := make(map[string][]Span, len(batchInputs))
	if len(batchInputs) > 0 {
		fmt.Fprintln(opfStderr, "→ OpenAI Privacy Filter: scanning checkpoints…")
		start := time.Now()
		batched, err := cfg.runtime.RedactBatch(ctx, batchInputs, cats)
		if err != nil {
			handleOPFFailure(ctx, cfg, err)
			return nil, fmt.Errorf("opf batch failed across %d blobs: %w", len(inputs), err)
		}
		fmt.Fprintf(opfStderr, "✓ OpenAI Privacy Filter: done (%.1fs, %d blobs)\n",
			time.Since(start).Seconds(), len(inputs))
		// A short return means the runtime gave us fewer span slices than
		// inputs — the tail leaves would receive zero OPF spans and the
		// rewrite would proceed as if OPF found nothing in them. With the
		// Entire-OPF-Applied trailer attached to the resulting commits,
		// that's silent under-redaction. Fail-closed: trip the breaker
		// and return an error so the orchestrator aborts before CAS.
		if len(batched) != len(batchInputs) {
			shortErr := fmt.Errorf("opf runtime returned %d span slices for %d inputs", len(batched), len(batchInputs))
			handleOPFFailure(ctx, cfg, shortErr)
			return nil, fmt.Errorf("opf batch short return: %w", shortErr)
		}
		for i, leaf := range batchInputs {
			spansByInput[leaf] = batched[i]
		}
	}

	// Pass 3: apply per-leaf regex layers + cached OPF spans per blob.
	out := make([][]byte, len(inputs))
	for i, in := range inputs {
		out[i] = applyToBlob(in, spansByInput, cfg)
	}
	return out, nil
}

// collectLeaves invokes add for every prose-shaped leaf in the blob.
// JSONL/JSON blobs walk their parsed structure; other blobs are
// treated as a single leaf (raw transcript text, prompt files, etc.).
//
// JSON parse failures fall back to whole-content treatment, matching
// RedactBlobBytes's behavior: a malformed JSON blob still gets the
// 7-layer pipeline applied, just without leaf-by-leaf precision.
func collectLeaves(in NamedBlob, add func(string)) {
	if isJSONLikeName(in.Name) {
		if _, err := jsonlContentImpl(string(in.Content), func(v string) string {
			add(v)
			return v
		}); err == nil {
			return
		}
		// JSONL parse failed — fall through to whole-content.
	}
	add(string(in.Content))
}

// applyToBlob produces the redacted bytes for a single blob, combining
// the 7 regex layers with the cached OPF spans for each leaf. The
// per-leaf closure mirrors JSONLContentWithPrivacyFilter's Pass 3.
func applyToBlob(in NamedBlob, spansByInput map[string][]Span, cfg *OPFConfig) []byte {
	applier := func(v string) string {
		regions := detectAllLayers(v)
		regions = append(regions, opfSpanRegions(v, spansByInput[v], cfg)...)
		return applyRegions(v, regions)
	}
	if isJSONLikeName(in.Name) {
		if redacted, err := jsonlContentImpl(string(in.Content), applier); err == nil {
			return []byte(redacted)
		}
	}
	return []byte(applier(string(in.Content)))
}

// apply7LayerToBlobs is the OPF-disabled fast path: each blob gets
// regex-only redaction with no shell-out. Returned slice is index-aligned
// with inputs.
func apply7LayerToBlobs(inputs []NamedBlob) [][]byte {
	out := make([][]byte, len(inputs))
	for i, in := range inputs {
		if isJSONLikeName(in.Name) {
			if redacted, err := jsonlContentImpl(string(in.Content), String); err == nil {
				out[i] = []byte(redacted)
				continue
			}
		}
		out[i] = []byte(String(string(in.Content)))
	}
	return out
}

// isJSONLikeName reports whether the blob name suggests JSON-aware
// redaction. Matches RedactBlobBytes's dispatch in checkpoint/.
func isJSONLikeName(name string) bool {
	return strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, ".json")
}

// SumProseLeafBytes returns the cumulative byte size of prose-shaped
// (has-space) leaves across inputs — the upper bound on what
// BatchBytesWithPrivacyFilter would send to OPF inference.
//
// Callers use this to enforce a cap before paying the OPF cost: a
// push with 100MB of mostly-structural JSON has tens of KB of actual
// leaves; a push with 100MB of dense prose has hundreds of MB. The
// blob-byte size doesn't tell you which without looking inside.
//
// Returns a CONSERVATIVE UPPER BOUND on what would go to OPF — same
// has-space gate and JSONL/JSON parse with whole-content fallback as
// the collector inside BatchBytesWithPrivacyFilter, BUT this function
// does NOT deduplicate identical leaves across blobs. The actual batch
// sent to OPF dedups by leaf-text, so a push with many repeated leaves
// will report higher byte counts here than OPF actually sees. Callers
// using this for cap enforcement get an over-strict bound, which is
// safe (false positives possible, false negatives impossible).
func SumProseLeafBytes(inputs []NamedBlob) int {
	var total int
	for _, in := range inputs {
		if isJSONLikeName(in.Name) {
			if _, err := jsonlContentImpl(string(in.Content), func(v string) string {
				if strings.ContainsRune(v, ' ') {
					total += len(v)
				}
				return v
			}); err == nil {
				continue
			}
			// JSON parse failed — fall through to whole-content (matches
			// the collector's fallback in BatchBytesWithPrivacyFilter).
		}
		if bytes.ContainsRune(in.Content, ' ') {
			total += len(in.Content)
		}
	}
	return total
}
