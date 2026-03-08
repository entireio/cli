package external

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// defaultRunTimeout is the maximum time an external binary call may take when
// the caller provides a context without a deadline.
var defaultRunTimeout = 30 * time.Second //nolint:gochecknoglobals // overridable for testing

// Agent implements agent.Agent by delegating to an external binary.
// Each method invokes a subcommand on the binary and parses the JSON response.
type Agent struct {
	binaryPath string
	info       *InfoResponse
}

// New creates an Agent by calling the binary's "info" subcommand
// to cache its metadata. Returns an error if the binary cannot be invoked or returns
// invalid/incompatible protocol data. The provided context bounds the "info" call;
// pass a context with a deadline to limit how long discovery waits for each binary.
func New(ctx context.Context, binaryPath string) (*Agent, error) {
	ea := &Agent{binaryPath: binaryPath}

	stdout, err := ea.run(ctx, nil, "info")
	if err != nil {
		return nil, fmt.Errorf("info: %w", err)
	}

	var info InfoResponse
	if err := json.Unmarshal(stdout, &info); err != nil {
		return nil, fmt.Errorf("info: invalid JSON: %w", err)
	}

	if info.ProtocolVersion != ProtocolVersion {
		return nil, fmt.Errorf("protocol version mismatch: binary reports %d, expected %d",
			info.ProtocolVersion, ProtocolVersion)
	}

	ea.info = &info
	return ea, nil
}

// Info returns the cached info response.
func (e *Agent) Info() *InfoResponse {
	return e.info
}

// --- Agent interface: Identity ---

func (e *Agent) Name() types.AgentName {
	return types.AgentName(e.info.Name)
}

func (e *Agent) Type() types.AgentType {
	return types.AgentType(e.info.Type)
}

func (e *Agent) Description() string {
	return e.info.Description
}

func (e *Agent) IsPreview() bool {
	return e.info.IsPreview
}

func (e *Agent) DetectPresence(ctx context.Context) (bool, error) {
	stdout, err := e.run(ctx, nil, "detect")
	if err != nil {
		return false, fmt.Errorf("detect: %w", err)
	}
	var resp DetectResponse
	if err := json.Unmarshal(stdout, &resp); err != nil {
		return false, fmt.Errorf("detect: invalid JSON: %w", err)
	}
	return resp.Present, nil
}

func (e *Agent) ProtectedDirs() []string {
	return e.info.ProtectedDirs
}

// --- Agent interface: Transcript Storage ---

func (e *Agent) ReadTranscript(sessionRef string) ([]byte, error) {
	return e.run(context.Background(), nil, "read-transcript", "--session-ref", sessionRef)
}

func (e *Agent) ChunkTranscript(ctx context.Context, content []byte, maxSize int) ([][]byte, error) {
	stdout, err := e.run(ctx, content, "chunk-transcript", "--max-size", strconv.Itoa(maxSize))
	if err != nil {
		return nil, fmt.Errorf("chunk-transcript: %w", err)
	}
	var resp ChunkResponse
	if err := json.Unmarshal(stdout, &resp); err != nil {
		return nil, fmt.Errorf("chunk-transcript: invalid JSON: %w", err)
	}
	return resp.Chunks, nil
}

func (e *Agent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	input, err := json.Marshal(ChunkResponse{Chunks: chunks})
	if err != nil {
		return nil, fmt.Errorf("reassemble-transcript: marshal: %w", err)
	}
	return e.run(context.Background(), input, "reassemble-transcript")
}

// --- Agent interface: Legacy methods ---

func (e *Agent) GetSessionID(input *agent.HookInput) string {
	data, err := marshalHookInput(input)
	if err != nil {
		return ""
	}
	stdout, err := e.run(context.Background(), data, "get-session-id")
	if err != nil {
		return ""
	}
	var resp SessionIDResponse
	if err := json.Unmarshal(stdout, &resp); err != nil {
		return ""
	}
	return resp.SessionID
}

func (e *Agent) GetSessionDir(repoPath string) (string, error) {
	stdout, err := e.run(context.Background(), nil, "get-session-dir", "--repo-path", repoPath)
	if err != nil {
		return "", fmt.Errorf("get-session-dir: %w", err)
	}
	var resp SessionDirResponse
	if err := json.Unmarshal(stdout, &resp); err != nil {
		return "", fmt.Errorf("get-session-dir: invalid JSON: %w", err)
	}
	return resp.SessionDir, nil
}

func (e *Agent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	stdout, err := e.run(context.Background(), nil, "resolve-session-file",
		"--session-dir", sessionDir, "--session-id", agentSessionID)
	if err != nil {
		return ""
	}
	var resp SessionFileResponse
	if err := json.Unmarshal(stdout, &resp); err != nil {
		return ""
	}
	return resp.SessionFile
}

func (e *Agent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	data, err := marshalHookInput(input)
	if err != nil {
		return nil, fmt.Errorf("read-session: marshal: %w", err)
	}
	stdout, err := e.run(context.Background(), data, "read-session")
	if err != nil {
		return nil, fmt.Errorf("read-session: %w", err)
	}
	var resp AgentSessionJSON
	if err := json.Unmarshal(stdout, &resp); err != nil {
		return nil, fmt.Errorf("read-session: invalid JSON: %w", err)
	}
	return unmarshalAgentSession(&resp)
}

func (e *Agent) WriteSession(ctx context.Context, session *agent.AgentSession) error {
	data, err := marshalAgentSession(session)
	if err != nil {
		return fmt.Errorf("write-session: marshal: %w", err)
	}
	_, err = e.run(ctx, data, "write-session")
	if err != nil {
		return fmt.Errorf("write-session: %w", err)
	}
	return nil
}

func (e *Agent) FormatResumeCommand(sessionID string) string {
	stdout, err := e.run(context.Background(), nil, "format-resume-command", "--session-id", sessionID)
	if err != nil {
		return ""
	}
	var resp ResumeCommandResponse
	if err := json.Unmarshal(stdout, &resp); err != nil {
		return ""
	}
	return resp.Command
}

// --- HookSupport methods (delegated when capabilities.hooks is true) ---

func (e *Agent) HookNames() []string {
	return e.info.HookNames
}

func (e *Agent) ParseHookEvent(ctx context.Context, hookName string, stdin io.Reader) (*agent.Event, error) {
	const maxParseHookBytes = 10 * 1024 * 1024 // 10 MB
	data, err := io.ReadAll(io.LimitReader(stdin, maxParseHookBytes))
	if err != nil {
		return nil, fmt.Errorf("parse-hook: read stdin: %w", err)
	}
	stdout, err := e.run(ctx, data, "parse-hook", "--hook", hookName)
	if err != nil {
		return nil, fmt.Errorf("parse-hook: %w", err)
	}
	// null means no lifecycle significance
	if bytes.Equal(bytes.TrimSpace(stdout), []byte("null")) {
		return nil, nil //nolint:nilnil // protocol contract: null = no event
	}
	var event eventJSON
	if err := json.Unmarshal(stdout, &event); err != nil {
		return nil, fmt.Errorf("parse-hook: invalid JSON: %w", err)
	}
	return event.toEvent()
}

func (e *Agent) InstallHooks(ctx context.Context, localDev bool, force bool) (int, error) {
	args := []string{"install-hooks"}
	if localDev {
		args = append(args, "--local-dev")
	}
	if force {
		args = append(args, "--force")
	}
	stdout, err := e.run(ctx, nil, args...)
	if err != nil {
		return 0, fmt.Errorf("install-hooks: %w", err)
	}
	var resp HooksInstalledCountResponse
	if err := json.Unmarshal(stdout, &resp); err != nil {
		return 0, fmt.Errorf("install-hooks: invalid JSON: %w", err)
	}
	return resp.HooksInstalled, nil
}

func (e *Agent) UninstallHooks(ctx context.Context) error {
	_, err := e.run(ctx, nil, "uninstall-hooks")
	if err != nil {
		return fmt.Errorf("uninstall-hooks: %w", err)
	}
	return nil
}

func (e *Agent) AreHooksInstalled(ctx context.Context) bool {
	stdout, err := e.run(ctx, nil, "are-hooks-installed")
	if err != nil {
		return false
	}
	var resp AreHooksInstalledResponse
	if err := json.Unmarshal(stdout, &resp); err != nil {
		return false
	}
	return resp.Installed
}

// --- TranscriptAnalyzer methods ---

func (e *Agent) GetTranscriptPosition(path string) (int, error) {
	stdout, err := e.run(context.Background(), nil, "get-transcript-position", "--path", path)
	if err != nil {
		return 0, fmt.Errorf("get-transcript-position: %w", err)
	}
	var resp TranscriptPositionResponse
	if err := json.Unmarshal(stdout, &resp); err != nil {
		return 0, fmt.Errorf("get-transcript-position: invalid JSON: %w", err)
	}
	return resp.Position, nil
}

func (e *Agent) ExtractModifiedFilesFromOffset(path string, startOffset int) ([]string, int, error) {
	stdout, err := e.run(context.Background(), nil, "extract-modified-files",
		"--path", path, "--offset", strconv.Itoa(startOffset))
	if err != nil {
		return nil, 0, fmt.Errorf("extract-modified-files: %w", err)
	}
	var resp ExtractFilesResponse
	if err := json.Unmarshal(stdout, &resp); err != nil {
		return nil, 0, fmt.Errorf("extract-modified-files: invalid JSON: %w", err)
	}
	return resp.Files, resp.CurrentPosition, nil
}

func (e *Agent) ExtractPrompts(sessionRef string, fromOffset int) ([]string, error) {
	stdout, err := e.run(context.Background(), nil, "extract-prompts",
		"--session-ref", sessionRef, "--offset", strconv.Itoa(fromOffset))
	if err != nil {
		return nil, fmt.Errorf("extract-prompts: %w", err)
	}
	var resp ExtractPromptsResponse
	if err := json.Unmarshal(stdout, &resp); err != nil {
		return nil, fmt.Errorf("extract-prompts: invalid JSON: %w", err)
	}
	return resp.Prompts, nil
}

func (e *Agent) ExtractSummary(sessionRef string) (string, error) {
	stdout, err := e.run(context.Background(), nil, "extract-summary", "--session-ref", sessionRef)
	if err != nil {
		return "", fmt.Errorf("extract-summary: %w", err)
	}
	var resp ExtractSummaryResponse
	if err := json.Unmarshal(stdout, &resp); err != nil {
		return "", fmt.Errorf("extract-summary: invalid JSON: %w", err)
	}
	return resp.Summary, nil
}

// --- TranscriptPreparer methods ---

func (e *Agent) PrepareTranscript(ctx context.Context, sessionRef string) error {
	_, err := e.run(ctx, nil, "prepare-transcript", "--session-ref", sessionRef)
	if err != nil {
		return fmt.Errorf("prepare-transcript: %w", err)
	}
	return nil
}

// --- TokenCalculator methods ---

func (e *Agent) CalculateTokenUsage(transcriptData []byte, fromOffset int) (*agent.TokenUsage, error) {
	stdout, err := e.run(context.Background(), transcriptData,
		"calculate-tokens", "--offset", strconv.Itoa(fromOffset))
	if err != nil {
		return nil, fmt.Errorf("calculate-tokens: %w", err)
	}
	var resp TokenUsageResponse
	if err := json.Unmarshal(stdout, &resp); err != nil {
		return nil, fmt.Errorf("calculate-tokens: invalid JSON: %w", err)
	}
	return convertTokenUsage(&resp), nil
}

// --- TextGenerator methods ---

func (e *Agent) GenerateText(ctx context.Context, prompt string, model string) (string, error) {
	stdout, err := e.run(ctx, []byte(prompt), "generate-text", "--model", model)
	if err != nil {
		return "", fmt.Errorf("generate-text: %w", err)
	}
	var resp GenerateTextResponse
	if err := json.Unmarshal(stdout, &resp); err != nil {
		return "", fmt.Errorf("generate-text: invalid JSON: %w", err)
	}
	return resp.Text, nil
}

// --- HookResponseWriter methods ---

func (e *Agent) WriteHookResponse(message string) error {
	_, err := e.run(context.Background(), nil, "write-hook-response", "--message", message)
	if err != nil {
		return fmt.Errorf("write-hook-response: %w", err)
	}
	return nil
}

// --- SubagentAwareExtractor methods ---

func (e *Agent) ExtractAllModifiedFiles(transcriptData []byte, fromOffset int, subagentsDir string) ([]string, error) {
	stdout, err := e.run(context.Background(), transcriptData,
		"extract-all-modified-files", "--offset", strconv.Itoa(fromOffset), "--subagents-dir", subagentsDir)
	if err != nil {
		return nil, fmt.Errorf("extract-all-modified-files: %w", err)
	}
	var resp ExtractFilesResponse
	if err := json.Unmarshal(stdout, &resp); err != nil {
		return nil, fmt.Errorf("extract-all-modified-files: invalid JSON: %w", err)
	}
	return resp.Files, nil
}

func (e *Agent) CalculateTotalTokenUsage(transcriptData []byte, fromOffset int, subagentsDir string) (*agent.TokenUsage, error) {
	stdout, err := e.run(context.Background(), transcriptData,
		"calculate-total-tokens", "--offset", strconv.Itoa(fromOffset), "--subagents-dir", subagentsDir)
	if err != nil {
		return nil, fmt.Errorf("calculate-total-tokens: %w", err)
	}
	var resp TokenUsageResponse
	if err := json.Unmarshal(stdout, &resp); err != nil {
		return nil, fmt.Errorf("calculate-total-tokens: invalid JSON: %w", err)
	}
	return convertTokenUsage(&resp), nil
}

// run executes a subcommand on the external binary and returns stdout bytes.
// If stdin is non-nil it is piped to the process. On non-zero exit, stderr is
// included in the returned error.
func (e *Agent) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	// Apply a default timeout when the caller hasn't set a deadline, so a hung
	// external binary can't block the CLI (or git hooks) indefinitely.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultRunTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, e.binaryPath, args...)
	// Ensure I/O goroutines are released shortly after the process is killed,
	// so cmd.Run() doesn't block waiting for pipe reads.
	cmd.WaitDelay = 3 * time.Second

	// Set environment: repo root + protocol version
	cmd.Env = append(cmd.Environ(),
		"ENTIRE_PROTOCOL_VERSION="+strconv.Itoa(ProtocolVersion),
	)
	if repoRoot, err := paths.WorktreeRoot(ctx); err == nil {
		cmd.Env = append(cmd.Env, "ENTIRE_REPO_ROOT="+repoRoot)
		cmd.Dir = repoRoot
	}

	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}

	const maxOutputBytes = 10 * 1024 * 1024 // 10 MB

	var stdoutBuf, stderrBuf bytes.Buffer
	stdoutLW := &limitedWriter{buf: &stdoutBuf, limit: maxOutputBytes}
	stderrLW := &limitedWriter{buf: &stderrBuf, limit: maxOutputBytes}
	cmd.Stdout = stdoutLW
	cmd.Stderr = stderrLW

	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderrBuf.String())
		if errMsg != "" {
			return nil, fmt.Errorf("%s: %s", args[0], errMsg)
		}
		return nil, fmt.Errorf("%s: %w", args[0], err)
	}

	if stdoutLW.truncated {
		return nil, fmt.Errorf("%s: output exceeded %d byte limit", args[0], maxOutputBytes)
	}

	return stdoutBuf.Bytes(), nil
}

// --- Helpers ---

// limitedWriter wraps a bytes.Buffer and stops writing after limit bytes,
// preventing unbounded memory growth from external process output.
type limitedWriter struct {
	buf       *bytes.Buffer
	limit     int
	truncated bool
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	remaining := w.limit - w.buf.Len()
	if remaining <= 0 {
		w.truncated = true
		return len(p), nil // discard, but report success so the process isn't killed
	}
	if len(p) > remaining {
		p = p[:remaining]
		w.truncated = true
	}
	n, err := w.buf.Write(p)
	if err != nil {
		return n, fmt.Errorf("writing to limited buffer: %w", err)
	}
	return n, nil
}

func marshalHookInput(input *agent.HookInput) ([]byte, error) {
	if input == nil {
		return []byte("{}"), nil
	}
	j := HookInputJSON{
		HookType:   string(input.HookType),
		SessionID:  input.SessionID,
		SessionRef: input.SessionRef,
		Timestamp:  input.Timestamp.Format(time.RFC3339),
		UserPrompt: input.UserPrompt,
		ToolName:   input.ToolName,
		ToolUseID:  input.ToolUseID,
		ToolInput:  input.ToolInput,
		RawData:    input.RawData,
	}
	data, err := json.Marshal(j)
	if err != nil {
		return nil, fmt.Errorf("marshal hook input: %w", err)
	}
	return data, nil
}

func marshalAgentSession(s *agent.AgentSession) ([]byte, error) {
	j := AgentSessionJSON{
		SessionID:     s.SessionID,
		AgentName:     string(s.AgentName),
		RepoPath:      s.RepoPath,
		SessionRef:    s.SessionRef,
		StartTime:     s.StartTime.Format(time.RFC3339),
		NativeData:    s.NativeData,
		ModifiedFiles: s.ModifiedFiles,
		NewFiles:      s.NewFiles,
		DeletedFiles:  s.DeletedFiles,
	}
	data, err := json.Marshal(j)
	if err != nil {
		return nil, fmt.Errorf("marshal agent session: %w", err)
	}
	return data, nil
}

func unmarshalAgentSession(j *AgentSessionJSON) (*agent.AgentSession, error) {
	var startTime time.Time
	if j.StartTime != "" {
		var err error
		startTime, err = time.Parse(time.RFC3339, j.StartTime)
		if err != nil {
			return nil, fmt.Errorf("invalid start_time: %w", err)
		}
	}
	return &agent.AgentSession{
		SessionID:     j.SessionID,
		AgentName:     types.AgentName(j.AgentName),
		RepoPath:      j.RepoPath,
		SessionRef:    j.SessionRef,
		StartTime:     startTime,
		NativeData:    j.NativeData,
		ModifiedFiles: j.ModifiedFiles,
		NewFiles:      j.NewFiles,
		DeletedFiles:  j.DeletedFiles,
	}, nil
}

func convertTokenUsage(r *TokenUsageResponse) *agent.TokenUsage {
	if r == nil {
		return nil
	}
	usage := &agent.TokenUsage{
		InputTokens:         r.InputTokens,
		CacheCreationTokens: r.CacheCreationTokens,
		CacheReadTokens:     r.CacheReadTokens,
		OutputTokens:        r.OutputTokens,
		APICallCount:        r.APICallCount,
	}
	if r.SubagentTokens != nil {
		usage.SubagentTokens = convertTokenUsage(r.SubagentTokens)
	}
	return usage
}
