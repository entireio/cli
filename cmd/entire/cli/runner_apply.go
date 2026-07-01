package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// parseTuneOutput extracts the runner-id -> new-template map the tuning model
// is instructed to emit as a single JSON object. The model may wrap the object
// in prose or code fences, so we slice from the first "{" to the last "}". An
// empty object ({}) is valid: the model is told to omit unchanged runners, so
// "{}" is the legitimate "no changes" result, not an error.
func parseTuneOutput(text string) (map[string]string, error) {
	obj := extractJSONObject(text)
	if obj == "" {
		return nil, errors.New("no JSON object found in model output")
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(obj), &m); err != nil {
		return nil, fmt.Errorf("parse model output as {runner: template}: %w", err)
	}
	return m, nil
}

var placeholderRe = regexp.MustCompile(`{{[^{}]+}}`)

// validateNewTemplate rejects a rewritten template that is empty or that
// introduces a {{placeholder}} not present in the original. An invented
// placeholder is unsafe: the backend only substitutes the known set, so a new
// token renders as literal "{{junk}}" in the prompt. Dropping a placeholder is
// safe — it just leaves a substitution slot unused (e.g. the model commonly
// drops the cosmetic {{branch}} since the diff is taken against HEAD) — so
// drops are allowed here and surfaced as a note by the caller instead.
func validateNewTemplate(oldTemplate, newTemplate string) error {
	if strings.TrimSpace(newTemplate) == "" {
		return errors.New("rewritten template is empty")
	}
	oldSet := placeholderSet(oldTemplate)
	var added []string
	for ph := range placeholderSet(newTemplate) {
		if !oldSet[ph] {
			added = append(added, ph)
		}
	}
	sort.Strings(added)
	if len(added) > 0 {
		return fmt.Errorf("rewritten template added unknown placeholder(s): %s", strings.Join(added, ", "))
	}
	return nil
}

// untailoredRunners returns the created runner IDs that tuning did NOT tailor
// (still generic defaults), sorted. These were scaffolded by onboarding but
// left unchanged — skipped, omitted by the model, or returned verbatim — so
// they must not be presented as repo-tailored.
func untailoredRunners(createdIDs []string, tailored map[string]bool) []string {
	var out []string
	for _, id := range createdIDs {
		if !tailored[normalizeRunnerID(id)] {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// droppedPlaceholders returns the placeholders present in oldTemplate but not in
// newTemplate, sorted. Used to inform the user when a rewrite stops using one.
func droppedPlaceholders(oldTemplate, newTemplate string) []string {
	newSet := placeholderSet(newTemplate)
	var dropped []string
	for ph := range placeholderSet(oldTemplate) {
		if !newSet[ph] {
			dropped = append(dropped, ph)
		}
	}
	sort.Strings(dropped)
	return dropped
}

func placeholderSet(s string) map[string]bool {
	set := make(map[string]bool)
	for _, ph := range placeholderRe.FindAllString(s, -1) {
		set[ph] = true
	}
	return set
}

// extractJSONObject returns the outermost {...} span in text, after stripping
// any surrounding markdown code fences. Returns "" when none is found.
func extractJSONObject(text string) string {
	text = stripCodeFences(strings.TrimSpace(text))
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return ""
	}
	return text[start : end+1]
}

func stripCodeFences(text string) string {
	if !strings.HasPrefix(text, "```") {
		return text
	}
	// Drop the opening fence line (``` or ```json) and the closing fence.
	if nl := strings.IndexByte(text, '\n'); nl >= 0 {
		text = text[nl+1:]
	}
	if i := strings.LastIndex(text, "```"); i >= 0 {
		text = text[:i]
	}
	return strings.TrimSpace(text)
}

// replaceRunnerTemplate swaps only the prompt.template value inside a runner
// JSON document, leaving every other field and the file's formatting
// byte-for-byte intact. It works on the raw bytes (not a re-marshal) so unknown
// or backend-managed fields are never dropped and the git diff stays scoped to
// the prompt change. Returns the original bytes unchanged when newTemplate
// matches the current template.
func replaceRunnerTemplate(raw []byte, newTemplate string) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("parse runner JSON: %w", err)
	}
	promptRaw, ok := top["prompt"]
	if !ok {
		return nil, errors.New("runner has no \"prompt\" object")
	}
	var promptObj map[string]json.RawMessage
	if err := json.Unmarshal(promptRaw, &promptObj); err != nil {
		return nil, fmt.Errorf("parse runner prompt object: %w", err)
	}
	// oldVal holds the original on-disk bytes of the template value, so it is a
	// guaranteed substring of raw.
	oldVal, ok := promptObj["template"]
	if !ok {
		return nil, errors.New("runner has no \"prompt.template\" field")
	}

	newVal, err := encodeJSONString(newTemplate)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(oldVal, newVal) {
		return raw, nil
	}

	if n := bytes.Count(raw, oldVal); n != 1 {
		return nil, fmt.Errorf("expected exactly one occurrence of the current template, found %d", n)
	}
	out := bytes.Replace(raw, oldVal, newVal, 1)
	if !json.Valid(out) {
		return nil, errors.New("template replacement produced invalid JSON")
	}
	return out, nil
}

// encodeJSONString encodes s as a JSON string without HTML escaping, so
// characters like <, >, and & stay literal — matching the style the runner
// files are authored in and keeping diffs minimal.
func encodeJSONString(s string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return nil, fmt.Errorf("encode template string: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
