package redact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/betterleaks/betterleaks/detect"
)

// secretPattern matches high-entropy strings that may be secrets.
// Note: / is excluded to prevent matching entire file paths as single tokens.
// Base64 and JWT tokens are still caught via high-entropy segments between slashes.
var secretPattern = regexp.MustCompile(`[A-Za-z0-9+_=-]{10,}`)

// credentialedURIPattern matches URLs that embed userinfo with a password, such
// as postgres://user:pass@host/db or redis://:pass@host/0. These often have
// moderate entropy and are not reliably covered by vendor-specific scanners.
var credentialedURIPattern = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]{1,31}://[^\s/?#@"'` + "`" + `<>:]*:[^\s/?#@"'` + "`" + `<>]+@[^\s"'` + "`" + `<>]+`)

// entropyThreshold is the minimum Shannon entropy for a string to be considered
// a secret. 4.5 was chosen through trial and error: high enough to avoid false
// positives on common words and identifiers, low enough to catch typical API keys
// and tokens which tend to have entropy well above 5.0.
const entropyThreshold = 4.5

// RedactedPlaceholder is the replacement text used for redacted secrets.
const RedactedPlaceholder = "REDACTED"

// RedactedBytes represents transcript data that has been through secret
// redaction. Consumers that require pre-redacted input (e.g., compact.Compact,
// checkpoint stores) accept this type to enforce the contract at compile time.
//
// Produced by JSONLBytes (primary constructor) or trusted wrappers for data
// previously persisted by checkpoint writers.
type RedactedBytes struct {
	data []byte
}

// Bytes returns the underlying byte slice.
func (r RedactedBytes) Bytes() []byte {
	return r.data
}

// Len returns the number of bytes in the redacted payload.
func (r RedactedBytes) Len() int {
	return len(r.data)
}

// AlreadyRedacted wraps transcript bytes known to already be redacted by a
// prior write path. Use this ONLY for trusted sources such as persisted
// checkpoint transcripts or controlled test fixtures. For fresh transcript
// input, use JSONLBytes.
func AlreadyRedacted(data []byte) RedactedBytes {
	return RedactedBytes{data: data}
}

var (
	betterleaksDetector     *detect.Detector
	betterleaksDetectorOnce sync.Once
)

func getDetector() *detect.Detector {
	betterleaksDetectorOnce.Do(func() {
		d, err := detect.NewDetectorDefaultConfig()
		if err != nil {
			return
		}
		betterleaksDetector = d
	})
	return betterleaksDetector
}

// region represents a byte range to redact.
type region struct{ start, end int }

// taggedRegion extends region with a label for typed replacement tokens.
// Empty label = secret (produces "REDACTED"). Non-empty = PII (produces "[REDACTED_<LABEL>]").
type taggedRegion struct {
	region

	label string
}

// String replaces secrets and PII in s using layered detection:
// 1. Entropy-based: high-entropy alphanumeric sequences (threshold 4.5)
// 2. Pattern-based: betterleaks regex rules (260+ known secret formats)
// 3. Credentialed URIs: URLs containing userinfo passwords
// 4. PII detection: email, phone, address patterns (only when configured via ConfigurePII)
// A string is redacted if ANY method flags it.
func String(s string) string {
	var regions []taggedRegion

	// 1. Entropy-based detection (secrets — always on).
	for _, loc := range secretPattern.FindAllStringIndex(s, -1) {
		start, end := loc[0], loc[1]

		// Don't consume characters that are part of JSON/string escape sequences.
		// Example: in "controller.go\nmodel.go", the regex could match "nmodel"
		// (consuming the 'n' from '\n'), and after replacement the '\' would be
		// followed by 'R' from "REDACTED", creating invalid escape '\R'.
		// Only skip for known JSON escape letters to avoid trimming real secrets
		// that happen to follow a literal backslash in decoded content.
		if start > 0 && s[start-1] == '\\' {
			switch s[start] {
			case 'n', 't', 'r', 'b', 'f', 'u', '"', '\\', '/':
				start++
				if end-start < 10 {
					continue
				}
			}
		}

		if shannonEntropy(s[start:end]) > entropyThreshold {
			regions = append(regions, taggedRegion{region: region{start, end}})
		}
	}

	// 2. Pattern-based detection via betterleaks (secrets — always on).
	if d := getDetector(); d != nil {
		for _, f := range d.DetectString(s) {
			if f.Secret == "" {
				continue
			}
			searchFrom := 0
			for {
				idx := strings.Index(s[searchFrom:], f.Secret)
				if idx < 0 {
					break
				}
				absIdx := searchFrom + idx
				regions = append(regions, taggedRegion{region: region{absIdx, absIdx + len(f.Secret)}})
				searchFrom = absIdx + len(f.Secret)
			}
		}
	}

	// 3. Credentialed URIs (secrets — always on).
	for _, loc := range credentialedURIPattern.FindAllStringIndex(s, -1) {
		regions = append(regions, taggedRegion{region: region{loc[0], loc[1]}})
	}

	// 4. PII detection (opt-in — only runs when configured).
	regions = append(regions, detectPII(getPIIConfig(), s)...)

	if len(regions) == 0 {
		return s
	}

	// Merge overlapping regions and build result.
	sort.Slice(regions, func(i, j int) bool {
		if regions[i].start != regions[j].start {
			return regions[i].start < regions[j].start
		}
		if regions[i].end != regions[j].end {
			return regions[i].end > regions[j].end // larger region first
		}
		return regions[i].label < regions[j].label // deterministic tie-break
	})
	merged := []taggedRegion{regions[0]}
	for _, r := range regions[1:] {
		last := &merged[len(merged)-1]
		if r.start <= last.end {
			if r.end > last.end {
				last.end = r.end
			}
			// Keep the existing label (first/larger region wins)
		} else {
			merged = append(merged, r)
		}
	}

	var b strings.Builder
	prev := 0
	for _, r := range merged {
		b.WriteString(s[prev:r.start])
		b.WriteString(replacementToken(r.label))
		prev = r.end
	}
	b.WriteString(s[prev:])
	return b.String()
}

// Bytes is a convenience wrapper around String for []byte content.
func Bytes(b []byte) []byte {
	s := string(b)
	redacted := String(s)
	if redacted == s {
		return b
	}
	return []byte(redacted)
}

// JSONLBytes redacts secrets in JSONL-formatted byte content and returns
// the result as RedactedBytes, certifying the output has been through redaction.
func JSONLBytes(b []byte) (RedactedBytes, error) {
	s := string(b)
	redacted, err := JSONLContent(s)
	if err != nil {
		return RedactedBytes{}, err
	}
	if redacted == s {
		return RedactedBytes{data: b}, nil
	}
	return RedactedBytes{data: []byte(redacted)}, nil
}

// JSONLContent parses each line as JSON to determine which string values
// need redaction, then performs targeted replacements on the raw JSON bytes.
// Lines with no secrets are returned unchanged, preserving original formatting.
//
// For multi-line JSON content (e.g., pretty-printed single JSON objects like
// OpenCode export), the function first attempts to parse the entire content as
// a single JSON value. This ensures field-aware redaction (which skips ID fields)
// is used instead of falling back to entropy-based detection on raw text lines,
// which would corrupt high-entropy identifiers.
func JSONLContent(content string) (string, error) {
	// Try parsing the entire content as a single JSON value first.
	// Uses a streaming decoder to avoid copying the full content into []byte.
	// After decoding, attempts a second Decode to confirm EOF — if it succeeds,
	// the content is JSONL (multiple values) and we fall through to line-by-line.
	trimmed := strings.TrimSpace(content)
	if len(trimmed) > 0 {
		dec := json.NewDecoder(strings.NewReader(trimmed))
		var parsed any
		if err := dec.Decode(&parsed); err == nil && isSingleJSONValue(dec) {
			// Content is a single JSON value (object/array) — redact field-aware.
			result, err := applyJSONReplacements(content, collectJSONLReplacements(parsed))
			if err != nil {
				return "", err
			}
			return result, nil
		}
	}

	// Fall back to line-by-line JSONL processing.
	lines := strings.Split(content, "\n")
	var b strings.Builder
	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		lineTrimmed := strings.TrimSpace(line)
		if lineTrimmed == "" {
			b.WriteString(line)
			continue
		}
		var parsed any
		if err := json.Unmarshal([]byte(lineTrimmed), &parsed); err != nil {
			b.WriteString(String(line))
			continue
		}
		result, err := applyJSONReplacements(line, collectJSONLReplacements(parsed))
		if err != nil {
			return "", err
		}
		b.WriteString(result)
	}
	return b.String(), nil
}

// applyJSONReplacements applies collected (original, redacted) string pairs
// to the raw JSON text, replacing JSON-encoded originals with their redacted forms.
// Returns s unchanged if repls is empty.
func applyJSONReplacements(s string, repls [][2]string) (string, error) {
	if len(repls) == 0 {
		return s, nil
	}
	for _, r := range repls {
		origJSON, err := jsonEncodeString(r[0])
		if err != nil {
			return "", err
		}
		replJSON, err := jsonEncodeString(r[1])
		if err != nil {
			return "", err
		}
		s = strings.ReplaceAll(s, origJSON, replJSON)
	}
	return s, nil
}

// isSingleJSONValue returns true if the decoder has reached EOF (no more
// top-level values). This distinguishes a single JSON value (e.g., pretty-printed
// object) from JSONL (multiple concatenated values). We attempt a second Decode
// and require io.EOF rather than relying on dec.More(), which is documented for
// use inside arrays/objects and not for top-level value boundaries.
func isSingleJSONValue(dec *json.Decoder) bool {
	var discard json.RawMessage
	return dec.Decode(&discard) == io.EOF
}

// collectJSONLReplacements walks a parsed JSON value and collects unique
// (original, redacted) string pairs for values that need redaction.
func collectJSONLReplacements(v any) [][2]string {
	seen := make(map[string]bool)
	var repls [][2]string
	var walk func(v any)
	walk = func(v any) {
		switch val := v.(type) {
		case map[string]any:
			if shouldSkipJSONLObject(val) {
				return
			}
			for k, child := range val {
				if shouldSkipJSONLField(k) {
					continue
				}
				walk(child)
			}
		case []any:
			for _, child := range val {
				walk(child)
			}
		case string:
			redacted := String(val)
			if redacted != val && !seen[val] {
				seen[val] = true
				repls = append(repls, [2]string{val, redacted})
			}
		}
	}
	walk(v)
	return repls
}

// shouldSkipJSONLField returns true if a JSON key should be excluded from scanning/redaction.
// Skips "signature" (exact), ID fields (ending in "id"/"ids"), and common path/directory fields.
func shouldSkipJSONLField(key string) bool {
	if key == "signature" {
		return true
	}
	lower := strings.ToLower(key)

	// Skip ID fields
	if strings.HasSuffix(lower, "id") || strings.HasSuffix(lower, "ids") {
		return true
	}

	// Skip common path and directory fields from agent transcripts.
	// These appear frequently in tool calls and are structural, not secrets.
	switch lower {
	case "filepath", "file_path", "cwd", "root", "directory", "dir", "path":
		return true
	}

	return false
}

// shouldSkipJSONLObject returns true if the object has "type":"image" or "type":"image_url".
func shouldSkipJSONLObject(obj map[string]any) bool {
	t, ok := obj["type"].(string)
	return ok && (strings.HasPrefix(t, "image") || t == "base64")
}

func shannonEntropy(s string) float64 {
	if len(s) == 0 {
		return 0
	}
	freq := make(map[byte]int)
	for i := range len(s) {
		freq[s[i]]++
	}
	length := float64(len(s))
	var entropy float64
	for _, count := range freq {
		p := float64(count) / length
		entropy -= p * math.Log2(p)
	}
	return entropy
}

// jsonEncodeString returns the JSON encoding of s without HTML escaping.
func jsonEncodeString(s string) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return "", fmt.Errorf("json encode string: %w", err)
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}
