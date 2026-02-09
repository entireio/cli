---
name: test-repo
description: Use this skill to test strategy changes against a fresh test repository. Invoke when the user asks to "test against a test repo", "validate the changes", or wants to verify session hooks, commits, and rewind functionality work correctly.
---

# Test Repository Skill

This skill validates the CLI's session management and rewind functionality by running an end-to-end test against a fresh temporary repository.

## When to Use

- User asks to "test against a test repo"
- User wants to validate strategy changes (manual-commit, auto-commit, shadow, dual)
- User asks to verify session hooks, commits, or rewind functionality
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
- Interactive exploration of checkpoint/rewind workflow
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
export STRATEGY=manual-commit  # or auto-commit, shadow, dual
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
.claude/skills/test-repo/test-harness.sh list-rewind-points
```

Expected results by strategy:

| Check | manual-commit/shadow | auto-commit/dual |
|-------|---------------------|------------------|
| Active branch | No Entire-* trailers | Entire-Checkpoint: trailer only |
| Session state | ✓ Exists | ✗ Not used |
| Shadow branch | ✓ entire/{hash} | ✗ None |
| Metadata branch | ✓ entire/checkpoints/v1 | ✓ entire/checkpoints/v1 |
| Rewind points | ✓ At least 1 | ✓ At least 1 |

#### 4. Test Rewind

```bash
.claude/skills/test-repo/test-harness.sh create-changes
.claude/skills/test-repo/test-harness.sh list-rewind-points  # Get checkpoint ID from output
.claude/skills/test-repo/test-harness.sh rewind <checkpoint-id>
.claude/skills/test-repo/test-harness.sh verify-rewind
```

**Expected Behavior:**
- **Manual-commit/shadow**: Shows warning listing untracked files that will be deleted (files created after the checkpoint that weren't present at session start)
- **Auto-commit/dual**: No warning (git reset doesn't delete untracked files)

Example warning output (manual-commit):
```
Warning: The following untracked files will be DELETED:
  - extra.js
```

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
.claude/skills/test-repo/test-harness.sh list-rewind-points
```

## Expected Results by Strategy

### Manual-Commit Strategy (default, alias: shadow)
- Active branch commits: **NO modifications** (no commits created by Entire)
- Shadow branches: `entire/<commit-hash[:7]>` created for checkpoints
- Metadata: stored on both shadow branches and `entire/checkpoints/v1` branch (condensed on user commits)
- Rewind: restores files from shadow branch commit tree (no git reset)
  - **Shows preview warning** listing untracked files that will be deleted
  - Preserves untracked files that existed at session start
- AllowsMainBranch: **true** (safe on main/master)

### Auto-Commit Strategy (alias: dual)
- Active branch commits: **clean commits** with only `Entire-Checkpoint: <12-hex-char>` trailer
- Shadow branches: none
- Metadata: stored on orphan `entire/checkpoints/v1` branch at sharded paths
- Rewind: full reset allowed if commit is only on current branch
  - Uses `git reset --hard` which doesn't delete untracked files
  - **No preview warnings** (untracked files are safe)
- AllowsMainBranch: **false** (creates commits on active branch)

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
/tmp/entire-bin rewind --list | jq '.[] | select(.is_task_checkpoint == true)'
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
| Rewind points | PASS/FAIL |
| Rewind restore | PASS/FAIL |

**Overall: PASS/FAIL**

[Any errors or notes]
```
