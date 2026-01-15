# Entire CLI Hooks for Claude Code

This document describes the hooks that Entire installs in Claude Code's `.claude/settings.json` to track AI-assisted development sessions.

## Overview

Entire integrates with Claude Code through six hooks that fire at different points during a session:

| Hook                     | Trigger                        | Purpose                                        |
| ------------------------ | ------------------------------ | ---------------------------------------------- |
| `SessionStart`           | New chat session begins        | Generate and persist Entire session ID         |
| `UserPromptSubmit`       | User submits a prompt          | Capture pre-prompt state, check for conflicts  |
| `Stop`                   | Claude finishes responding     | Create checkpoint with code + metadata         |
| `PreToolUse[Task]`       | Subagent is about to start     | Capture pre-task state, create starting marker |
| `PostToolUse[Task]`      | Subagent finishes              | Create final checkpoint for subagent work      |
| `PostToolUse[TodoWrite]` | Subagent updates its todo list | Create incremental checkpoint if files changed |

## `SessionStart`

- **Command**: `entire hooks claude-code session-start`
- **Handler**: `handleSessionStart()` in `hooks_claudecode_handlers.go:907`

Fires when a new chat session begins in Claude Code.

**What it does:**

1.  **Parse Input**: Reads session details from Claude Code's hook payload (`session_id`, `session_ref`).
2.  **Generate Entire Session ID**: Creates a date-prefixed identifier by combining today's date with the model's session ID (e.g., `2026-01-15-ab310c99-f579-4a12-8b3c-1234567890ab`).
3.  **Persist Session ID**: Writes the Entire session ID to `.entire/current_session`. This file is read by subsequent hooks to maintain session context across hook invocations, even if the session spans midnight (date boundary).

## `UserPromptSubmit`

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

3.  **Initialize Session Strategy**:
    - For strategies that implement `SessionInitializer`, calls `InitializeSession()`.
    - **Manual-commit strategy**: Creates or validates the shadow branch (`entire/<HEAD-hash[:7]>`), saves session state to `.git/entire-sessions/<session-id>.json` with `BaseCommit`, `WorktreePath`, and `AgentType`.
    - Handles shadow branch conflicts (from other worktrees) and session ID conflicts with appropriate error messages and recovery options.

## `Stop`

- **Command**: `entire hooks claude-code stop`
- **Handler**: `commitWithMetadata()` in `hooks_claudecode_handlers.go:288`

Fires when Claude finishes responding. Does **not** fire on user interrupt (Ctrl+C).

**What it does:**

1.  **Parse Transcript**:

    - Reads the JSONL transcript from the path provided by Claude Code.
    - For strategies that track transcript position (auto-commit), reads from `CondensedTranscriptLines` offset to only parse new lines since the last checkpoint.
    - Extracts **modified files** by scanning for Write/Edit tool uses in the transcript.

2.  **Extract and Save Metadata** (to `.entire/metadata/<session-id>/`):

    - `full.jsonl` - Copy of the complete transcript.
    - `prompt.txt` - All user prompts from this interaction, separated by `---`.
    - `summary.txt` - The last assistant message (used as checkpoint summary).
    - `context.md` - Generated context file with commit message, session info, and file lists.

3.  **Compute File Changes**:

    - **Modified files**: Extracted from transcript (Write/Edit tool invocations).
    - **New files**: Compare current untracked files against pre-prompt state to find files created by Claude.
    - **Deleted files**: Query `git status` for tracked files that no longer exist.
    - All paths are normalized relative to repo root (not cwd) to handle Claude running from subdirectories.

4.  **Generate Commit Message**: Derives from the last user prompt, truncated and formatted appropriately.

5.  **Invoke Strategy**:

    - Builds a `SaveContext` with session ID, file lists, metadata paths, and git author info.
    - Calls `strategy.SaveChanges(ctx)` to create the checkpoint.
    - **Manual-commit**: Builds a git tree in-memory and commits to the shadow branch.
    - **Auto-commit**: Creates a commit on the active branch with the `Entire-Checkpoint` trailer.

6.  **Update Session State** (auto-commit only): Updates `CondensedTranscriptLines` to track transcript position for incremental parsing.

7.  **Cleanup**: Deletes the temporary `.entire/tmp/pre-prompt-<session-id>.json` file.

## `PreToolUse[Task]`

- **Command**: `entire hooks claude-code pre-task`
- **Handler**: `handlePreTask()` in `hooks_claudecode_handlers.go:668`

Fires just before a subagent (Task tool) begins execution. Creates a clear starting point for the subagent's work.

**What it does:**

1.  **Parse Input**: Extracts `tool_use_id`, `session_id`, `transcript_path`, and `tool_input` from the hook payload.

2.  **Extract Subagent Info**: Parses `tool_input` to get:

    - `subagent_type` - The type of subagent (e.g., "Explore", "reviewer", "dev").
    - `description` - The task description (e.g., "Find authentication files").

3.  **Capture Pre-Task State**:

    - Runs `git status` to get current untracked files.
    - Saves to `.entire/tmp/pre-task-<tool-use-id>.json`.
    - This baseline is used by `PostToolUse[Task]` to determine which files the subagent created.

4.  **Create Starting Checkpoint**:
    - Calls `strategy.SaveTaskCheckpoint()` with `IncrementalSequence: 0` and `IncrementalType: "task_start"`.
    - Creates a commit with no file changes, just metadata marking "Starting: \<description\>".
    - This allows rewinding to the exact state before the subagent made any changes.
    - Includes subagent type and description in commit metadata for better rewind UX.

## `PostToolUse[Task]`

- **Command**: `entire hooks claude-code post-task`
- **Handler**: `handlePostTask()` in `hooks_claudecode_handlers.go:770`

Fires after a subagent finishes its work. Creates the final checkpoint for the subagent's task.

**What it does:**

1.  **Parse Input**: Extracts `tool_use_id`, `agent_id` (from `tool_response.agentId`), `session_id`, `transcript_path`, and `tool_input`.

2.  **Locate Subagent Transcript**:

    - Constructs path: `<transcript_dir>/agent-<agent_id>.jsonl`.
    - If the subagent transcript exists, uses it for file extraction; otherwise falls back to main transcript.

3.  **Extract Modified Files**: Parses the transcript (subagent or main) to find Write/Edit tool invocations.

4.  **Compute New Files**:

    - Loads pre-task state from `.entire/tmp/pre-task-<tool-use-id>.json`.
    - Compares current untracked files against the pre-task snapshot.
    - Files that are now untracked but weren't before = files created by the subagent.

5.  **Find Checkpoint UUID**: Scans the main transcript for any checkpoint UUID associated with this `tool_use_id` (used for rewind linking).

6.  **Save Final Checkpoint**:

    - Builds `TaskCheckpointContext` with all file changes, transcript paths, subagent info, and checkpoint UUID.
    - Calls `strategy.SaveTaskCheckpoint(ctx)`.
    - Creates a commit with the subagent's file changes and metadata including the subagent transcript.

7.  **Cleanup**: Deletes `.entire/tmp/pre-task-<tool-use-id>.json`.

## `PostToolUse[TodoWrite]`

- **Command**: `entire hooks claude-code post-todo`
- **Handler**: `handlePostTodo()` in `hooks_claudecode_handlers.go:550`

Fires whenever a subagent updates its todo list. Enables fine-grained, incremental checkpointing _during_ subagent execution.

**What it does:**

1.  **Subagent Context Check**:

    - Looks for an active pre-task file (`.entire/tmp/pre-task-*.json`).
    - If no pre-task file exists, this is a main agent `TodoWrite` - skip silently.
    - This ensures incremental checkpoints only happen inside subagent Task tool invocations.

2.  **Detect File Changes**:

    - Calls `DetectChangedFiles()` to check for modifications since the last checkpoint.
    - Compares against git worktree status for modified, new, and deleted files.

3.  **Skip if No Changes**: If no files have changed, logs a message and returns without creating a checkpoint.

4.  **Extract Todo Content**:

    - Parses the `tool_input.todos` array from the hook payload.
    - Looks for the **last completed** todo item - this represents the work just finished.
    - If no completed items (first TodoWrite), uses "Planning: N todos" format.
    - This becomes the checkpoint description (e.g., "Completed: Add user authentication endpoint").

5.  **Get Checkpoint Sequence**: Calls `GetNextCheckpointSequence()` to get an incrementing number for this task.

6.  **Save Incremental Checkpoint**:
    - Builds `TaskCheckpointContext` with `IsIncremental: true`, sequence number, and todo content.
    - Calls `strategy.SaveTaskCheckpoint(ctx)`.
    - Creates a small commit capturing the incremental progress.

**Example sequence during a subagent Task:**

```
PreToolUse[Task]      → Checkpoint #0: "Starting: Implement auth feature"
PostToolUse[TodoWrite] → Checkpoint #1: "Planning: 5 todos"
PostToolUse[TodoWrite] → Checkpoint #2: "Completed: Create user model"
PostToolUse[TodoWrite] → Checkpoint #3: "Completed: Add login endpoint"
PostToolUse[Task]      → Checkpoint #4: Final checkpoint with all changes
```
