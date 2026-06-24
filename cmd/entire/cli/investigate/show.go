package investigate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/investigate/flowchart"
	"github.com/entireio/cli/cmd/entire/cli/mdrender"
)

// ShowInput drives RunShow.
type ShowInput struct {
	// RunID is the run id (or run-id prefix) to display. Empty means
	// "show the only manifest, or list options if more than one exists".
	RunID string
	// Out is the destination writer for the rendered summary + findings.
	Out io.Writer
	// ErrOut is the destination writer for user-facing error/help messages.
	ErrOut io.Writer
	// HTML renders the findings to a styled findings.html in the per-run
	// directory and opens it in the browser, instead of printing to Out.
	HTML bool
}

// ShowDeps collects what RunShow needs that's test-injectable.
type ShowDeps struct {
	ManifestStore *LocalManifestStore
}

// RunShow prints the saved investigation summary + findings for the
// requested run id. Resolution rules:
//   - empty RunID + exactly one manifest → use that manifest
//   - empty RunID + multiple manifests   → list candidates, return error
//   - non-empty RunID: exact match wins; otherwise unique-prefix match;
//     otherwise return an "ambiguous" or "not found" error
//
// Findings come from manifest.FindingsContent when present (terminal
// outcomes), or by reading manifest.FindingsDoc from disk (paused /
// cancelled runs whose per-run dir still exists). Both paths missing
// is a soft state — the header is printed with an explanatory line.
func RunShow(ctx context.Context, in ShowInput, deps ShowDeps) error {
	if deps.ManifestStore == nil {
		return errors.New("show: manifest store not wired")
	}

	// Fast path: a full 12-hex run id resolves via Glob + one file read.
	runID := strings.TrimSpace(in.RunID)
	if IsValidRunID(runID) {
		m, ok, err := deps.ManifestStore.FindByRunID(ctx, runID)
		if err != nil {
			return fmt.Errorf("find manifest %s: %w", runID, err)
		}
		if !ok {
			return fmt.Errorf("no investigation found with run id %q", runID)
		}
		return emitShow(ctx, in, deps, m)
	}

	manifests, err := deps.ManifestStore.List(ctx)
	if err != nil {
		return fmt.Errorf("list manifests: %w", err)
	}
	if len(manifests) == 0 {
		fmt.Fprintln(in.Out, "No local investigations found.")
		return nil
	}

	if runID == "" {
		if len(manifests) == 1 {
			return emitShow(ctx, in, deps, manifests[0])
		}
		return ambiguousRunIDError(manifests, "")
	}

	resolved, err := ResolveByRunID(manifests, runID)
	if err != nil {
		return err
	}
	return emitShow(ctx, in, deps, resolved[0])
}

// emitShow renders a resolved manifest either to the terminal (default) or as
// an HTML page (in.HTML).
func emitShow(ctx context.Context, in ShowInput, deps ShowDeps, m LocalManifest) error {
	if in.HTML {
		return emitShowHTML(ctx, in, deps, m)
	}
	printShowSummary(in.Out, m)
	printShowFindings(in.Out, m)
	return nil
}

// emitShowHTML renders m to findings.html in the per-run directory, prints the
// path, and opens it in the browser. A manifest with no findings body prints
// the same soft notice as the terminal path and writes nothing. Browser-open
// failures are non-fatal: the path is already printed for manual opening.
func emitShowHTML(ctx context.Context, in ShowInput, deps ShowDeps, m LocalManifest) error {
	doc, err := RenderFindingsHTML(m)
	if errors.Is(err, errNoFindingsContent) {
		fmt.Fprintf(in.Out, "No findings content available for run %s.\n", m.RunID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("render findings html: %w", err)
	}

	dir := deps.ManifestStore.RunDir(m.RunID)
	if mkErr := os.MkdirAll(dir, 0o750); mkErr != nil {
		return fmt.Errorf("create investigation dir: %w", mkErr)
	}
	htmlPath := filepath.Join(dir, "findings.html")
	// 0o600: findings embed the user-supplied prompt and findings body, matching
	// the mode used for findings.md / state.json.
	if writeErr := os.WriteFile(htmlPath, []byte(doc), 0o600); writeErr != nil {
		return fmt.Errorf("write findings html: %w", writeErr)
	}
	// Print/open an absolute path: the git common dir resolves relative to cwd,
	// and a relative path is fragile for the user to open and for the OS opener
	// to resolve. Best-effort — fall back to the joined path on failure.
	if abs, absErr := filepath.Abs(htmlPath); absErr == nil {
		htmlPath = abs
	}
	fmt.Fprintf(in.Out, "Wrote %s\n", htmlPath)

	// Non-fatal: a headless host (or a missing opener) just means the user opens
	// the printed path themselves.
	_ = openInBrowser(ctx, htmlPath) //nolint:errcheck // best-effort; path already printed
	return nil
}

// printShowSummary writes the header block (prompt, agents, outcome,
// timestamps, stances per agent) to w. Keeps the format compact and
// stable so users can grep its output.
func printShowSummary(w io.Writer, m LocalManifest) {
	fmt.Fprintf(w, "Investigation %s\n", m.RunID)
	if m.Topic != "" {
		fmt.Fprintf(w, "Prompt:   %s\n", m.Topic)
	}
	if len(m.Agents) > 0 {
		fmt.Fprintf(w, "Agents:   %s\n", strings.Join(m.Agents, ", "))
	}
	if m.Outcome != "" {
		fmt.Fprintf(w, "Outcome:  %s\n", m.Outcome)
	}
	if !m.StartedAt.IsZero() {
		fmt.Fprintf(w, "Started:  %s\n", m.StartedAt.UTC().Format("2006-01-02 15:04:05Z"))
	}
	if !m.EndedAt.IsZero() {
		fmt.Fprintf(w, "Ended:    %s\n", m.EndedAt.UTC().Format("2006-01-02 15:04:05Z"))
	}
	if len(m.StancesByAgent) > 0 {
		keys := make([]string, 0, len(m.StancesByAgent))
		for k := range m.StancesByAgent {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Last stance per agent:")
		for _, k := range keys {
			fmt.Fprintf(w, "  %s: %s\n", k, m.StancesByAgent[k])
		}
	}
	fmt.Fprintln(w)
}

// printShowFindings writes the findings content to w. Prefers the
// manifest's embedded content (set on terminal outcomes); falls back
// to reading the on-disk findings file (still present for paused or
// cancelled runs). Body is rendered through mdrender for terminal
// output; raw markdown passes through for piped/NO_COLOR output.
func printShowFindings(w io.Writer, m LocalManifest) {
	body := findingsBody(m)
	if body == "" {
		fmt.Fprintf(w, "No findings content available for run %s.\n", m.RunID)
		return
	}
	// Reorder/filter sections to match the HTML view so both outputs agree.
	writeRenderedFindings(w, orderedFindingsMarkdown(body))
}

// findingsBody returns the findings markdown for m, preferring the manifest's
// embedded content (set on terminal outcomes) and falling back to the on-disk
// findings file (still present for paused / cancelled runs). Returns "" when
// neither source yields content. Shared by the terminal renderer and the HTML
// renderer so both resolve findings identically.
func findingsBody(m LocalManifest) string {
	switch {
	case m.FindingsContent != "":
		return m.FindingsContent
	case m.FindingsDoc != "" && filepath.IsAbs(m.FindingsDoc):
		// FindingsDoc is contractually absolute (see LocalManifest docs).
		// Refuse to read relative paths: those would resolve against the
		// current process cwd, which may differ from where the run wrote
		// findings.md, and could surface unrelated content.
		if data, err := os.ReadFile(m.FindingsDoc); err == nil {
			return string(data)
		}
	}
	return ""
}

// writeRenderedFindings renders findings markdown to w, ensuring a trailing
// newline. Shared by `investigate show` and the post-run footer so both get
// identical treatment.
//
// For piped/NO_COLOR output the raw markdown is written unchanged so it stays
// grep-friendly and renders natively on GitHub/docs. For a styled terminal,
// ```mermaid blocks that are renderable flowcharts are converted to indented
// text outlines and printed verbatim — NOT through mdrender, because glamour
// word-wraps content and would corrupt the diagram's indentation. The
// markdown around each diagram is still rendered through mdrender.
func writeRenderedFindings(w io.Writer, body string) {
	if !interactive.ShouldStyle(w) {
		fmt.Fprint(w, body)
		if !strings.HasSuffix(body, "\n") {
			fmt.Fprintln(w)
		}
		return
	}

	for _, seg := range flowchart.SplitRenderable(body) {
		if seg.Diagram != "" {
			// Print the diagram outside glamour, padded with blank lines so
			// it sits apart from the surrounding rendered markdown.
			fmt.Fprintf(w, "\n%s\n\n", seg.Diagram)
			continue
		}
		rendered, err := mdrender.RenderForWriter(w, seg.Markdown)
		if err != nil {
			// Glamour failure: fall back to raw markdown so the user still
			// sees the content.
			rendered = seg.Markdown
		}
		fmt.Fprint(w, rendered)
	}
	fmt.Fprintln(w)
}
