package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/textutil"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

// Compile-time interface assertions.
var (
	_ agent.TranscriptAnalyzer          = (*CodexAgent)(nil)
	_ agent.TokenCalculator             = (*CodexAgent)(nil)
	_ agent.PromptExtractor             = (*CodexAgent)(nil)
	_ agent.RestoredSessionPathResolver = (*CodexAgent)(nil)
	_ agent.SubagentAwareExtractor      = (*CodexAgent)(nil)
)

// rolloutLine is the top-level JSONL line structure in Codex rollout files.
type rolloutLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"` // "session_meta", "response_item", "event_msg", "turn_context"
	Payload   json.RawMessage `json:"payload"`
}

const rolloutLineTypeResponseItem = "response_item"

// sessionMetaPayload is the payload for type="session_meta" lines.
type sessionMetaPayload struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	// ThreadSource is "user" for a user-initiated session and "subagent" for a
	// spawned subagent's thread. Codex fires a subagent's own UserPromptSubmit/Stop
	// hooks tagged with the PARENT session_id but the CHILD transcript, so this
	// marker lets the turn handlers skip them.
	ThreadSource string `json:"thread_source"`
	// Source is "startup"/"cli"/... (a string) for user sessions, or an object
	// {"subagent":{"thread_spawn":{...}}} for spawned subagents. Used as a fallback
	// subagent signal on older rollouts that predate thread_source.
	Source json.RawMessage `json:"source,omitempty"`
}

// responseItemPayload is the payload for type="response_item" lines.
type responseItemPayload struct {
	Type      string          `json:"type"` // "message", "custom_tool_call", "custom_tool_call_output", "local_shell_call", "function_call", etc.
	Role      string          `json:"role,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     string          `json:"input,omitempty"`     // apply_patch input (plain text, not JSON)
	Arguments string          `json:"arguments,omitempty"` // function_call arguments (JSON-encoded string)
	CallID    string          `json:"call_id,omitempty"`   // links a function_call to its function_call_output
	Output    json.RawMessage `json:"output,omitempty"`    // function_call_output payload (string or object)
	Content   json.RawMessage `json:"content,omitempty"`   // for messages
}

// contentItem is a single content block in a message.
type contentItem struct {
	Type string `json:"type"` // "input_text", "output_text"
	Text string `json:"text"`
}

// eventMsgPayload is the payload for type="event_msg" lines.
type eventMsgPayload struct {
	Type string          `json:"type"` // "token_count", "task_started", "user_message", "agent_message", "task_complete"
	Info json.RawMessage `json:"info,omitempty"`
}

// tokenCountInfo contains token usage data from event_msg.token_count.
type tokenCountInfo struct {
	TotalTokenUsage *tokenUsageData `json:"total_token_usage,omitempty"`
}

// tokenUsageData maps to Codex's token usage fields.
type tokenUsageData struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
	TotalTokens           int `json:"total_tokens"`
}

// Apply-patch envelope verbs Codex uses in tool_input.command — see
// codex-rs/core/src/tools/handlers/apply_patch.rs. Capture group 1 is the
// verb, group 2 is the path.
const (
	applyPatchVerbAdd    = "Add"
	applyPatchVerbUpdate = "Update"
	applyPatchVerbDelete = "Delete"
)

var (
	applyPatchFileRegex = regexp.MustCompile(`\*\*\* (Add|Update|Delete) File: (.+)`)
	applyPatchMoveRegex = regexp.MustCompile(`\*\*\* Move to: (.+)`)
)

// GetTranscriptPosition returns the current line count of a Codex rollout transcript.
func (c *CodexAgent) GetTranscriptPosition(path string) (int, error) {
	if path == "" {
		return 0, nil
	}

	file, err := os.Open(path) //nolint:gosec // Path comes from agent hook input
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to open transcript: %w", err)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	lineCount := 0
	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil {
			if readErr == io.EOF {
				if len(line) > 0 {
					lineCount++
				}
				break
			}
			return 0, fmt.Errorf("failed to read transcript: %w", readErr)
		}
		lineCount++
	}
	return lineCount, nil
}

// ExtractModifiedFilesFromOffset extracts files modified since a given line offset.
func (c *CodexAgent) ExtractModifiedFilesFromOffset(path string, startOffset int) (files []string, currentPosition int, err error) {
	if path == "" {
		return nil, 0, nil
	}

	file, openErr := os.Open(path) //nolint:gosec // Path comes from agent hook input
	if openErr != nil {
		return nil, 0, fmt.Errorf("failed to open transcript: %w", openErr)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	seen := make(map[string]struct{})
	lineNum := 0

	for {
		lineData, readErr := reader.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return nil, 0, fmt.Errorf("failed to read transcript: %w", readErr)
		}

		if len(lineData) > 0 {
			lineNum++
			if lineNum > startOffset {
				for _, f := range extractFilesFromLine(lineData) {
					if _, ok := seen[f]; !ok {
						seen[f] = struct{}{}
						files = append(files, f)
					}
				}
			}
		}

		if readErr == io.EOF {
			break
		}
	}

	return files, lineNum, nil
}

// extractFilesFromLine extracts modified file paths from a single rollout JSONL line.
func extractFilesFromLine(lineData []byte) []string {
	var line rolloutLine
	if json.Unmarshal(lineData, &line) != nil {
		return nil
	}

	if line.Type != rolloutLineTypeResponseItem {
		return nil
	}

	var payload responseItemPayload
	if json.Unmarshal(line.Payload, &payload) != nil {
		return nil
	}

	// apply_patch custom tool calls contain file paths in the input text
	if payload.Type == "custom_tool_call" && payload.Name == "apply_patch" {
		return extractFilesFromApplyPatch(payload.Input)
	}

	return nil
}

// extractFilesFromApplyPatch returns every file path in an apply_patch envelope,
// across Add/Update/Delete entries, deduplicated.
func extractFilesFromApplyPatch(input string) []string {
	added, modified, deleted := classifyApplyPatchPaths(input)
	total := len(added) + len(modified) + len(deleted)
	if total == 0 {
		return nil
	}
	files := make([]string, 0, total)
	files = append(files, added...)
	files = append(files, modified...)
	files = append(files, deleted...)
	return files
}

// classifyApplyPatchPaths splits an apply_patch envelope into added, modified,
// and deleted file paths. The grammar (codex-rs/apply-patch/src/parser.rs)
// supports renames via "*** Update File: old\n*** Move to: new", which we
// reclassify as a Delete on the source path and an Add on the destination.
// Add and Delete are sticky — subsequent Updates on the same path don't
// downgrade them. Each bucket is sorted for deterministic output.
func classifyApplyPatchPaths(input string) (added, modified, deleted []string) {
	bucket := make(map[string]string)
	var lastUpdate string
	for _, line := range strings.Split(input, "\n") {
		if m := applyPatchFileRegex.FindStringSubmatch(line); m != nil {
			verb := m[1]
			path := strings.TrimSpace(m[2])
			if path == "" {
				continue
			}
			if verb == applyPatchVerbUpdate {
				lastUpdate = path
			} else {
				lastUpdate = ""
			}
			if existing, ok := bucket[path]; ok {
				if existing == applyPatchVerbAdd || existing == applyPatchVerbDelete {
					continue
				}
			}
			bucket[path] = verb
			continue
		}
		if m := applyPatchMoveRegex.FindStringSubmatch(line); m != nil {
			target := strings.TrimSpace(m[1])
			if target == "" {
				continue
			}
			if lastUpdate != "" {
				if existing, ok := bucket[lastUpdate]; !ok || (existing != applyPatchVerbAdd && existing != applyPatchVerbDelete) {
					bucket[lastUpdate] = applyPatchVerbDelete
				}
			}
			if existing, ok := bucket[target]; !ok || (existing != applyPatchVerbAdd && existing != applyPatchVerbDelete) {
				bucket[target] = applyPatchVerbAdd
			}
			lastUpdate = ""
		}
	}
	for path, verb := range bucket {
		switch verb {
		case applyPatchVerbAdd:
			added = append(added, path)
		case applyPatchVerbUpdate:
			modified = append(modified, path)
		case applyPatchVerbDelete:
			deleted = append(deleted, path)
		}
	}
	sort.Strings(added)
	sort.Strings(modified)
	sort.Strings(deleted)
	return added, modified, deleted
}

// CalculateTokenUsage computes token usage from the transcript starting at the given line offset.
// Codex reports cumulative total_token_usage, so we compute the delta between the last
// token_count at/before the offset and the last token_count after the offset.
func (c *CodexAgent) CalculateTokenUsage(transcriptData []byte, fromOffset int) (*agent.TokenUsage, error) {
	var baselineUsage *tokenUsageData // last token_count at or before offset
	var lastUsage *tokenUsageData     // last token_count after offset
	apiCalls := 0
	lineNum := 0

	for _, lineData := range splitJSONL(transcriptData) {
		lineNum++

		var line rolloutLine
		if json.Unmarshal(lineData, &line) != nil {
			continue
		}
		if line.Type != "event_msg" {
			continue
		}
		var evt eventMsgPayload
		if json.Unmarshal(line.Payload, &evt) != nil {
			continue
		}
		if evt.Type != "token_count" || len(evt.Info) == 0 {
			continue
		}
		var info tokenCountInfo
		if json.Unmarshal(evt.Info, &info) != nil || info.TotalTokenUsage == nil {
			continue
		}

		if lineNum <= fromOffset {
			baselineUsage = info.TotalTokenUsage
		} else {
			lastUsage = info.TotalTokenUsage
			apiCalls++
		}
	}

	if lastUsage == nil {
		return nil, nil //nolint:nilnil // no usage data found
	}

	// Subtract baseline to get the delta for this checkpoint range
	inputTokens := lastUsage.InputTokens
	cacheReadTokens := lastUsage.CachedInputTokens
	outputTokens := lastUsage.OutputTokens
	if baselineUsage != nil {
		inputTokens -= baselineUsage.InputTokens
		cacheReadTokens -= baselineUsage.CachedInputTokens
		outputTokens -= baselineUsage.OutputTokens
	}

	freshInputTokens := inputTokens - cacheReadTokens
	if freshInputTokens < 0 {
		freshInputTokens = 0
	}

	return &agent.TokenUsage{
		InputTokens:     freshInputTokens,
		CacheReadTokens: cacheReadTokens,
		OutputTokens:    outputTokens,
		APICallCount:    apiCalls,
	}, nil
}

// ExtractAllModifiedFiles returns the deduplicated set of files modified by the
// main session (from fromOffset) plus every spawned subagent. Codex subagents
// have their own rollout files, discovered from the parent's agent-management
// tool calls; each child's apply_patch edits are merged in. subagentsDir (the
// framework's sibling-file hint) is ignored — Codex self-resolves child rollouts
// from CODEX_HOME/sessions.
func (c *CodexAgent) ExtractAllModifiedFiles(transcriptData []byte, fromOffset int, _ string) ([]string, error) {
	seen := make(map[string]struct{})
	var files []string
	add := func(fs []string) {
		for _, f := range fs {
			if _, ok := seen[f]; !ok {
				seen[f] = struct{}{}
				files = append(files, f)
			}
		}
	}

	add(extractFilesFromBytes(transcriptData, fromOffset))
	for _, childData := range c.readSubagentRollouts(transcriptData, fromOffset) {
		add(extractFilesFromBytes(childData, 0))
	}
	return files, nil
}

// CalculateTotalTokenUsage returns the main session's token usage (from
// fromOffset) with each spawned subagent's usage aggregated into SubagentTokens.
func (c *CodexAgent) CalculateTotalTokenUsage(transcriptData []byte, fromOffset int, _ string) (*agent.TokenUsage, error) {
	mainUsage, err := c.CalculateTokenUsage(transcriptData, fromOffset)
	if err != nil {
		return nil, err
	}

	sub := &agent.TokenUsage{}
	children := c.readSubagentRollouts(transcriptData, fromOffset)
	counted := 0
	for _, childData := range children {
		u, uerr := c.CalculateTokenUsage(childData, 0)
		if uerr != nil || u == nil {
			continue
		}
		counted++
		sub.InputTokens += u.InputTokens
		sub.CacheReadTokens += u.CacheReadTokens
		sub.CacheCreationTokens += u.CacheCreationTokens
		sub.OutputTokens += u.OutputTokens
		sub.APICallCount += u.APICallCount
	}
	// Breadcrumb so a silent zero-attribution (e.g. Codex wire-format drift) is
	// visible: how many child rollouts were read vs how many yielded token data.
	if len(children) > 0 {
		logging.Debug(context.Background(), "codex subagent token aggregation",
			slog.Int("rollouts_read", len(children)), slog.Int("with_token_data", counted))
	}
	// No subagents spawned in this checkpoint range: preserve CalculateTokenUsage's
	// semantics exactly, including its nil "no token data" return — don't
	// materialize an empty &TokenUsage{} where the base method returned nil.
	if sub.APICallCount == 0 {
		return mainUsage, nil
	}
	if mainUsage == nil {
		mainUsage = &agent.TokenUsage{}
	}
	mainUsage.SubagentTokens = sub
	return mainUsage, nil
}

// extractFilesFromBytes returns modified file paths from rollout JSONL bytes,
// skipping the first fromOffset lines.
func extractFilesFromBytes(data []byte, fromOffset int) []string {
	seen := make(map[string]struct{})
	var files []string
	lineNum := 0
	for _, lineData := range splitJSONL(data) {
		lineNum++
		if lineNum <= fromOffset {
			continue
		}
		for _, f := range extractFilesFromLine(lineData) {
			if _, ok := seen[f]; !ok {
				seen[f] = struct{}{}
				files = append(files, f)
			}
		}
	}
	return files
}

// readSubagentRollouts returns the raw bytes of every subagent rollout spawned
// in the parent transcript AT OR AFTER fromOffset. Scoping discovery to
// fromOffset is essential: a Codex rollout grows across turns within one file,
// so without it every subagent ever spawned would be re-attributed to every
// later checkpoint. Best-effort: children whose rollout can't be resolved or
// read are skipped (e.g. archived/cleaned up).
//
// Results are memoized per (fromOffset, len(parent)) because the turn-end path
// calls this from both ExtractAllModifiedFiles and CalculateTotalTokenUsage on
// the same agent instance (created per hook invocation, used sequentially), so
// the memo is single-invocation-scoped and needs no synchronization. The len
// component keys distinct turns apart, so a reused instance never gets a stale
// hit.
func (c *CodexAgent) readSubagentRollouts(parent []byte, fromOffset int) [][]byte {
	key := fmt.Sprintf("%d:%d", fromOffset, len(parent))
	if cached, ok := c.subagentRollouts[key]; ok {
		return cached
	}
	out := c.readSubagentRolloutsUncached(parent, fromOffset)
	if c.subagentRollouts == nil {
		c.subagentRollouts = make(map[string][][]byte, 1)
	}
	c.subagentRollouts[key] = out
	return out
}

func (c *CodexAgent) readSubagentRolloutsUncached(parent []byte, fromOffset int) [][]byte {
	ids := extractSpawnedAgentIDs(parent, fromOffset)
	if len(ids) == 0 {
		return nil
	}
	sessionDir, err := c.GetSessionDir("")
	if err != nil || sessionDir == "" {
		return nil
	}
	var out [][]byte
	for _, id := range ids {
		path := findRolloutBySessionID(sessionDir, id)
		if path == "" {
			logging.Debug(context.Background(), "codex subagent rollout not found",
				slog.String("agent_id", id))
			continue
		}
		data, rerr := os.ReadFile(path) //nolint:gosec // path resolved from codex sessions dir keyed by a validated agent id
		if rerr != nil {
			logging.Debug(context.Background(), "codex subagent rollout read failed",
				slog.String("agent_id", id), slog.String("error", rerr.Error()))
			continue
		}
		out = append(out, data)
	}
	if len(ids) != len(out) {
		logging.Debug(context.Background(), "codex subagent rollouts: some unresolved",
			slog.Int("discovered", len(ids)), slog.Int("resolved", len(out)))
	}
	return out
}

// extractSpawnedAgentIDs returns the deduplicated thread-ids of subagents
// SPAWNED in the current checkpoint range (lines after fromOffset). Codex's
// spawn_agent tool returns the new child id in its function_call_output
// ({"agent_id":"...","nickname":"..."}), so discovery keys off spawn outputs —
// not wait_agent/close_agent/resume_agent references. This attributes each child
// exactly once, in the turn it was spawned: a child resumed or re-waited in a
// later turn is not rediscovered, so its cumulative tokens/files are never
// double-counted across turns.
//
// Deliberate trade-off: a child spawned in a PRIOR range and resumed in this one
// is therefore NOT re-attributed here, so any NEW work it does during the resumed
// range is under-counted. We accept this over the alternative (cross-turn
// double-counting, the worse and previously-shipped bug) because: the child's
// work is still captured as its own task checkpoint, resume across checkpoint
// boundaries is uncommon, and exact resume accounting would need per-child
// cross-turn offset state. logResumedOutOfRangeChildren surfaces the case so the
// under-count is observable rather than silent. See AGENT.md.
func extractSpawnedAgentIDs(data []byte, fromOffset int) []string {
	lines := splitJSONL(data)

	// First pass: collect call_ids of spawn_agent calls across the whole
	// transcript, so a spawn call just before the offset still matches its output
	// landing in range.
	spawnCallIDs := make(map[string]struct{})
	for _, lineData := range lines {
		var line rolloutLine
		if json.Unmarshal(lineData, &line) != nil || line.Type != rolloutLineTypeResponseItem {
			continue
		}
		var payload responseItemPayload
		if json.Unmarshal(line.Payload, &payload) != nil {
			continue
		}
		if payload.Type == "function_call" && payload.Name == "spawn_agent" && payload.CallID != "" {
			spawnCallIDs[payload.CallID] = struct{}{}
		}
	}

	// Second pass: a spawn_agent OUTPUT in range carries the new child's agent_id.
	seen := make(map[string]struct{})
	var ids []string
	lineNum := 0
	for _, lineData := range lines {
		lineNum++
		if lineNum <= fromOffset {
			continue
		}
		var line rolloutLine
		if json.Unmarshal(lineData, &line) != nil || line.Type != rolloutLineTypeResponseItem {
			continue
		}
		var payload responseItemPayload
		if json.Unmarshal(line.Payload, &payload) != nil || payload.Type != "function_call_output" {
			continue
		}
		if _, ok := spawnCallIDs[payload.CallID]; !ok {
			continue
		}
		id := agentIDFromSpawnOutput(payload.Output)
		if id == "" || validation.ValidateAgentSessionID(id) != nil {
			// A confirmed spawn_agent output that yields no usable agent_id is a
			// wire-format drift signal (Codex changed the output shape) — surface it
			// rather than silently dropping the child's attribution.
			logging.Debug(context.Background(), "codex spawn_agent output yielded no usable agent_id",
				slog.String("call_id", payload.CallID))
			continue
		}
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	logResumedOutOfRangeChildren(lines, fromOffset, seen)
	return ids
}

// logResumedOutOfRangeChildren emits a debug breadcrumb when a resume_agent call
// in range targets a child that was NOT spawned in range (so it is intentionally
// not re-attributed this turn — the documented resume under-count trade-off).
func logResumedOutOfRangeChildren(lines [][]byte, fromOffset int, spawnedInRange map[string]struct{}) {
	lineNum := 0
	for _, lineData := range lines {
		lineNum++
		if lineNum <= fromOffset {
			continue
		}
		var line rolloutLine
		if json.Unmarshal(lineData, &line) != nil || line.Type != rolloutLineTypeResponseItem {
			continue
		}
		var payload responseItemPayload
		if json.Unmarshal(line.Payload, &payload) != nil || payload.Type != "function_call" || payload.Name != "resume_agent" {
			continue
		}
		var args struct {
			ID     string `json:"id"`
			Target string `json:"target"`
		}
		if json.Unmarshal([]byte(payload.Arguments), &args) != nil {
			continue
		}
		target := args.ID
		if target == "" {
			target = args.Target
		}
		if target == "" || validation.ValidateAgentSessionID(target) != nil {
			continue
		}
		if _, ok := spawnedInRange[target]; !ok {
			logging.Debug(context.Background(), "codex resumed subagent from a prior range; new work under-counted this turn (known trade-off, see AGENT.md)",
				slog.String("agent_id", target))
		}
	}
}

// agentIDFromSpawnOutput extracts the spawned child's agent_id from a spawn_agent
// function_call_output payload. Codex's wire format puts the output in the
// `output` field as a JSON *string* whose contents are JSON
// (`"output":"{\"agent_id\":\"…\",\"nickname\":\"…\"}"`), so the string form is
// the real case; the object form is also accepted defensively. Returns "" for
// anything without an agent_id (e.g. a shell tool's plain-text output).
func agentIDFromSpawnOutput(raw json.RawMessage) string {
	var o struct {
		AgentID string `json:"agent_id"`
	}
	// Object form: "output":{"agent_id":...}
	if json.Unmarshal(raw, &o) == nil && o.AgentID != "" {
		return o.AgentID
	}
	// String form (real Codex): "output":"{\"agent_id\":...}"
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		if json.Unmarshal([]byte(s), &o) == nil {
			return o.AgentID
		}
	}
	return ""
}

// ExtractPrompts returns user prompts from the transcript starting at the given offset.
func (c *CodexAgent) ExtractPrompts(sessionRef string, fromOffset int) ([]string, error) {
	data, err := os.ReadFile(sessionRef) //nolint:gosec // Path comes from agent hook input
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}

	var prompts []string
	lineNum := 0

	for _, lineData := range splitJSONL(data) {
		lineNum++
		if lineNum <= fromOffset {
			continue
		}

		var line rolloutLine
		if json.Unmarshal(lineData, &line) != nil {
			continue
		}

		if line.Type != rolloutLineTypeResponseItem {
			continue
		}

		var payload responseItemPayload
		if json.Unmarshal(line.Payload, &payload) != nil {
			continue
		}

		if payload.Type != "message" || payload.Role != "user" {
			continue
		}

		// Extract text from content items, skipping Codex's system-injected
		// content blocks (environment_context, AGENTS.md, subagent_notification,
		// turn_aborted, ...) — those are not user-authored prompts and would
		// otherwise surface as the "prompt" in `entire status` and commit messages.
		var items []contentItem
		if json.Unmarshal(payload.Content, &items) != nil {
			continue
		}
		for _, item := range items {
			if item.Type != "input_text" {
				continue
			}
			text := strings.TrimSpace(item.Text)
			if text == "" || textutil.IsCodexSyntheticContent(text) {
				continue
			}
			prompts = append(prompts, text)
		}
	}

	return prompts, nil
}

// SanitizePortableTranscript strips encrypted history fragments that cannot be
// replayed when Entire reconstructs a Codex rollout outside its original
// session context.
func SanitizePortableTranscript(data []byte) []byte {
	lines := splitJSONL(data)
	if len(lines) == 0 {
		return data
	}

	sanitized := make([][]byte, 0, len(lines))
	for _, lineData := range lines {
		updated, keep := sanitizeRolloutLine(lineData)
		if !keep {
			continue
		}
		sanitized = append(sanitized, updated)
	}

	if len(sanitized) == 0 {
		return data
	}
	return agent.ReassembleJSONL(sanitized)
}

func sanitizeRestoredTranscript(data []byte) []byte {
	return SanitizePortableTranscript(data)
}

func sanitizeRolloutLine(lineData []byte) ([]byte, bool) {
	var line rolloutLine
	if err := json.Unmarshal(lineData, &line); err != nil {
		return lineData, true
	}
	if line.Type == "compacted" {
		return sanitizeCompactedLine(line)
	}
	if line.Type != rolloutLineTypeResponseItem {
		return lineData, true
	}

	var payload map[string]any
	if err := json.Unmarshal(line.Payload, &payload); err != nil {
		return lineData, true
	}

	itemType, ok := payload["type"].(string)
	if !ok {
		return lineData, true
	}
	switch itemType {
	case "reasoning":
		delete(payload, "encrypted_content")
	case "compaction", "compaction_summary":
		return nil, false
	default:
		return lineData, true
	}

	encodedPayload, err := json.Marshal(payload)
	if err != nil {
		return lineData, true
	}
	line.Payload = encodedPayload

	encodedLine, err := json.Marshal(line)
	if err != nil {
		return lineData, true
	}
	return encodedLine, true
}

func sanitizeCompactedLine(line rolloutLine) ([]byte, bool) {
	var payload map[string]any
	if err := json.Unmarshal(line.Payload, &payload); err != nil {
		return mustMarshalRolloutLine(line), true
	}

	replacementHistory, ok := payload["replacement_history"].([]any)
	if !ok {
		return mustMarshalRolloutLine(line), true
	}

	sanitizedHistory := sanitizeHistoryItems(replacementHistory)
	payload["replacement_history"] = sanitizedHistory

	encodedPayload, err := json.Marshal(payload)
	if err != nil {
		return mustMarshalRolloutLine(line), true
	}
	line.Payload = encodedPayload

	return mustMarshalRolloutLine(line), true
}

func sanitizeHistoryItems(items []any) []any {
	sanitized := make([]any, 0, len(items))
	for _, item := range items {
		itemMap, ok := item.(map[string]any)
		if !ok {
			sanitized = append(sanitized, item)
			continue
		}

		itemType, ok := itemMap["type"].(string)
		if !ok {
			sanitized = append(sanitized, itemMap)
			continue
		}
		switch itemType {
		case "reasoning":
			delete(itemMap, "encrypted_content")
		case "compaction", "compaction_summary":
			continue
		}

		sanitized = append(sanitized, itemMap)
	}
	return sanitized
}

func mustMarshalRolloutLine(line rolloutLine) []byte {
	encodedLine, err := json.Marshal(line)
	if err != nil {
		return nil
	}
	return encodedLine
}

// splitJSONL splits JSONL bytes into individual lines, skipping empty lines.
func splitJSONL(data []byte) [][]byte {
	var lines [][]byte
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) > 0 {
			lines = append(lines, line)
		}
	}
	return lines
}

// isCodexSubagentRollout reports whether the rollout at path is a spawned
// subagent's thread (session_meta.thread_source == "subagent") rather than a
// user-initiated session. Codex fires a subagent's own UserPromptSubmit/Stop
// hooks tagged with the PARENT session_id but the CHILD transcript; those must
// not drive the parent's TurnStart/TurnEnd (the subagent's lifecycle is tracked
// via SubagentStart/SubagentEnd). Best-effort: reads only the first line and
// returns false if the marker can't be determined, preserving normal turn
// handling for user sessions and older rollouts without thread_source.
func isCodexSubagentRollout(path string) bool {
	if path == "" {
		return false
	}
	f, err := os.Open(path) //nolint:gosec // path comes from agent hook input
	if err != nil {
		return false
	}
	defer f.Close()

	line, err := bufio.NewReader(f).ReadBytes('\n')
	if len(line) == 0 && err != nil {
		return false
	}
	var rl rolloutLine
	if json.Unmarshal(line, &rl) != nil || rl.Type != "session_meta" {
		return false
	}
	var meta sessionMetaPayload
	if json.Unmarshal(rl.Payload, &meta) != nil {
		return false
	}
	if meta.ThreadSource == "subagent" {
		return true
	}
	// Fallback for rollouts without thread_source (older Codex, or an async-write
	// race where thread_source isn't flushed yet): a spawned thread's session_meta
	// carries source = {"subagent":{"thread_spawn":{...}}}. A user session's source
	// is a plain string ("startup"/"cli"/...), which fails this object unmarshal.
	return metaSourceIsSubagent(meta.Source)
}

// metaSourceIsSubagent reports whether a session_meta `source` value is the
// nested subagent form ({"subagent":{...}}) rather than a plain string.
func metaSourceIsSubagent(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var s struct {
		Subagent json.RawMessage `json:"subagent"`
	}
	if json.Unmarshal(raw, &s) != nil {
		return false
	}
	return len(s.Subagent) > 0
}

func parseSessionStartTime(data []byte) (time.Time, error) {
	lines := splitJSONL(data)
	if len(lines) == 0 {
		return time.Time{}, errors.New("transcript is empty")
	}

	var line rolloutLine
	if err := json.Unmarshal(lines[0], &line); err != nil {
		return time.Time{}, fmt.Errorf("parse first transcript line: %w", err)
	}
	if line.Type != "session_meta" {
		return time.Time{}, fmt.Errorf("first transcript line is %q, want session_meta", line.Type)
	}

	var meta sessionMetaPayload
	if err := json.Unmarshal(line.Payload, &meta); err != nil {
		return time.Time{}, fmt.Errorf("parse session_meta payload: %w", err)
	}
	if meta.Timestamp == "" {
		return time.Time{}, errors.New("session_meta timestamp is empty")
	}

	startTime, err := time.Parse(time.RFC3339Nano, meta.Timestamp)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse session_meta timestamp %q: %w", meta.Timestamp, err)
	}
	return startTime, nil
}
