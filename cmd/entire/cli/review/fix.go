package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/mdrender"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/stringutil"
)

const reviewCommandBinary = "entire review"

// reviewFindingsRequest is what `entire review --findings` was asked for.
type reviewFindingsRequest struct {
	Handle string
	// JSON prints the handle's findings as JSON (--json).
	JSON bool
}

func runReviewFindings(ctx context.Context, cmd *cobra.Command, req reviewFindingsRequest, deps Deps) error {
	silentErr := deps.NewSilentError
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
	handle := strings.TrimSpace(req.Handle)
	if handle != "" {
		manifest, findErr := findReviewManifestByHandle(manifests, handle)
		if findErr != nil {
			cmd.SilenceUsage = true
			fmt.Fprintln(cmd.ErrOrStderr(), findErr.Error())
			printReviewFindingsHandles(cmd.ErrOrStderr(), manifests)
			return wrapReviewSilentError(silentErr, findErr)
		}
		if req.JSON {
			return printReviewFindingsJSON(cmd, manifest, manifests, deps)
		}
		printReviewManifestDetail(cmd.OutOrStdout(), manifest)
		return nil
	}
	if len(manifests) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No local review findings found.")
		return nil
	}
	if reviewCommandIsInteractive(cmd) {
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

// Finding is one finding as `entire review --findings <handle> --json`
// prints it. Each key is named after the `entire trail finding add` flag
// that posts it; a location key is left out when the finding has none.
type Finding struct {
	Severity   string   `json:"severity,omitempty"`
	Confidence *float64 `json:"confidence,omitempty"`
	File       string   `json:"file,omitempty"`
	Line       int      `json:"line,omitempty"`
	EndLine    int      `json:"end_line,omitempty"`
	Body       string   `json:"body"`
}

// reviewFindingsJSON is what `entire review --findings <handle> --json`
// prints: the saved run and its findings.
type reviewFindingsJSON struct {
	Handle    string    `json:"handle"`
	CreatedAt time.Time `json:"created_at,omitzero"`
	// HeadSHA is the commit the review ran on.
	HeadSHA  string    `json:"head_sha,omitempty"`
	Findings []Finding `json:"findings"`
}

// printReviewFindingsJSON prints a saved run's findings as JSON: those a
// trail post of the run would send. A run that left no report, or was saved
// before its report was kept, prints nothing to stdout and fails, so a
// script never reads it as a review with nothing to report.
func printReviewFindingsJSON(cmd *cobra.Command, manifest LocalReviewManifest, manifests []LocalReviewManifest, deps Deps) error {
	findings, err := savedReviewFindings(manifest, deps.TrailFindings)
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
		return wrapReviewSilentError(deps.NewSilentError, err)
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	if err := enc.Encode(reviewFindingsJSON{
		Handle:    reviewManifestUniqueHandle(manifest, manifests),
		CreatedAt: manifest.CreatedAt,
		HeadSHA:   manifest.StartingSHA,
		Findings:  findings,
	}); err != nil {
		return fmt.Errorf("print review findings: %w", err)
	}
	return nil
}

// savedReviewFindings splits a saved run's report into findings with split.
func savedReviewFindings(manifest LocalReviewManifest, split func(TrailReport) []Finding) ([]Finding, error) {
	report := manifest.Report
	switch {
	case report == nil:
		return nil, errors.New("these review findings were saved before --json could read them; run the review again")
	case report.Unreadable != "":
		return nil, fmt.Errorf("no findings to print: %s", report.Unreadable)
	case split == nil:
		return nil, errors.New("--findings --json is not available here")
	}
	findings := split(TrailReport{Comments: report.Comments, Prose: report.Prose})
	if findings == nil {
		findings = []Finding{}
	}
	return findings, nil
}

// reviewManifestUniqueHandle is the handle the review's completion footer
// prints for manifest, or, when another saved run shares it (two runs that
// started in the same second), one that selects only this run.
func reviewManifestUniqueHandle(manifest LocalReviewManifest, manifests []LocalReviewManifest) string {
	if handle := reviewManifestCompletionHandle(manifest); reviewManifestHandleCounts(manifests)[handle] == 1 {
		return handle
	}
	return reviewManifestViewHandle(manifest, manifests)
}

func findReviewManifestByHandle(manifests []LocalReviewManifest, handle string) (LocalReviewManifest, error) {
	var matched []LocalReviewManifest
	for _, manifest := range manifests {
		if reviewManifestHasHandle(manifest, handle) {
			matched = append(matched, manifest)
		}
	}
	switch len(matched) {
	case 0:
		return LocalReviewManifest{}, fmt.Errorf("no local review findings match %q", handle)
	case 1:
		return matched[0], nil
	default:
		return LocalReviewManifest{}, fmt.Errorf("local review findings handle %q is ambiguous", handle)
	}
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
	handle := reviewManifestCompletionHandle(manifest)
	if handle == "" {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Browse findings:")
	fmt.Fprintf(w, "  %s\n", reviewFindingsCommand(handle))
}

func reviewManifestHandle(manifest LocalReviewManifest) string {
	if handles := reviewManifestHandles(manifest); len(handles) > 0 {
		return handles[0]
	}
	return ""
}

func reviewManifestCompletionHandle(manifest LocalReviewManifest) string {
	if !manifest.CreatedAt.IsZero() {
		return reviewManifestTimeHandle(manifest.CreatedAt)
	}
	return reviewManifestHandle(manifest)
}

func printReviewFindingsList(w io.Writer, manifests []LocalReviewManifest) {
	fmt.Fprintln(w, "Review Findings")
	fmt.Fprintln(w)
	for _, manifest := range manifests {
		fmt.Fprintf(w, "%s\n", reviewManifestListLabel(manifest))
		if handle := reviewManifestViewHandle(manifest, manifests); handle != "" {
			fmt.Fprintf(w, "  view: %s\n", reviewFindingsCommand(handle))
		}
	}
}

func reviewFindingsCommand(handle string) string {
	quoted := "'" + strings.ReplaceAll(handle, "'", "'\\''") + "'"
	return fmt.Sprintf("%s --findings %s", reviewCommandBinary, quoted)
}

func printReviewFindingsHandles(w io.Writer, manifests []LocalReviewManifest) {
	handles := reviewAvailableManifestHandles(manifests)
	if len(handles) == 0 {
		return
	}
	fmt.Fprintln(w, "Available findings:")
	for _, handle := range handles {
		fmt.Fprintf(w, "  view: %s\n", reviewFindingsCommand(handle))
	}
}

func reviewManifestViewHandle(manifest LocalReviewManifest, manifests []LocalReviewManifest) string {
	counts := reviewManifestHandleCounts(manifests)
	for _, handle := range reviewManifestHandles(manifest) {
		if counts[handle] == 1 {
			return handle
		}
	}
	return ""
}

func reviewAvailableManifestHandles(manifests []LocalReviewManifest) []string {
	counts := reviewManifestHandleCounts(manifests)
	var handles []string
	for _, manifest := range manifests {
		for _, handle := range reviewManifestHandles(manifest) {
			if counts[handle] == 1 {
				handles = append(handles, handle)
			}
		}
	}
	return dedupeStrings(handles)
}

func reviewManifestHandleCounts(manifests []LocalReviewManifest) map[string]int {
	counts := make(map[string]int)
	for _, manifest := range manifests {
		for _, handle := range reviewManifestHandles(manifest) {
			counts[handle]++
		}
	}
	return counts
}

func printReviewManifestDetail(w io.Writer, manifest LocalReviewManifest) {
	fmt.Fprintf(w, "Review findings from %s\n\n", reviewManifestListLabel(manifest))
	for _, source := range manifest.Sources {
		printRenderedReviewSection(w, source.Label, stripTrailJSON(source.Output, manifest.TrailFindingsJSON))
	}
	if strings.TrimSpace(manifest.AggregateOutput) != "" {
		printRenderedReviewSection(w, "Aggregate summary", stripTrailJSON(manifest.AggregateOutput, manifest.TrailFindingsJSON))
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
		if text := strings.TrimSpace(stripTrailJSON(source.Output, manifest.TrailFindingsJSON)); text != "" {
			return stringutil.TruncateRunes(strings.Join(strings.Fields(text), " "), 70, "...")
		}
	}
	if text := strings.TrimSpace(stripTrailJSON(manifest.AggregateOutput, manifest.TrailFindingsJSON)); text != "" {
		return stringutil.TruncateRunes(strings.Join(strings.Fields(text), " "), 70, "...")
	}
	return ""
}

func reviewManifestHasHandle(manifest LocalReviewManifest, handle string) bool {
	return slices.Contains(reviewManifestHandles(manifest), handle)
}

func reviewManifestHandles(manifest LocalReviewManifest) []string {
	var handles []string
	for _, source := range manifest.Sources {
		if id := strings.TrimSpace(source.SessionID); id != "" {
			handles = append(handles, id)
		}
	}
	if !manifest.CreatedAt.IsZero() {
		handles = append(handles, reviewManifestTimeHandle(manifest.CreatedAt))
	}
	return dedupeStrings(handles)
}

func reviewManifestTimeHandle(t time.Time) string {
	return t.UTC().Format("20060102T150405")
}
