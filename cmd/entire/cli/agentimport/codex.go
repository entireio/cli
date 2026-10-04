package agentimport

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
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
		func(rawLines [][]byte, start, end int, truncated []byte) (*Turn, error) {
			tokens, err := ag.CalculateTokenUsage(truncated, start)
			if err != nil {
				return nil, fmt.Errorf("token usage: %w", err)
			}
			prompt, _ := codexPromptText(rawLines[start])
			return &Turn{
				UUID:       strconv.Itoa(start),
				Prompt:     prompt,
				CreatedAt:  codexLineTime(rawLines[start]),
				Tokens:     tokens,
				CommitSHAs: codexCommitSHAsInRange(rawLines, start, end),
			}, nil
		})
}

var (
	codexGitCommit     = regexp.MustCompile(`^(?:(?:[A-Za-z_][A-Za-z0-9_]*=[^\s]+)\s+)*(?:command\s+)?git\s+(?:-C\s+(?:"[^"]+"|'[^']+'|\S+)\s+)?commit(?:\s|$)`)
	codexJSExecCommand = regexp.MustCompile(`tools\.exec_command\s*\(\s*\{\s*cmd\s*:\s*`)
	codexCommitHeader  = regexp.MustCompile(`(?m)^\[[^]\r\n]+\s+([0-9a-fA-F]{7,40})\]`)
	codexExitCode      = regexp.MustCompile(`(?mi)(?:^|\s)(?:exit_code=|Process exited with code\s+)([0-9]+)(?:[.\s]|$)`)
)

// codexCommitSHAsInRange returns commit SHAs from paired Codex exec calls in
// [start, end). Only outputs belonging to a git commit command are considered.
// Explicit non-zero exit codes reject the output; successful candidates must
// use Git's standard "[branch <sha>] subject" commit header.
func codexCommitSHAsInRange(rawLines [][]byte, start, end int) []string {
	commitCalls := make(map[string]struct{})
	var shas []string
	for i := start; i < end && i < len(rawLines); i++ {
		var line struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(rawLines[i], &line); err != nil || line.Type != "response_item" {
			continue
		}
		var envelope struct {
			Type      string          `json:"type"`
			Name      string          `json:"name"`
			CallID    string          `json:"call_id"`
			Input     string          `json:"input"`
			Arguments string          `json:"arguments"`
			Output    json.RawMessage `json:"output"`
		}
		if err := json.Unmarshal(line.Payload, &envelope); err != nil {
			continue
		}
		switch envelope.Type {
		case "custom_tool_call", "function_call":
			if envelope.CallID != "" && codexCallContainsGitCommit(envelope) {
				commitCalls[envelope.CallID] = struct{}{}
			}
		case "custom_tool_call_output", "function_call_output":
			if _, ok := commitCalls[envelope.CallID]; !ok {
				continue
			}
			text := codexOutputText(envelope.Output)
			if code := codexExitCode.FindStringSubmatch(text); len(code) == 2 && code[1] != "0" {
				continue
			}
			for _, match := range codexCommitHeader.FindAllStringSubmatch(text, -1) {
				if len(match) == 2 && !slices.Contains(shas, match[1]) {
					shas = append(shas, match[1])
				}
			}
		}
	}
	return shas
}

func codexCallContainsGitCommit(call struct {
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	CallID    string          `json:"call_id"`
	Input     string          `json:"input"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`
}) bool {
	for _, command := range codexCallCommands(call.Type, call.Name, call.Input, call.Arguments) {
		if codexExecContainsGitCommit(command) {
			return true
		}
	}
	return false
}

func codexCallCommands(callType, name, input, arguments string) []string {
	switch {
	case callType == "function_call" && name == "exec_command":
		var args struct {
			Cmd string `json:"cmd"`
		}
		if json.Unmarshal([]byte(arguments), &args) == nil && args.Cmd != "" {
			return []string{args.Cmd}
		}
	case callType == "custom_tool_call" && name == "exec":
		commands := codexJSExecCommands(input)
		if len(commands) > 0 {
			return commands
		}
		return []string{input}
	}
	return nil
}

func codexJSExecCommands(source string) []string {
	if !strings.Contains(source, "tools.exec_command") {
		return nil
	}
	var commands []string
	for _, match := range codexJSExecCommand.FindAllStringIndex(source, -1) {
		if command, _, ok := codexJSStringLiteral(source[match[1]:]); ok {
			commands = append(commands, command)
		}
	}
	return commands
}

func codexJSStringLiteral(source string) (value string, consumed int, ok bool) {
	if source == "" || (source[0] != '\'' && source[0] != '"' && source[0] != '`') {
		return "", 0, false
	}
	quote := source[0]
	var b strings.Builder
	for i := 1; i < len(source); i++ {
		c := source[i]
		if c == quote {
			return b.String(), i + 1, true
		}
		if quote == '`' && c == '$' && i+1 < len(source) && source[i+1] == '{' {
			return "", 0, false
		}
		if c != '\\' || i+1 >= len(source) {
			b.WriteByte(c)
			continue
		}
		i++
		switch source[i] {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case '\n':
			// JavaScript line continuation.
		default:
			b.WriteByte(source[i])
		}
	}
	return "", 0, false
}

func codexExecContainsGitCommit(command string) bool {
	for _, segment := range codexShellCommandSegments(command) {
		if codexGitCommit.MatchString(segment) {
			return true
		}
	}
	return false
}

// codexShellCommandSegments splits only on unquoted shell control operators.
// It deliberately does not try to execute or fully parse shell syntax; it only
// establishes command boundaries so text inside quotes cannot masquerade as a
// git commit invocation.
func codexShellCommandSegments(command string) []string {
	var segments []string
	start := 0
	var quote byte
	escaped := false
	appendSegment := func(end int) {
		if segment := strings.TrimSpace(command[start:end]); segment != "" {
			segments = append(segments, segment)
		}
	}
	for i := 0; i < len(command); i++ {
		c := command[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' || c == '`' {
			quote = c
			continue
		}
		delimiterWidth := 0
		switch c {
		case '\n', ';':
			delimiterWidth = 1
		case '|':
			delimiterWidth = 1
			if i+1 < len(command) && command[i+1] == '|' {
				delimiterWidth = 2
			}
		case '&':
			if i+1 < len(command) && command[i+1] == '&' {
				delimiterWidth = 2
			}
		}
		if delimiterWidth == 0 {
			continue
		}
		appendSegment(i)
		i += delimiterWidth - 1
		start = i + 1
	}
	appendSegment(len(command))
	return segments
}

func codexOutputText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var item struct {
		Text     string `json:"text"`
		Output   string `json:"output"`
		ExitCode *int   `json:"exit_code"`
	}
	if json.Unmarshal(raw, &item) == nil {
		return codexOutputItemText(item.Text, item.Output, item.ExitCode)
	}
	var items []struct {
		Text     string `json:"text"`
		Output   string `json:"output"`
		ExitCode *int   `json:"exit_code"`
	}
	if json.Unmarshal(raw, &items) != nil {
		return ""
	}
	var parts []string
	for _, entry := range items {
		if value := codexOutputItemText(entry.Text, entry.Output, entry.ExitCode); value != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, "\n")
}

func codexOutputItemText(text, output string, exitCode *int) string {
	value := strings.Join([]string{text, output}, "\n")
	if exitCode != nil {
		value += "\nexit_code=" + strconv.Itoa(*exitCode)
	}
	return value
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
