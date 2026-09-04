// Package codex implements the Agent interface for OpenAI's Codex CLI.
package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(agent.AgentNameCodex, NewCodexAgent)
}

// CodexAgent implements the Agent interface for OpenAI's Codex CLI.
//
//nolint:revive // CodexAgent is clearer than Agent in this context
type CodexAgent struct {
	CommandRunner agent.TextCommandRunner
	// RolloutRoots overrides the active and archived rollout roots for callers
	// that already know them (notably tests). Nil uses Codex's normal home.
	RolloutRoots []string
	// loadRollout and walkDir are package-private deterministic test seams.
	// Production uses verified same-descriptor reads plus the bounded,
	// incremental directory walker.
	loadRollout func(string) (loadedRollout, error)
	walkDir     func(string, fs.WalkDirFunc) error
	// scanLimits and observeRolloutRead are deterministic test seams for the
	// fallback rollout budget. Production uses defaultRolloutScanLimits and no
	// observer.
	scanLimits         *rolloutScanLimits
	observeRolloutRead func(string, int)
}

type loadedRollout struct {
	Path string
	Data []byte
}

const (
	rolloutScanTimeout       = 500 * time.Millisecond
	rolloutCandidateLimit    = 20_000
	rolloutMetadataByteLimit = int64(64 << 10)
	rolloutBodyByteLimit     = int64(128 << 20)
	rolloutAggregateLimit    = int64(256 << 20)
	rolloutReadDirBatch      = 128
	rolloutBodyReadChunk     = 32 << 10
	rolloutMetadataChunk     = 1 << 10
)

type rolloutScanLimits struct {
	timeout            time.Duration
	candidateLimit     int
	metadataByteLimit  int64
	bodyByteLimit      int64
	aggregateByteLimit int64
	readDirBatch       int
	now                func() time.Time
}

var defaultRolloutScanLimits = rolloutScanLimits{ //nolint:gochecknoglobals // immutable production defaults
	timeout:            rolloutScanTimeout,
	candidateLimit:     rolloutCandidateLimit,
	metadataByteLimit:  rolloutMetadataByteLimit,
	bodyByteLimit:      rolloutBodyByteLimit,
	aggregateByteLimit: rolloutAggregateLimit,
	readDirBatch:       rolloutReadDirBatch,
	now:                time.Now,
}

var errRolloutScanBudget = errors.New("codex rollout scan budget exceeded")

type rolloutScanBudget struct {
	ctx            context.Context
	limits         rolloutScanLimits
	deadline       time.Time
	candidates     int
	aggregateBytes int64
}

func newRolloutScanBudget(ctx context.Context, limits rolloutScanLimits) *rolloutScanBudget {
	if limits.now == nil {
		limits.now = time.Now
	}
	if limits.readDirBatch <= 0 {
		limits.readDirBatch = rolloutReadDirBatch
	}
	return &rolloutScanBudget{
		ctx:      ctx,
		limits:   limits,
		deadline: limits.now().Add(limits.timeout),
	}
}

func (b *rolloutScanBudget) check() error {
	if err := b.ctx.Err(); err != nil {
		return fmt.Errorf("rollout scan canceled: %w: %w", err, errRolloutScanBudget)
	}
	if b.limits.timeout > 0 && !b.limits.now().Before(b.deadline) {
		return fmt.Errorf("rollout scan deadline reached: %w", errRolloutScanBudget)
	}
	return nil
}

func (b *rolloutScanBudget) observeCandidate() error {
	if err := b.check(); err != nil {
		return err
	}
	b.candidates++
	if b.limits.candidateLimit >= 0 && b.candidates > b.limits.candidateLimit {
		return fmt.Errorf("rollout candidate limit %d exceeded: %w", b.limits.candidateLimit, errRolloutScanBudget)
	}
	return nil
}

func (b *rolloutScanBudget) observeBytes(count int64) error {
	if count < 0 || count > b.limits.aggregateByteLimit || b.aggregateBytes > b.limits.aggregateByteLimit-count {
		return fmt.Errorf("aggregate rollout byte limit %d exceeded: %w", b.limits.aggregateByteLimit, errRolloutScanBudget)
	}
	b.aggregateBytes += count
	return nil
}

func readRegularRolloutContext(ctx context.Context, path string, byteLimit int64, observe func(string, int)) (loadedRollout, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return loadedRollout{}, fmt.Errorf("lstat rollout: %w", err)
	}
	if !info.Mode().IsRegular() {
		return loadedRollout{}, errors.New("rollout is not a regular file")
	}
	file, opened, err := openRolloutFile(path, info)
	if err != nil {
		return loadedRollout{}, err
	}
	defer file.Close()
	if opened.Size() > byteLimit {
		return loadedRollout{}, fmt.Errorf("rollout size %d exceeds limit %d", opened.Size(), byteLimit)
	}
	data, err := readRolloutBody(file, rolloutReadOptions{
		path: path, byteLimit: byteLimit, check: ctx.Err, observe: observe,
		limitErr: fmt.Errorf("rollout exceeds byte limit %d", byteLimit),
	})
	if err != nil {
		return loadedRollout{}, fmt.Errorf("read rollout: %w", err)
	}
	return loadedRollout{Path: path, Data: data}, nil
}

func openRolloutFile(path string, before fs.FileInfo) (*os.File, fs.FileInfo, error) {
	file, err := os.Open(path) //nolint:gosec // Caller rejects special entries; descriptor Stat verifies the opened file.
	if err != nil {
		return nil, nil, fmt.Errorf("open rollout: %w", err)
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("stat opened rollout: %w", err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		_ = file.Close()
		return nil, nil, errors.New("rollout changed or is not a regular file")
	}
	return file, opened, nil
}

type rolloutReadOptions struct {
	path      string
	byteLimit int64
	check     func() error
	account   func(int64) error
	observe   func(string, int)
	limitErr  error
}

func readRolloutBody(file *os.File, opts rolloutReadOptions) ([]byte, error) {
	data := make([]byte, 0)
	buffer := make([]byte, rolloutBodyReadChunk)
	for {
		if err := opts.check(); err != nil {
			return nil, err
		}
		n, err := file.Read(buffer)
		if n > 0 {
			if opts.account != nil {
				if accountErr := opts.account(int64(n)); accountErr != nil {
					return nil, accountErr
				}
			}
			if opts.observe != nil {
				opts.observe(opts.path, n)
			}
			if int64(len(data)+n) > opts.byteLimit {
				return nil, opts.limitErr
			}
			data = append(data, buffer[:n]...)
		}
		if errors.Is(err, io.EOF) {
			return data, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read rollout body: %w", err)
		}
	}
}

func (c *CodexAgent) loadCandidateRollout(ctx context.Context, path string) (loadedRollout, error) {
	if c.loadRollout != nil {
		return c.loadRollout(path)
	}
	return readRegularRolloutContext(ctx, path, rolloutBodyByteLimit, c.observeRolloutRead)
}

func (c *CodexAgent) loadVerifiedRollout(ctx context.Context, path, agentID string) (loadedRollout, bool) {
	loaded, err := c.loadCandidateRollout(ctx, path)
	if err != nil {
		return loadedRollout{}, false
	}
	if loaded.Path == "" {
		loaded.Path = path
	}
	if loaded.Path != path {
		return loadedRollout{}, false
	}
	id, err := sessionMetaID(loaded.Data)
	if err != nil || id != agentID {
		return loadedRollout{}, false
	}
	return loaded, true
}

func (c *CodexAgent) rolloutRoots() []string {
	if c.RolloutRoots != nil {
		return c.RolloutRoots
	}
	sessionDir, err := c.GetSessionDir("")
	if err != nil {
		return nil
	}
	codexHome, err := resolveCodexHome()
	if err != nil {
		return []string{sessionDir}
	}
	return []string{sessionDir, filepath.Join(codexHome, "archived_sessions")}
}

func (c *CodexAgent) loadDirectRollout(ctx context.Context, ref agent.SubagentReference) (loadedRollout, bool) {
	for _, path := range []string{ref.DeclaredTranscriptPath, ref.ResolvedTranscriptPath} {
		if path == "" {
			continue
		}
		if loaded, ok := c.loadVerifiedRollout(ctx, path, ref.AgentID); ok {
			return loaded, true
		}
	}
	return loadedRollout{}, false
}

func (c *CodexAgent) walkRollouts(ctx context.Context, root string, budget *rolloutScanBudget, visit func(string, fs.DirEntry) error) error {
	if c.walkDir != nil {
		return c.walkDir(root, func(path string, entry fs.DirEntry, entryErr error) error {
			if entryErr != nil {
				if path == root && errors.Is(entryErr, fs.ErrNotExist) {
					return nil
				}
				return entryErr
			}
			if err := budget.check(); err != nil {
				return err
			}
			return visit(path, entry)
		})
	}
	if err := walkRolloutsIncremental(ctx, root, budget, visit); err != nil {
		return fmt.Errorf("walk Codex rollouts: %w", err)
	}
	return nil
}

func walkRolloutsIncremental(ctx context.Context, root string, budget *rolloutScanBudget, visit func(string, fs.DirEntry) error) error {
	if err := budget.check(); err != nil {
		return err
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("lstat rollout root: %w", err)
	}
	if !rootInfo.IsDir() {
		return nil
	}
	return walkRolloutDirectory(ctx, root, budget, visit)
}

func walkRolloutDirectory(ctx context.Context, dirPath string, budget *rolloutScanBudget, visit func(string, fs.DirEntry) error) error {
	dir, err := os.Open(dirPath) //nolint:gosec // rollout root is user configuration or Codex's own directory
	if err != nil {
		return fmt.Errorf("open rollout directory: %w", err)
	}
	defer dir.Close()

	for {
		if err := budget.check(); err != nil {
			return err
		}
		entries, readErr := dir.ReadDir(budget.limits.readDirBatch)
		for _, entry := range entries {
			if err := budget.check(); err != nil {
				return err
			}
			path := filepath.Join(dirPath, entry.Name())
			if entry.IsDir() {
				if err := walkRolloutDirectory(ctx, path, budget, visit); err != nil {
					return err
				}
				continue
			}
			if err := visit(path, entry); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("read rollout directory: %w", readErr)
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("walk rollout directory canceled: %w", err)
		}
	}
}

func (c *CodexAgent) inspectFallbackCandidate(
	path string,
	agentIDs map[string]struct{},
	budget *rolloutScanBudget,
) (string, loadedRollout, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", loadedRollout{}, fmt.Errorf("lstat rollout candidate: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", loadedRollout{}, nil
	}

	if c.loadRollout != nil {
		loaded, loadErr := c.loadRollout(path)
		if loadErr != nil {
			return "", loadedRollout{}, loadErr
		}
		if loaded.Path == "" {
			loaded.Path = path
		}
		if loaded.Path != path {
			return "", loadedRollout{}, errors.New("rollout loader returned a different path")
		}
		id, metaErr := sessionMetaID(loaded.Data)
		if metaErr != nil {
			return "", loadedRollout{}, metaErr
		}
		if _, wanted := agentIDs[id]; !wanted {
			return id, loadedRollout{}, nil
		}
		if int64(len(loaded.Data)) > budget.limits.bodyByteLimit {
			return "", loadedRollout{}, errRolloutScanBudget
		}
		if err := budget.observeBytes(int64(len(loaded.Data))); err != nil {
			return "", loadedRollout{}, err
		}
		return id, loaded, nil
	}

	file, opened, err := openRolloutFile(path, info)
	if err != nil {
		return "", loadedRollout{}, err
	}
	defer file.Close()

	metadata, err := c.readFallbackMetadata(file, path, budget)
	if err != nil {
		return "", loadedRollout{}, err
	}
	id, err := sessionMetaID(metadata)
	if err != nil {
		return "", loadedRollout{}, err
	}
	if _, wanted := agentIDs[id]; !wanted {
		return id, loadedRollout{}, nil
	}
	if opened.Size() > budget.limits.bodyByteLimit {
		return "", loadedRollout{}, fmt.Errorf("rollout body size %d exceeds limit %d: %w", opened.Size(), budget.limits.bodyByteLimit, errRolloutScanBudget)
	}
	if opened.Size() > budget.limits.aggregateByteLimit-budget.aggregateBytes {
		return "", loadedRollout{}, fmt.Errorf("aggregate rollout size exceeds limit %d: %w", budget.limits.aggregateByteLimit, errRolloutScanBudget)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", loadedRollout{}, fmt.Errorf("seek rollout candidate: %w", err)
	}
	data, err := readRolloutBody(file, rolloutReadOptions{
		path: path, byteLimit: budget.limits.bodyByteLimit, check: budget.check,
		account: budget.observeBytes, observe: c.observeRolloutRead, limitErr: errRolloutScanBudget,
	})
	if err != nil {
		return "", loadedRollout{}, err
	}
	return id, loadedRollout{Path: path, Data: data}, nil
}

func (c *CodexAgent) readFallbackMetadata(file *os.File, path string, budget *rolloutScanBudget) ([]byte, error) {
	data := make([]byte, 0, min(rolloutMetadataChunk, int(budget.limits.metadataByteLimit)))
	buffer := make([]byte, rolloutMetadataChunk)
	for {
		if err := budget.check(); err != nil {
			return nil, err
		}
		remaining := budget.limits.metadataByteLimit + 1 - int64(len(data))
		if remaining <= 0 {
			return nil, fmt.Errorf("rollout metadata exceeds limit %d: %w", budget.limits.metadataByteLimit, errRolloutScanBudget)
		}
		readSize := len(buffer)
		if int64(readSize) > remaining {
			readSize = int(remaining)
		}
		n, err := file.Read(buffer[:readSize])
		if n > 0 {
			if budgetErr := budget.observeBytes(int64(n)); budgetErr != nil {
				return nil, budgetErr
			}
			if c.observeRolloutRead != nil {
				c.observeRolloutRead(path, n)
			}
			chunk := buffer[:n]
			if newline := indexByte(chunk, '\n'); newline >= 0 {
				data = append(data, chunk[:newline+1]...)
				if int64(len(data)) > budget.limits.metadataByteLimit {
					return nil, fmt.Errorf("rollout metadata exceeds limit %d: %w", budget.limits.metadataByteLimit, errRolloutScanBudget)
				}
				return data, nil
			}
			data = append(data, chunk...)
			if int64(len(data)) > budget.limits.metadataByteLimit {
				return nil, fmt.Errorf("rollout metadata exceeds limit %d: %w", budget.limits.metadataByteLimit, errRolloutScanBudget)
			}
		}
		if errors.Is(err, io.EOF) {
			if len(data) == 0 {
				return nil, errors.New("rollout metadata is empty")
			}
			return data, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read rollout metadata: %w", err)
		}
	}
}

func indexByte(data []byte, target byte) int {
	for index, value := range data {
		if value == target {
			return index
		}
	}
	return -1
}

// scanFallbackRollouts scans every configured root once. Any traversal or
// regular-candidate metadata failure discards all results: partial results
// cannot prove a child ID is unique.
func (c *CodexAgent) scanFallbackRollouts(ctx context.Context, agentIDs map[string]struct{}) (map[string]loadedRollout, error) {
	if len(agentIDs) == 0 {
		return map[string]loadedRollout{}, nil
	}
	limits := defaultRolloutScanLimits
	if c.scanLimits != nil {
		limits = *c.scanLimits
	}
	budget := newRolloutScanBudget(ctx, limits)
	matches := make(map[string][]loadedRollout)
	seenPaths := make(map[string]struct{})
	for _, root := range c.rolloutRoots() {
		if root == "" {
			continue
		}
		walkErr := c.walkRollouts(ctx, root, budget, func(path string, entry fs.DirEntry) error {
			if entry.IsDir() || filepath.Ext(path) != ".jsonl" {
				return nil
			}
			if err := budget.observeCandidate(); err != nil {
				return err
			}
			id, loaded, err := c.inspectFallbackCandidate(path, agentIDs, budget)
			if err != nil {
				return fmt.Errorf("inspect rollout candidate: %w", err)
			}
			if loaded.Path == "" {
				return nil
			}
			if _, duplicate := seenPaths[path]; !duplicate {
				seenPaths[path] = struct{}{}
				matches[id] = append(matches[id], loaded)
			}
			return nil
		})
		if walkErr != nil {
			return nil, walkErr
		}
	}
	resolved := make(map[string]loadedRollout)
	for id, candidates := range matches {
		if len(candidates) == 1 {
			resolved[id] = candidates[0]
		}
	}
	return resolved, nil
}

// NewCodexAgent creates a new Codex agent instance.
func NewCodexAgent() agent.Agent {
	return &CodexAgent{}
}

// Name returns the agent registry key.
func (c *CodexAgent) Name() types.AgentName {
	return agent.AgentNameCodex
}

// Type returns the agent type identifier.
func (c *CodexAgent) Type() types.AgentType {
	return agent.AgentTypeCodex
}

// Description returns a human-readable description.
func (c *CodexAgent) Description() string {
	return "Codex - OpenAI's CLI coding agent"
}

// IsPreview returns true because this is a new integration.
func (c *CodexAgent) IsPreview() bool { return true }

// DetectPresence checks if Codex is configured in the repository.
func (c *CodexAgent) DetectPresence(ctx context.Context) (bool, error) {
	return c.AreHooksInstalled(ctx)
}

// GetSessionID extracts the session ID from hook input.
func (c *CodexAgent) GetSessionID(input *agent.HookInput) string {
	return input.SessionID
}

// resolveCodexHome returns the Codex home directory (CODEX_HOME or ~/.codex).
func resolveCodexHome() (string, error) {
	if codexHome := os.Getenv("CODEX_HOME"); codexHome != "" {
		return codexHome, nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(homeDir, ".codex"), nil
}

// GetSessionDir returns the directory where Codex stores session transcripts.
// Codex stores transcripts under CODEX_HOME/sessions/YYYY/MM/DD/.
func (c *CodexAgent) GetSessionDir(_ string) (string, error) {
	if override := os.Getenv("ENTIRE_TEST_CODEX_SESSION_DIR"); override != "" {
		return override, nil
	}
	codexHome, err := resolveCodexHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(codexHome, "sessions"), nil
}

// ResolveSessionFile returns the path to a Codex session transcript file.
// Codex provides the transcript path directly in hook payloads as an absolute path.
// When only a session ID is available, callers recover it from the
// sessions/YYYY/MM/DD/rollout-...-<session-id>.jsonl layout.
func (c *CodexAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	if filepath.IsAbs(agentSessionID) {
		return agentSessionID
	}
	if path := findRolloutBySessionID(sessionDir, agentSessionID); path != "" {
		return path
	}
	if sessionDir != "" {
		return filepath.Join(sessionDir, agentSessionID+".jsonl")
	}
	return agentSessionID
}

// ResolveRestoredSessionFile returns the canonical Codex rollout path for a
// restored session so `codex resume <id>` can rediscover it.
func (c *CodexAgent) ResolveRestoredSessionFile(sessionDir, agentSessionID string, transcript []byte) (string, error) {
	if err := validation.ValidateAgentSessionID(agentSessionID); err != nil {
		return "", fmt.Errorf("validate agent session ID: %w", err)
	}
	startTime, err := parseSessionStartTime(transcript)
	if err != nil {
		return "", fmt.Errorf("parse session start time: %w", err)
	}
	return restoredRolloutPath(sessionDir, agentSessionID, startTime), nil
}

// ProtectedDirs returns directories that Codex uses for config/state.
func (c *CodexAgent) ProtectedDirs() []string { return []string{".codex"} }

// ReadSession reads a session from Codex's storage (JSONL rollout file).
func (c *CodexAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	if input.SessionRef == "" {
		return nil, errors.New("session reference (transcript path) is required")
	}

	data, err := os.ReadFile(input.SessionRef)
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}

	startTime, err := parseSessionStartTime(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse session start time: %w", err)
	}

	// Extract modified files from the rollout transcript (best-effort, deduplicated).
	var modifiedFiles []string
	seen := make(map[string]struct{})
	for _, lineData := range splitJSONL(data) {
		for _, f := range extractFilesFromLine(lineData) {
			if _, exists := seen[f]; !exists {
				seen[f] = struct{}{}
				modifiedFiles = append(modifiedFiles, f)
			}
		}
	}

	return &agent.AgentSession{
		SessionID:     input.SessionID,
		AgentName:     c.Name(),
		SessionRef:    input.SessionRef,
		StartTime:     startTime,
		NativeData:    data,
		ModifiedFiles: modifiedFiles,
	}, nil
}

// WriteSession writes a session to Codex's storage (JSONL rollout file).
func (c *CodexAgent) WriteSession(_ context.Context, session *agent.AgentSession) error {
	if session == nil {
		return errors.New("session is nil")
	}

	if session.AgentName != "" && session.AgentName != c.Name() {
		return fmt.Errorf("session belongs to agent %q, not %q", session.AgentName, c.Name())
	}

	if session.SessionRef == "" {
		return errors.New("session reference (transcript path) is required")
	}

	if len(session.NativeData) == 0 {
		return errors.New("session has no native data to write")
	}

	dataToWrite := SanitizePortableTranscript(session.NativeData)
	if err := os.WriteFile(session.SessionRef, dataToWrite, 0o600); err != nil {
		return fmt.Errorf("failed to write transcript: %w", err)
	}

	return nil
}

// FormatResumeCommand returns the command to resume a Codex session.
func (c *CodexAgent) FormatResumeCommand(sessionID string) string {
	return "codex resume " + sessionID
}

// ReadTranscript reads the raw JSONL transcript bytes for a session.
func (c *CodexAgent) ReadTranscript(sessionRef string) ([]byte, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path comes from agent hook input
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}
	return data, nil
}

// ChunkTranscript splits a JSONL transcript at line boundaries.
func (c *CodexAgent) ChunkTranscript(_ context.Context, content []byte, maxSize int) ([][]byte, error) {
	chunks, err := agent.ChunkJSONL(content, maxSize)
	if err != nil {
		return nil, fmt.Errorf("failed to chunk JSONL transcript: %w", err)
	}
	return chunks, nil
}

// ReassembleTranscript concatenates JSONL chunks with newlines.
func (c *CodexAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	return agent.ReassembleJSONL(chunks), nil
}

func restoredRolloutPath(codexHome, agentSessionID string, startTime time.Time) string {
	timestamp := startTime.UTC()
	datePath := filepath.Join(
		codexHome,
		timestamp.Format("2006"),
		timestamp.Format("01"),
		timestamp.Format("02"),
	)
	filename := fmt.Sprintf("rollout-%s-%s.jsonl", timestamp.Format("2006-01-02T15-04-05"), agentSessionID)
	return filepath.Join(datePath, filename)
}

// LaunchCmd builds an exec.Cmd for `codex "<initialPrompt>"`. Stdio is wired
// to the caller's TTY so the agent runs foreground and the user interacts
// normally. The call site is expected to Run() and wait. Hooks inherit the
// parent environment.
func (c *CodexAgent) LaunchCmd(ctx context.Context, initialPrompt string) (*exec.Cmd, error) {
	bin, err := exec.LookPath("codex")
	if err != nil {
		return nil, fmt.Errorf("codex binary not on PATH: %w", err)
	}
	cmd := exec.CommandContext(ctx, bin, initialPrompt)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	return cmd, nil
}

func findRolloutBySessionID(codexHome, agentSessionID string) string {
	if codexHome == "" || validation.ValidateAgentSessionID(agentSessionID) != nil {
		return ""
	}

	patterns := []string{
		filepath.Join(codexHome, "rollout-*-"+agentSessionID+".jsonl"),
		filepath.Join(codexHome, "*", "*", "*", "rollout-*-"+agentSessionID+".jsonl"),
		filepath.Join(filepath.Dir(codexHome), "archived_sessions", "*", "*", "*", "rollout-*-"+agentSessionID+".jsonl"),
	}
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil || len(matches) == 0 {
			continue
		}
		// Multiple restored rollouts for the same session ID can exist. Return the
		// lexicographically latest path so newer dated restores win deterministically.
		sort.Strings(matches)
		return matches[len(matches)-1]
	}

	return ""
}
