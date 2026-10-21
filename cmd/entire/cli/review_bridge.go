package cli

// review_bridge.go wires cli-package implementations into the review
// subpackage's NewCommand Deps struct. Functions that need checkpoint
// access (headHasReviewCheckpoint) and per-agent reviewer constructors
// (launchableReviewerFor) live here to avoid the import cycle:
//   review → checkpoint → codex → review
//   review → claudecode/codex/pi → review

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/agent/pi"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
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
		ResolveTarget:           resolveReviewTarget,
		CheckoutTarget:          checkoutReviewTarget,
		InspectTrust:            inspectReviewTrust,
		RemoveTarget:            removeReviewTarget,
	}
}

// postReviewToTrail posts the final review report to the current branch's
// trail as findings, implementing the review subpackage's "trail" output
// destination. It lives in the cli package because the data API client and
// auth flow do.
func postReviewToTrail(ctx context.Context, out io.Writer, profileName string, report cliReview.TrailReport) error {
	if len(report.Comments) == 0 && strings.TrimSpace(report.Prose) == "" {
		return errors.New("no review output to post")
	}
	inputs := reviewTrailFindingInputs(profileName, report)
	return runAuthenticatedTrailAPI(ctx, out, false, "", func(ctx context.Context, client *api.Client, repoID string) error {
		target, err := resolveTrailReviewTarget(ctx, client, repoID, "", "", "")
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

// reviewTrailFindingInputs turns a trail report into trail findings.
// Structured comments map directly; prose falls back to splitting top-level
// markdown bullets, which keeps custom judge prompts that ignore the JSON
// contract structurally correct.
func reviewTrailFindingInputs(profileName string, report cliReview.TrailReport) []api.TrailReviewCommentInput {
	inputs := reviewTrailFindingInputsFromComments(report.Comments)
	verdict := report.Prose
	if strings.TrimSpace(verdict) == "" {
		return inputs
	}
	items := splitReviewVerdictFindings(verdict)
	if len(items) == 0 {
		// The verdict spans the whole change, so it uses "verdict" kind:
		// the API requires a valid granularity and rejects an empty value.
		return append(inputs, reviewTrailFindingInputWithKind(profileName, verdict, "verdict"))
	}
	for _, item := range items {
		input := reviewTrailFindingInputWithKind(profileName, item, "finding")
		enrichReviewTrailFindingInputFromMarkdown(&input, item)
		inputs = append(inputs, input)
	}
	return inputs
}

func reviewTrailFindingInputsFromComments(comments []cliReview.TrailComment) []api.TrailReviewCommentInput {
	inputs := make([]api.TrailReviewCommentInput, 0, len(comments))
	for _, comment := range comments {
		input := api.TrailReviewCommentInput{
			ClientID: generateTrailReviewClientID(),
			Body:     stringPtr(strings.TrimSpace(comment.Body)),
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
	return inputs
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

func reviewTrailLocationFromJSON(loc cliReview.TrailLocation) api.TrailReviewLocationCreateRequest {
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

// splitReviewVerdictFindings splits a prose verdict into its top-level
// findings. A line holding only a severity ("### High", "**Low:**") or a
// Markdown heading that is not itself a marked finding, such as the
// "## <reviewer>" separating combined narratives, ends the current finding
// and is not one itself. Text under a heading is preamble when a finding
// follows it and a finding of its own when none does, so a section without
// list items is not lost. Code blocks always continue the current text, so
// their "#" or "- " lines neither start nor end a finding.
func splitReviewVerdictFindings(verdict string) []string {
	var findings []string
	var current, section strings.Builder
	inSection, inFence := false, false
	flush := func(b *strings.Builder) {
		item := strings.TrimSpace(b.String())
		b.Reset()
		if item != "" {
			findings = append(findings, item)
		}
	}
	appendLine := func(line string) {
		target := &current
		if current.Len() == 0 {
			if !inSection || (section.Len() == 0 && strings.TrimSpace(line) == "") {
				return
			}
			target = &section
		}
		if target.Len() > 0 {
			target.WriteByte('\n')
		}
		target.WriteString(line)
	}
	startFinding := func(item string) {
		flush(&current)
		section.Reset()
		current.WriteString(item)
	}
	startSection := func() {
		flush(&current)
		flush(&section)
		inSection = true
	}
	for _, line := range strings.Split(strings.TrimSpace(verdict), "\n") {
		if fence := isCodeFence(line); inFence || fence {
			if fence {
				inFence = !inFence
			}
			appendLine(line)
			continue
		}
		if isSeverityHeading(line) {
			startSection()
			continue
		}
		if item, ok := topLevelBulletText(line); ok {
			startFinding(item)
			continue
		}
		if item, ok := topLevelMarkedFindingText(line); ok {
			startFinding(item)
			continue
		}
		if isMarkdownHeading(line) {
			startSection()
			continue
		}
		appendLine(line)
	}
	flush(&current)
	flush(&section)
	return findings
}

func isCodeFence(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")
}

// isSeverityHeading reports whether line, unindented, holds only a severity
// label.
func isSeverityHeading(line string) bool {
	if line != strings.TrimLeft(line, " \t") {
		return false
	}
	return normalizeReviewTrailSeverity(strings.TrimLeft(strings.TrimSpace(line), "#> ")) != nil
}

// isMarkdownHeading reports whether line is an unindented ATX heading:
// one to six "#" followed by a space or nothing.
func isMarkdownHeading(line string) bool {
	rest := strings.TrimLeft(line, "#")
	level := len(line) - len(rest)
	return level >= 1 && level <= 6 && (strings.TrimSpace(rest) == "" || strings.HasPrefix(rest, " "))
}

func topLevelBulletText(line string) (string, bool) {
	trimmedRight := strings.TrimRight(line, " \t")
	leading := len(trimmedRight) - len(strings.TrimLeft(trimmedRight, " \t"))
	if leading != 0 {
		return "", false
	}
	item, ok := cliReview.ListItemText(strings.TrimSpace(trimmedRight))
	if !ok || item == "" {
		return "", false
	}
	return item, true
}

func topLevelMarkedFindingText(line string) (string, bool) {
	trimmedRight := strings.TrimRight(line, " \t")
	leading := len(trimmedRight) - len(strings.TrimLeft(trimmedRight, " \t"))
	if leading != 0 {
		return "", false
	}
	trimmed := strings.TrimSpace(trimmedRight)
	if !cliReview.StartsWithSeverityMarker(trimmed) {
		return "", false
	}
	return trimmed, true
}

// trailReviewWebURL builds the browser URL for a trail, matching the server's
// `<web-origin>/<forge>/<owner>/<repo>/trails/<number>/<branch>` layout.
// Returns "" when the target lacks the parts needed for a stable link.
func trailReviewWebURL(target trailReviewTarget) string {
	if target.Trail.Number <= 0 || target.Host == "" || target.Owner == "" || target.Repo == "" {
		return ""
	}
	base, err := auth.WebBaseURL()
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%s/%s/%s/%s/trails/%d/%s",
		base, target.Host, target.Owner, target.Repo, target.Trail.Number, target.Trail.Branch)
}

// launchableReviewerFor returns the AgentReviewer for agents with a review-runner
// adapter, or nil for agents that are known to Entire but not yet wired into
// `entire review` fan-out. This lives in the cli package to avoid the import cycle:
//
//	review/cmd.go → claudecode/codex/pi → review
func launchableReviewerFor(agentName string) reviewtypes.AgentReviewer {
	switch agentName {
	case string(agent.AgentNameClaudeCode):
		return claudecode.NewReviewer()
	case string(agent.AgentNameCodex):
		return codex.NewReviewer()
	case string(agent.AgentNamePi):
		return pi.NewReviewer()
	default:
		return nil
	}
}
