# Entire CLI Hooks for Claude Code

This document describes the hooks that Entire installs in Claude Code's `.claude/settings.json` to track AI-assisted development sessions.

## Overview

Entire integrates with Claude Code through six hooks that fire at different points during a session:

| Hook                     | Trigger                        | Purpose                                        |
| ------------------------ | ------------------------------ | ---------------------------------------------- |
| `SessionStart`           | New chat session begins        | Generate and persist Entire session ID         |
| `UserPromptSubmit`       | User submits a prompt          | Capture pre-prompt state, check for conflicts  |
| `Stop`                   | Claude finishes responding     | Record the turn end in session state           |
| `PreToolUse[Agent]`      | Subagent is about to start     | Capture pre-task state for diff computation    |
| `PostToolUse[Agent]`     | Subagent finishes              | Complete the subagent's task record            |

> **Tool matcher note.** Claude Code's subagent dispatch tool is `Agent` (there was never a `Task` tool). Older CLI versions installed the subagent hooks under `Task`, where they silently never fired; re-run `entire enable --force` to strip the stale entries and reinstall under the matchers above. See [tools-reference](https://code.claude.com/docs/en/tools-reference.md) and [hooks matcher rules](https://code.claude.com/docs/en/hooks.md).
>
> Older CLI versions also installed a `post-todo` hook under `TodoWrite` and later `TaskCreate|TaskUpdate`, which wrote incremental subagent checkpoints to a shadow branch. It is no longer installed: any install (`entire enable`, with or without `--force`) prunes it as a stale managed hook. The `entire hooks claude-code post-todo` subcommand stays registered and only reads its input, so configs that still carry the hook keep working until then.

### Critical Capabilities

  1. Prompt blocking - UserPromptSubmit hook needs to support returning a response to be shown in the cli session.
    - Claude Code allows us to return a JSON response to stdout that can:
      - Allow continuation: {"continue": true}
      - Block with message: {"continue": false, "stopReason": "Your message here"}
  2. Transcript access - Hooks receive transcript path; system needs to read it for:
    - Extracting user prompts
    - Extracting modified files
    - Generating summaries
  3. Tool use ID tracking - For subagent checkpoints, need unique tool_use_id to correlate PreToolUse and PostToolUse events
  4. Stdin parsing - Hook input comes as JSON on stdin; agent must define its input schema

## Detailed Hook Info

### `SessionStart`

- **Command**: `entire hooks claude-code session-start`
- **Handler**: `handleSessionStart()` in `hooks_claudecode_handlers.go:907`

Fires when a new chat session begins in Claude Code.

**What it does:**

1.  **Parse Input**: Reads session details from Claude Code's hook payload (`session_id`, `session_ref`).
2.  **Generate Entire Session ID**: Creates a date-prefixed identifier by combining today's date with the model's session ID (e.g., `2026-01-15-ab310c99-f579-4a12-8b3c-1234567890ab`).
3.  **Persist Session ID**: Writes the Entire session ID to `.entire/current_session`. This file is read by subsequent hooks to maintain session context across hook invocations, even if the session spans midnight (date boundary).

### `UserPromptSubmit`

- **Command**: `entire hooks claude-code user-prompt-submit`
- **Handler**: `captureInitialState()` in `hooks_claudecode_handlers.go:248`

Fires every time the user submits a prompt. Prepares the repository state tracking _before_ Claude makes any changes.

**What it does:**

1.  **Concurrent Session Check**:

    - Queries the strategy to check if another Entire session has uncommitted checkpoints on the same HEAD commit.
    - If a conflict is found, outputs a JSON response that blocks the prompt and shows a warning to the user.
    - The warning includes the other session's initial prompt (if available) and a resume command.
    - Sets `ConcurrentWarningShown` flag in session state so the user can continue on the next prompt.
    - Once the user commits their changes (or the conflict resolves), the flag is cleared automatically.

2.  **Capture Pre-Prompt State**:

    - Runs `git status` to get a list of all **untracked files** in the repository.
    - Saves this list to `.entire/tmp/pre-prompt-<session-id>.json`.
    - This baseline is compared later (in the `Stop` hook) to determine which files were newly created by Claude.
    - Records the current transcript line count (`StepTranscriptStart`) for incremental token usage calculation.

3.  **Initialize Session Strategy**:
    - For strategies that implement `SessionInitializer`, calls `InitializeSession()`.
    - **Manual-commit strategy**: saves session state to `.git/entire-sessions/<session-id>.json` with `BaseCommit`, `WorktreePath`, and `AgentType`. No branch is created.

### `Stop`

- **Command**: `entire hooks claude-code stop`
- **Handler**: `commitWithMetadata()` in `hooks_claudecode_handlers.go:288`

Fires when Claude finishes responding. Does **not** fire on user interrupt (Ctrl+C).

**What it does:**

1.  **Parse Transcript**:

    - Reads the JSONL transcript from the path provided by Claude Code.
    - Parses the full transcript. `CheckpointTranscriptStart` (or `StepTranscriptStart` from pre-prompt state) is used to detect whether new content exists since the last checkpoint.
    - Extracts **modified files** by scanning for Write/Edit tool uses in the transcript.

2.  **Extract and Save Metadata** (to `.entire/metadata/<session-id>/`):

    - `full.jsonl` - Sanitized copy of the complete transcript (see `agent.TranscriptSanitizer`). The next Stop rewrites it from the agent's own transcript.
    - `prompt.txt` - Checkpoint-scoped user prompts, separated by `---`.

    Both files are a staging buffer for the checkpoint writer, not the durable
    copy. The durable copy is written to the checkpoint store
    (`entire/checkpoints/v1` or per-checkpoint refs) when the work is
    committed. `clearFilesystemStagedFiles` releases both (plus a legacy
    `full.log`) once the session's work is condensed and no carry-forward
    files remain.

3.  **Compute File Changes**:

    - **Modified files**: Extracted from transcript (Write/Edit tool invocations).
    - **New files**: Compare current untracked files against pre-prompt state to find files created by Claude.
    - **Deleted files**: Query `git status` for tracked files that no longer exist.
    - All paths are normalized relative to repo root (not cwd) to handle Claude running from subdirectories.

4.  **Generate Commit Message**: Derives from the last user prompt, truncated and formatted appropriately.

5.  **Calculate Token Usage**:

    - Parses the transcript from `StepTranscriptStart` (captured at prompt start) to calculate tokens used in this turn.
    - Extracts token counts from assistant messages: input tokens, cache creation/read tokens, output tokens.
    - Deduplicates by message ID (streaming creates multiple rows per message; uses highest output_tokens).
    - Finds spawned subagents by scanning for `agentId:` in Task tool results.
    - Calculates subagent token usage from their transcript files under
      `paths.SubagentsDir`.
    - Aggregates into a `TokenUsage` struct with nested `SubagentTokens`.

6.  **Invoke Strategy**:

    - Builds a `StepContext` with session ID, file lists, metadata paths, and token usage.
    - Calls `ManualCommitStrategy.SaveStep`, which records the step in session state (step count, files touched, and each touched file's blob hash). No git objects are written; the checkpoint is written at commit time.
    - Token usage accumulates on session state and is stored in the checkpoint's `metadata.json` at condensation.

7.  **Update Session State**: Updates `CheckpointTranscriptStart` to track transcript position for detecting new content in future checkpoints.

8.  **Cleanup**: Deletes the temporary `.entire/tmp/pre-prompt-<session-id>.json` file.

### `PreToolUse[Agent]`

- **Command**: `entire hooks claude-code pre-task`
- **Handler**: `handlePreTask()` in `hooks_claudecode_handlers.go:668`

Fires just before a subagent (Agent tool) begins execution. Captures the current state so that file changes can be computed when the task completes.

**What it does:**

1.  **Parse Input**: Extracts `tool_use_id`, `session_id`, `transcript_path`, and `tool_input` from the hook payload.

2.  **Extract Subagent Info**: Parses `tool_input` to get:

    - `subagent_type` - The type of subagent (e.g., "Explore", "reviewer", "dev").
    - `description` - The task description (e.g., "Find authentication files").

3.  **Capture Pre-Task State**:

    - Runs `git status` to get current untracked files.
    - Saves to `.entire/tmp/pre-task-<tool-use-id>.json`.
    - This baseline is used by `PostToolUse[Agent]` to determine which files the subagent created.
    - **Note**: No checkpoint/commit is created at this stage. Nothing is written to git during a subagent run; its work reaches a checkpoint when the parent session is condensed at commit time.

### `PostToolUse[Agent]`

- **Command**: `entire hooks claude-code post-task`
- **Handler**: `handlePostTask()` in `hooks_claudecode_handlers.go:770`

Fires after a subagent finishes its work. Completes the subagent's task record on session state; the record is materialized into the parent session's checkpoint at the next condensation.

**What it does:**

1.  **Parse Input**: Extracts `tool_use_id`, `agent_id` (from `tool_response.agentId`), `session_id`, `transcript_path`, and `tool_input`.

2.  **Locate Subagent Transcript** — `ResolveAgentTranscriptPath`, which prefers
    `paths.SubagentsDir` (`<transcript_dir>/<session_id>/subagents/agent-<agent_id>.jsonl`,
    where Claude Code writes it today, alongside an unused-by-Entire
    `agent-<agent_id>.meta.json` sidecar) and falls back to the legacy sibling
    `<transcript_dir>/agent-<agent_id>.jsonl`. See that function for why the order
    matters. If it resolves, it is used for file extraction; otherwise extraction
    falls back to the main transcript, where a subagent's Write/Edit calls do not appear.

3.  **Extract Modified Files**: Parses the transcript (subagent or main) to find Write/Edit tool invocations.

4.  **Compute New Files**:

    - Loads pre-task state from `.entire/tmp/pre-task-<tool-use-id>.json`.
    - Compares current untracked files against the pre-task snapshot.
    - Files that are now untracked but weren't before = files created by the subagent.

5.  **Find Checkpoint UUID**: Scans the main transcript for any checkpoint UUID associated with this `tool_use_id` (used for rewind linking).

6.  **Complete the Task Record**: `strategy.CompleteTaskRecord` attaches the files, declared transcript path, and token usage to the session's `TaskRecord` exactly once and merges the files into `FilesTouched` (without hashes, so they are matched by name at commit time). No git objects are written.

7.  **Cleanup**: Deletes `.entire/tmp/pre-task-<tool-use-id>.json`.

