// Package orcarouter implements the Agent interface for OrcaRouter, an
// OpenAI-compatible AI gateway (https://www.orcarouter.ai). Unlike the coding
// agents in this directory, OrcaRouter is not a coding tool that Entire hooks
// into: it is a summary-generation provider that speaks the OpenAI chat
// completions protocol directly over HTTP. Registering it here makes it
// available to the same summary-provider machinery as claude-code, gemini, and
// the other text generators, so `entire configure --summarize-provider
// orcarouter` and `entire explain --generate` work with no new config surface.
package orcarouter

import (
	"context"
	"errors"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(AgentNameOrcaRouter, NewOrcaRouterAgent)
}

const (
	// AgentNameOrcaRouter is the registry key used with
	// `entire configure --summarize-provider orcarouter`.
	AgentNameOrcaRouter types.AgentName = "orcarouter"
	// AgentTypeOrcaRouter is the display name stored in settings and metadata.
	AgentTypeOrcaRouter types.AgentType = "OrcaRouter"
)

// errNoSessionStorage is returned by the session/transcript methods that a
// text-generation-only provider has no storage for. It is a package-local
// sentinel rather than a new agent-contract error because none of the callers
// branch on it — they only need a non-nil error to skip the agent (see
// agent.AgentForTranscriptPath, which ignores agents whose GetSessionDir
// fails).
var errNoSessionStorage = errors.New("orcarouter: provider has no session storage")

// OrcaRouterAgent implements agent.Agent for the OrcaRouter gateway. It is a
// text-generation-only provider: it has no session hooks, transcripts, or
// resume surface, so the Agent interface methods that those features need are
// no-ops that fail closed.
//
//nolint:revive // OrcaRouterAgent is clearer than Agent in this context
type OrcaRouterAgent struct{}

// NewOrcaRouterAgent creates a new OrcaRouter agent instance.
func NewOrcaRouterAgent() agent.Agent {
	return &OrcaRouterAgent{}
}

// Name returns the agent registry key.
func (o *OrcaRouterAgent) Name() types.AgentName { return AgentNameOrcaRouter }

// Type returns the agent type identifier.
func (o *OrcaRouterAgent) Type() types.AgentType { return AgentTypeOrcaRouter }

// Description returns a human-readable description.
func (o *OrcaRouterAgent) Description() string {
	return "OrcaRouter - OpenAI-compatible AI gateway for models and agents"
}

// IsPreview returns true because this is a new integration.
func (o *OrcaRouterAgent) IsPreview() bool { return true }

// DetectPresence reports whether OrcaRouter is configured in the repository.
// It always returns false: OrcaRouter is a summary-generation provider, not a
// coding agent with repo-level hooks or config, so it must never be
// auto-detected as an installed agent by `entire enable`. Summary-provider
// availability is gated on the ORCAROUTER_API_KEY environment variable instead
// (see the summary-provider machinery in the cli package).
func (o *OrcaRouterAgent) DetectPresence(_ context.Context) (bool, error) { return false, nil }

// ProtectedDirs returns directories that OrcaRouter uses for config/state.
// The provider is stateless, so there are none.
func (o *OrcaRouterAgent) ProtectedDirs() []string { return nil }

// GetSessionID extracts the session ID from hook input. OrcaRouter has no
// session hooks, so this always returns the empty string.
func (o *OrcaRouterAgent) GetSessionID(_ *agent.HookInput) string { return "" }

// GetSessionDir returns the directory where OrcaRouter stores session
// transcripts. The provider has no transcript storage.
func (o *OrcaRouterAgent) GetSessionDir(_ string) (string, error) {
	return "", errNoSessionStorage
}

// ResolveSessionFile returns the path to a session transcript file.
func (o *OrcaRouterAgent) ResolveSessionFile(_, _ string) string { return "" }

// ReadSession reads a session from OrcaRouter's storage.
func (o *OrcaRouterAgent) ReadSession(_ *agent.HookInput) (*agent.AgentSession, error) {
	return nil, errNoSessionStorage
}

// WriteSession writes a session to OrcaRouter's storage.
func (o *OrcaRouterAgent) WriteSession(_ context.Context, _ *agent.AgentSession) error {
	return errNoSessionStorage
}

// FormatResumeCommand returns the command to resume an OrcaRouter session.
func (o *OrcaRouterAgent) FormatResumeCommand(_ string) string { return "" }

// ReadTranscript reads the raw transcript bytes for a session.
func (o *OrcaRouterAgent) ReadTranscript(_ string) ([]byte, error) {
	return nil, errNoSessionStorage
}

// ChunkTranscript splits a transcript into chunks if it exceeds maxSize.
// The provider stores no transcripts, so there is nothing to chunk.
func (o *OrcaRouterAgent) ChunkTranscript(_ context.Context, _ []byte, _ int) ([][]byte, error) {
	return nil, errNoSessionStorage
}

// ReassembleTranscript combines chunks back into a single transcript.
func (o *OrcaRouterAgent) ReassembleTranscript(_ [][]byte) ([]byte, error) {
	return nil, errNoSessionStorage
}
