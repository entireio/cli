package cli

// review_bridge.go wires cli-package implementations into the review
// subpackage's NewCommand Deps struct. Functions that need checkpoint
// access (headHasReviewCheckpoint) and per-agent reviewer constructors
// (launchableReviewerFor) live here to avoid the import cycle:
//   review → checkpoint → codex → review
//   review → claudecode/codex/geminicli → review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/entireio/cli/cmd/entire/cli/api"
	cliReview "github.com/entireio/cli/cmd/entire/cli/review"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

const (
	reviewTrailGranularityWholeChange = "whole_change"
	reviewTrailGranularityFile        = "file"
	reviewTrailGranularityLine        = "line"
	reviewTrailGranularityRange       = "range"
)

// buildReviewDeps builds the review.Deps struct used by review.NewCommand.
func buildReviewDeps() cliReview.Deps {
	return cliReview.Deps{
		GetAgentsWithHooksInstalled: GetAgentsWithHooksInstalled,
		NewSilentError: func(err error) error {
			return NewSilentError(err)
		},
		HeadHasReviewCheckpoint: headHasReviewCheckpoint,
		ReviewCheckpointContext: reviewCheckpointContext,
		ReviewerFor:             launchableReviewerFor,
		PostReviewToTrail:       postReviewToTrail,
	}
}

// postReviewToTrail posts the final review verdict to the current branch's
// trail as a finding, implementing the review subpackage's "trail" output
// destination. It lives in the cli package because the data API client and
// auth flow do.
func postReviewToTrail(ctx context.Context, out io.Writer, profileName, verdict string) error {
	if strings.TrimSpace(verdict) == "" {
		return errors.New("no review output to post")
	}
	inputs := reviewTrailFindingInputs(profileName, verdict)
	if len(inputs) == 0 {
		fmt.Fprintln(out, "Nothing to report, so nothing was posted to the trail.")
		return nil
	}
	return runAuthenticatedDataAPI(ctx, out, false, func(ctx context.Context, client *api.Client) error {
		target, err := resolveTrailReviewTarget(ctx, client, "")
		if err != nil {
			return err
		}
		if _, err := createTrailReviewFindings(ctx, client, target.Trail.ID, inputs); err != nil {
			return err
		}
		findingWord := "findings"
		if len(inputs) == 1 {
			findingWord = "finding"
		}
		if target.Trail.Number > 0 {
			fmt.Fprintf(out, "Posted the review verdict to trail #%d as %d %s.\n", target.Trail.Number, len(inputs), findingWord)
		} else {
			fmt.Fprintf(out, "Posted the review verdict to the trail as %d %s.\n", len(inputs), findingWord)
		}
		if link := trailReviewWebURL(target); link != "" {
			fmt.Fprintf(out, "View the trail: %s\n", link)
		}
		return nil
	})
}

// reviewTrailFindingInputs turns a final review verdict into trail findings.
// It first accepts the runner-style last JSON line format
// {"summary":"","comments":[...]}; when absent, it falls back to splitting
// top-level markdown bullets. This keeps trail output structurally correct even
// when custom judge prompts produce prose.
func reviewTrailFindingInputs(profileName, verdict string) []api.TrailReviewCommentInput {
	if inputs, ok := reviewTrailFindingInputsFromJSON(verdict); ok {
		return inputs
	}
	items := splitReviewVerdictFindings(verdict)
	if len(items) == 0 {
		// The verdict spans the whole change, so it uses "verdict" kind:
		// the API requires a valid granularity and rejects an empty value.
		return []api.TrailReviewCommentInput{reviewTrailFindingInputWithKind(profileName, verdict, "verdict")}
	}
	inputs := make([]api.TrailReviewCommentInput, 0, len(items))
	for _, item := range items {
		input := reviewTrailFindingInputWithKind(profileName, item, "finding")
		enrichReviewTrailFindingInputFromMarkdown(&input, item)
		inputs = append(inputs, input)
	}
	return inputs
}

func reviewTrailFindingInputsFromJSON(verdict string) ([]api.TrailReviewCommentInput, bool) {
	line := lastNonEmptyLine(verdict)
	if !strings.HasPrefix(line, "{") || !strings.Contains(line, "\"comments\"") {
		return nil, false
	}
	var payload reviewTrailJSONOutput
	if err := json.Unmarshal([]byte(line), &payload); err != nil {
		return nil, false
	}
	inputs := make([]api.TrailReviewCommentInput, 0, len(payload.Comments))
	for _, comment := range payload.Comments {
		body := strings.TrimSpace(comment.Body)
		if body == "" {
			continue
		}
		input := api.TrailReviewCommentInput{
			ClientID: generateTrailReviewClientID(),
			Body:     stringPtr(body),
			Location: reviewTrailLocationFromJSON(comment.Location),
		}
		if sev := normalizeReviewTrailSeverity(comment.Severity); sev != nil {
			input.Severity = sev
		}
		if comment.Confidence != nil && *comment.Confidence >= 0 && *comment.Confidence <= 1 {
			input.Confidence = comment.Confidence
		}
		inputs = append(inputs, input)
	}
	return inputs, true
}

func reviewTrailFindingInputWithKind(profileName, text, kind string) api.TrailReviewCommentInput {
	body := strings.TrimSpace(text)
	if p := strings.TrimSpace(profileName); p != "" {
		body = fmt.Sprintf("Review %s (profile: %s)\n\n%s", kind, p, body)
	}
	return api.TrailReviewCommentInput{
		ClientID: generateTrailReviewClientID(),
		Body:     stringPtr(body),
		Location: api.TrailReviewLocationCreateRequest{Granularity: reviewTrailGranularityWholeChange},
	}
}

type reviewTrailJSONOutput struct {
	Summary  string                   `json:"summary"`
	Comments []reviewTrailJSONComment `json:"comments"`
}

type reviewTrailJSONComment struct {
	Severity   string                  `json:"severity"`
	Confidence *float64                `json:"confidence"`
	Body       string                  `json:"body"`
	Location   reviewTrailJSONLocation `json:"location"`
}

type reviewTrailJSONLocation struct {
	Granularity  string `json:"granularity"`
	FilePath     string `json:"file_path"`
	StartLine    int    `json:"start_line"`
	EndLine      int    `json:"end_line"`
	SelectedText string `json:"selected_text"`
}

func reviewTrailLocationFromJSON(loc reviewTrailJSONLocation) api.TrailReviewLocationCreateRequest {
	filePath := strings.TrimSpace(loc.FilePath)
	withSelectedText := func(req api.TrailReviewLocationCreateRequest) api.TrailReviewLocationCreateRequest {
		if strings.TrimSpace(loc.SelectedText) != "" {
			req.SelectedText = stringPtr(loc.SelectedText)
		}
		return req
	}
	switch strings.ToLower(strings.TrimSpace(loc.Granularity)) {
	case reviewTrailGranularityLine:
		if filePath != "" && loc.StartLine > 0 {
			return withSelectedText(api.TrailReviewLocationCreateRequest{Granularity: reviewTrailGranularityLine, FilePath: stringPtr(filePath), StartLine: &loc.StartLine})
		}
	case reviewTrailGranularityRange:
		if filePath != "" && loc.StartLine > 0 && loc.EndLine > loc.StartLine {
			return withSelectedText(api.TrailReviewLocationCreateRequest{Granularity: reviewTrailGranularityRange, FilePath: stringPtr(filePath), StartLine: &loc.StartLine, EndLine: &loc.EndLine})
		}
		if filePath != "" && loc.StartLine > 0 {
			// Preserve the precise start-line anchor for malformed, missing, or
			// single-line ranges instead of silently degrading to whole-change.
			return withSelectedText(api.TrailReviewLocationCreateRequest{Granularity: reviewTrailGranularityLine, FilePath: stringPtr(filePath), StartLine: &loc.StartLine})
		}
	case reviewTrailGranularityFile:
		if filePath != "" {
			return api.TrailReviewLocationCreateRequest{Granularity: reviewTrailGranularityFile, FilePath: stringPtr(filePath)}
		}
	}
	return api.TrailReviewLocationCreateRequest{Granularity: reviewTrailGranularityWholeChange}
}

func enrichReviewTrailFindingInputFromMarkdown(input *api.TrailReviewCommentInput, body string) {
	if input == nil {
		return
	}
	if sev := inferReviewTrailSeverity(body); sev != nil {
		input.Severity = sev
	}
	if loc, ok := inferReviewTrailLocation(body); ok {
		input.Location = loc
	}
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

var reviewTrailLocationPattern = regexp.MustCompile("(?:^|[\\s(`])([A-Za-z0-9_./-]+\\.[A-Za-z0-9_+-]+):(\\d+)")

func inferReviewTrailLocation(body string) (api.TrailReviewLocationCreateRequest, bool) {
	match := reviewTrailLocationPattern.FindStringSubmatch(body)
	if len(match) != 3 {
		return api.TrailReviewLocationCreateRequest{}, false
	}
	line, err := strconv.Atoi(match[2])
	if err != nil || line <= 0 {
		return api.TrailReviewLocationCreateRequest{}, false
	}
	return api.TrailReviewLocationCreateRequest{Granularity: reviewTrailGranularityLine, FilePath: stringPtr(match[1]), StartLine: &line}, true
}

func inferReviewTrailSeverity(body string) *string {
	prefix := strings.ToLower(body)
	if len(prefix) > 120 {
		prefix = prefix[:120]
	}
	switch {
	case strings.Contains(prefix, "[p0]") || strings.Contains(prefix, "[p1]") || strings.Contains(prefix, "[high]") || strings.Contains(prefix, "critical"):
		return stringPtr(trailReviewSeverityHigh)
	case strings.Contains(prefix, "[p2]") || strings.Contains(prefix, "[medium]"):
		return stringPtr(trailReviewSeverityMedium)
	case strings.Contains(prefix, "[p3]") || strings.Contains(prefix, "[low]") || strings.Contains(prefix, "[nit]") || strings.Contains(prefix, "nit:"):
		return stringPtr(trailReviewSeverityLow)
	default:
		return nil
	}
}

func normalizeReviewTrailSeverity(raw string) *string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.Trim(s, "[](){}:*_ ")
	switch s {
	case trailReviewSeverityHigh, trailReviewSeverityMedium, trailReviewSeverityLow:
		return stringPtr(s)
	case "p0", "p1", "critical":
		return stringPtr(trailReviewSeverityHigh)
	case "p2":
		return stringPtr(trailReviewSeverityMedium)
	case "p3", "nit", "nits":
		return stringPtr(trailReviewSeverityLow)
	default:
		return nil
	}
}

func splitReviewVerdictFindings(verdict string) []string {
	var findings []string
	var current strings.Builder
	flush := func() {
		item := strings.TrimSpace(current.String())
		current.Reset()
		if item != "" {
			findings = append(findings, item)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(verdict), "\n") {
		if item, ok := topLevelBulletText(line); ok {
			flush()
			current.WriteString(item)
			continue
		}
		if item, ok := topLevelMarkedFindingText(line); ok {
			flush()
			current.WriteString(item)
			continue
		}
		if current.Len() == 0 {
			continue
		}
		current.WriteByte('\n')
		current.WriteString(line)
	}
	flush()
	return findings
}

func topLevelBulletText(line string) (string, bool) {
	trimmedRight := strings.TrimRight(line, " \t")
	leading := len(trimmedRight) - len(strings.TrimLeft(trimmedRight, " \t"))
	if leading != 0 {
		return "", false
	}
	trimmed := strings.TrimSpace(trimmedRight)
	if len(trimmed) < 3 {
		return "", false
	}
	if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") || strings.HasPrefix(trimmed, "+ ") {
		return strings.TrimSpace(trimmed[2:]), true
	}
	for i, r := range trimmed {
		if r >= '0' && r <= '9' {
			continue
		}
		if r == '.' && i > 0 && i+1 < len(trimmed) && trimmed[i+1] == ' ' {
			return strings.TrimSpace(trimmed[i+2:]), true
		}
		return "", false
	}
	return "", false
}

func topLevelMarkedFindingText(line string) (string, bool) {
	trimmedRight := strings.TrimRight(line, " \t")
	leading := len(trimmedRight) - len(strings.TrimLeft(trimmedRight, " \t"))
	if leading != 0 {
		return "", false
	}
	trimmed := strings.TrimSpace(trimmedRight)
	if !startsWithReviewSeverityMarker(trimmed) {
		return "", false
	}
	return trimmed, true
}

func startsWithReviewSeverityMarker(s string) bool {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "**")
	s = strings.TrimPrefix(s, "__")
	s = strings.TrimSpace(s)
	lower := strings.ToLower(s)
	for _, marker := range []string{"[critical]", "[high]", "[medium]", "[low]", "[p0]", "[p1]", "[p2]", "[p3]", "[nit]"} {
		if strings.HasPrefix(lower, marker) {
			return true
		}
	}
	for _, marker := range []string{"critical", "high", "medium", "low", "p0", "p1", "p2", "p3", "nit"} {
		if strings.HasPrefix(lower, marker+" —") || strings.HasPrefix(lower, marker+" -") || strings.HasPrefix(lower, marker+":") {
			return true
		}
	}
	return false
}

// trailWebURL builds the browser URL for a trail, matching the server's
// `<base>/<forge>/<owner>/<repo>/trails/<number>/<branch>` layout (the web UI
// shares the API origin). Returns "" when the target lacks the parts needed for
// a stable link.
func trailReviewWebURL(target trailReviewTarget) string {
	if target.Trail.Number <= 0 || target.Host == "" || target.Owner == "" || target.Repo == "" {
		return ""
	}
	base := strings.TrimRight(api.BaseURL(), "/")
	return fmt.Sprintf("%s/%s/%s/%s/trails/%d/%s",
		base, target.Host, target.Owner, target.Repo, target.Trail.Number, target.Trail.Branch)
}

// launchableReviewerFor returns the AgentReviewer for agents with a review-runner
// adapter, or nil for agents that are known to Entire but not yet wired into
// `entire review` fan-out. This lives in the cli package to avoid the import cycle:
//
//	review/cmd.go → claudecode/codex/geminicli → review
func launchableReviewerFor(agentName string) reviewtypes.AgentReviewer {
	switch agentName {
	case string(agent.AgentNameClaudeCode):
		return claudecode.NewReviewer()
	case string(agent.AgentNameCodex):
		return codex.NewReviewer()
	case string(agent.AgentNameGemini):
		return geminicli.NewReviewer()
	default:
		return nil
	}
}
