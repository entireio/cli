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
		printShowSummary(in.Out, m)
		printShowFindings(in.Out, m)
		return nil
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
			printShowSummary(in.Out, manifests[0])
			printShowFindings(in.Out, manifests[0])
			return nil
		}
		return ambiguousRunIDError(manifests, "")
	}

	resolved, err := ResolveByRunID(manifests, runID)
	if err != nil {
		return err
	}
	printShowSummary(in.Out, resolved[0])
	printShowFindings(in.Out, resolved[0])
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
	body := ""
	switch {
	case m.FindingsContent != "":
		body = m.FindingsContent
	case m.FindingsDoc != "" && filepath.IsAbs(m.FindingsDoc):
		// FindingsDoc is contractually absolute (see LocalManifest docs).
		// Refuse to read relative paths: those would resolve against the
		// current process cwd, which may differ from where the run wrote
		// findings.md, and could surface unrelated content.
		if data, err := os.ReadFile(m.FindingsDoc); err == nil {
			body = string(data)
		}
	}
	if body == "" {
		fmt.Fprintf(w, "No findings content available for run %s.\n", m.RunID)
		return
	}
	rendered, err := mdrender.RenderForWriter(w, body)
	if err != nil {
		// Glamour failure: fall back to raw markdown so the user still
		// sees the content.
		rendered = body
	}
	fmt.Fprint(w, rendered)
	if !strings.HasSuffix(rendered, "\n") {
		fmt.Fprintln(w)
	}
}
