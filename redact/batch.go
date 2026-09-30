package redact

import (
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
//
// ID is the blob's git object hash. It is what the OPF span cache is keyed
// by (see ScanBlobsWithPrivacyFilter); a blob without an ID can be redacted
// in one pass by BatchBytesWithPrivacyFilter but never cached.
type NamedBlob struct {
	Name    string
	Content []byte
	ID      string
}

// BatchBytesWithPrivacyFilter redacts N blobs with one OPF inference call
// per opfBatchChunkBytes of unique leaf text instead of one per blob, so
// typical input is a single call. Returns redacted bytes in input order
// (output[i] is the redaction of inputs[i]).
//
// Failure semantics — fail-closed: any error from the OPF runtime
// returns a non-nil error. Callers running this for privacy-critical
// operations (e.g. the pre-push rewrite) must abort rather than
// proceed with partially-redacted content. The per-blob
// JSONLContentWithPrivacyFilter falls back to the regex-only pipeline
// (the eight always-on/opt-in layers, no OPF) on batch failure; this
// batched variant intentionally does not, because the only caller
// (cross-blob walker) needs an explicit signal that OPF did not
// finish.
//
// When OPF is unconfigured, disabled, or the per-process circuit
// breaker has tripped, returns regex-only output for every blob with
// no error. This matches the existing non-batched paths and keeps the
// caller's hot-path code clean. Enabled with zero effective categories
// is different: that returns ErrOPFNoEnabledCategories, because the
// caller is about to stamp the Entire-OPF-Applied trailer and a silent
// regex-only success here would make that attestation false. Enabled
// with a nil runtime errors for the same reason. Both fail-closed
// checks run BEFORE the breaker check so the guarantee is
// unconditional — a tripped breaker must not downgrade a
// misconfiguration back into silent regex-only success.
func BatchBytesWithPrivacyFilter(ctx context.Context, inputs []NamedBlob) ([][]byte, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	cfg := getOPFConfig()
	if cfg == nil || !cfg.Enabled {
		return applyRegexLayersToBlobs(inputs), nil
	}
	cats := enabledCategories(cfg)
	if len(cats) == 0 {
		return nil, ErrOPFNoEnabledCategories
	}
	if cfg.runtime == nil {
		return nil, errOPFNilRuntime
	}
	if opfBreakerTripped.Load() {
		return applyRegexLayersToBlobs(inputs), nil
	}

	spansByInput, err := scanProseLeaves(ctx, cfg, cats, uniqueProseLeaves(inputs), len(inputs))
	if err != nil {
		return nil, err
	}

	// Pass 3: apply per-leaf regex layers + cached OPF spans per blob.
	out := make([][]byte, len(inputs))
	for i, in := range inputs {
		out[i] = applyToBlob(in, spansByInput, cfg)
	}
	return out, nil
}

// uniqueProseLeaves collects the prose-shaped leaves of inputs, in first-seen
// order and deduplicated by text. The has-space gate excludes structural strings
// (paths, IDs, snake_case keys) that would pay model cost for no benefit. OPF is
// a pure function of its input text, so identical leaves in different blobs
// share one inference result.
func uniqueProseLeaves(inputs []NamedBlob) []string {
	seen := make(map[string]struct{})
	var leaves []string
	for _, in := range inputs {
		collectLeaves(in, func(v string) {
			if !isProseLeaf(v) {
				return
			}
			if _, ok := seen[v]; ok {
				return
			}
			seen[v] = struct{}{}
			leaves = append(leaves, v)
		})
	}
	return leaves
}

// isProseLeaf is the has-space gate shared by collection, scanning and the
// cached apply path, so all three agree on which leaves OPF covers.
func isProseLeaf(v string) bool {
	return strings.ContainsRune(v, ' ')
}

// scanProseLeaves runs the model over leaves in calls of at most
// opfBatchChunkBytes each (see chunkOPFBatchInputs), so each call's adaptive
// deadline tracks its own size. blobCount only feeds messages.
//
// Fail-closed: a runtime error or a short return trips the breaker and returns
// an error, because the caller is about to treat the result as a complete scan.
func scanProseLeaves(ctx context.Context, cfg *OPFConfig, cats []string, leaves []string, blobCount int) (map[string][]Span, error) {
	spansByInput := make(map[string][]Span, len(leaves))
	if len(leaves) == 0 {
		return spansByInput, nil
	}
	fmt.Fprintln(opfStderr, "→ OpenAI Privacy Filter: scanning checkpoints…")
	start := time.Now()
	for _, chunk := range chunkOPFBatchInputs(leaves, opfBatchChunkBytes) {
		batched, err := cfg.runtime.RedactBatch(ctx, chunk, cats)
		if err != nil {
			handleOPFFailure(ctx, cfg, err)
			return nil, fmt.Errorf("opf batch failed across %d blobs: %w", blobCount, err)
		}
		// A short return means the runtime gave us fewer span slices than
		// inputs — the tail leaves would receive zero OPF spans and the
		// rewrite would proceed as if OPF found nothing in them. With the
		// Entire-OPF-Applied trailer attached to the resulting commits,
		// that's silent under-redaction. Fail-closed: trip the breaker
		// and return an error so the orchestrator aborts before CAS.
		if len(batched) != len(chunk) {
			shortErr := fmt.Errorf("opf runtime returned %d span slices for %d inputs", len(batched), len(chunk))
			handleOPFFailure(ctx, cfg, shortErr)
			return nil, fmt.Errorf("opf batch short return: %w", shortErr)
		}
		for i, leaf := range chunk {
			spansByInput[leaf] = batched[i]
		}
	}
	fmt.Fprintf(opfStderr, "✓ OpenAI Privacy Filter: done (%.1fs, %d blobs)\n",
		time.Since(start).Seconds(), blobCount)
	return spansByInput, nil
}

// opfBatchChunkBytes bounds the text one opf invocation carries. Without it
// every unique leaf of a unit of work goes to a single call, so one large
// checkpoint ref could need more time than opfTimeoutCeiling allows: the call
// is killed, the breaker trips, and the ref comes up first again on every
// later flush, starving every ref queued behind it. At the benchmarked rate a
// 1 MiB chunk finishes in about 20 minutes, well inside its own ~40-minute
// adaptive deadline, and the ~6s model load it adds is under 1% of that.
const opfBatchChunkBytes = 1024 * 1024

// chunkOPFBatchInputs splits inputs, in order, into consecutive groups whose
// joined length (each input plus its separator) stays within limit. An input
// larger than limit on its own gets a group of its own rather than being
// split, since a span cannot be attributed across a cut in the text.
func chunkOPFBatchInputs(inputs []string, limit int) [][]string {
	var chunks [][]string
	var current []string
	size := 0
	for _, in := range inputs {
		n := len(in) + len(opfBatchSeparator)
		if len(current) > 0 && size+n > limit {
			chunks = append(chunks, current)
			current, size = nil, 0
		}
		current = append(current, in)
		size += n
	}
	if len(current) > 0 {
		chunks = append(chunks, current)
	}
	return chunks
}

// collectLeaves invokes add for every prose-shaped leaf in the blob.
// JSONL/JSON blobs walk their parsed structure; other blobs are
// treated as a single leaf (raw transcript text, prompt files, etc.).
//
// JSON parse failures fall back to whole-content treatment, matching
// RedactBlobBytes's behavior: a malformed JSON blob still gets the
// regex-only pipeline applied, just without leaf-by-leaf precision.
func collectLeaves(in NamedBlob, add func(string)) {
	if isJSONLikeName(in.Name) {
		if _, err := jsonlContentImpl(string(in.Content), func(v string) string {
			add(v)
			return v
		}, concurrencyUnsafeRedactor); err == nil {
			return
		}
		// JSONL parse failed — fall through to whole-content.
	}
	add(string(in.Content))
}

// applyToBlob produces the redacted bytes for a single blob, combining
// the always-on/opt-in regex layers with the cached OPF spans for each leaf. The
// per-leaf closure mirrors JSONLContentWithPrivacyFilter's Pass 3.
func applyToBlob(in NamedBlob, spansByInput map[string][]Span, cfg *OPFConfig) []byte {
	applier := func(v string) string {
		regions := detectAllLayers(v)
		regions = append(regions, opfSpanRegions(v, spansByInput[v], cfg)...)
		return applyRegions(v, regions)
	}
	if isJSONLikeName(in.Name) {
		if redacted, err := jsonlContentImpl(string(in.Content), applier, concurrencySafeRedactor); err == nil {
			return []byte(redacted)
		}
	}
	return []byte(applier(string(in.Content)))
}

// applyRegexLayersToBlobs is the OPF-disabled fast path: each blob gets
// regex-only redaction with no shell-out. Returned slice is index-aligned
// with inputs.
func applyRegexLayersToBlobs(inputs []NamedBlob) [][]byte {
	out := make([][]byte, len(inputs))
	for i, in := range inputs {
		if isJSONLikeName(in.Name) {
			if redacted, err := jsonlContentImpl(string(in.Content), String, concurrencySafeRedactor); err == nil {
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

// SumProseLeafBytes returns the deduplicated byte size of unique
// prose-shaped (has-space) leaves across inputs — exactly what
// BatchBytesWithPrivacyFilter will send to OPF inference.
//
// Callers use this to enforce a cap before paying the OPF cost: a
// push with 100MB of mostly-structural JSON has tens of KB of actual
// leaves; a push with 100MB of dense prose has hundreds of MB. The
// blob-byte size doesn't tell you which without looking inside.
//
// Uses the same has-space gate, JSONL/JSON leaf extraction, and
// leaf-text dedup as Pass 1 of BatchBytesWithPrivacyFilter, so this is
// not a conservative bound — it is the actual count OPF will process.
// A checkpoint ref's ancestry can repeat the same blob (e.g. an
// unchanged full.jsonl carried across several commits); counting it
// once here matches the single inference result the batch call reuses
// for every repeat.
func SumProseLeafBytes(inputs []NamedBlob) int {
	total := 0
	for _, leaf := range uniqueProseLeaves(inputs) {
		total += len(leaf)
	}
	return total
}
