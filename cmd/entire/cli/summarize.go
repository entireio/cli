package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"entire.io/cli/cmd/entire/cli/logging"
	"entire.io/cli/cmd/entire/cli/stringutil"
)

var (
	// loggingOnce ensures logging is initialized only once for summarization
	loggingOnce sync.Once
)

// ensureLoggingInitialized initializes the general-purpose logger once.
func ensureLoggingInitialized() {
	loggingOnce.Do(func() {
		_ = logging.InitGeneral() //nolint:errcheck // Best-effort initialization, falls back to stderr
	})
}

// Summary holds the extracted/generated summary for a checkpoint.
// Uses AI summarization via Claude CLI when available, falls back to heuristic extraction.
type Summary struct {
	Intent         string   // The user's original intent (first prompt)
	Outcome        string   // The final outcome (last assistant response)
	Learnings      []string // Key learnings from the session
	FrictionPoints []string // Friction points encountered
}

// maxSummaryFieldLength is the maximum length (in runes) for Intent and Outcome fields.
const maxSummaryFieldLength = 200

// maxTranscriptMessages is the maximum number of messages to include when calling Claude.
// This keeps the prompt within reasonable token limits.
const maxTranscriptMessages = 50

// claudeTimeout is the timeout for Claude CLI calls.
const claudeTimeout = 60 * time.Second

// ErrClaudeCLINotFound is returned when the claude CLI is not available.
var ErrClaudeCLINotFound = errors.New("claude CLI not found")

// GenerateSummary extracts a summary from a transcript.
// Uses heuristic extraction (first user prompt, last assistant message).
// For AI-powered summarization, use GenerateAISummary instead.
//
//nolint:unparam // error return kept for API consistency with GenerateAISummary
func GenerateSummary(transcript []transcriptLine) (*Summary, error) {
	prompts := extractUserPrompts(transcript)

	var intent string
	if len(prompts) > 0 {
		intent = stringutil.TruncateRunes(prompts[0], maxSummaryFieldLength, "...")
	}

	outcome := extractLastAssistantMessage(transcript)
	outcome = stringutil.TruncateRunes(outcome, maxSummaryFieldLength, "...")

	return &Summary{
		Intent:         intent,
		Outcome:        outcome,
		Learnings:      nil,
		FrictionPoints: nil,
	}, nil
}

// SummaryResult holds a summary along with token usage information.
type SummaryResult struct {
	Summary        *Summary
	InputTokens    int
	OutputTokens   int
	UsedFallback   bool   // True if heuristic extraction was used instead of AI
	FallbackReason string // Reason for fallback (error message from Claude CLI)
}

// GenerateAISummary generates a summary using Claude CLI.
// Falls back to heuristic extraction if Claude CLI is unavailable or fails.
func GenerateAISummary(ctx context.Context, transcript []transcriptLine) (*Summary, error) {
	result, err := GenerateAISummaryWithUsage(ctx, transcript)
	if err != nil {
		return nil, err
	}
	return result.Summary, nil
}

// GenerateAISummaryWithUsage generates a summary using Claude CLI and returns token usage.
// Falls back to heuristic extraction if Claude CLI is unavailable or fails.
func GenerateAISummaryWithUsage(ctx context.Context, transcript []transcriptLine) (*SummaryResult, error) {
	// Format transcript for AI
	transcriptText := formatTranscriptForAI(transcript)
	if transcriptText == "" {
		summary, err := GenerateSummary(transcript)
		return &SummaryResult{Summary: summary}, err
	}

	// Truncate if too long
	if len(transcript) > maxTranscriptMessages {
		transcriptText = truncateTranscriptForAI(transcript, maxTranscriptMessages)
	}

	// Build prompt
	prompt := buildCheckpointSummaryPrompt(transcriptText)

	// Call Claude CLI
	response, claudeErr := callClaudeWithUsage(ctx, prompt)
	if claudeErr != nil {
		// Fall back to heuristic extraction if Claude fails for any reason
		// Log the error for debugging
		ensureLoggingInitialized()
		logCtx := logging.WithComponent(ctx, "summarize")
		logging.Debug(logCtx, "Claude CLI failed, using heuristic fallback", "error", claudeErr.Error())

		summary, _ := GenerateSummary(transcript) //nolint:errcheck // GenerateSummary never fails
		return &SummaryResult{Summary: summary, UsedFallback: true, FallbackReason: claudeErr.Error()}, nil
	}

	// Parse response
	summary, parseErr := parseSummaryResponse(response.Result)
	if parseErr != nil {
		// Fall back to heuristic extraction if parsing fails
		ensureLoggingInitialized()
		logCtx := logging.WithComponent(ctx, "summarize")
		logging.Debug(logCtx, "Failed to parse Claude response, using heuristic fallback", "error", parseErr.Error())

		summary, _ = GenerateSummary(transcript) //nolint:errcheck // GenerateSummary never fails
		return &SummaryResult{
			Summary:        summary,
			InputTokens:    response.InputTokens,
			OutputTokens:   response.OutputTokens,
			UsedFallback:   true,
			FallbackReason: parseErr.Error(),
		}, nil
	}

	return &SummaryResult{
		Summary:      summary,
		InputTokens:  response.InputTokens,
		OutputTokens: response.OutputTokens,
	}, nil
}

// formatTranscriptForAI formats a transcript into a human-readable text format
// suitable for sending to Claude.
func formatTranscriptForAI(transcript []transcriptLine) string {
	if len(transcript) == 0 {
		return ""
	}

	var sb strings.Builder
	for _, line := range transcript {
		switch line.Type {
		case transcriptTypeUser:
			text := extractTextFromMessage(line.Message)
			if text != "" {
				sb.WriteString("User: ")
				sb.WriteString(text)
				sb.WriteString("\n\n")
			}
		case transcriptTypeAssistant:
			text := extractTextFromAssistantMessage(line.Message)
			if text != "" {
				sb.WriteString("Assistant: ")
				sb.WriteString(text)
				sb.WriteString("\n\n")
			}
		}
	}
	return sb.String()
}

// truncateTranscriptForAI keeps only the last N messages from the transcript
// and formats them for AI consumption.
func truncateTranscriptForAI(transcript []transcriptLine, maxMessages int) string {
	if len(transcript) <= maxMessages {
		return formatTranscriptForAI(transcript)
	}

	start := len(transcript) - maxMessages
	if start < 0 {
		start = 0
	}
	return formatTranscriptForAI(transcript[start:])
}

// extractTextFromMessage extracts text content from a user message JSON.
func extractTextFromMessage(message json.RawMessage) string {
	var msg userMessage
	if err := json.Unmarshal(message, &msg); err != nil {
		return ""
	}

	// Handle string content
	if str, ok := msg.Content.(string); ok {
		return str
	}

	// Handle array content (only if it contains text blocks)
	if arr, ok := msg.Content.([]interface{}); ok {
		var texts []string
		for _, item := range arr {
			if m, ok := item.(map[string]interface{}); ok {
				if m["type"] == contentTypeText {
					if text, ok := m["text"].(string); ok {
						texts = append(texts, text)
					}
				}
			}
		}
		if len(texts) > 0 {
			return strings.Join(texts, "\n\n")
		}
	}

	return ""
}

// extractTextFromAssistantMessage extracts text content from an assistant message JSON.
func extractTextFromAssistantMessage(message json.RawMessage) string {
	var msg assistantMessage
	if err := json.Unmarshal(message, &msg); err != nil {
		return ""
	}

	var texts []string
	for _, block := range msg.Content {
		if block.Type == contentTypeText && block.Text != "" {
			texts = append(texts, block.Text)
		}
	}

	return strings.Join(texts, "\n\n")
}

// ClaudeResponse holds the response from a Claude CLI call including token usage.
type ClaudeResponse struct {
	Result       string
	InputTokens  int
	OutputTokens int
}

// callClaudeWithUsage calls the Claude CLI and returns the response including token usage.
func callClaudeWithUsage(ctx context.Context, prompt string) (*ClaudeResponse, error) {
	// Check if claude CLI exists
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return nil, ErrClaudeCLINotFound
	}

	// Create context with timeout
	ctx, cancel := context.WithTimeout(ctx, claudeTimeout)
	defer cancel()

	// Run claude CLI with print mode and JSON output.
	// Run from temp dir to avoid triggering project-specific hooks that could interfere.
	cmd := exec.CommandContext(ctx, claudePath, "-p", prompt, "--output-format", "json") //nolint:gosec // claudePath is from exec.LookPath, prompt is user content
	cmd.Dir = os.TempDir()
	output, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// Include exit code, stderr, and any output for debugging
			return nil, fmt.Errorf("claude CLI failed (exit %d): %s (output: %s)",
				exitErr.ExitCode(), string(exitErr.Stderr), string(output))
		}
		return nil, fmt.Errorf("claude CLI failed: %w (output: %s)", err, string(output))
	}

	// Parse JSON output to extract the result text and token usage
	var claudeOutput struct {
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
		// Token usage is nested under "usage"
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(output, &claudeOutput); err != nil {
		// If not JSON, return raw output
		return &ClaudeResponse{Result: string(output)}, nil
	}

	// Check for errors
	if claudeOutput.IsError {
		return nil, errors.New("claude CLI returned error")
	}

	// If result is empty, Claude might not have responded
	if claudeOutput.Result == "" {
		// Log the full output for debugging
		ensureLoggingInitialized()
		logging.Debug(context.Background(), "Claude CLI returned empty result",
			"full_output", string(output),
			"is_error", claudeOutput.IsError,
			"input_tokens", claudeOutput.Usage.InputTokens,
			"output_tokens", claudeOutput.Usage.OutputTokens)
		return nil, errors.New("claude CLI returned empty result")
	}

	return &ClaudeResponse{
		Result:       claudeOutput.Result,
		InputTokens:  claudeOutput.Usage.InputTokens,
		OutputTokens: claudeOutput.Usage.OutputTokens,
	}, nil
}

// summaryResponse represents the expected JSON structure from Claude.
type summaryResponse struct {
	Intent         string   `json:"intent"`
	Outcome        string   `json:"outcome"`
	Learnings      []string `json:"learnings"`
	FrictionPoints []string `json:"friction_points"`
}

// parseSummaryResponse parses the JSON response from Claude into a Summary.
// Handles cases where Claude wraps JSON in explanatory text.
func parseSummaryResponse(response string) (*Summary, error) {
	if response == "" {
		return nil, errors.New("empty response")
	}

	// Try to parse the entire response as JSON first
	var sr summaryResponse
	if err := json.Unmarshal([]byte(response), &sr); err == nil {
		return &Summary{
			Intent:         sr.Intent,
			Outcome:        sr.Outcome,
			Learnings:      sr.Learnings,
			FrictionPoints: sr.FrictionPoints,
		}, nil
	}

	// If that fails, try to extract JSON from the response
	// Claude sometimes wraps JSON in explanatory text
	startIdx := strings.Index(response, "{")
	endIdx := strings.LastIndex(response, "}")

	if startIdx == -1 || endIdx == -1 || endIdx <= startIdx {
		return nil, errors.New("no valid JSON found in response")
	}

	jsonStr := response[startIdx : endIdx+1]
	if err := json.Unmarshal([]byte(jsonStr), &sr); err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %w", err)
	}

	return &Summary{
		Intent:         sr.Intent,
		Outcome:        sr.Outcome,
		Learnings:      sr.Learnings,
		FrictionPoints: sr.FrictionPoints,
	}, nil
}

// buildCheckpointSummaryPrompt builds the prompt for summarizing a checkpoint transcript.
func buildCheckpointSummaryPrompt(transcriptText string) string {
	return "Summarize this coding session transcript. Be concise.\n\nExtract:\n1. Intent: What was the user trying to accomplish? (1 sentence)\n2. Outcome: What was the result? (1 sentence)\n3. Learnings: Key technical insights (2-3 bullet points, or empty array if none)\n4. Friction Points: Difficulties encountered (2-3 bullet points, or empty array if none)\n\nRespond ONLY with JSON, no markdown:\n{\"intent\": \"...\", \"outcome\": \"...\", \"learnings\": [...], \"friction_points\": [...]}\n\nTranscript:\n" + transcriptText
}

// buildBranchSummaryPrompt builds the prompt for summarizing a branch from checkpoint summaries.
func buildBranchSummaryPrompt(checkpointSummaries string) string {
	return fmt.Sprintf(`Summarize the overall work done on this development branch.

These are the individual session summaries:
%s

Provide a high-level summary:
1. Intent: What was the overall goal? (1-2 sentences)
2. Outcome: What was accomplished? (1-2 sentences)

Respond ONLY with JSON, no markdown:
{"intent": "...", "outcome": "..."}`, checkpointSummaries)
}

// isSummaryEmpty returns true if the summary has no meaningful content.
func isSummaryEmpty(summary *Summary) bool {
	if summary == nil {
		return true
	}
	return summary.Intent == "" &&
		summary.Outcome == "" &&
		len(summary.Learnings) == 0 &&
		len(summary.FrictionPoints) == 0
}

// GenerateBranchSummary aggregates checkpoint summaries into a branch-level summary.
// Takes a list of checkpoint intent strings and uses Claude to generate an overall summary.
// Returns nil if there are no checkpoint summaries or if summarization fails.
func GenerateBranchSummary(ctx context.Context, checkpointIntents []string) (*Summary, error) {
	result, err := GenerateBranchSummaryWithUsage(ctx, checkpointIntents)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, nil //nolint:nilnil // Intentional: nothing to summarize
	}
	return result.Summary, nil
}

// GenerateBranchSummaryWithUsage aggregates checkpoint summaries into a branch-level summary
// and returns token usage information.
func GenerateBranchSummaryWithUsage(ctx context.Context, checkpointIntents []string) (*SummaryResult, error) {
	if len(checkpointIntents) == 0 {
		return nil, nil //nolint:nilnil // Intentional: no intents means nothing to summarize
	}

	// Format checkpoint summaries for the prompt
	var sb strings.Builder
	for _, intent := range checkpointIntents {
		if intent != "" {
			sb.WriteString("- ")
			sb.WriteString(intent)
			sb.WriteString("\n")
		}
	}

	summariesText := sb.String()
	if summariesText == "" {
		return nil, nil //nolint:nilnil // Intentional: all empty intents means nothing to summarize
	}

	// Build and send prompt
	prompt := buildBranchSummaryPrompt(summariesText)

	response, err := callClaudeWithUsage(ctx, prompt)
	if err != nil {
		// Return nil on failure - branch summary is optional
		return nil, err
	}

	// Parse response (branch summary only has intent and outcome)
	summary, err := parseSummaryResponse(response.Result)
	if err != nil {
		return nil, err
	}

	return &SummaryResult{
		Summary:      summary,
		InputTokens:  response.InputTokens,
		OutputTokens: response.OutputTokens,
	}, nil
}
