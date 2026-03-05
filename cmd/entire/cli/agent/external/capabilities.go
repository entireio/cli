package external

import (
	"context"
	"errors"
	"io"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// Wrap returns the Agent wrapped as an agent.Agent that implements ALL optional
// interfaces (forwarding to the underlying external agent) plus CapabilityDeclarer.
// The As* helpers in the agent package use DeclaredCapabilities() to gate access,
// so callers only see capabilities the external binary actually declared.
func Wrap(ea *Agent) (agent.Agent, error) {
	if ea == nil {
		return nil, errors.New("unable to wrap nil agent")
	}
	return &wrappedAgent{
		ea:   ea,
		caps: ea.info.Capabilities,
	}, nil
}

// wrappedAgent forwards all agent.Agent and optional interface methods to the
// underlying external Agent. Capability gating is handled by DeclaredCapabilities()
// and the As* helpers in the agent package.
type wrappedAgent struct {
	ea   *Agent
	caps agent.DeclaredCaps
}

// --- CapabilityDeclarer ---

func (w *wrappedAgent) DeclaredCapabilities() agent.DeclaredCaps { return w.caps }

// --- agent.Agent ---

func (w *wrappedAgent) Name() types.AgentName { return w.ea.Name() }
func (w *wrappedAgent) Type() types.AgentType { return w.ea.Type() }
func (w *wrappedAgent) Description() string   { return w.ea.Description() }
func (w *wrappedAgent) IsPreview() bool       { return w.ea.IsPreview() }
func (w *wrappedAgent) DetectPresence(ctx context.Context) (bool, error) {
	return w.ea.DetectPresence(ctx)
}
func (w *wrappedAgent) ProtectedDirs() []string                   { return w.ea.ProtectedDirs() }
func (w *wrappedAgent) ReadTranscript(ref string) ([]byte, error) { return w.ea.ReadTranscript(ref) }
func (w *wrappedAgent) ChunkTranscript(ctx context.Context, c []byte, m int) ([][]byte, error) {
	return w.ea.ChunkTranscript(ctx, c, m)
}
func (w *wrappedAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	return w.ea.ReassembleTranscript(chunks)
}
func (w *wrappedAgent) GetSessionID(input *agent.HookInput) string { return w.ea.GetSessionID(input) }
func (w *wrappedAgent) GetSessionDir(repoPath string) (string, error) {
	return w.ea.GetSessionDir(repoPath)
}
func (w *wrappedAgent) ResolveSessionFile(dir, id string) string {
	return w.ea.ResolveSessionFile(dir, id)
}
func (w *wrappedAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	return w.ea.ReadSession(input)
}
func (w *wrappedAgent) WriteSession(ctx context.Context, s *agent.AgentSession) error {
	return w.ea.WriteSession(ctx, s)
}
func (w *wrappedAgent) FormatResumeCommand(id string) string { return w.ea.FormatResumeCommand(id) }

// --- HookSupport ---

func (w *wrappedAgent) HookNames() []string { return w.ea.HookNames() }
func (w *wrappedAgent) ParseHookEvent(ctx context.Context, name string, stdin io.Reader) (*agent.Event, error) {
	return w.ea.ParseHookEvent(ctx, name, stdin)
}
func (w *wrappedAgent) InstallHooks(ctx context.Context, localDev bool, force bool) (int, error) {
	return w.ea.InstallHooks(ctx, localDev, force)
}
func (w *wrappedAgent) UninstallHooks(ctx context.Context) error { return w.ea.UninstallHooks(ctx) }
func (w *wrappedAgent) AreHooksInstalled(ctx context.Context) bool {
	return w.ea.AreHooksInstalled(ctx)
}

// --- TranscriptAnalyzer ---

func (w *wrappedAgent) GetTranscriptPosition(path string) (int, error) {
	return w.ea.GetTranscriptPosition(path)
}
func (w *wrappedAgent) ExtractModifiedFilesFromOffset(path string, offset int) ([]string, int, error) {
	return w.ea.ExtractModifiedFilesFromOffset(path, offset)
}
func (w *wrappedAgent) ExtractPrompts(ref string, offset int) ([]string, error) {
	return w.ea.ExtractPrompts(ref, offset)
}
func (w *wrappedAgent) ExtractSummary(ref string) (string, error) {
	return w.ea.ExtractSummary(ref)
}

// --- TranscriptPreparer ---

func (w *wrappedAgent) PrepareTranscript(ctx context.Context, ref string) error {
	return w.ea.PrepareTranscript(ctx, ref)
}

// --- TokenCalculator ---

func (w *wrappedAgent) CalculateTokenUsage(data []byte, offset int) (*agent.TokenUsage, error) {
	return w.ea.CalculateTokenUsage(data, offset)
}

// --- TextGenerator ---

func (w *wrappedAgent) GenerateText(ctx context.Context, prompt string, model string) (string, error) {
	return w.ea.GenerateText(ctx, prompt, model)
}

// --- HookResponseWriter ---

func (w *wrappedAgent) WriteHookResponse(message string) error {
	return w.ea.WriteHookResponse(message)
}

// --- SubagentAwareExtractor ---

func (w *wrappedAgent) ExtractAllModifiedFiles(data []byte, offset int, dir string) ([]string, error) {
	return w.ea.ExtractAllModifiedFiles(data, offset, dir)
}
func (w *wrappedAgent) CalculateTotalTokenUsage(data []byte, offset int, dir string) (*agent.TokenUsage, error) {
	return w.ea.CalculateTotalTokenUsage(data, offset, dir)
}

var (
	_ agent.CapabilityDeclarer     = (*wrappedAgent)(nil)
	_ agent.HookSupport            = (*wrappedAgent)(nil)
	_ agent.TranscriptAnalyzer     = (*wrappedAgent)(nil)
	_ agent.TranscriptPreparer     = (*wrappedAgent)(nil)
	_ agent.TokenCalculator        = (*wrappedAgent)(nil)
	_ agent.TextGenerator          = (*wrappedAgent)(nil)
	_ agent.HookResponseWriter     = (*wrappedAgent)(nil)
	_ agent.SubagentAwareExtractor = (*wrappedAgent)(nil)
)
