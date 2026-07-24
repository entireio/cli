package geminicli

import (
	"context"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// GenerateText sends a prompt to the Gemini CLI and returns the raw text response.
//
// The prompt is piped to the Gemini CLI via stdin rather than embedded in argv.
// Per gemini --help, the -p/--prompt flag is appended to any input read from
// stdin; we pass a single-space placeholder to trigger headless (non-interactive)
// mode and let stdin carry the actual content, avoiding argv size limits.
func (g *GeminiCLIAgent) GenerateText(ctx context.Context, prompt, model string) (string, error) {
	args := []string{"-p", " "}
	if model != "" {
		args = append(args, "--model", model)
	}
	res, runErr := agent.RunIsolatedTextGeneratorCLIRaw(ctx, g.CommandRunner, "gemini", args, prompt)
	return agent.HandleTextGenResult(res, runErr, agent.AgentNameGemini, "gemini CLI returned empty output", classifyGeminiAuthPhrase) //nolint:wrapcheck // return unwrapped: the explain layer renders label+message from the typed error, so a wrap prefix would leak into user output. errors.As (*TextGenError) / errors.Is (ctx sentinel) must reach it unflattened.
}

// classifyGeminiAuthPhrase is the extraClassify hook for gemini-cli: its
// auth-failure stderr exits non-zero with no HTTP status, so the shared
// baseline misses it. The phrase is verbatim from observed stderr.
//
// Deliberately does NOT match a bare "GEMINI_API_KEY" mention. The env var name
// also appears in quota messages ("Quota exceeded for GEMINI_API_KEY") and
// deprecation notices, so matching it alone reports a working credential as an
// auth failure — the speculative-phrase failure mode this hook exists to avoid.
// "Please set an Auth method" is specific to the real failure and already
// covers the captured fixture, which contains both strings.
func classifyGeminiAuthPhrase(stderr string) agent.TextGenErrorKind {
	if strings.Contains(strings.ToLower(stderr), "please set an auth method") {
		return agent.TextGenErrorAuth
	}
	return agent.TextGenErrorUnknown
}
