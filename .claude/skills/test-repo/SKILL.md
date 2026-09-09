---
name: test-repo
description: Use this skill to test strategy changes against a fresh test repository. Invoke when the user asks to "test against a test repo", "validate the changes", or wants to verify session hooks, commits, and checkpoint creation work correctly.
---

# Test Repository Skill

This skill validates the CLI's session management and checkpoint creation by running an end-to-end test against a fresh temporary repository.

## When to Use

- User asks to "test against a test repo"
- User wants to validate strategy changes (manual-commit)
- User asks to verify session hooks, commits, or checkpoint creation
- After making changes to strategy code

## Testing Approaches

**Automated Testing (recommended for validation):**
```bash
mise run test:integration
```
Run the comprehensive integration test suite. Best for verifying correctness after code changes.

**Manual Testing (this skill):**
Use the test harness for:
- Debugging specific strategy behaviors
- Interactive exploration of the checkpoint workflow
- Manual verification of edge cases
- Understanding how the system works step-by-step

## Test Procedure

### Setup

**Step 1: Build the CLI**

```bash
go build -o /tmp/entire-bin ./cmd/entire
```

**Step 2: Approve the test harness (one-time)**

Add this pattern to your Claude Code approved commands, or approve it once when prompted:

```json
{
  "approvedBashCommands": [
    ".claude/skills/test-repo/test-harness.sh*"
  ]
}
```

**Optional: Set strategy** (defaults to `manual-commit`):

```bash
export STRATEGY=manual-commit
```

### Test Steps

Execute these steps in order:

#### 1. Setup Test Environment

```bash
.claude/skills/test-repo/test-harness.sh setup-repo
.claude/skills/test-repo/test-harness.sh configure-strategy
```

#### 2. Simulate Session

```bash
.claude/skills/test-repo/test-harness.sh start-session
.claude/skills/test-repo/test-harness.sh create-files
.claude/skills/test-repo/test-harness.sh create-transcript
.claude/skills/test-repo/test-harness.sh stop-session
```

#### 3. Verify Results

```bash
.claude/skills/test-repo/test-harness.sh verify-commit
.claude/skills/test-repo/test-harness.sh verify-session-state
.claude/skills/test-repo/test-harness.sh verify-shadow-branch
.claude/skills/test-repo/test-harness.sh verify-metadata-branch
.claude/skills/test-repo/test-harness.sh list-pending-checkpoints
```

Expected results:

| Check | Result |
|-------|--------|
| Active branch | Optional Entire-Checkpoint: trailer |
| Session state | ✓ Exists |
| Shadow branch | ✓ entire/{hash} |
| Metadata branch | ✓ entire/checkpoints/v1 |
| Pending checkpoints | ✓ At least 1 |

#### 4. Check Listing After Further Changes

```bash
.claude/skills/test-repo/test-harness.sh create-changes
.claude/skills/test-repo/test-harness.sh list-pending-checkpoints
```

**Expected Behavior:**
- The checkpoint from step 2 is still listed; the new uncommitted changes do not
  disturb it. There is no restore step: the CLI has no `rewind` command, and
  nothing writes a checkpoint back over the worktree.

#### 5. Cleanup

```bash
.claude/skills/test-repo/test-harness.sh cleanup
```

### Quick Commands

Show environment info:
```bash
.claude/skills/test-repo/test-harness.sh info
```

Run full test in one go:
```bash
go build -o /tmp/entire-bin ./cmd/entire && \
.claude/skills/test-repo/test-harness.sh setup-repo && \
.claude/skills/test-repo/test-harness.sh configure-strategy && \
.claude/skills/test-repo/test-harness.sh start-session && \
.claude/skills/test-repo/test-harness.sh create-files && \
.claude/skills/test-repo/test-harness.sh create-transcript && \
.claude/skills/test-repo/test-harness.sh stop-session && \
.claude/skills/test-repo/test-harness.sh verify-metadata-branch && \
.claude/skills/test-repo/test-harness.sh list-pending-checkpoints
```

## Expected Results by Strategy

### Manual-Commit Strategy (default)
- Active branch commits: **NO modifications** (no commits created by Entire)
- Shadow branches: `entire/<commit-hash[:7]>` created for checkpoints
- Metadata: stored on both shadow branches and `entire/checkpoints/v1` branch (condensed on user commits)
- AllowsMainBranch: **true** (safe on main/master)

## Additional Testing (Optional)

### Test Subagent Checkpoints

For testing task checkpoints (subagent execution):

```bash
# After user-prompt-submit, simulate a task execution
TOOL_USE_ID="toolu_test123"

# Pre-task hook (before subagent starts)
echo "{\"session_id\": \"$SESSION_ID\", \"transcript_path\": \"$TRANSCRIPT_DIR/transcript.jsonl\", \"tool_use_id\": \"$TOOL_USE_ID\", \"tool_input\": {\"subagent_type\": \"dev\", \"description\": \"Test task\"}}" | \
  ENTIRE_TEST_CLAUDE_PROJECT_DIR="$TRANSCRIPT_DIR" \
  /tmp/entire-bin hooks claude-code pre-task

# Create subagent transcript
mkdir -p "$TRANSCRIPT_DIR/tasks/$TOOL_USE_ID"
echo '{"type":"human","message":{"content":"Test task"}}' > "$TRANSCRIPT_DIR/tasks/$TOOL_USE_ID/agent-test.jsonl"

# Post-task hook (after subagent completes)
echo "{\"session_id\": \"$SESSION_ID\", \"transcript_path\": \"$TRANSCRIPT_DIR/transcript.jsonl\", \"tool_use_id\": \"$TOOL_USE_ID\", \"tool_response\": {\"agentId\": \"test-agent\"}}" | \
  ENTIRE_TEST_CLAUDE_PROJECT_DIR="$TRANSCRIPT_DIR" \
  /tmp/entire-bin hooks claude-code post-task

# Verify task checkpoint created
/tmp/entire-bin checkpoint list --pending --json | jq '.[] | select(.is_task_checkpoint == true)'
```

### Test User Commits (Condensation)

For manual-commit, test log condensation:

```bash
# Create a user commit (triggers post-commit hook)
git add app.js
git commit -m "Add greeting function"

# Verify logs condensed to entire/checkpoints/v1
git show entire/checkpoints/v1 --stat | grep -E "^[0-9a-f]{2}/[0-9a-f]"

# Verify shadow branch still exists
git branch -a | grep "entire/[0-9a-f]"
```

## Available Claude Code Hooks

All hooks use the command: `entire hooks claude-code <hook-name>`

- `user-prompt-submit` - Called when user submits a prompt (before session starts)
- `session-start` - Called when session starts
- `stop` - Called when session stops (creates checkpoint)
- `pre-task` - Called before Task tool execution
- `post-task` - Called after Task tool execution
- `post-todo` - Called after TodoWrite tool execution (for incremental checkpoints)

## Report Format

After running the test, report:

```
## Test Results: [STRATEGY] Strategy

| Step | Result |
|------|--------|
| Build CLI | PASS/FAIL |
| Create repo | PASS/FAIL |
| Session hooks | PASS/FAIL |
| Clean commits | PASS/FAIL |
| Metadata branch | PASS/FAIL |
| Pending checkpoints | PASS/FAIL |

**Overall: PASS/FAIL**

[Any errors or notes]
```
