package agent

// ToolInvocation is one tool call recorded in a transcript, reduced to the
// fields callers ask operational questions about ("did this session run X?").
// Content-free by construction: no prompts, no file contents, no tool output.
// Command is the raw shell command, which is user content — callers must treat
// it as something to match against, never as something to store or transmit.
type ToolInvocation struct {
	// Tool is the agent-native tool name, e.g. "Bash", "Agent", "Task".
	Tool string
	// Command is the shell command for shell tools, empty otherwise.
	Command string
	// SubagentType is the dispatched subagent's name for subagent-spawning
	// tools, empty otherwise.
	SubagentType string
}

// ToolInvocationScanner is implemented by agents whose transcript records tool
// calls structurally, so a caller can ask "did this session invoke X" instead
// of substring-probing the transcript and matching every mention of X.
//
// An agent with no walker here MUST NOT implement this. Not implementing it is
// the honest answer, and the dispatcher below turns it into a reportable
// "cannot tell": callers are required to distinguish that from "did not run",
// because a fabricated "did not run" is indistinguishable from a real one in
// aggregate.
//
// "No walker yet" is the only reason any agent is absent — do not read it as
// "impossible". Cursor in particular shares this JSONL shape (see the
// transcript package doc) and does record tool_use blocks; the "contains no
// tool_use blocks" comments in cmd/entire/cli/agent/cursor date to 2026-03,
// are pinned by no test, and are stale. What blocks a Cursor implementation is
// narrower: its input key for a shell command is unconfirmed, so reusing
// ToolInvocation.Command would risk the very false negative this interface
// exists to prevent. A name-based matcher (subagent dispatch) would work today.
type ToolInvocationScanner interface {
	// ScanToolInvocations calls visit for each recorded tool invocation and
	// returns true as soon as visit does, stopping the walk.
	//
	// hints is a PERFORMANCE contract, not a semantic filter: an implementation
	// may skip parsing any transcript line whose raw bytes contain none of the
	// hints. A caller must therefore pass hints that every invocation it can
	// possibly match is guaranteed to contain literally, or pass nil to visit
	// everything. Passing a hint that its own matcher can accept without is a
	// silent false negative, so callers should pin the relationship with a test.
	ScanToolInvocations(transcriptData []byte, hints [][]byte, visit func(ToolInvocation) bool) bool
}

// ScanToolInvocations walks ag's recorded tool invocations, returning whether
// visit accepted one and whether the agent's transcript format can be walked
// at all.
//
// supported is false when ag is nil, the transcript is empty, or the agent does
// not implement ToolInvocationScanner. Callers MUST NOT collapse
// (found=false, supported=false) into "did not invoke" — seeing nothing because
// there was nothing to see is a different fact from seeing nothing because we
// cannot look, and a metric that conflates them reports a confident number over
// a population it silently cannot measure.
func ScanToolInvocations(ag Agent, transcriptData []byte, hints [][]byte, visit func(ToolInvocation) bool) (found, supported bool) {
	if ag == nil || len(transcriptData) == 0 {
		return false, false
	}
	scanner, ok := AsToolInvocationScanner(ag)
	if !ok {
		return false, false
	}
	return scanner.ScanToolInvocations(transcriptData, hints, visit), true
}
