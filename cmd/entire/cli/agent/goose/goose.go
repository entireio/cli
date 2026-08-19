// Package goose implements the Agent interface for Goose
// (https://goose-docs.ai/), the Agentic AI Foundation's open-source coding
// agent.
//
// Goose stores conversations in a SQLite database rather than in per-session
// files, so this integration follows the same shape as the OpenCode one: read
// through the agent's canonical export command, write through its canonical
// import command, and never touch the database directly. See AGENT.md for the
// protocol notes and their provenance.
package goose

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
	agent.Register(agent.AgentNameGoose, NewGooseAgent)
}

//nolint:revive // GooseAgent is clearer than Agent in this context
type GooseAgent struct{}

// NewGooseAgent creates a new Goose agent instance.
func NewGooseAgent() agent.Agent {
	return &GooseAgent{}
}

// --- Identity ---

func (a *GooseAgent) Name() types.AgentName { return agent.AgentNameGoose }
func (a *GooseAgent) Type() types.AgentType { return agent.AgentTypeGoose }
func (a *GooseAgent) Description() string {
	return "Goose - open-source AI coding agent (Agentic AI Foundation)"
}
func (a *GooseAgent) IsPreview() bool { return true }

// ProtectedDirs returns repo-relative directories Entire should not track.
// .agents/ is Goose's Open Plugins root: it holds the hooks Entire installs
// plus any user plugins, recipes and skills.
func (a *GooseAgent) ProtectedDirs() []string { return []string{".agents"} }

// DetectPresence reports whether this repo looks like a Goose project.
func (a *GooseAgent) DetectPresence(ctx context.Context) (bool, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "."
	}
	if _, err := os.Stat(filepath.Join(repoRoot, pluginsDirName)); err == nil {
		return true, nil
	}
	return false, nil
}

// --- Transcript Storage ---

// ReadTranscript reads the cached export JSON for a session. sessionRef is the
// path PrepareTranscript wrote to, not a path owned by Goose.
func (a *GooseAgent) ReadTranscript(sessionRef string) ([]byte, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path derived from validated session ID
	if err != nil {
		return nil, fmt.Errorf("failed to read goose transcript: %w", err)
	}
	return data, nil
}

// ChunkTranscript splits a Goose export by distributing conversation entries
// across chunks, repeating the session envelope in each one.
//
// The envelope is carried as a raw field map rather than through ExportSession
// so that top-level keys this package does not model (recipe, extension_data,
// project_id, ...) survive a chunk/reassemble round-trip.
func (a *GooseAgent) ChunkTranscript(_ context.Context, content []byte, maxSize int) ([][]byte, error) {
	fields, messages, err := splitExport(content)
	if err != nil {
		return nil, err
	}
	if len(messages) == 0 {
		return [][]byte{content}, nil
	}

	// Base size is the envelope with an empty conversation array.
	base, err := withConversation(fields, nil)
	if err != nil {
		return nil, err
	}
	baseSize := len(base)

	var (
		chunks  [][]byte
		current []json.RawMessage
		size    = baseSize
	)
	for _, msg := range messages {
		msgSize := len(msg) + 1 // +1 for the comma separator
		if size+msgSize > maxSize && len(current) > 0 {
			chunk, err := withConversation(fields, current)
			if err != nil {
				return nil, err
			}
			chunks = append(chunks, chunk)
			current = nil
			size = baseSize
		}
		current = append(current, msg)
		size += msgSize
	}
	if len(current) > 0 {
		chunk, err := withConversation(fields, current)
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, chunk)
	}
	if len(chunks) == 0 {
		return nil, errors.New("failed to create any chunks")
	}
	return chunks, nil
}

// ReassembleTranscript merges Goose export chunks by concatenating their
// conversation arrays. The envelope from the first chunk wins, mirroring how
// ChunkTranscript repeats it.
func (a *GooseAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	if len(chunks) == 0 {
		return nil, errors.New("no chunks to reassemble")
	}

	var (
		envelope map[string]json.RawMessage
		all      []json.RawMessage
	)
	for i, chunk := range chunks {
		fields, messages, err := splitExport(chunk)
		if err != nil {
			return nil, fmt.Errorf("failed to parse chunk %d: %w", i, err)
		}
		if i == 0 {
			envelope = fields
		}
		all = append(all, messages...)
	}
	return withConversation(envelope, all)
}

// --- Session handling ---

func (a *GooseAgent) GetSessionID(input *agent.HookInput) string {
	return input.SessionID
}

// GetSessionDir returns the directory Entire caches Goose exports in.
//
// Goose keeps conversations in SQLite, so there is no agent-owned directory to
// point at. These files are a handoff between the export command and the hook
// handler; once checkpointed the data lives on git refs and the file is
// disposable.
func (a *GooseAgent) GetSessionDir(repoPath string) (string, error) {
	if override := os.Getenv("ENTIRE_TEST_GOOSE_PROJECT_DIR"); override != "" {
		return override, nil
	}
	return filepath.Join(os.TempDir(), "entire-goose", sanitizePathForGoose(repoPath)), nil
}

// ResolveSessionFile builds the cache path for a session's export.
//
// SECURITY: agentSessionID becomes a path component, so callers handling
// untrusted input must pre-validate it with validation.ValidateSessionID.
// sessionTranscriptPath does exactly that before reaching this method; see
// security_contract_test.go.
func (a *GooseAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	return filepath.Join(sessionDir, agentSessionID+".json")
}

func (a *GooseAgent) ReadSession(input *agent.HookInput) (*agent.AgentSession, error) {
	if input.SessionRef == "" {
		return nil, errors.New("no session ref provided")
	}
	data, err := os.ReadFile(input.SessionRef)
	if err != nil {
		return nil, fmt.Errorf("failed to read session: %w", err)
	}

	modifiedFiles, err := ExtractModifiedFiles(data)
	if err != nil {
		// Non-fatal: the session is still usable without a file list, and git
		// status remains as a fallback source.
		logging.Warn(context.Background(), "failed to extract modified files from goose session",
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

// WriteSession restores a session into Goose's database via `goose session
// import`, which is what makes `goose session --resume --session-id <id>` work
// after a rewind.
func (a *GooseAgent) WriteSession(ctx context.Context, session *agent.AgentSession) error {
	if session == nil {
		return errors.New("nil session")
	}
	if len(session.NativeData) == 0 {
		return errors.New("no session data to write")
	}

	tmpFile, err := os.CreateTemp("", "entire-goose-export-*.json")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write(session.NativeData); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("failed to write export data: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	return runGooseImportFn(ctx, tmpFile.Name())
}

// FormatResumeCommand returns the command that resumes a Goose session.
//
// --session-id requires --resume ("Specify a session ID to resume. Requires
// --resume." per `goose session --help`), so the two flags are always emitted
// together. With no ID, bare `goose session --resume` continues the most recent
// session, which is the closest useful behaviour.
func (a *GooseAgent) FormatResumeCommand(sessionID string) string {
	if strings.TrimSpace(sessionID) == "" {
		return "goose session --resume"
	}
	return "goose session --resume --session-id " + sessionID
}

// nonAlphanumericRegex matches any non-alphanumeric character.
var nonAlphanumericRegex = regexp.MustCompile(`[^a-zA-Z0-9]`)

// sanitizePathForGoose converts a path into a safe single directory name, the
// same way the Claude, Gemini and OpenCode integrations do.
func sanitizePathForGoose(path string) string {
	return nonAlphanumericRegex.ReplaceAllString(path, "-")
}
