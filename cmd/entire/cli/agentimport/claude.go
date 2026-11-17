package agentimport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
	transcriptlib "github.com/entireio/cli/transcript"
)

// claudeImporter imports Claude Code transcripts (~/.claude/projects/<slug>/*.jsonl).
type claudeImporter struct{}

func (claudeImporter) Name() string { return "claude-code" }

func (claudeImporter) AgentType() types.AgentType { return agent.AgentTypeClaudeCode }

// Discover returns Claude transcript files for the repo modified within the
// lookback window whose recorded cwd is inside repoRoot (see
// recordedCwdIn). overridePath replaces the default ~/.claude/projects/<slug>
// dir; sessionFilter, when non-empty, keeps only matching session IDs (the
// file stem).
func (claudeImporter) Discover(repoRoot, overridePath string, now time.Time, sessionFilter []string) ([]SessionFile, error) {
	dir, err := resolveDir(repoRoot, overridePath, "claude", (&claudecode.ClaudeCodeAgent{}).GetSessionDir)
	if err != nil {
		return nil, err
	}
	return discoverSessionFiles(dir, now, sessionFilter, jsonlSessionResolver(identitySessionID), recordedCwdIn(repoRoot))
}

// SplitTurns produces one Turn per user-prompt line. Main-agent token usage for
// each turn is computed on the slice [LineStart, LineEnd) so turns don't
// double-count later turns; subagent token usage is discovered from the full
// prefix and rescoped to a per-turn delta by splitLineTurns (see
// rescopeSubagentTokensToDeltas). tool_result lines (Type == "user" but no text
// content) do not start a turn.
func (claudeImporter) SplitTurns(sf SessionFile, full []byte) ([]Turn, error) {
	subagentsDir := paths.SubagentsDir(filepath.Dir(sf.Path), sf.SessionID)
	ag := &claudecode.ClaudeCodeAgent{}
	return splitLineTurns(splitRawLines(full), isUserPromptLine,
		func(rawLines [][]byte, start, end int, truncated []byte) (*Turn, error) {
			tokens, err := ag.CalculateTotalTokenUsage(truncated, start, subagentsDir)
			if err != nil {
				return nil, fmt.Errorf("token usage: %w", err)
			}
			var rec struct {
				UUID      string          `json:"uuid"`
				Message   json.RawMessage `json:"message"`
				Timestamp string          `json:"timestamp"`
			}
			if err := json.Unmarshal(rawLines[start], &rec); err != nil {
				//nolint:nilerr // skip defensively; the line already parsed in isUserPromptLine
				return nil, nil
			}
			return &Turn{
				UUID:          rec.UUID,
				Prompt:        transcript.ExtractUserContent(rec.Message),
				Model:         modelInRange(rawLines, start, end),
				CreatedAt:     parseTimestamp(rec.Timestamp),
				Tokens:        tokens,
				CommitSHAs:    commitSHAsInRange(rawLines, start, end),
				CommitWindows: commitWindowsInRange(rawLines, start, end),
			}, nil
		})
}

// claudeExtraFields are line fields not modeled by transcript.Line.
type claudeExtraFields struct {
	ParentUUID string `json:"parentUuid"`
	Timestamp  string `json:"timestamp"`
	Message    struct {
		Model string `json:"model"`
	} `json:"message"`
}

// modelInRange returns the model from the first assistant message within
// [start, end), or "" when none carries one. The model lives on assistant
// lines, not the user-prompt line.
func modelInRange(rawLines [][]byte, start, end int) string {
	for i := start; i < end && i < len(rawLines); i++ {
		var line transcript.Line
		if err := json.Unmarshal(rawLines[i], &line); err != nil {
			continue
		}
		if line.Type != "assistant" {
			continue
		}
		var ex claudeExtraFields
		if err := json.Unmarshal(rawLines[i], &ex); err != nil {
			continue
		}
		if ex.Message.Model != "" {
			return ex.Message.Model
		}
	}
	return ""
}

// commitSHAsInRange returns the commit SHAs recorded by gitOperation
// tool-result records within [start, end), in order. Only kind "committed"
// is collected — other kinds (or commit-less gitOperation records like
// push/branch/pr) are ignored. SHAs may be abbreviated; resolution happens
// in Run.
func commitSHAsInRange(rawLines [][]byte, start, end int) []string {
	var shas []string
	for i := start; i < end && i < len(rawLines); i++ {
		var rec struct {
			ToolUseResult struct {
				GitOperation struct {
					Commit struct {
						SHA  string `json:"sha"`
						Kind string `json:"kind"`
					} `json:"commit"`
				} `json:"gitOperation"`
			} `json:"toolUseResult"`
		}
		if err := json.Unmarshal(rawLines[i], &rec); err != nil {
			continue
		}
		c := rec.ToolUseResult.GitOperation.Commit
		if c.SHA != "" && c.Kind == "committed" {
			shas = append(shas, c.SHA)
		}
	}
	return shas
}

// commitCommandPattern matches a shell command that runs git, any global
// options (-C dir, -c key=val, --no-pager, ...), then a subcommand that writes
// commits onto the current branch. Merge, cherry-pick, revert, rebase and am
// count alongside commit because agents land commits through them too. The
// subcommand must end there, so commit-tree and merge-base don't match.
var commitCommandPattern = regexp.MustCompile(`\bgit(?:\s+-[Cc]\s+\S+|\s+--?[A-Za-z][\w-]*(?:=\S+)?)*\s+(?:commit|cherry-pick|revert|merge|rebase|am)(?:\s|[;&|)]|$)`)

// The closing quote keeps toolUseToken from matching "tool_use_id", which
// every tool_result line carries.
var (
	toolUseToken    = []byte(`"tool_use"`)
	toolResultToken = []byte(`"tool_result"`)
)

// commitWindowsInRange returns the spans of the Bash calls within [start, end)
// whose command matches commitCommandPattern, in result order. Each runs from
// the line holding the tool_use to the line holding its tool_result, so a
// commit made by the call has a committer time inside it. The start is
// truncated to the second because git's committer time has second precision.
// A call with no result in the turn has no end and is skipped. A background
// call's result arrives before its command finishes, so its commit lands
// after the span and the turn falls back.
func commitWindowsInRange(rawLines [][]byte, start, end int) []TimeWindow {
	var windows []TimeWindow
	calls := make(map[string]time.Time) // tool_use id -> call time
	for i := start; i < end && i < len(rawLines); i++ {
		raw := rawLines[i]
		// Byte probes skip the JSON decode on the many lines that can't
		// matter; tool results routinely run to megabytes.
		mayCall := bytes.Contains(raw, toolUseToken)
		mayResult := len(calls) > 0 && bytes.Contains(raw, toolResultToken)
		if !mayCall && !mayResult {
			continue
		}
		var rec struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
			Message   struct {
				Content []struct {
					Type      string          `json:"type"`
					ID        string          `json:"id"`
					Name      string          `json:"name"`
					Input     json.RawMessage `json:"input"`
					ToolUseID string          `json:"tool_use_id"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(raw, &rec); err != nil {
			continue
		}
		at := parseTimestamp(rec.Timestamp)
		if at.IsZero() {
			continue
		}
		for _, block := range rec.Message.Content {
			switch {
			case rec.Type == transcriptlib.TypeAssistant && block.Type == transcriptlib.ContentTypeToolUse &&
				block.Name == "Bash" && block.ID != "":
				var input transcript.ToolInput
				if json.Unmarshal(block.Input, &input) == nil && commitCommandPattern.MatchString(input.Command) {
					calls[block.ID] = at.Truncate(time.Second)
				}
			case rec.Type == transcriptlib.TypeUser && block.Type == "tool_result":
				if callAt, ok := calls[block.ToolUseID]; ok {
					windows = append(windows, TimeWindow{Start: callAt, End: at})
					delete(calls, block.ToolUseID)
				}
			}
		}
	}
	return windows
}

// isUserPromptLine reports whether a raw JSONL line is a genuine user-prompt
// turn start: type "user" (or role "user") with non-empty extractable text.
// tool_result lines are type "user" but carry no text, so they return false.
func isUserPromptLine(raw []byte) bool {
	var line transcript.Line
	if err := json.Unmarshal(raw, &line); err != nil {
		return false
	}
	typ := line.Type
	if typ == "" {
		typ = line.Role
	}
	if typ != "user" {
		return false
	}
	return transcript.ExtractUserContent(line.Message) != ""
}

// splitRawLines splits content into raw lines using the same newline-counted
// indices as transcript.SliceFromLine. A trailing empty segment is dropped.
func splitRawLines(content []byte) [][]byte {
	if len(content) == 0 {
		return nil
	}
	parts := strings.Split(string(content), "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	out := make([][]byte, len(parts))
	for i, p := range parts {
		out[i] = []byte(p)
	}
	return out
}

// joinLines reassembles raw lines into newline-terminated bytes.
func joinLines(lines [][]byte) []byte {
	if len(lines) == 0 {
		return nil
	}
	strs := make([]string, len(lines))
	for i, l := range lines {
		strs[i] = string(l)
	}
	return []byte(strings.Join(strs, "\n") + "\n")
}
