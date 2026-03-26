# OpenCode CLI Bugs Found During E2E Testing

These are real bugs in `cmd/entire/cli/` exposed by E2E tests running with the opencode agent. They should NOT be worked around in tests.

---

## Bug 1: Mid-turn commits don't get checkpoint trailers

**Tests:** `TestMidTurnCommit_DifferentFilesThanPreviousTurn`, `TestMultiSessionSequential`, `TestAgentAmendsCommit`, `TestRapidSequentialCommits`, `TestAgentCommitsMidTurnUserCommitsRemainder`

**Symptom:** When opencode commits mid-turn, the commit has no `Entire-Checkpoint` trailer. `WaitForCheckpoint` times out because `entire/checkpoints/v1` never advances.

**Root cause:** When the agent commits mid-turn (before the turn-end hook), `state.FilesTouched` is empty because file tracking only happens at turn-end via `SaveStep`. The `prepare-commit-msg` hook calls `sessionHasNewContent()` which checks `stagedFilesOverlapWithContent()` — but with an empty `FilesTouched`, there's no overlap, so no trailer is added.

**Evidence from `entire.log`:**
```json
{"msg":"stagedFilesOverlapWithContent: no overlapping files found","staged_files":2,"files_touched":1}
{"msg":"sessionHasNewContent: staged files overlap check","staged_files":2,"result":false}
{"msg":"prepare-commit-msg: no content to link"}
```

The `staged_files:2` are the new files from turn 2, but `files_touched:1` is from turn 1's session. Turn 2's session has no files tracked yet because turn-end hasn't fired.

**Suspected location:** `manual_commit_hooks.go` around the `sessionHasNewContent` / `stagedFilesOverlapWithContent` logic. The hook needs a way to detect files from the current turn before `SaveStep` runs — either by populating `FilesTouched` at turn-start, or by checking the live transcript/working tree diff.

**Why this is opencode-specific:** Interactive agents (Claude Code, Cursor) reuse the same session across prompts, so `FilesTouched` accumulates across turns. OpenCode creates a new session per `RunPrompt` invocation, so the second session's `FilesTouched` is always empty at commit time.

---

## Bug 2: Shadow branches left orphaned after carry-forward

**Tests:** `TestStashModificationsToTrackedFiles`, `TestEndedSessionUserCommitsAfterExit`, `TestAgentAmendsCommit`

**Symptom:** After all commits and condensation complete, a shadow branch like `entire/7f391e3-e3b0c4` persists. `AssertNoShadowBranches` fails.

**Root cause:** When the post-commit hook condenses and there are remaining uncommitted agent files, `carryForwardToNewShadowBranch()` creates a NEW shadow branch at the current HEAD hash. The OLD shadow branch is deleted. But the carry-forward branch is never cleaned up if:
1. The next commit creates yet another shadow branch (only the newest gets deleted)
2. The session ends without another commit

**Evidence from `entire.log`:**
```json
// First commit condensation
{"msg":"session condensed","checkpoint_id":"e92659dc9ebc"}
{"msg":"shadow branch deleted","shadow_branch":"entire/0bbf4cb-e3b0c4"}
// Carry-forward creates intermediate branch (not visible in log but inferred from hash)

// Second commit condensation
{"msg":"session condensed","checkpoint_id":"9d0c23836aa7"}
{"msg":"shadow branch deleted","shadow_branch":"entire/ab493e5-e3b0c4"}
// But entire/7f391e3-e3b0c4 (the carry-forward branch) was never deleted
```

**Suspected location:** `manual_commit_hooks.go` — the `shadowBranchesToDelete` map in `condenseAndUpdateState()` only tracks branches that get condensed, not intermediate carry-forward branches. The carry-forward function creates a branch but doesn't register it for cleanup.

---

## Bug 3: Agent-internal files included in `files_touched`

**Tests:** `TestCheckpointMetadataDeepValidation`

**Symptom:** Checkpoint metadata `files_touched` contains `.opencode/plugins/entire.ts` and `opencode.json` alongside the actual user file (`validated.go`).

**Evidence from test report:**
```
listA (expected): ["validated.go"]
listB (actual):   [".opencode/plugins/entire.ts", "opencode.json", "validated.go"]
```

**Root cause:** The file detection logic that populates `files_touched` doesn't filter out agent-internal config files. OpenCode creates `.opencode/plugins/entire.ts` and `opencode.json` as part of its session setup. These appear as new/modified files in the working tree and get included in the checkpoint metadata.

**Evidence from `entire.log`:**
```json
{"msg":"files changed during session","modified":2,"new":2,"deleted":0}
{"msg":"post-commit: carry-forward prep","files":[".opencode/plugins/entire.ts","opencode.json","src/a.go","src/b.go"]}
```

**Suspected location:** The checkpoint creation flow in `manual_commit_hooks.go` / `common.go`. The `isProtectedPath()` function exists to filter protected directories during rewind, but this filtering is NOT applied when computing `files_touched`. The `.opencode/` directory should be excluded from `files_touched` the same way `.entire/` is.
