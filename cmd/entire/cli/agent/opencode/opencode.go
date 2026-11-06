// Package opencode implements the Agent interface for OpenCode.
package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

//nolint:gochecknoinits // Agent self-registration is the intended pattern
func init() {
	agent.Register(agent.AgentNameOpenCode, NewOpenCodeAgent)
}

//nolint:revive // OpenCodeAgent is clearer than Agent in this context
type OpenCodeAgent struct {
	// CommandRunner overrides text-generation subprocess creation when non-nil.
	CommandRunner agent.TextCommandRunner
}

// NewOpenCodeAgent creates a new OpenCode agent instance.
func NewOpenCodeAgent() agent.Agent {
	return &OpenCodeAgent{}
}

// --- Identity ---

func (a *OpenCodeAgent) Name() types.AgentName    { return agent.AgentNameOpenCode }
func (a *OpenCodeAgent) Type() types.AgentType    { return agent.AgentTypeOpenCode }
func (a *OpenCodeAgent) Description() string      { return "OpenCode - AI-powered terminal coding agent" }
func (a *OpenCodeAgent) ProtectedDirs() []string  { return []string{".opencode"} }
func (a *OpenCodeAgent) ProtectedFiles() []string { return []string{"opencode.json"} }

func (a *OpenCodeAgent) DetectPresence(ctx context.Context) (bool, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "."
	}
	// Check for .opencode directory or opencode.json config
	if _, err := os.Stat(filepath.Join(repoRoot, ".opencode")); err == nil {
		return true, nil
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "opencode.json")); err == nil {
		return true, nil
	}
	return false, nil
}

// --- Transcript Storage ---

// ReadTranscript reads the transcript for a session.
// The sessionRef is expected to be a path to the export JSON file.
func (a *OpenCodeAgent) ReadTranscript(sessionRef string) ([]byte, error) {
	data, err := agent.ReadTranscriptFile(sessionRef)
	if err != nil {
		return nil, fmt.Errorf("failed to read opencode transcript: %w", err)
	}
	return data, nil
}

// ChunkTranscript splits an OpenCode export JSON transcript by distributing messages across chunks.
// OpenCode uses JSON format with {"info": {...}, "messages": [...]} structure.
// Info and messages are carried as raw JSON, so fields the typed export
// structs do not model (such as the OpenCode 2 originals resume imports)
// survive.
func (a *OpenCodeAgent) ChunkTranscript(_ context.Context, content []byte, maxSize int) ([][]byte, error) {
	var session rawExportSession
	if err := json.Unmarshal(content, &session); err != nil {
		return nil, fmt.Errorf("failed to parse export session for chunking: %w", err)
	}

	if len(session.Messages) == 0 {
		return [][]byte{content}, nil
	}

	// Base JSON structure size: {"info":<info>,"messages":[ ... ]}
	baseSize := len(`{"info":`) + len(session.Info) + len(`,"messages":[]}`)

	var chunks [][]byte
	var currentMessages []json.RawMessage
	currentSize := baseSize

	for _, msg := range session.Messages {
		msgSize := len(msg) + 1 // +1 for comma separator

		if currentSize+msgSize > maxSize && len(currentMessages) > 0 {
			chunkData, err := json.Marshal(rawExportSession{Info: session.Info, Messages: currentMessages})
			if err != nil {
				return nil, fmt.Errorf("failed to marshal chunk: %w", err)
			}
			chunks = append(chunks, chunkData)

			currentMessages = nil
			currentSize = baseSize
		}

		currentMessages = append(currentMessages, msg)
		currentSize += msgSize
	}

	if len(currentMessages) > 0 {
		chunkData, err := json.Marshal(rawExportSession{Info: session.Info, Messages: currentMessages})
		if err != nil {
			return nil, fmt.Errorf("failed to marshal final chunk: %w", err)
		}
		chunks = append(chunks, chunkData)
	}

	if len(chunks) == 0 {
		return nil, errors.New("failed to create any chunks")
	}

	return chunks, nil
}

// rawExportSession is an export with its info and messages left as raw JSON.
type rawExportSession struct {
	Info     json.RawMessage   `json:"info"`
	Messages []json.RawMessage `json:"messages"`
}

// ReassembleTranscript merges OpenCode export JSON chunks by combining their message arrays.
func (a *OpenCodeAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	if len(chunks) == 0 {
		return nil, errors.New("no chunks to reassemble")
	}

	var allMessages []json.RawMessage
	var sessionInfo json.RawMessage

	for i, chunk := range chunks {
		var session rawExportSession
		if err := json.Unmarshal(chunk, &session); err != nil {
			return nil, fmt.Errorf("failed to unmarshal chunk %d: %w", i, err)
		}
		if i == 0 {
			sessionInfo = session.Info // Preserve session info from first chunk
		}
		allMessages = append(allMessages, session.Messages...)
	}

	result, err := json.Marshal(rawExportSession{Info: sessionInfo, Messages: allMessages})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal reassembled transcript: %w", err)
	}
	return result, nil
}

// --- Legacy methods ---

func (a *OpenCodeAgent) GetSessionID(input *agent.HookInput) string {
	return input.SessionID
}

// GetSessionDir returns the directory where Entire stores OpenCode session transcripts.
// Transcripts are ephemeral handoff files between the TS plugin and the Go hook handler.
// Once checkpointed, the data lives on git refs and the file is disposable.
// Stored in os.TempDir()/entire-opencode/<sanitized-path>/ to avoid squatting on
// OpenCode's own directories (~/.opencode/ is project-level, not home-level).
func (a *OpenCodeAgent) GetSessionDir(repoPath string) (string, error) {
	// Check for test environment override
	if override := os.Getenv("ENTIRE_TEST_OPENCODE_PROJECT_DIR"); override != "" {
		return override, nil
	}

	projectDir := SanitizePathForOpenCode(repoPath)
	return filepath.Join(os.TempDir(), "entire-opencode", projectDir), nil
}

func (a *OpenCodeAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	return filepath.Join(sessionDir, agentSessionID+".json")
}

func (a *OpenCodeAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	if input.SessionRef == "" {
		return nil, errors.New("no session ref provided")
	}
	data, err := agent.ReadTranscriptFile(input.SessionRef)
	if err != nil {
		return nil, fmt.Errorf("failed to read session: %w", err)
	}

	// Parse to extract computed fields
	modifiedFiles, err := ExtractModifiedFiles(data)
	if err != nil {
		// Non-fatal: we can still return the session without modified files
		logging.Warn(context.Background(), "failed to extract modified files from opencode session",
			slog.String("session_ref", input.SessionRef),
			slog.String("error", err.Error()),
		)
		modifiedFiles = nil
	}

	return &agent.AgentSession{
		AgentName:     a.Name(),
		SessionID:     input.SessionID,
		SessionRef:    input.SessionRef,
		NativeData:    data,
		ModifiedFiles: modifiedFiles,
	}, nil
}

func (a *OpenCodeAgent) WriteSession(ctx context.Context, session *agent.AgentSession) error {
	if session == nil {
		return errors.New("nil session")
	}
	if len(session.NativeData) == 0 {
		return errors.New("no session data to write")
	}

	// Import the session into OpenCode's database.
	// This enables `opencode -s <id>` to resume the restored session.
	return a.importSessionIntoOpenCode(ctx, session)
}

// importSessionIntoOpenCode writes the export JSON to a temp file and runs
// `opencode import` to restore the session into OpenCode's database.
// An existing session is deleted first so the import replaces its messages.
//
// OpenCode 2 never deletes: a session OpenCode still has resumes as it is.
// A missing one is imported from the OpenCode 2 messages the stored
// transcript keeps (see openCodeV2ImportPayload); a transcript captured from
// OpenCode 1 has none, and OpenCode 2 cannot import the 1.x shape.
func (a *OpenCodeAgent) importSessionIntoOpenCode(ctx context.Context, session *agent.AgentSession) error {
	sessionID := session.SessionID
	if !detectOpenCodeCLI(ctx).legacy {
		exists, err := openCodeSessionExists(ctx, sessionID)
		if err != nil {
			return err
		}
		if exists {
			logging.Debug(ctx, "opencode session already present; skipping import",
				slog.String("session_id", sessionID))
			return nil
		}
		dir := session.RepoPath
		if dir == "" {
			if dir, err = paths.WorktreeRoot(ctx); err != nil {
				return fmt.Errorf("resolve worktree for OpenCode import: %w", err)
			}
		}
		payload, err := openCodeV2ImportPayload(session.NativeData, sessionID, dir)
		if errors.Is(err, errNotOpenCode2Capture) {
			return fmt.Errorf("OpenCode session %s is not in OpenCode's local store, and its checkpoint has no OpenCode 2 messages to import (captured with OpenCode 1 or an older Entire)", sessionID)
		}
		if err != nil {
			return err
		}
		return withTempExport(payload, func(path string) error {
			return runOpenCodeSessionImport(ctx, path, dir)
		})
	}
	// Delete existing session first so reimport replaces it cleanly.
	// opencode import uses ON CONFLICT DO NOTHING, so existing messages
	// would be skipped without this step.
	if err := runOpenCodeSessionDelete(ctx, sessionID); err != nil {
		// Non-fatal: session might not exist yet (first session).
		// Import will still work for new sessions; restoring an existing session
		// would leave stale messages.
		logging.Warn(ctx, "could not delete existing opencode session",
			slog.String("session_id", sessionID),
			slog.String("error", err.Error()),
		)
	}
	data, err := withoutStoredPermissions(session.NativeData)
	if err != nil {
		return err
	}
	return withTempExport(data, func(path string) error {
		return runOpenCodeImport(ctx, path)
	})
}

// withoutStoredPermissions removes the session's permission rules from a
// stored OpenCode 1 transcript before it is imported. They come from
// checkpoint data, so restoring them would let whoever can push checkpoints
// change how tools are approved in the resuming user's OpenCode; the session
// then follows that user's own configuration.
func withoutStoredPermissions(data []byte) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse stored OpenCode transcript: %w", err)
	}
	var info map[string]json.RawMessage
	if err := json.Unmarshal(doc["info"], &info); err != nil {
		return nil, fmt.Errorf("parse stored OpenCode session info: %w", err)
	}
	if _, ok := info["permission"]; !ok {
		return data, nil
	}
	delete(info, "permission")
	rawInfo, err := json.Marshal(info)
	if err != nil {
		return nil, fmt.Errorf("encode OpenCode session info: %w", err)
	}
	doc["info"] = rawInfo
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode OpenCode import: %w", err)
	}
	return out, nil
}

// withTempExport writes export JSON to a temp file for `opencode import`,
// calls fn with its path and removes it.
func withTempExport(data []byte, fn func(path string) error) error {
	tmpFile, err := os.CreateTemp("", "entire-opencode-export-*.json")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("failed to write export data: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}
	return fn(tmpFile.Name())
}

func (a *OpenCodeAgent) FormatResumeCommand(sessionID string) string {
	if strings.TrimSpace(sessionID) == "" {
		return "opencode"
	}
	return "opencode -s " + sessionID
}

// nonAlphanumericRegex matches any non-alphanumeric character.
var nonAlphanumericRegex = regexp.MustCompile(`[^a-zA-Z0-9]`)

// SanitizePathForOpenCode converts a path to a safe directory name.
// Replaces any non-alphanumeric character with a dash (same approach as Claude).
func SanitizePathForOpenCode(path string) string {
	return nonAlphanumericRegex.ReplaceAllString(path, "-")
}
