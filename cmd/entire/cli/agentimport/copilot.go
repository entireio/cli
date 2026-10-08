package agentimport

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/copilotcli"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// maxCopilotLineBytes bounds the scanner buffer when reading events.jsonl;
// individual Copilot events (e.g. large tool payloads) can exceed bufio's
// default 64 KB line limit.
const maxCopilotLineBytes = 10 * 1024 * 1024

// copilotImporter imports Copilot CLI transcripts. Copilot stores sessions
// flat (~/.copilot/session-state/<id>/events.jsonl), not per-repo, so Discover
// reads each session's session.start event and keeps only those whose
// cwd/gitRoot is the repo root or a descendant.
type copilotImporter struct{}

func (copilotImporter) Name() string { return string(agent.AgentNameCopilotCLI) }

func (copilotImporter) AgentType() types.AgentType { return agent.AgentTypeCopilotCLI }

// Discover returns Copilot session transcripts belonging to this repo (by the
// session.start context) modified within the lookback window. The session ID is
// the session-state subdirectory name.
func (copilotImporter) Discover(repoRoot, overridePath string, now time.Time, sessionFilter []string) ([]SessionFile, error) {
	dir, err := resolveDir(repoRoot, overridePath, "copilot", (&copilotcli.CopilotCLIAgent{}).GetSessionDir)
	if err != nil {
		return nil, err
	}
	// Each session is a subdirectory holding events.jsonl; keep only those whose
	// session.start places them in this repo.
	return discoverSessionFiles(dir, now, sessionFilter, func(dir string, e os.DirEntry) (string, string, bool) {
		if !e.IsDir() {
			return "", "", false
		}
		path := filepath.Join(dir, e.Name(), "events.jsonl")
		if !copilotSessionInRepo(path, repoRoot) {
			return "", "", false
		}
		return e.Name(), path, true
	})
}

// copilotSessionInRepo reports whether the session's session.start event places
// it in this repo (gitRoot or cwd is the repo root or a descendant). Sessions
// whose location can't be determined are treated as not belonging to the repo.
func copilotSessionInRepo(path, repoRoot string) bool {
	f, err := os.Open(path) //nolint:gosec // path discovered under the configured session dir
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	// Scan line-by-line and stop at the first session.start (normally line 0)
	// rather than slurping the whole transcript, which can be large.
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), maxCopilotLineBytes)
	for scanner.Scan() {
		var evt struct {
			Type string `json:"type"`
			Data struct {
				Context struct {
					Cwd     string `json:"cwd"`
					GitRoot string `json:"gitRoot"`
				} `json:"context"`
			} `json:"data"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &evt); err != nil || evt.Type != "session.start" {
			continue
		}
		return repoMatches(evt.Data.Context.GitRoot, repoRoot) || repoMatches(evt.Data.Context.Cwd, repoRoot)
	}
	return false
}

// SplitTurns produces one Turn per user.message event, bounded by the next.
// Token usage is delegated to the Copilot agent (per-turn slices sum
// assistant.message outputTokens); the model is read once from the transcript.
func (copilotImporter) SplitTurns(sf SessionFile, full []byte) ([]Turn, error) {
	ag := &copilotcli.CopilotCLIAgent{}
	model := copilotcli.ExtractModelFromTranscript(context.Background(), sf.Path)
	return splitLineTurns(splitRawLines(full),
		func(raw []byte) bool { _, ok := copilotPromptText(raw); return ok },
		func(rawLines [][]byte, start, _ int, truncated []byte) (*Turn, error) {
			tokens, err := ag.CalculateTokenUsage(truncated, start)
			if err != nil {
				return nil, fmt.Errorf("token usage: %w", err)
			}
			var evt struct {
				ID        string          `json:"id"`
				Timestamp json.RawMessage `json:"timestamp"`
			}
			if err := json.Unmarshal(rawLines[start], &evt); err != nil {
				//nolint:nilerr // skip defensively; the line already parsed in copilotPromptText
				return nil, nil
			}
			// Copilot timestamps may be numeric epoch-millis or an RFC3339
			// string; decode via the agent's dual-format parser so a numeric
			// timestamp doesn't fail the turn.
			createdAt, tsErr := copilotcli.ParseTimestamp(evt.Timestamp)
			if tsErr != nil {
				// A malformed timestamp degrades to the zero time rather than
				// dropping the turn.
				createdAt = time.Time{}
			}
			prompt, _ := copilotPromptText(rawLines[start])
			return &Turn{UUID: evt.ID, Prompt: prompt, Model: model, CreatedAt: createdAt, Tokens: tokens}, nil
		})
}

// copilotPromptText reports whether a raw events.jsonl line is a user.message
// and returns its content. Other event types return false.
func copilotPromptText(raw []byte) (string, bool) {
	var evt struct {
		Type string `json:"type"`
		Data struct {
			Content string `json:"content"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &evt); err != nil || evt.Type != "user.message" {
		return "", false
	}
	if evt.Data.Content == "" {
		return "", false
	}
	return evt.Data.Content, true
}
