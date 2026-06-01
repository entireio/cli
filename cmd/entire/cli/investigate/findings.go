package investigate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// runInvestigateFindings handles `entire investigate --findings`: prints
// a plain list of saved investigations with `entire investigate fix
// <run-id>` hints.
func runInvestigateFindings(ctx context.Context, cmd *cobra.Command, silentErr func(error) error) error {
	if _, err := paths.WorktreeRoot(ctx); err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Run `entire enable` first.")
		return wrapSilent(silentErr, errors.New("not a git repository"))
	}
	store, err := NewLocalManifestStore(ctx)
	if err != nil {
		return fmt.Errorf("open manifest store: %w", err)
	}
	manifests, err := store.List(ctx)
	if err != nil {
		return fmt.Errorf("list manifests: %w", err)
	}
	if len(manifests) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No local investigations found.")
		return nil
	}
	// Always print the full list — users reach for --findings to see all
	// runs. The `fix:` hint per row gives them the next step.
	printInvestigateFindingsList(cmd.OutOrStdout(), manifests)
	return nil
}

// PrintInvestigateFindingsListForTest exposes printInvestigateFindingsList
// to tests in package investigate_test.
func PrintInvestigateFindingsListForTest(w io.Writer, manifests []LocalManifest) {
	printInvestigateFindingsList(w, manifests)
}

// printInvestigateFindingsList renders the non-TTY list view. Each
// manifest gets a header row, a `view:` hint (pointing at
// `entire investigate show <run-id>` which works regardless of where the
// findings live), and a `fix:` hint (the apply-findings next step). When
// findings are still on disk (paused/cancelled), an additional `path:`
// line points at the file for direct inspection.
func printInvestigateFindingsList(w io.Writer, manifests []LocalManifest) {
	fmt.Fprintln(w, "Investigations")
	fmt.Fprintln(w)
	for _, m := range manifests {
		fmt.Fprintln(w, investigateManifestListLabel(m))
		fmt.Fprintf(w, "  view:    entire investigate show %s\n", m.RunID)
		// `fix` only makes sense for terminal outcomes (Quorum/Stalled).
		// Paused/Cancelled runs need to be resumed (or cleaned), not fed
		// into a coding agent off of partial findings.
		switch m.Outcome {
		case string(OutcomePaused), string(OutcomeCancelled):
			fmt.Fprintf(w, "  resume:  entire investigate --continue %s\n", m.RunID)
		default:
			fmt.Fprintf(w, "  fix:     entire investigate fix %s\n", m.RunID)
		}
		// Add the on-disk path only when it points at a still-present
		// file (paused/cancelled). Terminal outcomes auto-clean the
		// per-run dir, so printing the stale path would be misleading.
		if m.FindingsContent == "" && m.FindingsDoc != "" {
			fmt.Fprintf(w, "  path:    %s\n", m.FindingsDoc)
		}
	}
}

// investigateManifestListLabel formats one manifest for picker / list
// display. Format: "<run-id> · <topic> · <agents> · <relative-time>".
func investigateManifestListLabel(m LocalManifest) string {
	when := relativeTimeLabel(m.StartedAt)
	parts := []string{m.RunID}
	if topic := strings.TrimSpace(m.Topic); topic != "" {
		parts = append(parts, topic)
	}
	if len(m.Agents) > 0 {
		parts = append(parts, strings.Join(m.Agents, ", "))
	}
	if when != "" {
		parts = append(parts, when)
	}
	return strings.Join(parts, " · ")
}

// relativeTimeLabel formats t as a coarse "Nm ago" / "Nh ago" / "Nd ago"
// string suitable for picker labels. Returns the empty string for the
// zero value.
func relativeTimeLabel(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
