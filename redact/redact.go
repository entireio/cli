package redact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/zricethezav/gitleaks/v8/detect"
)

// secretPattern matches high-entropy strings that may be secrets.
var secretPattern = regexp.MustCompile(`[A-Za-z0-9/+_=-]{10,}`)

// entropyThreshold is the minimum Shannon entropy for a string to be considered
// a secret. 4.5 was chosen through trial and error: high enough to avoid false
// positives on common words and identifiers, low enough to catch typical API keys
// and tokens which tend to have entropy well above 5.0.
const entropyThreshold = 4.5

var (
	gitleaksDetector     *detect.Detector
	gitleaksDetectorOnce sync.Once
)

func getDetector() *detect.Detector {
	gitleaksDetectorOnce.Do(func() {
		d, err := detect.NewDetectorDefaultConfig()
		if err != nil {
			return
		}
		gitleaksDetector = d
	})
	return gitleaksDetector
}

// region represents a byte range to redact.
type region struct{ start, end int }

// String replaces secrets in s with "REDACTED" using layered detection:
// 1. Entropy-based: high-entropy alphanumeric sequences (threshold 4.5)
// 2. Pattern-based: gitleaks regex rules (180+ known secret formats)
// A string is redacted if EITHER method flags it.
func String(s string) string {
	var regions []region

	// 1. Entropy-based detection.
	for _, loc := range secretPattern.FindAllStringIndex(s, -1) {
		if shannonEntropy(s[loc[0]:loc[1]]) > entropyThreshold {
			regions = append(regions, region{loc[0], loc[1]})
		}
	}

	// 2. Pattern-based detection via gitleaks.
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
				regions = append(regions, region{absIdx, absIdx + len(f.Secret)})
				searchFrom = absIdx + len(f.Secret)
			}
		}
	}

	if len(regions) == 0 {
		return s
	}

	// Merge overlapping regions and build result.
	sort.Slice(regions, func(i, j int) bool {
		return regions[i].start < regions[j].start
	})
	merged := []region{regions[0]}
	for _, r := range regions[1:] {
		last := &merged[len(merged)-1]
		if r.start <= last.end {
			if r.end > last.end {
				last.end = r.end
			}
		} else {
			merged = append(merged, r)
		}
	}

	var b strings.Builder
	prev := 0
	for _, r := range merged {
		b.WriteString(s[prev:r.start])
		b.WriteString("REDACTED")
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

// JSONLBytes is a convenience wrapper around JSONLContent for []byte content.
func JSONLBytes(b []byte) ([]byte, error) {
	s := string(b)
	redacted, err := JSONLContent(s)
	if err != nil {
		return nil, err
	}
	if redacted == s {
		return b, nil
	}
	return []byte(redacted), nil
}

// JSONBytes is a convenience wrapper around JSONContent for []byte content.
// JSON content is redacted with JSON-aware field skipping (e.g. keys ending in "id").
func JSONBytes(b []byte) ([]byte, error) {
	s := string(b)
	redacted, err := JSONContent(s)
	if err != nil {
		return nil, err
	}
	if redacted == s {
		return b, nil
	}
	return []byte(redacted), nil
}

// JSONLContent parses each line as JSON to determine which string values
// need redaction, then performs targeted replacements on the raw JSON bytes.
// Lines with no secrets are returned unchanged, preserving original formatting.
func JSONLContent(content string) (string, error) {
	lines := strings.Split(content, "\n")
	var b strings.Builder
	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			b.WriteString(line)
			continue
		}
		var parsed any
		if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
			b.WriteString(String(line))
			continue
		}
		repls := collectJSONLReplacements(parsed)
		if len(repls) == 0 {
			b.WriteString(line)
			continue
		}
		result := line
		for _, r := range repls {
			origJSON, err := jsonEncodeString(r[0])
			if err != nil {
				return "", err
			}
			replJSON, err := jsonEncodeString(r[1])
			if err != nil {
				return "", err
			}
			result = strings.ReplaceAll(result, origJSON, replJSON)
		}
		b.WriteString(result)
	}
	return b.String(), nil
}

// JSONContent parses a JSON string and redacts string values while skipping
// identifier fields (keys ending in "id") and non-user payload fields.
func JSONContent(content string) (string, error) {
	var parsed any
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		return "", fmt.Errorf("unmarshal JSON content: %w", err)
	}

	redacted := redactJSONValue(parsed)
	if reflect.DeepEqual(parsed, redacted) {
		return content, nil
	}

	return marshalJSONPreservingFormatting(redacted, content)
}

// redactJSONValue walks parsed JSON and redacts string values while preserving
// skipped keys (e.g. ending in "id") and non-user payload fields.
func redactJSONValue(v any) any {
	switch val := v.(type) {
	case map[string]any:
		if shouldSkipJSONLObject(val) {
			return val
		}
		out := make(map[string]any, len(val))
		for key, child := range val {
			if shouldSkipJSONField(key) {
				out[key] = child
				continue
			}
			out[key] = redactJSONValue(child)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, child := range val {
			out[i] = redactJSONValue(child)
		}
		return out
	case string:
		return String(val)
	default:
		return val
	}
}

// marshalJSONPreservingFormatting marshals v while preserving basic formatting
// characteristics from original (indentation and trailing newline).
func marshalJSONPreservingFormatting(v any, original string) (string, error) {
	// Preserve compact style for single-line JSON.
	if !strings.Contains(original, "\n") {
		out, err := json.Marshal(v)
		if err != nil {
			return "", fmt.Errorf("marshal JSON content: %w", err)
		}
		if strings.HasSuffix(original, "\n") {
			out = append(out, '\n')
		}
		return string(out), nil
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	indent := detectJSONIndent(original)
	if indent == "" {
		indent = "  "
	}
	enc.SetIndent("", indent)
	if err := enc.Encode(v); err != nil {
		return "", fmt.Errorf("encode formatted JSON content: %w", err)
	}

	out := buf.String()
	if !strings.HasSuffix(original, "\n") {
		out = strings.TrimSuffix(out, "\n")
	}
	return out, nil
}

// detectJSONIndent returns the leading indentation used by the first indented
// non-empty line in content.
func detectJSONIndent(content string) string {
	lines := strings.Split(content, "\n")
	for i := 1; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			continue
		}
		trimmed := strings.TrimLeft(line, " \t")
		if len(trimmed) == len(line) {
			continue
		}
		return line[:len(line)-len(trimmed)]
	}
	return ""
}

// collectJSONLReplacements walks a parsed JSON value and collects unique
// (original, redacted) string pairs for values that need redaction.
func collectJSONLReplacements(v any) [][2]string {
	return collectJSONReplacements(v)
}

// collectJSONReplacements walks a parsed JSON value and collects unique
// (original, redacted) string pairs for values that need redaction.
func collectJSONReplacements(v any) [][2]string {
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
				if shouldSkipJSONField(k) {
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
// Skips "signature" (exact) and any key ending in "id" (case-insensitive).
func shouldSkipJSONLField(key string) bool {
	return shouldSkipJSONField(key)
}

// shouldSkipJSONField returns true if a JSON key should be excluded from scanning/redaction.
// Skips "signature" (exact) and any key ending in "id" (case-insensitive).
func shouldSkipJSONField(key string) bool {
	if key == "signature" {
		return true
	}
	lower := strings.ToLower(key)
	return strings.HasSuffix(lower, "id") || strings.HasSuffix(lower, "ids")
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
