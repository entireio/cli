package agentimport

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// codexImporter imports Codex rollout transcripts. Codex stores sessions
// globally (CODEX_HOME/sessions/YYYY/MM/DD/rollout-*.jsonl), not per-repo, so
// Discover walks the tree and keeps only sessions whose session_meta cwd is the
// repo root or a descendant of it.
type codexImporter struct{}

func (codexImporter) Name() string { return string(agent.AgentNameCodex) }

func (codexImporter) AgentType() types.AgentType { return agent.AgentTypeCodex }

// codexSessionMeta is the subset of a Codex session_meta payload import needs.
type codexSessionMeta struct {
	ID  string `json:"id"`
	Cwd string `json:"cwd"`
}

// Discover walks the Codex sessions tree and returns transcripts belonging to
// this repo (by session_meta cwd) modified within the lookback window.
func (codexImporter) Discover(repoRoot, overridePath string, now time.Time, sessionFilter []string) ([]SessionFile, error) {
	dir, err := resolveDir(repoRoot, overridePath, "codex", (&codex.CodexAgent{}).GetSessionDir)
	if err != nil {
		return nil, err
	}
	cutoff := now.AddDate(0, 0, -LookbackDays)
	var out []SessionFile
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil // missing root or vanished entry: nothing to import
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil || info.ModTime().Before(cutoff) {
			return nil //nolint:nilerr // skip unreadable/old entries, keep walking
		}
		meta, metaErr := codexReadSessionMeta(path)
		if metaErr != nil || !repoMatches(meta.Cwd, repoRoot) {
			return nil //nolint:nilerr // skip sessions we can't attribute to this repo
		}
		sessionID := meta.ID
		if sessionID == "" {
			sessionID = strings.TrimSuffix(d.Name(), ".jsonl")
		}
		if len(sessionFilter) > 0 && !slices.Contains(sessionFilter, sessionID) {
			return nil
		}
		out = append(out, SessionFile{Path: path, SessionID: sessionID})
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk codex sessions: %w", walkErr)
	}
	slices.SortFunc(out, func(a, b SessionFile) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// codexReadSessionMeta reads the first JSONL line of a rollout file and returns
// its session_meta payload. The first line must be session_meta by Codex's
// format.
func codexReadSessionMeta(path string) (codexSessionMeta, error) {
	f, err := os.Open(path) //nolint:gosec // path discovered by walking the configured session dir
	if err != nil {
		return codexSessionMeta{}, fmt.Errorf("open rollout: %w", err)
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReader(f)
	first, err := r.ReadBytes('\n')
	if len(first) == 0 && err != nil {
		return codexSessionMeta{}, fmt.Errorf("read session_meta line: %w", err)
	}
	var line struct {
		Type    string           `json:"type"`
		Payload codexSessionMeta `json:"payload"`
	}
	if jsonErr := json.Unmarshal(first, &line); jsonErr != nil || line.Type != "session_meta" {
		return codexSessionMeta{}, errors.New("first line is not session_meta")
	}
	return line.Payload, nil
}

// SplitTurns produces one Turn per user response_item, bounded by the next.
// Codex response_items carry no per-message UUID, so the turn's stable key is
// its (append-only) start line index. Token usage is delegated to the Codex
// agent, which computes the cumulative-usage delta for the line range.
func (codexImporter) SplitTurns(_ SessionFile, full []byte) ([]Turn, error) {
	ag := &codex.CodexAgent{}
	return splitLineTurns(splitRawLines(full),
		func(raw []byte) bool { _, ok := codexPromptText(raw); return ok },
		func(rawLines [][]byte, start, _ int, truncated []byte) (*Turn, error) {
			tokens, err := ag.CalculateTokenUsage(truncated, start)
			if err != nil {
				return nil, fmt.Errorf("token usage: %w", err)
			}
			prompt, _ := codexPromptText(rawLines[start])
			return &Turn{
				UUID:      strconv.Itoa(start),
				Prompt:    prompt,
				CreatedAt: codexLineTime(rawLines[start]),
				Tokens:    tokens,
			}, nil
		})
}

// codexPromptText reports whether a raw rollout line is a user-prompt
// response_item and returns its concatenated input_text. Assistant messages,
// tool calls, and event_msg lines return false.
func codexPromptText(raw []byte) (string, bool) {
	var line struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &line); err != nil || line.Type != "response_item" {
		return "", false
	}
	var payload struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(line.Payload, &payload); err != nil {
		return "", false
	}
	if payload.Type != "message" || payload.Role != "user" {
		return "", false
	}
	var texts []string
	for _, item := range payload.Content {
		if item.Type == "input_text" {
			if t := strings.TrimSpace(item.Text); t != "" {
				texts = append(texts, t)
			}
		}
	}
	if len(texts) == 0 {
		return "", false
	}
	return strings.Join(texts, "\n\n"), true
}

// codexLineTime returns the RFC3339 timestamp on a rollout line, or the zero
// time when absent or unparseable.
func codexLineTime(raw []byte) time.Time {
	var line struct {
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal(raw, &line); err != nil {
		return time.Time{}
	}
	return parseTimestamp(line.Timestamp)
}

// repoMatches reports whether cwd is the repo root or a descendant of it. Both
// paths are normalized (cleaned, symlinks resolved best-effort) before
// comparison. Used by the global/flat-store importers (Codex, Copilot) to keep
// only sessions belonging to this repo.
func repoMatches(cwd, repoRoot string) bool {
	if cwd == "" || repoRoot == "" {
		return false
	}
	rel, err := filepath.Rel(normalizePath(repoRoot), normalizePath(cwd))
	if err != nil {
		return false
	}
	return !strings.HasPrefix(rel, "..")
}

// normalizePath cleans a path and resolves symlinks when possible, so a cwd
// recorded through a symlinked path (e.g. macOS /var → /private/var) still
// matches the repo root. Falls back to the cleaned path when the target does
// not exist on this machine.
func normalizePath(p string) string {
	cleaned := filepath.Clean(p)
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		return resolved
	}
	return cleaned
}
