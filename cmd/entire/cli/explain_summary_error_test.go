package cli

import (
	"reflect"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// assertRendered pins the full (label, rows, err) failure block exactly. The
// rendered block is user-facing UX established by 963 (Claude wording) and
// extended per-provider here, so drift must be deliberate, not incidental.
func assertRendered(t *testing.T, in *agent.TextGenError, wantLabel string, wantRows []explainRow, wantErr string) {
	t.Helper()
	label, rows, err := renderTextGenError(in)
	if label != wantLabel {
		t.Errorf("label = %q, want %q", label, wantLabel)
	}
	if !reflect.DeepEqual(rows, wantRows) {
		t.Errorf("rows = %+v, want %+v", rows, wantRows)
	}
	if err == nil {
		t.Fatal("expected structured error")
	}
	if err.Error() != wantErr {
		t.Errorf("err = %q, want %q", err.Error(), wantErr)
	}
}

func TestRenderTextGenError_ClaudeWordingMatches963(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		in        *agent.TextGenError
		wantLabel string
		wantRows  []explainRow
		wantErr   string
	}{
		{
			name:      "claude auth, envelope provides message",
			in:        &agent.TextGenError{Kind: agent.TextGenErrorAuth, Provider: agent.AgentNameClaudeCode, Message: "Invalid API key"},
			wantLabel: "Claude authentication failed",
			wantRows: []explainRow{
				{Label: "message", Value: "Invalid API key"},
				{Label: "try", Value: "run `claude login` and retry"},
			},
			wantErr: "Claude authentication failed: Invalid API key",
		},
		{
			name:      "claude auth, empty message still synthesizes 963 remediation",
			in:        &agent.TextGenError{Kind: agent.TextGenErrorAuth, Provider: agent.AgentNameClaudeCode},
			wantLabel: "Claude authentication failed",
			wantRows: []explainRow{
				{Label: "try", Value: "run `claude login` and retry"},
			},
			wantErr: "Claude authentication failed",
		},
		{
			name:      "claude rate limit, with message",
			in:        &agent.TextGenError{Kind: agent.TextGenErrorRateLimit, Provider: agent.AgentNameClaudeCode, Message: "429"},
			wantLabel: "Claude rejected the summary request due to rate limits or quota",
			wantRows: []explainRow{
				{Label: "message", Value: "429"},
				{Label: "try", Value: "wait and retry"},
			},
			wantErr: "Claude rejected the summary request due to rate limits or quota: 429",
		},
		{
			name:      "claude config, with message",
			in:        &agent.TextGenError{Kind: agent.TextGenErrorConfig, Provider: agent.AgentNameClaudeCode, Message: "model not found"},
			wantLabel: "Claude rejected the summary request",
			wantRows: []explainRow{
				{Label: "message", Value: "model not found"},
				{Label: "try", Value: "check your Claude CLI config and selected model"},
			},
			wantErr: "Claude rejected the summary request: model not found",
		},
		{
			name:      "claude CLI missing (no message, no model)",
			in:        &agent.TextGenError{Kind: agent.TextGenErrorCLIMissing, Provider: agent.AgentNameClaudeCode},
			wantLabel: "Claude CLI is not installed or not on PATH",
			wantRows:  nil,
			wantErr:   "Claude CLI is not installed or not on PATH",
		},
		// Unknown-branch outputs are pinned exactly — the wording ("Claude API
		// returned HTTP N", "Claude CLI exited with code N") is a minor evolution
		// from 963 ("Anthropic API", lowercase "claude CLI"). Accepting this drift
		// normalizes capitalization with the other Claude-prefixed branches.
		{
			name:      "claude unknown with APIStatus renders HTTP status",
			in:        &agent.TextGenError{Kind: agent.TextGenErrorUnknown, Provider: agent.AgentNameClaudeCode, APIStatus: 500},
			wantLabel: "Claude failed to generate the summary",
			wantRows: []explainRow{
				{Label: "detail", Value: "(Claude API returned HTTP 500)"},
			},
			wantErr: "Claude failed to generate the summary (Claude API returned HTTP 500)",
		},
		{
			name:      "claude unknown with ExitCode renders exit code",
			in:        &agent.TextGenError{Kind: agent.TextGenErrorUnknown, Provider: agent.AgentNameClaudeCode, ExitCode: 137},
			wantLabel: "Claude failed to generate the summary",
			wantRows: []explainRow{
				{Label: "detail", Value: "(Claude CLI exited with code 137)"},
			},
			wantErr: "Claude failed to generate the summary (Claude CLI exited with code 137)",
		},
		{
			name:      "claude unknown with negative ExitCode renders abnormal",
			in:        &agent.TextGenError{Kind: agent.TextGenErrorUnknown, Provider: agent.AgentNameClaudeCode, ExitCode: -1},
			wantLabel: "Claude failed to generate the summary",
			wantRows: []explainRow{
				{Label: "detail", Value: "(Claude CLI terminated abnormally — no exit code captured)"},
			},
			wantErr: "Claude failed to generate the summary (Claude CLI terminated abnormally — no exit code captured)",
		},
		{
			name:      "claude all-zero unknown renders diagnostic sentinel",
			in:        &agent.TextGenError{Kind: agent.TextGenErrorUnknown, Provider: agent.AgentNameClaudeCode},
			wantLabel: "Claude failed to generate the summary",
			wantRows: []explainRow{
				{Label: "detail", Value: "(no diagnostic detail available from Claude CLI)"},
			},
			wantErr: "Claude failed to generate the summary (no diagnostic detail available from Claude CLI)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertRendered(t, tc.in, tc.wantLabel, tc.wantRows, tc.wantErr)
		})
	}
}

// TestRenderTextGenError_NonClaudeProvidersUseStderrVerbatim pins the
// non-Claude rendering rule exactly because this is the behavioral divergence
// from Claude's 963-style synthesis: for these providers with Message present,
// the block carries the CLI's own stderr verbatim and NO synthesized "try"
// row — their stderr already carries its authoritative remediation.
func TestRenderTextGenError_NonClaudeProvidersUseStderrVerbatim(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		in        *agent.TextGenError
		wantLabel string
		wantRows  []explainRow
		wantErr   string
	}{
		// The non-Claude providers share the same "no synthesis appended" rule.
		// One representative case pins it exactly; other providers are covered
		// by their package's generate_test.go and the shared matrix.
		{
			name: "non-Claude auth renders stderr verbatim with no fallback appended",
			in: &agent.TextGenError{
				Kind:     agent.TextGenErrorAuth,
				Provider: agent.AgentNameGemini,
				Message:  "Please set an Auth method in your settings.json or specify one of: GEMINI_API_KEY",
			},
			wantLabel: "Gemini authentication failed",
			wantRows: []explainRow{
				{Label: "message", Value: "Please set an Auth method in your settings.json or specify one of: GEMINI_API_KEY"},
			},
			wantErr: "Gemini authentication failed: Please set an Auth method in your settings.json or specify one of: GEMINI_API_KEY",
		},
		{
			name:      "codex CLI missing (no message, no model)",
			in:        &agent.TextGenError{Kind: agent.TextGenErrorCLIMissing, Provider: agent.AgentNameCodex},
			wantLabel: "Codex CLI is not installed or not on PATH",
			wantRows:  nil,
			wantErr:   "Codex CLI is not installed or not on PATH",
		},
		{
			name:      "gemini empty-message auth falls back to generic synthesis",
			in:        &agent.TextGenError{Kind: agent.TextGenErrorAuth, Provider: agent.AgentNameGemini},
			wantLabel: "Gemini authentication failed",
			wantRows: []explainRow{
				{Label: "try", Value: "check your Gemini CLI authentication"},
			},
			wantErr: "Gemini authentication failed",
		},
		{
			name:      "pi empty-message auth falls back to generic synthesis",
			in:        &agent.TextGenError{Kind: agent.TextGenErrorAuth, Provider: agent.AgentNamePi},
			wantLabel: "Pi authentication failed",
			wantRows: []explainRow{
				{Label: "try", Value: "check your Pi CLI authentication"},
			},
			wantErr: "Pi authentication failed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertRendered(t, tc.in, tc.wantLabel, tc.wantRows, tc.wantErr)
		})
	}
}
