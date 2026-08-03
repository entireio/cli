package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/stringutil"
)

const (
	tuneDocCap               = 6000 // max chars embedded per doc (CLAUDE.md etc.)
	tuneReadmeCap            = 2000
	tuneTopFiles             = 15 // hottest files to surface from checkpoints
	tuneMaxTrailsForFindings = 8  // trails to pull findings from in the trails tier
)

const (
	sourceCheckpoint  = "checkpoint"
	sourceCheckpoints = "checkpoints"
	sourceTrail       = "trail"
	sourceTrails      = "trails"
)

// tuneSources selects which data tiers gatherTuningContext collects.
type tuneSources struct {
	repo        bool
	prs         bool
	checkpoints bool
	trails      bool
}

func allTuneSources() tuneSources {
	return tuneSources{repo: true, prs: true, checkpoints: true, trails: true}
}

func parseTuneSources(list []string) (tuneSources, error) {
	if len(list) == 0 {
		return allTuneSources(), nil
	}
	var s tuneSources
	for _, item := range list {
		switch strings.ToLower(strings.TrimSpace(item)) {
		case "":
			continue
		case "all":
			return allTuneSources(), nil
		case "repo":
			s.repo = true
		case "pr", "prs", "issue", "issues":
			s.prs = true
		case sourceCheckpoint, sourceCheckpoints:
			s.checkpoints = true
		case sourceTrail, sourceTrails:
			s.trails = true
		default:
			return s, fmt.Errorf("unknown source %q (valid: repo, prs, checkpoints, trails, all)", item)
		}
	}
	return s, nil
}

// gatherTuningContext builds the markdown "tuning brief" from the selected
// tiers. Every tier is best-effort: a tier that is unavailable (no gh, no
// checkpoints, trails not enabled) records a one-line skip note instead of
// failing the command.
func gatherTuningContext(ctx context.Context, errW io.Writer, repoRoot string, src tuneSources, limit int, insecureHTTP bool) string {
	var b strings.Builder

	if src.repo {
		b.WriteString("### Repository (static)\n\n")
		b.WriteString(gatherRepoStatics(repoRoot))
		b.WriteString("\n")
	}
	if src.prs {
		b.WriteString("### Merged PRs & issues\n\n")
		b.WriteString(gatherPRsAndIssues(ctx, limit))
		b.WriteString("\n")
	}
	if src.checkpoints {
		b.WriteString("### Checkpoint history (what changes look like here)\n\n")
		b.WriteString(gatherCheckpoints(ctx))
		b.WriteString("\n")
	}
	if src.trails {
		b.WriteString("### Trail history & past findings (eval feedback loop)\n\n")
		b.WriteString(gatherTrails(ctx, errW, limit, insecureHTTP))
		b.WriteString("\n")
	}

	return b.String()
}

func skip(reason string) string { return "_skipped: " + reason + "_\n" }

func gatherRepoStatics(repoRoot string) string {
	var b strings.Builder

	if mod, ok := readCapped(filepath.Join(repoRoot, "go.mod"), 400); ok {
		for _, line := range strings.Split(mod, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "module ") || strings.HasPrefix(line, "go ") {
				fmt.Fprintf(&b, "- %s\n", line)
			}
		}
	}

	if entries, err := os.ReadDir(repoRoot); err == nil {
		var dirs []string
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				dirs = append(dirs, e.Name())
			}
		}
		if len(dirs) > 0 {
			fmt.Fprintf(&b, "- Top-level dirs: %s\n", strings.Join(dirs, ", "))
		}
	}
	b.WriteString("\n")

	for _, doc := range []struct {
		name string
		cap  int
	}{
		{"CLAUDE.md", tuneDocCap},
		{"AGENTS.md", tuneDocCap},
		{"README.md", tuneReadmeCap},
	} {
		text, ok := readCapped(filepath.Join(repoRoot, doc.name), doc.cap)
		if !ok || strings.TrimSpace(text) == "" {
			continue
		}
		fmt.Fprintf(&b, "#### %s\n\n", doc.name)
		b.WriteString(text)
		b.WriteString("\n\n")
	}

	if b.Len() == 0 {
		return skip("no go.mod, CLAUDE.md, AGENTS.md, or README.md found")
	}
	return b.String()
}

// readCapped reads a file and truncates it to maxLen characters, appending a
// truncation marker when cut. Returns ok=false when the file can't be read.
func readCapped(path string, maxLen int) (string, bool) {
	data, err := os.ReadFile(path) //nolint:gosec // caller passes repo-root-relative paths
	if err != nil {
		return "", false
	}
	s := string(data)
	if len(s) > maxLen {
		s = s[:maxLen] + "\n…(truncated)…"
	}
	return s, true
}

func gatherPRsAndIssues(ctx context.Context, limit int) string {
	if _, err := exec.LookPath("gh"); err != nil {
		return skip("gh CLI not on PATH")
	}
	return gatherGHItems(ctx, "pr", "merged", "Recent merged PRs", limit) + "\n" +
		gatherGHItems(ctx, "issue", "all", "Recent issues", limit)
}

// gatherGHItems lists one kind of GitHub item (pr/issue) in the given state and
// renders it as a markdown bullet list, or a skip note when gh fails.
func gatherGHItems(ctx context.Context, kind, state, header string, limit int) string {
	items, err := runGHList(ctx, kind, "--state", state, "--limit", strconv.Itoa(limit),
		"--json", "number,title,labels")
	if err != nil {
		return skip(fmt.Sprintf("gh %s list failed: %s", kind, oneLine(err.Error())))
	}
	if len(items) == 0 {
		return header + ": none\n"
	}
	var b strings.Builder
	b.WriteString(header + ":\n")
	for _, p := range items {
		fmt.Fprintf(&b, "- #%d %s%s\n", p.Number, oneLine(p.Title), labelSuffix(p.Labels))
	}
	return b.String()
}

type ghLabel struct {
	Name string `json:"name"`
}

type ghItem struct {
	Number int       `json:"number"`
	Title  string    `json:"title"`
	Labels []ghLabel `json:"labels"`
}

func runGHList(ctx context.Context, kind string, args ...string) ([]ghItem, error) {
	full := append([]string{kind, "list"}, args...)
	cmd := exec.CommandContext(ctx, "gh", full...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh %s list: %w", kind, err)
	}
	var items []ghItem
	if err := json.Unmarshal(out, &items); err != nil {
		return nil, fmt.Errorf("decode gh output: %w", err)
	}
	return items, nil
}

func labelSuffix(labels []ghLabel) string {
	if len(labels) == 0 {
		return ""
	}
	names := make([]string, 0, len(labels))
	for _, l := range labels {
		names = append(names, l.Name)
	}
	return " [" + strings.Join(names, ", ") + "]"
}

func gatherCheckpoints(ctx context.Context) string {
	checkpoints, err := strategy.ListCheckpoints(ctx)
	if err != nil {
		return skip("could not list checkpoints: " + oneLine(err.Error()))
	}
	if len(checkpoints) == 0 {
		return skip("no committed checkpoints in this repo yet")
	}

	fileCount := map[string]int{}
	agentCount := map[string]int{}
	var newest, oldest time.Time
	for _, c := range checkpoints {
		for _, f := range c.FilesTouched {
			fileCount[f]++
		}
		if a := strings.TrimSpace(string(c.Agent)); a != "" {
			agentCount[a]++
		}
		if c.CreatedAt.IsZero() {
			continue
		}
		if newest.IsZero() || c.CreatedAt.After(newest) {
			newest = c.CreatedAt
		}
		if oldest.IsZero() || c.CreatedAt.Before(oldest) {
			oldest = c.CreatedAt
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "- %d committed checkpoints", len(checkpoints))
	if !newest.IsZero() {
		fmt.Fprintf(&b, " (%s to %s)", oldest.Format("2006-01-02"), newest.Format("2006-01-02"))
	}
	b.WriteString("\n")
	if len(agentCount) > 0 {
		fmt.Fprintf(&b, "- Agents: %s\n", joinCounts(agentCount, 5))
	}
	if len(fileCount) > 0 {
		b.WriteString("- Most frequently changed files (churn hotspots):\n")
		for _, fc := range topCounts(fileCount, tuneTopFiles) {
			fmt.Fprintf(&b, "  - %s (%d)\n", fc.key, fc.n)
		}
	}
	return b.String()
}

func gatherTrails(ctx context.Context, errW io.Writer, limit int, insecureHTTP bool) string {
	var out strings.Builder
	err := runAuthenticatedDataAPI(ctx, errW, insecureHTTP, func(ctx context.Context, client *api.Client) error {
		forge, owner, repo, err := resolveTrailRemote(ctx)
		if err != nil {
			return err
		}
		resp, err := client.Get(ctx, trailsBasePath(forge, owner, repo)+trailListQuery(nil, "", limit))
		if err != nil {
			return fmt.Errorf("list trails: %w", err)
		}
		defer resp.Body.Close()
		if err := checkTrailResponse(resp); err != nil {
			return err
		}
		var list api.TrailListResponse
		if err := api.DecodeJSON(resp, &list); err != nil {
			return fmt.Errorf("decode trail list: %w", err)
		}
		if len(list.Trails) == 0 {
			out.WriteString("Trails are enabled but none exist yet.\n")
			return nil
		}
		fmt.Fprintf(&out, "- %d recent trails\n", len(list.Trails))

		sevCount := map[string]int{}
		statusCount := map[string]int{}
		fileCount := map[string]int{}
		total := 0
		fetchFailures := 0
		scanned := list.Trails
		if len(scanned) > tuneMaxTrailsForFindings {
			scanned = scanned[:tuneMaxTrailsForFindings]
		}
		for i := range scanned {
			comments, err := fetchAllTrailReviewComments(ctx, client, scanned[i].ID, trailReviewSummaryOptions())
			if err != nil {
				fetchFailures++
				continue
			}
			for _, c := range comments {
				total++
				if c.Severity != nil {
					sevCount[*c.Severity]++
				}
				statusCount[c.Status]++
				if c.Location.FilePath != nil {
					fileCount[*c.Location.FilePath]++
				}
			}
		}
		if total == 0 {
			// Distinguish "genuinely no findings" from "couldn't fetch them" —
			// they imply very different things for the tuning model.
			if fetchFailures > 0 {
				fmt.Fprintf(&out, "- Could not fetch review findings (%d of %d trails errored).\n", fetchFailures, len(scanned))
			} else {
				out.WriteString("- No past review findings recorded.\n")
			}
			return nil
		}
		fmt.Fprintf(&out, "- %d past review findings across %d trails\n", total, len(scanned))
		if fetchFailures > 0 {
			fmt.Fprintf(&out, "  - note: %d of %d trails' findings could not be fetched\n", fetchFailures, len(scanned))
		}
		if len(sevCount) > 0 {
			fmt.Fprintf(&out, "  - by severity: %s\n", joinCounts(sevCount, 5))
		}
		if len(statusCount) > 0 {
			// resolved/dismissed/stale findings are a calibration signal:
			// dismissed or stale findings hint the eval over-fired.
			fmt.Fprintf(&out, "  - by status: %s\n", joinCounts(statusCount, 6))
		}
		if len(fileCount) > 0 {
			out.WriteString("  - files most flagged:\n")
			for _, fc := range topCounts(fileCount, 10) {
				fmt.Fprintf(&out, "    - %s (%d)\n", fc.key, fc.n)
			}
		}
		return nil
	})
	if err != nil {
		return skip("trails unavailable: " + oneLine(err.Error()))
	}
	return out.String()
}

type keyCount struct {
	key string
	n   int
}

func topCounts(m map[string]int, n int) []keyCount {
	out := make([]keyCount, 0, len(m))
	for k, v := range m {
		out = append(out, keyCount{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		return out[i].key < out[j].key
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func joinCounts(m map[string]int, n int) string {
	parts := make([]string, 0, n)
	for _, fc := range topCounts(m, n) {
		parts = append(parts, fmt.Sprintf("%s=%d", fc.key, fc.n))
	}
	return strings.Join(parts, ", ")
}

func oneLine(s string) string {
	return stringutil.TruncateRunes(stringutil.CollapseWhitespace(s), 200, "…")
}
