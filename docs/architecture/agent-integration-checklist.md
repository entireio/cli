# Agent Integration Checklist

This document provides requirements and a checklist for integrating new AI coding agents with Entire. Use this to validate your implementation is correct and complete.

For step-by-step implementation instructions, code templates, and testing patterns, see the [Agent Implementation Guide](agent-guide.md).

## Core Principle: Full Transcript Storage

Entire stores the **complete session transcript** at every checkpoint, not incremental diffs. This enables:

- Simple rewind: restore the full transcript, agent resumes from that state
- No dependency on previous checkpoints being intact
- Consistent behavior across all checkpoint types (committed, uncommitted)

**Each checkpoint must contain the full session history up to that point.**

## Core Principle: Native Format Preservation

Store transcripts in the **agent's native format**. Any transformation or normalization should only be done to support CLI features (rewind, resume, summarization, file extraction), not for backend or web UI consumption.

**Why:**
- The backend/web UI should handle format differences, not the CLI
- Transforming for downstream consumers couples the CLI to their requirements
- Native formats ensure compatibility with agent's own import/export tools
- Reduces risk of data loss from lossy transformations
- Format changes that break the UI can be fixed with a backend deploy; CLI changes require a full release cycle and user adoption

**Do:**
- Store the raw transcript as the agent produces it
- Parse the native format when CLI features need specific data (e.g., extract file paths for `entire status`)
- Let the backend normalize formats for display

**Don't:**
- Create a "universal transcript format" in the CLI
- Transform logs to match what the web UI expects
- Strip or restructure data to simplify backend processing
- Create intermediate formats (e.g., converting JSON to JSONL for "easier parsing")
- Reconstruct transcripts from events when a canonical export exists

## Integration Checklist

### Transcript Capture

See Guide: [Transcript Format Guide](agent-guide.md#transcript-format-guide), [TranscriptAnalyzer](agent-guide.md#transcriptanalyzer), [TranscriptPreparer](agent-guide.md#transcriptpreparer)

- [ ] **Full transcript on every turn**: At turn-end, capture the complete session transcript, not just events since the last checkpoint
- [ ] **Resumed session handling**: When a user resumes an existing session, the transcript must include all historical messages, not just new ones since the plugin/hook loaded
- [ ] **Use agent's canonical export**: Prefer the agent's native export command (e.g., reading Claude's JSONL file, Gemini's JSON, Cursor's JSONL, Copilot CLI's JSONL, OpenCode's `opencode export` JSON) over manually reconstructing from events
- [ ] **No custom formats**: Store the agent's native format directly in `NativeData` - do not convert between formats (e.g., JSON to JSONL) or create intermediate representations
- [ ] **Graceful degradation**: If the canonical source is unavailable (e.g., agent shutting down), fall back to best-effort capture with clear documentation of limitations

### Session Storage Abstraction

See Guide: [Step 3 - Core Agent Interface](agent-guide.md#step-3-implement-core-agent-interface-youragentgo)

- [ ] **`WriteSession` implementation**: Agent must implement `WriteSession(AgentSession)` to restore sessions
- [ ] **File-based agents** (Claude, Gemini, Cursor, Copilot CLI): Write `NativeData` to `SessionRef` path
- [ ] **Database-backed agents** (OpenCode): Write `NativeData` to file, then import into native storage (the native format should be what the agent's import command expects)
- [ ] **Single format per agent**: Store only the agent's native format in `NativeData` - no separate fields for different representations of the same data

### Hook Events

See Guide: [Step 4 - ParseHookEvent](agent-guide.md#step-4-implement-parsehookevent-lifecyclego), [Event Mapping Reference](agent-guide.md#event-mapping-reference)

Map agent-native hooks to these `EventType` constants (see `agent/event.go`):

- [ ] **TurnStart**: Fire when user submits a prompt (for pre-prompt state capture)
- [ ] **TurnEnd**: Fire when agent finishes responding (for checkpoint creation)
- [ ] **SessionStart**: Fire when a new session begins
- [ ] **SessionEnd**: Fire when session is explicitly ended (optional but recommended)

### Rewind/Resume Support

- [ ] **Rewind restores full state**: After rewind, agent can continue from that point with full context
- [ ] **Resume command**: `FormatResumeCommand()` returns the CLI command to resume a session
- [ ] **Session ID preservation**: Restored sessions maintain original session ID where possible

### Testing

See Guide: [Testing Patterns](agent-guide.md#testing-patterns)

- [ ] **New session**: Create session, multiple turns, verify full transcript at each checkpoint
- [ ] **Resumed session**: Resume existing session, add turns, verify checkpoint includes historical messages
- [ ] **Rewind**: Rewind to earlier checkpoint, verify agent can continue from that state
- [ ] **Agent shutdown**: Verify graceful handling if agent exits during checkpoint
