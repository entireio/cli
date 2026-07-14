package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// displayNameFor maps an AgentName to the proper-noun display string used in
// user-facing error messages. Unknown providers fall through to the registry
// key so we never render an empty prefix.
func displayNameFor(p types.AgentName) string {
	switch p {
	case agent.AgentNameClaudeCode:
		return "Claude"
	case agent.AgentNameCodex:
		return "Codex"
	case agent.AgentNameGemini:
		return "Gemini"
	case agent.AgentNameCursor:
		return "Cursor"
	case agent.AgentNameCopilotCLI:
		return "Copilot"
	case agent.AgentNamePi:
		return "Pi"
	default:
		return string(p)
	}
}

// kindPrefix returns the user-facing prefix for a given TextGenErrorKind,
// parameterized by the provider's display name. CLIMissing is handled
// separately by renderTextGenError because its message omits the prefix
// entirely; Unknown falls through to the default branch.
func kindPrefix(k agent.TextGenErrorKind, displayName string) string {
	switch k { //nolint:exhaustive // CLIMissing handled separately, Unknown in default
	case agent.TextGenErrorAuth:
		return displayName + " authentication failed"
	case agent.TextGenErrorRateLimit:
		return displayName + " rejected the summary request due to rate limits or quota"
	case agent.TextGenErrorConfig:
		return displayName + " rejected the summary request"
	default:
		return displayName + " failed to generate the summary"
	}
}

// syntheticFallback holds a generic remediation line per-provider per-kind.
// It is applied only when the provider is in
// providersNeedingSynthesizedRemediation AND the envelope-derived Message is
// absent (so we have nothing better to show), OR — for Claude — in addition
// to Message (preserving 963's established remediation wording, now
// rendered as a lowercase "try" row in the failure block).
//
// Non-Claude entries are deliberately generic ("Check your X CLI
// authentication") rather than inventing CLI-specific subcommands: the
// user's actual CLI stderr already carries the authoritative remediation,
// and inventing fake commands like `gemini auth login` would mislead users
// when the real subcommand is different.
var syntheticFallback = map[types.AgentName]map[agent.TextGenErrorKind]string{
	agent.AgentNameClaudeCode: {
		agent.TextGenErrorAuth:      "run `claude login` and retry",
		agent.TextGenErrorRateLimit: "wait and retry",
		agent.TextGenErrorConfig:    "check your Claude CLI config and selected model",
	},
	agent.AgentNameCodex: {
		agent.TextGenErrorAuth:      "check your Codex CLI authentication",
		agent.TextGenErrorRateLimit: "wait and retry",
		agent.TextGenErrorConfig:    "check your Codex CLI config and selected model",
	},
	agent.AgentNameGemini: {
		agent.TextGenErrorAuth:      "check your Gemini CLI authentication",
		agent.TextGenErrorRateLimit: "wait and retry",
		agent.TextGenErrorConfig:    "check your Gemini CLI config and selected model",
	},
	agent.AgentNameCursor: {
		agent.TextGenErrorAuth:      "check your Cursor CLI authentication",
		agent.TextGenErrorRateLimit: "wait and retry",
		agent.TextGenErrorConfig:    "check your Cursor CLI config and selected model",
	},
	agent.AgentNameCopilotCLI: {
		agent.TextGenErrorAuth:      "check your Copilot CLI authentication",
		agent.TextGenErrorRateLimit: "wait and retry",
		agent.TextGenErrorConfig:    "check your Copilot CLI config and selected model",
	},
	agent.AgentNamePi: {
		agent.TextGenErrorAuth:      "check your Pi CLI authentication",
		agent.TextGenErrorRateLimit: "wait and retry",
		agent.TextGenErrorConfig:    "check your Pi CLI config and selected model",
	},
}

// providersNeedingSynthesizedRemediation lists providers whose envelope
// rarely carries an actionable remediation line, so we append our
// syntheticFallback even when a Message is already present.
//
// Claude is the only such provider today: its structured envelope surfaces
// a terse API error (e.g. "Invalid API key") without telling the user what
// to do about it, so 963's user-facing output appended "Run `claude login`
// and retry". Non-Claude CLIs (codex, gemini, cursor, copilot) emit full
// human-readable remediation in their own stderr output, which we capture
// verbatim into Message — synthesizing another remediation line on top
// would produce noisy or contradictory output.
var providersNeedingSynthesizedRemediation = map[types.AgentName]bool{
	agent.AgentNameClaudeCode: true,
}

// formatTextGenErrorSuffix builds a non-empty diagnostic suffix for the
// Unknown fallthrough path. Mirrors 963's Claude-specific suffix helper —
// prefers the envelope Message, then HTTP status, then a real exit code, then the
// "abnormal termination" branch for ExitCode < 0, and finally a sentinel so
// the user never sees "<Display> failed to generate the summary" followed
// by nothing.
//
// Note: parameterizing on displayName produces "Claude API returned HTTP N"
// where 963 used "Anthropic API" and lowercase "claude CLI". This
// normalization is intentional and pinned exactly by
// explain_summary_error_test.go. The load-bearing 963 messages
// (auth/rate-limit/config/CLI-missing) remain byte-identical.
func formatTextGenErrorSuffix(e *agent.TextGenError, displayName string) string {
	if e.Message != "" {
		return ": " + e.Message
	}
	switch {
	case e.APIStatus != 0:
		return fmt.Sprintf(" (%s API returned HTTP %d)", displayName, e.APIStatus)
	case e.ExitCode > 0:
		return fmt.Sprintf(" (%s CLI exited with code %d)", displayName, e.ExitCode)
	case e.ExitCode < 0:
		return fmt.Sprintf(" (%s CLI terminated abnormally — no exit code captured)", displayName)
	default:
		return fmt.Sprintf(" (no diagnostic detail available from %s CLI)", displayName)
	}
}

// renderTextGenError maps a typed *agent.TextGenError to a structured
// failure block matching formatCheckpointSummaryError's contract: a
// user-visible label, supporting rows, and a structured error for
// NewSilentError. Claude's wording preserves 963's established baseline;
// non-Claude providers prefer their CLI's own stderr verbatim (captured in
// Message) and only synthesize a generic remediation row when Message is
// empty.
func renderTextGenError(e *agent.TextGenError) (string, []explainRow, error) {
	displayName := displayNameFor(e.Provider)

	if e.Kind == agent.TextGenErrorCLIMissing {
		// Short, provider-agnostic: the CLI isn't even present, so there's
		// no stderr to show and no useful kind-specific remediation beyond
		// "install it".
		label := displayName + " CLI is not installed or not on PATH"
		return label, nil, errors.New(label)
	}

	if e.Kind == agent.TextGenErrorUnknown {
		label := kindPrefix(e.Kind, displayName)
		suffix := formatTextGenErrorSuffix(e, displayName)
		rows := []explainRow{
			{Label: "detail", Value: strings.TrimPrefix(strings.TrimPrefix(suffix, ": "), " ")},
		}
		return label, rows, fmt.Errorf("%s%s", label, suffix)
	}

	label := kindPrefix(e.Kind, displayName)
	var rows []explainRow
	if e.Message != "" {
		rows = append(rows, explainRow{Label: "message", Value: e.Message})
	}
	// Remediation row: synthesized when the provider's envelope lacks
	// actionable guidance (Claude), or when the CLI's own stderr gave us
	// nothing to show. Non-Claude CLIs with a Message carry their own
	// remediation verbatim, so adding ours would be noisy or contradictory.
	if fallback := syntheticFallback[e.Provider][e.Kind]; fallback != "" &&
		(providersNeedingSynthesizedRemediation[e.Provider] || e.Message == "") {
		rows = append(rows, explainRow{Label: "try", Value: fallback})
	}
	suffix := ""
	if e.Message != "" {
		suffix = ": " + e.Message
	}
	return label, rows, fmt.Errorf("%s%s", label, suffix)
}
