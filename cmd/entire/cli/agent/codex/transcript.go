package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Compile-time interface assertions.
var (
	_ agent.TranscriptAnalyzer          = (*CodexAgent)(nil)
	_ agent.TokenCalculator             = (*CodexAgent)(nil)
	_ agent.InventoryAwareExtractor     = (*CodexAgent)(nil)
	_ agent.PromptExtractor             = (*CodexAgent)(nil)
	_ agent.RestoredSessionPathResolver = (*CodexAgent)(nil)
	_ agent.TranscriptSanitizer         = (*CodexAgent)(nil)
)

func sessionMetaID(data []byte) (string, error) {
	lines := splitJSONL(data)
	if len(lines) == 0 {
		return "", errors.New("rollout is empty")
	}
	var line rolloutLine
	if err := json.Unmarshal(lines[0], &line); err != nil {
		return "", fmt.Errorf("parse first rollout record: %w", err)
	}
	if line.Type != rolloutLineTypeSessionMeta {
		return "", fmt.Errorf("first transcript line is %q, want session_meta", line.Type)
	}
	var meta sessionMetaPayload
	if err := json.Unmarshal(line.Payload, &meta); err != nil {
		return "", fmt.Errorf("parse session_meta payload: %w", err)
	}
	if meta.ID == "" {
		return "", errors.New("session_meta id is empty")
	}
	return meta.ID, nil
}

// rolloutLine is the top-level JSONL line structure in Codex rollout files.
type rolloutLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"` // "session_meta", "response_item", "event_msg", "turn_context"
	Payload   json.RawMessage `json:"payload"`
}

const (
	rolloutLineTypeResponseItem = "response_item"
	rolloutLineTypeSessionMeta  = "session_meta"
	rolloutLineTypeEventMsg     = "event_msg"
	eventMsgTypeTokenCount      = "token_count"
)

// rolloutClassification identifies whether a rollout belongs to a root thread
// or a child thread. Lifecycle hooks mutate the root session, so uncertainty is
// intentionally distinct from root and must not be treated as a root rollout.
type rolloutClassification uint8

const (
	rolloutUnknown rolloutClassification = iota
	rolloutRoot
	rolloutChild
)

type rolloutClassificationIssue string

const (
	rolloutIssueNullPath           rolloutClassificationIssue = "null_transcript_path"
	rolloutIssueUnreadable         rolloutClassificationIssue = "unreadable_transcript"
	rolloutIssueMalformedMetadata  rolloutClassificationIssue = "malformed_session_metadata"
	rolloutIssueUnclassifiedSource rolloutClassificationIssue = "unclassified_source"
)

type rolloutClassificationResult struct {
	Classification rolloutClassification
	Issue          rolloutClassificationIssue
	Detail         string
}

// sessionMetaPayload is the payload for type="session_meta" lines.
type sessionMetaPayload struct {
	ID           string          `json:"id"`
	Timestamp    string          `json:"timestamp"`
	ThreadSource string          `json:"thread_source"`
	Source       json.RawMessage `json:"source"`
}

// classifyRolloutDetailed reads only the rollout's session_meta record. Newer
// Codex rollouts use thread_source; older rollouts use source.
func classifyRolloutDetailed(path string) rolloutClassificationResult {
	if path == "" {
		return rolloutClassificationResult{Classification: rolloutUnknown, Issue: rolloutIssueNullPath}
	}

	file, err := os.Open(path) //nolint:gosec // Path comes from agent hook input
	if err != nil {
		return rolloutClassificationResult{Classification: rolloutUnknown, Issue: rolloutIssueUnreadable, Detail: "open"}
	}
	defer file.Close()

	lineData, err := bufio.NewReader(file).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return rolloutClassificationResult{Classification: rolloutUnknown, Issue: rolloutIssueUnreadable, Detail: "read"}
	}

	var line rolloutLine
	if json.Unmarshal(lineData, &line) != nil {
		return rolloutClassificationResult{Classification: rolloutUnknown, Issue: rolloutIssueMalformedMetadata, Detail: "first_record_json"}
	}
	if line.Type != rolloutLineTypeSessionMeta {
		return rolloutClassificationResult{Classification: rolloutUnknown, Issue: rolloutIssueMalformedMetadata, Detail: "first_record_type"}
	}

	var meta sessionMetaPayload
	if json.Unmarshal(line.Payload, &meta) != nil {
		return rolloutClassificationResult{Classification: rolloutUnknown, Issue: rolloutIssueMalformedMetadata, Detail: "session_meta_payload"}
	}

	switch meta.ThreadSource {
	case "user":
		return rolloutClassificationResult{Classification: rolloutRoot}
	case "subagent":
		return rolloutClassificationResult{Classification: rolloutChild}
	case "":
		// Fall through to the legacy source encoding.
	default:
		return rolloutClassificationResult{
			Classification: rolloutUnknown,
			Issue:          rolloutIssueUnclassifiedSource,
			Detail:         safeRolloutSource(meta.ThreadSource),
		}
	}

	var source string
	if json.Unmarshal(meta.Source, &source) == nil {
		switch source {
		case "startup", "resume", "clear", "compact", "cli", codexExecCommand, "vscode", "mcp":
			return rolloutClassificationResult{Classification: rolloutRoot}
		default:
			return rolloutClassificationResult{
				Classification: rolloutUnknown,
				Issue:          rolloutIssueUnclassifiedSource,
				Detail:         safeRolloutSource(source),
			}
		}
	}

	var structuredSource struct {
		Subagent json.RawMessage `json:"subagent"`
	}
	if json.Unmarshal(meta.Source, &structuredSource) == nil &&
		len(structuredSource.Subagent) > 0 &&
		!bytes.Equal(structuredSource.Subagent, []byte("null")) {
		return rolloutClassificationResult{Classification: rolloutChild}
	}

	return rolloutClassificationResult{Classification: rolloutUnknown, Issue: rolloutIssueUnclassifiedSource, Detail: "missing_or_structured_legacy_source"}
}

func safeRolloutSource(source string) string {
	const maxSourceRunes = 128
	runes := []rune(strings.ToValidUTF8(source, "�"))
	if len(runes) > maxSourceRunes {
		runes = runes[:maxSourceRunes]
	}
	return string(runes)
}

// responseItemPayload is the payload for type="response_item" lines.
type responseItemPayload struct {
	Type    string          `json:"type"` // "message", "custom_tool_call", "custom_tool_call_output", "local_shell_call", "function_call", etc.
	Role    string          `json:"role,omitempty"`
	Name    string          `json:"name,omitempty"`
	Input   string          `json:"input,omitempty"`   // apply_patch input (plain text, not JSON)
	Content json.RawMessage `json:"content,omitempty"` // for messages
}

// contentItem is a single content block in a message.
type contentItem struct {
	Type string `json:"type"` // "input_text", "output_text"
	Text string `json:"text"`
}

// eventMsgPayload is the payload for type="event_msg" lines.
type eventMsgPayload struct {
	Type   string          `json:"type"` // "token_count", "task_started", "user_message", "agent_message", "task_complete"
	TurnID *string         `json:"turn_id,omitempty"`
	Info   json.RawMessage `json:"info,omitempty"`
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

// exactTokenUsageData uses pointers so a native zero is distinguishable from a
// field Codex did not report. It is used for child cumulative snapshots, where
// approximation would turn an incomplete inventory into a misleading total.
type exactTokenUsageData struct {
	InputTokens           *int `json:"input_tokens"`
	CachedInputTokens     *int `json:"cached_input_tokens"`
	OutputTokens          *int `json:"output_tokens"`
	ReasoningOutputTokens *int `json:"reasoning_output_tokens"`
	TotalTokens           *int `json:"total_tokens"`
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
	return extractFilesFromParsedLine(line)
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
		if line.Type != rolloutLineTypeEventMsg {
			continue
		}
		var evt eventMsgPayload
		if json.Unmarshal(line.Payload, &evt) != nil {
			continue
		}
		if evt.Type != eventMsgTypeTokenCount || len(evt.Info) == 0 {
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

type rolloutAnalysis struct {
	ModifiedFiles   []string
	TerminalTurnIDs []string
	ExactTokenUsage *agent.TokenUsage
}

// analyzeRollout extracts every piece of child evidence in one JSONL pass.
// Each evidence channel keeps its own validity: malformed task boundaries
// invalidate terminal turns without discarding file paths already observed,
// while a malformed final token snapshot makes exact usage unavailable.
func analyzeRollout(data []byte, fromOffset int) rolloutAnalysis {
	var result rolloutAnalysis
	terminalValid := true
	openTurn := ""
	seenTurns := make(map[string]struct{})
	seenFiles := make(map[string]struct{})
	var lastTokenInfo json.RawMessage
	foundToken := false

	for index, lineData := range splitJSONL(data) {
		var line rolloutLine
		if json.Unmarshal(lineData, &line) != nil {
			terminalValid = false
			continue
		}
		if index+1 > fromOffset {
			for _, file := range extractFilesFromParsedLine(line) {
				if _, seen := seenFiles[file]; !seen {
					seenFiles[file] = struct{}{}
					result.ModifiedFiles = append(result.ModifiedFiles, file)
				}
			}
		}
		if line.Type != rolloutLineTypeEventMsg {
			continue
		}

		var header struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line.Payload, &header) != nil {
			terminalValid = false
			continue
		}
		if header.Type != eventMsgTypeTokenCount && header.Type != "task_started" && header.Type != "task_complete" {
			continue
		}
		var event eventMsgPayload
		if json.Unmarshal(line.Payload, &event) != nil {
			if header.Type != eventMsgTypeTokenCount {
				terminalValid = false
			}
			continue
		}
		switch header.Type {
		case eventMsgTypeTokenCount:
			foundToken = true
			lastTokenInfo = event.Info
		case "task_started":
			if openTurn != "" || event.TurnID == nil || *event.TurnID == "" {
				terminalValid = false
				continue
			}
			if _, duplicate := seenTurns[*event.TurnID]; duplicate {
				terminalValid = false
				continue
			}
			openTurn = *event.TurnID
		case "task_complete":
			if openTurn == "" || (event.TurnID != nil && (*event.TurnID == "" || *event.TurnID != openTurn)) {
				terminalValid = false
				continue
			}
			result.TerminalTurnIDs = append(result.TerminalTurnIDs, openTurn)
			seenTurns[openTurn] = struct{}{}
			openTurn = ""
		}
	}
	if !terminalValid || openTurn != "" {
		result.TerminalTurnIDs = nil
	}
	if foundToken {
		result.ExactTokenUsage = exactUsageFromInfo(lastTokenInfo)
	}
	return result
}

func extractFilesFromParsedLine(line rolloutLine) []string {
	if line.Type != rolloutLineTypeResponseItem {
		return nil
	}
	var payload responseItemPayload
	if json.Unmarshal(line.Payload, &payload) != nil || payload.Type != "custom_tool_call" || payload.Name != "apply_patch" {
		return nil
	}
	return extractFilesFromApplyPatch(payload.Input)
}

func exactUsageFromInfo(lastInfo json.RawMessage) *agent.TokenUsage {
	if len(lastInfo) == 0 {
		return nil
	}
	var info struct {
		TotalTokenUsage *exactTokenUsageData `json:"total_token_usage"`
	}
	if json.Unmarshal(lastInfo, &info) != nil || info.TotalTokenUsage == nil {
		return nil
	}
	usage := info.TotalTokenUsage
	if usage.InputTokens == nil || usage.CachedInputTokens == nil || usage.OutputTokens == nil {
		return nil
	}
	input, cached, output := *usage.InputTokens, *usage.CachedInputTokens, *usage.OutputTokens
	if input < 0 || cached < 0 || output < 0 || cached > input {
		return nil
	}
	if usage.ReasoningOutputTokens != nil && (*usage.ReasoningOutputTokens < 0 || *usage.ReasoningOutputTokens > output) {
		return nil
	}
	if usage.TotalTokens != nil && (*usage.TotalTokens < 0 || *usage.TotalTokens != input+output) {
		return nil
	}
	return &agent.TokenUsage{InputTokens: input - cached, CacheReadTokens: cached, OutputTokens: output}
}

// ExtractWithSubagentInventory gathers evidence only for refs supplied by the
// caller's authoritative ledger. It never discovers children from transcript
// text, filenames, timestamps, or token-count events.
func (c *CodexAgent) ExtractWithSubagentInventory(ctx context.Context, parent []byte, fromOffset int, refs []agent.SubagentReference) (agent.InventoryExtraction, error) {
	result := agent.InventoryExtraction{ModifiedFiles: analyzeRollout(parent, fromOffset).ModifiedFiles}
	parentUsage, err := c.CalculateTokenUsage(parent, fromOffset)
	if err != nil {
		return result, err
	}
	complete := true
	var childTotal *agent.TokenUsage
	resolved := make([]loadedRollout, len(refs))
	unresolvedIDs := make(map[string]struct{})
	for index, ref := range refs {
		if loaded, ok := c.loadDirectRollout(ctx, ref); ok {
			resolved[index] = loaded
		} else if ref.AgentID != "" {
			unresolvedIDs[ref.AgentID] = struct{}{}
		}
	}
	fallback, fallbackErr := c.scanFallbackRollouts(ctx, unresolvedIDs)
	if fallbackErr != nil {
		fallback = nil
	}
	for index, ref := range refs {
		if resolved[index].Path == "" {
			resolved[index] = fallback[ref.AgentID]
		}
	}
	for index, ref := range refs {
		analysis := agent.SubagentAnalysis{AgentID: ref.AgentID}
		loaded := resolved[index]
		analysis.ResolvedPath = loaded.Path
		if loaded.Path == "" {
			complete = false
			result.Children = append(result.Children, analysis)
			continue
		}
		rollout := analyzeRollout(loaded.Data, 0)
		analysis.ModifiedFiles = rollout.ModifiedFiles
		analysis.TerminalTurnIDs = rollout.TerminalTurnIDs
		analysis.TokenUsage = rollout.ExactTokenUsage
		if analysis.TokenUsage == nil {
			complete = false
		} else {
			childTotal = addExactUsage(childTotal, analysis.TokenUsage)
		}
		result.ModifiedFiles = appendUniqueFiles(result.ModifiedFiles, analysis.ModifiedFiles)
		result.Children = append(result.Children, analysis)
	}
	result.TokenUsage = withChildCoverage(parentUsage, complete)
	if complete && len(refs) > 0 {
		result.TokenUsage.SubagentTokens = childTotal
	}
	return result, nil
}

func withChildCoverage(usage *agent.TokenUsage, complete bool) *agent.TokenUsage {
	if usage == nil {
		return &agent.TokenUsage{SubagentTokensComplete: &complete}
	}
	result := *usage
	result.SubagentTokens = nil
	result.SubagentTokensComplete = &complete
	return &result
}

func appendUniqueFiles(files, additions []string) []string {
	seen := make(map[string]struct{}, len(files)+len(additions))
	for _, file := range files {
		seen[file] = struct{}{}
	}
	for _, file := range additions {
		if _, exists := seen[file]; !exists {
			seen[file] = struct{}{}
			files = append(files, file)
		}
	}
	return files
}

func addExactUsage(total, addition *agent.TokenUsage) *agent.TokenUsage {
	if total == nil {
		cloned := *addition
		return &cloned
	}
	total.InputTokens += addition.InputTokens
	total.CacheReadTokens += addition.CacheReadTokens
	total.OutputTokens += addition.OutputTokens
	return total
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

		// Extract text from content items
		var items []contentItem
		if json.Unmarshal(payload.Content, &items) != nil {
			continue
		}
		for _, item := range items {
			text := strings.TrimSpace(item.Text)
			if text != "" && item.Type == "input_text" {
				prompts = append(prompts, text)
			}
		}
	}

	return prompts, nil
}

// SanitizePortableTranscript strips encrypted history fragments that cannot be
// replayed when Entire reconstructs a Codex rollout outside its original
// session context.
func SanitizePortableTranscript(data []byte) []byte {
	if !mayNeedSanitizing(data) {
		return data
	}

	lines := splitJSONL(data)
	if len(lines) == 0 {
		return data
	}

	sanitized := make([][]byte, 0, len(lines))
	changed := false
	for _, lineData := range lines {
		updated, keep := sanitizeRolloutLine(lineData)
		if !keep {
			changed = true
			continue
		}
		if !bytes.Equal(updated, lineData) {
			changed = true
		}
		sanitized = append(sanitized, updated)
	}

	// Nothing to strip: hand back the original bytes rather than paying the
	// reassembly copy. Callers rely on this being cheap — sanitization is
	// idempotent precisely so every storage path can call it without tracking
	// whether an upstream path already did.
	if !changed {
		return data
	}
	if len(sanitized) == 0 {
		return data
	}
	return agent.ReassembleJSONL(sanitized)
}

// sanitizeMarkers are the substrings that gate every transformation
// sanitizeRolloutLine performs: dropping "compaction"/"compaction_summary" items,
// rewriting "compacted" lines, and deleting "encrypted_content" from "reasoning"
// items. A transcript containing none of them cannot be altered, so one scan lets
// us skip unmarshalling every line.
//
// Deliberately over-broad ("compact" covers compacted/compaction/
// compaction_summary): a false positive just falls through to the full pass, while
// a false negative would silently skip sanitization.
var sanitizeMarkers = [][]byte{
	[]byte("encrypted_content"),
	[]byte("compact"),
	[]byte("reasoning"),
}

func mayNeedSanitizing(data []byte) bool {
	for _, marker := range sanitizeMarkers {
		if bytes.Contains(data, marker) {
			return true
		}
	}
	return false
}

// SanitizeTranscriptForStorage implements agent.TranscriptSanitizer. Codex rollouts
// embed encrypted reasoning payloads and compaction blobs that are bound to the
// originating session, so Entire strips them from its stored copy while leaving
// Codex's own rollout file untouched.
func (c *CodexAgent) SanitizeTranscriptForStorage(data []byte) []byte {
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
	case "reasoning", "compaction", "compaction_summary":
		// Strip the non-replayable payload but KEEP the line. Dropping these lines
		// (as this used to) shortened the stored transcript relative to the agent's
		// rollout, while CheckpointTranscriptStart is counted on the rollout — so
		// every offset into a stored Codex transcript was off by the number of
		// dropped lines before it. Stripping in place keeps the two line numberings
		// identical, which is what the offset's five consumers assume.
		//
		// Nested compaction items inside a "compacted" line's replacement_history
		// are still removed outright (see sanitizeHistoryItems): those are array
		// elements within a single line, so removing them cannot shift line numbers.
		delete(payload, "encrypted_content")
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

func parseSessionStartTime(data []byte) (time.Time, error) {
	lines := splitJSONL(data)
	if len(lines) == 0 {
		return time.Time{}, errors.New("transcript is empty")
	}

	var line rolloutLine
	if err := json.Unmarshal(lines[0], &line); err != nil {
		return time.Time{}, fmt.Errorf("parse first transcript line: %w", err)
	}
	if line.Type != rolloutLineTypeSessionMeta {
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
