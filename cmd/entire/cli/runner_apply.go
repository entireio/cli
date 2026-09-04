package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/sergi/go-diff/diffmatchpatch"
)

// tunedRunner is one accepted rewrite: the runner, its new file bytes, and the
// new template on its own. The template is kept because --dry-run diffs the
// template rather than the JSON file.
type tunedRunner struct {
	runner   tuneRunner
	newRaw   []byte
	template string
}

// runTuning runs the prompt through an already-resolved summary provider
// (prompt -> text) and turns the runner-id -> template map it returns into
// accepted rewrites. Rejections — out of scope, invalid template, unpatchable
// file — are reported on errW and counted rather than returned, so one bad
// runner does not sink the rest. Nothing is written here: the caller chooses
// between applying the changes and previewing them.
//
// The provider is resolved by the caller, before it gathers repository signal:
// resolution can fail outright or stop to ask which provider to use, and
// neither belongs after the seconds the gather costs.
func runTuning(ctx context.Context, errW io.Writer, provider *checkpointSummaryProvider, runners []tuneRunner, prompt, debugDir string) (changes []tunedRunner, skipped int, err error) {
	// provider.TextGenerator is the plain prompt->text generator, and is
	// guaranteed non-nil: its constructor fails when the agent has none.
	// provider.Generator is deliberately not used — that is a
	// summarize.Generator, which turns a transcript into a checkpoint Summary.
	stop := startSpinner(errW, fmt.Sprintf("Tuning %d runner(s) with %s", len(runners), provider.DisplayName))
	out, err := provider.TextGenerator.GenerateText(ctx, prompt, provider.Model)
	stop(err == nil)
	if err != nil {
		return nil, 0, fmt.Errorf("agent run failed: %w", err)
	}
	if debugDir != "" {
		writeTuneDebug(errW, debugDir, "response.txt", out)
	}

	templates, err := parseTuneOutput(out)
	if err != nil {
		return nil, 0, err
	}
	changes, skipped = classifyTuneProposals(errW, runners, templates)
	return changes, skipped, nil
}

// classifyTuneProposals turns the model's runner-id -> template map into the
// rewrites that will actually be used, reporting each rejection on errW and
// counting it. It is separate from the provider call so the accept/reject rules
// can be tested against canned proposals.
//
// A rejection is never fatal: one unusable proposal must not cost the other
// runners their tailoring.
func classifyTuneProposals(errW io.Writer, runners []tuneRunner, templates map[string]string) (changes []tunedRunner, skipped int) {
	byID := make(map[string]tuneRunner, len(runners))
	for _, r := range runners {
		byID[normalizeRunnerID(r.ID)] = r
	}

	// Sorted so the skip/note messages and the preview diff come out in a stable
	// order rather than in Go's randomized map order.
	for _, id := range slices.Sorted(maps.Keys(templates)) {
		tmpl := templates[id]
		r, ok := byID[normalizeRunnerID(id)]
		if !ok {
			fmt.Fprintf(errW, "skip %q: not a runner in scope\n", id)
			skipped++
			continue
		}
		if err := validateNewTemplate(r.Template, tmpl); err != nil {
			fmt.Fprintf(errW, "skip %s: %v\n", r.ID, err)
			skipped++
			continue
		}
		if dropped := droppedPlaceholders(r.Template, tmpl); len(dropped) > 0 {
			fmt.Fprintf(errW, "note: %s no longer references %v\n", r.ID, dropped)
		}
		newRaw, err := replaceRunnerTemplate(r.Raw, tmpl)
		if err != nil {
			fmt.Fprintf(errW, "skip %s: %v\n", r.ID, err)
			skipped++
			continue
		}
		if bytes.Equal(newRaw, r.Raw) {
			continue // model returned the current template verbatim — benign no-op
		}
		changes = append(changes, tunedRunner{runner: r, newRaw: newRaw, template: tmpl})
	}
	return changes, skipped
}

// applyTunedRunners writes each accepted rewrite over its runner file.
// createdIDs are runners this invocation just scaffolded from defaults; any of
// those left un-tailored is flagged so it is not committed as if it were
// repo-specific.
func applyTunedRunners(w, errW io.Writer, repoRoot string, changes []tunedRunner, skipped int, createdIDs []string) error {
	root, err := entiredir.OpenAt(repoRoot)
	if err != nil {
		return fmt.Errorf("open %s: %w", paths.EntireDir, err)
	}

	tailored := make(map[string]bool, len(changes))
	for _, c := range changes {
		if err := entiredir.WriteFile(root, c.runner.Name, c.newRaw, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", c.runner.Path, err)
		}
		fmt.Fprintf(w, "updated %s\n", filepath.Base(c.runner.Path))
		tailored[normalizeRunnerID(c.runner.ID)] = true
	}

	switch {
	case len(changes) > 0:
		fmt.Fprintf(w, "\nUpdated %d runner(s). Review with: git diff %s\n",
			len(changes), filepath.Join(paths.EntireDir, runnersName))
	case len(createdIDs) == 0 && skipped > 0:
		// Existing runners, model proposed templates, all rejected — a failed run.
		// (When onboarding just created the set, an un-tailored runner is reported
		// below as a generic default instead, which is more actionable.)
		return fmt.Errorf("model proposed %d template(s) but all were rejected or out of scope (see messages above)", skipped)
	case len(createdIDs) == 0:
		fmt.Fprintln(w, "No runner changes proposed.")
	}

	// Runners onboarding scaffolded but tailoring did not change remain the
	// generic defaults. Those are working minimal prompts (valid output
	// contract), so they are committable as-is — just note which are generic.
	if untailored := untailoredRunners(createdIDs, tailored); len(untailored) > 0 {
		fmt.Fprintf(errW, "\n%d runner(s) kept as working defaults (generic, not tailored to this repo): %s\n",
			len(untailored), strings.Join(untailored, ", "))
		fmt.Fprintln(errW, "They are functional as-is; re-run `entire runner setup -y` to tailor them.")
	}
	return nil
}

// previewTunedRunners prints what tailoring would change and writes nothing.
// It diffs each runner's prompt template rather than its JSON file: the
// template is the only field that changes and it is stored as one long JSON
// string, so a file-level diff would be a single unreadable line.
func previewTunedRunners(w, errW io.Writer, inScope int, changes []tunedRunner, skipped int) {
	for _, c := range changes {
		fmt.Fprintf(w, "=== %s — prompt.template would change ===\n", c.runner.ID)
		fmt.Fprint(w, renderTemplateDiff(c.runner.Template, c.template))
		fmt.Fprintln(w)
	}

	switch {
	case len(changes) > 0:
		fmt.Fprintf(errW, "%d of %d runner(s) would change", len(changes), inScope)
		if skipped > 0 {
			fmt.Fprintf(errW, ", %d proposal(s) rejected", skipped)
		}
		fmt.Fprintln(errW, ". Nothing was written — re-run with --yes to apply.")
	case skipped > 0:
		fmt.Fprintf(errW, "No runner would change: all %d proposal(s) were rejected or out of scope (see messages above).\n", skipped)
	default:
		fmt.Fprintln(errW, "No runner changes proposed.")
	}
}

// diffContextLines is how many unchanged template lines to keep either side of
// a change in the --dry-run preview. A tailored template is mostly rewritten,
// so the point of the collapse is the long unchanged tail (the output-JSON
// contract), not economy on the changed part.
const diffContextLines = 3

// renderTemplateDiff renders a line-level diff of two prompt templates, with
// unchanged runs longer than twice the context collapsed to a count.
// diffmatchpatch is character-oriented, so the templates are folded to one
// char per line first (the DiffLinesToChars/DiffCharsToLines pattern, as in
// strategy/manual_commit_attribution.go).
func renderTemplateDiff(oldText, newText string) string {
	dmp := diffmatchpatch.New()
	a, b, lines := dmp.DiffLinesToChars(oldText, newText)
	diffs := dmp.DiffCharsToLines(dmp.DiffMain(a, b, false), lines)

	var out strings.Builder
	for _, d := range diffs {
		switch d.Type {
		case diffmatchpatch.DiffInsert:
			writeDiffLines(&out, "+", splitLines(d.Text))
		case diffmatchpatch.DiffDelete:
			writeDiffLines(&out, "-", splitLines(d.Text))
		case diffmatchpatch.DiffEqual:
			ls := splitLines(d.Text)
			if len(ls) <= 2*diffContextLines {
				writeDiffLines(&out, " ", ls)
				continue
			}
			writeDiffLines(&out, " ", ls[:diffContextLines])
			fmt.Fprintf(&out, "@@ %d unchanged line(s) @@\n", len(ls)-2*diffContextLines)
			writeDiffLines(&out, " ", ls[len(ls)-diffContextLines:])
		}
	}
	return out.String()
}

func writeDiffLines(out *strings.Builder, prefix string, lines []string) {
	for _, line := range lines {
		out.WriteString(prefix)
		out.WriteString(line)
		out.WriteByte('\n')
	}
}

// splitLines splits a diff chunk into lines, dropping the empty element a
// trailing newline produces so it is not rendered as a blank diff line. The
// empty-string guard matters: Split("") is [""], which would render one blank.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

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
