package review

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/mdrender"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/stringutil"
)

const reviewCommandBinary = "entire review"

func runReviewFindings(ctx context.Context, cmd *cobra.Command, silentErr func(error) error) error {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Run `entire enable` first.")
		return wrapReviewSilentError(silentErr, errors.New("not a git repository"))
	}
	manifests, err := loadLocalReviewManifests(ctx, worktreeRoot)
	if err != nil {
		return err
	}
	if len(manifests) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No local review findings found.")
		return nil
	}
	if interactive.IsTerminalWriter(cmd.OutOrStdout()) && interactive.CanPromptInteractively() {
		manifest, pickErr := promptForReviewManifest(ctx, manifests)
		if pickErr != nil {
			return pickErr
		}
		printReviewManifestDetail(cmd.OutOrStdout(), manifest)
		return nil
	}
	printReviewFindingsList(cmd.OutOrStdout(), manifests)
	return nil
}

func wrapReviewSilentError(silentErr func(error) error, err error) error {
	if silentErr == nil {
		return err
	}
	return silentErr(err)
}

func promptForReviewManifest(ctx context.Context, manifests []LocalReviewManifest) (LocalReviewManifest, error) {
	options := make([]huh.Option[int], len(manifests))
	for i, manifest := range manifests {
		options[i] = huh.NewOption(reviewManifestListLabel(manifest), i)
	}
	picked := 0
	form := newAccessibleForm(huh.NewGroup(
		huh.NewSelect[int]().
			Title("Select review findings").
			Options(options...).
			Height(min(len(options)+1, 10)).
			Value(&picked),
	))
	if err := form.RunWithContext(ctx); err != nil {
		return LocalReviewManifest{}, fmt.Errorf("review findings picker: %w", err)
	}
	return manifests[picked], nil
}

// reviewPickerHeight reserves the title + description lines huh.MultiSelect
// subtracts from Height before sizing its option viewport. Shared by the
// profile master picker.
func reviewPickerHeight(optionCount int) int {
	return min(optionCount+3, 14)
}

func writeReviewCompletionFooter(w io.Writer, manifest LocalReviewManifest) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Review complete.")
	if reviewManifestHandle(manifest) == "" {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Browse findings:")
	fmt.Fprintf(w, "  %s --findings\n", reviewCommandBinary)
}

func reviewManifestHandle(manifest LocalReviewManifest) string {
	for _, source := range manifest.Sources {
		if source.SessionID != "" {
			return source.SessionID
		}
	}
	return ""
}

func printReviewFindingsList(w io.Writer, manifests []LocalReviewManifest) {
	fmt.Fprintln(w, "Review Findings")
	fmt.Fprintln(w)
	for _, manifest := range manifests {
		fmt.Fprintf(w, "%s\n", reviewManifestListLabel(manifest))
	}
}

func printReviewManifestDetail(w io.Writer, manifest LocalReviewManifest) {
	fmt.Fprintf(w, "Review findings from %s\n\n", reviewManifestListLabel(manifest))
	for _, source := range manifest.Sources {
		printRenderedReviewSection(w, source.Label, source.Output)
	}
	if strings.TrimSpace(manifest.AggregateOutput) != "" {
		printRenderedReviewSection(w, "Aggregate summary", manifest.AggregateOutput)
	}
}

func printRenderedReviewSection(w io.Writer, title string, body string) {
	markdown := fmt.Sprintf("## %s\n\n%s\n", title, strings.TrimSpace(body))
	rendered, err := mdrender.RenderForWriter(w, markdown)
	if err != nil {
		rendered = markdown
	}
	fmt.Fprint(w, rendered)
	if !strings.HasSuffix(rendered, "\n") {
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w)
}

func reviewManifestListLabel(manifest LocalReviewManifest) string {
	handle := reviewManifestHandle(manifest)
	if handle == "" {
		handle = "unknown-session"
	}
	agents := make([]string, 0, len(manifest.Sources))
	for _, source := range manifest.Sources {
		if source.Label != "" {
			agents = append(agents, source.Label)
			continue
		}
		agents = append(agents, source.Agent)
	}
	preview := reviewManifestPreview(manifest)
	if preview != "" {
		return fmt.Sprintf("%s · local · %s · %s", handle, strings.Join(agents, ", "), preview)
	}
	return fmt.Sprintf("%s · local · %s", handle, strings.Join(agents, ", "))
}

func reviewManifestPreview(manifest LocalReviewManifest) string {
	for _, source := range manifest.Sources {
		if text := strings.TrimSpace(source.Output); text != "" {
			return stringutil.TruncateRunes(strings.Join(strings.Fields(text), " "), 70, "...")
		}
	}
	if text := strings.TrimSpace(manifest.AggregateOutput); text != "" {
		return stringutil.TruncateRunes(strings.Join(strings.Fields(text), " "), 70, "...")
	}
	return ""
}
