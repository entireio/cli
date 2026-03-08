# Implement Command

Build the agent Go package using strict E2E-first TDD. Unit tests are written ONLY after all E2E tests pass.

## Prerequisites

- The research command's one-pager at `cmd/entire/cli/agent/$AGENT_PACKAGE/AGENT.md`
- The E2E test runner already added (from `write-tests` command)
- If no one-pager exists, read the agent's docs and ask the user about hook events, transcript format, and config

## Core Principle: E2E-First TDD

1. **E2E tests are the spec.** The existing `ForEachAgent` test scenarios define "working". You implement until they pass.
2. **Watch it fail first.** Every E2E tier starts by running the test and observing the failure. If you haven't seen the failure, you don't understand what needs fixing.
3. **Minimum viable fix.** At each failure, implement only the code needed to make that specific assertion pass. Don't anticipate future tiers.
4. **`/debug-e2e` is your debugger.** When an E2E test fails, use the artifact directory with `/debug-e2e` before guessing at fixes.
5. **No unit tests during Steps 4-13.** Unit tests are written in Step 14 after all E2E tiers pass, using real data from E2E runs as golden fixtures.
6. **Format and lint, don't unit test.** Between E2E tiers, run `mise run fmt && mise run lint` to keep code clean. Any earlier `mise run test` invocations (e.g., in Step 3) are strictly compile-only sanity checks — no `mise run test` between E2E tiers (Steps 4-13).
7. **If you didn't watch it fail, you don't know if it tests the right thing.**

**Do NOT write unit tests during Steps 4-13.** All test writing is consolidated in Step 14.

## Procedure

### Step 1: Read Implementation Guide

Read these files thoroughly before writing any code:

1. `docs/architecture/agent-guide.md` — Authoritative implementation guide with code templates. Read thoroughly.
2. `docs/architecture/agent-integration-checklist.md` — Validation criteria for completeness.
3. `cmd/entire/cli/agent/agent.go` — Read to find the exact `Agent` interface and all optional interfaces.
4. `cmd/entire/cli/agent/event.go` — Read to find `EventType` constants and shared parsing helpers.

### Step 2: Read Reference Implementation

Read `cmd/entire/cli/agent/$AGENT_PACKAGE/AGENT.md` (the one-pager from the research phase) for the agent's hook mechanism, transcript format, and config structure.

Run `Glob("cmd/entire/cli/agent/*/")` to find all existing agent packages. Check the one-pager's "Hook Mechanism" and "Gaps & Limitations" sections to pick the best reference — choose an agent with a similar hook mechanism to your target. Read all `*.go` files (skip `*_test.go` on first pass) in the chosen reference.

### Step 3: Create Bare-Minimum Compiling Package

Create the agent package directory and stub out every required interface method so the project compiles.

```
cmd/entire/cli/agent/$AGENT_PACKAGE/
```

**What to create:**

1. **`${AGENT_PACKAGE}.go`** — Struct definition, `init()` with `agent.Register(agent.AgentName("$AGENT_KEY"), New)`, and stub implementations for every method in the `Agent` interface — refer to `agent.go` from Step 1. Include `HookSupport` methods in `lifecycle.go` and `hooks.go`.
2. **`types.go`** — Hook input struct(s) with JSON tags matching the one-pager's "Hook input (stdin JSON)" section.
3. **`lifecycle.go`** — Stub `ParseHookEvent()` that returns `nil, nil` for all inputs. Use the one-pager's "Hook names" table for the native hook name → Entire EventType mapping.
4. **`hooks.go`** — Stub `InstallHooks()`, `UninstallHooks()`, `AreHooksInstalled()` that return nil/false. Use the one-pager's "Config file" and "Hook registration" sections for the config path and format.
5. **`transcript.go`** — Stub `TranscriptAnalyzer` methods if the one-pager's "Transcript" section indicates the agent supports transcript analysis. Use the one-pager for transcript location and format.

**Wire up blank imports:**

- Ensure the blank import `_ "github.com/entireio/cli/cmd/entire/cli/agent/$AGENT_PACKAGE"` exists in `cmd/entire/cli/hooks_cmd.go`

**Verify compilation (compile-only sanity check, not unit-test-driven development):**

```bash
mise run fmt && mise run lint && mise run test
```

Everything must compile and pass existing tests before proceeding. Fix any issues.

**Commit:** Use `/commit` to commit all files. Skip if no files changed.

**Standing instruction for Steps 4-12:** If you need agent-specific information (hook format, transcript location, config structure), check `AGENT.md` first. If `AGENT.md` doesn't cover what you need, you may search external docs — but always update `AGENT.md` with anything new you discover so future steps don't need to re-search.

### Step 4: E2E Tier 1 — `TestHumanOnlyChangesAndCommits`

This test requires no agent prompts — it only exercises hooks, so it's the fastest feedback loop.

**What it exercises:**
- `InstallHooks()` — real hook installation in the agent's config
- `AreHooksInstalled()` — detection that hooks are present
- `ParseHookEvent()` — at minimum, the event types needed for session start and turn end (see `EventType` constants in `event.go`)
- Basic hook invocation flow (the test calls hooks directly via the CLI)

**Cycle:**

1. Run: `mise run test:e2e --agent $AGENT_SLUG TestHumanOnlyChangesAndCommits`
2. **Watch it fail** — read the failure output carefully
3. If there are artifact dirs, use `/debug-e2e {artifact-dir}` to understand what happened
4. Implement the minimum code to fix the first failure
5. Repeat until the test passes

Run: `mise run fmt && mise run lint`

**Commit:** Use `/commit` to commit all files. Skip if no files changed.

### Step 5: E2E Tier 2 — `TestSingleSessionManualCommit`

The foundational test. This exercises the full agent lifecycle: start session → agent prompt → agent produces files → user commits → session ends.

**What it exercises:**
- Complete `ParseHookEvent()` for all lifecycle event types from `event.go`. Use the one-pager's hook mapping table to translate native hook names to `EventType` constants.
- `GetSessionDir` / `ResolveSessionFile` — finding the agent's session/transcript files
- `ReadTranscript` / `ChunkTranscript` / `ReassembleTranscript` — reading native transcript format
- `TranscriptAnalyzer` methods (see `agent.go` for current method signatures)

**Cycle:**

1. Run: `mise run test:e2e --agent $AGENT_SLUG TestSingleSessionManualCommit`
2. **Watch it fail** — read the failure output carefully
3. Use `/debug-e2e {artifact-dir}` to understand what happened
4. Implement the minimum code to fix the first failure
5. Repeat until the test passes

Run: `mise run fmt && mise run lint`

**Commit:** Use `/commit` to commit all files. Skip if no files changed.

### Step 6: E2E Tier 2b — `TestCheckpointMetadataDeepValidation`

Validates transcript quality: JSONL validity, content hash correctness, prompt extraction accuracy.

**What it exercises:**
- Transcript content stored at checkpoints is valid JSONL
- Content hash matches the stored transcript
- User prompts are correctly extracted
- Metadata fields are populated

**Cycle:**

1. Run: `mise run test:e2e --agent $AGENT_SLUG TestCheckpointMetadataDeepValidation`
2. **Watch it fail** — this test often exposes subtle transcript formatting bugs
3. Use `/debug-e2e {artifact-dir}` on any failures
4. Fix and repeat

Run: `mise run fmt && mise run lint`

**Commit:** Use `/commit` to commit all files. Skip if no files changed.

### Step 7: E2E Tier 3 — `TestSingleSessionAgentCommitInTurn`

Agent creates files and commits them within a single prompt turn. Tests the in-turn commit path.

**What it exercises:**
- Hook events firing during an agent's commit (post-commit hooks while agent is active)
- Checkpoint creation when agent commits mid-turn
- Usually no new agent-specific code needed — this tests the strategy's handling of agent commits

**Cycle:**

1. Run: `mise run test:e2e --agent $AGENT_SLUG TestSingleSessionAgentCommitInTurn`
2. **Watch it fail** — use `/debug-e2e {artifact-dir}` on failures
3. Fix and repeat — if the agent doesn't support committing, skip this test

Run: `mise run fmt && mise run lint`

**Commit:** Use `/commit` to commit all files. Skip if no files changed.

### Step 8: E2E Tier 4 — Multi-Session Tests

Run these tests to validate multi-session behavior:

- `TestMultiSessionManualCommit` — Two sessions, both produce files, user commits
- `TestMultiSessionSequential` — Sessions run one after another
- `TestEndedSessionUserCommitsAfterExit` — User commits after session ends

**Cycle (for each test):**

1. Run: `mise run test:e2e --agent $AGENT_SLUG TestMultiSessionManualCommit`
2. **Watch it fail** — use `/debug-e2e {artifact-dir}` on failures
3. Fix and repeat
4. Move to next test

These tests rarely need new agent code — they exercise the strategy layer.

Run: `mise run fmt && mise run lint`

**Commit:** Use `/commit` to commit all files. Skip if no files changed.

### Step 9: E2E Tier 5 — File Operation Edge Cases

Run these tests for file operation correctness:

- `TestModifyExistingTrackedFile` — Agent modifies (not creates) a file
- `TestUserSplitsAgentChanges` — User stages only some of the agent's changes
- `TestDeletedFilesCommitDeletion` — Agent deletes a file, user commits the deletion
- `TestMixedNewAndModifiedFiles` — Agent both creates and modifies files

**Cycle:** Same as above — run each test, **watch it fail**, use `/debug-e2e` on failures, fix, repeat.

Run: `mise run fmt && mise run lint`

**Commit:** Use `/commit` to commit all files. Skip if no files changed.

### Step 10: Optional Interfaces

Read `cmd/entire/cli/agent/agent.go` for all optional interfaces. For each one the one-pager's "Gaps & Limitations" or "Transcript" sections suggest is feasible:

- **`TranscriptPreparer`** — If the agent needs pre-processing before transcript storage
- **`TokenCalculator`** — If the agent provides token usage data
- **`SubagentAwareExtractor`** — If the agent has subagent/tool-use patterns

For each optional interface:

1. Implement the methods based on `AGENT.md` and reference implementation
2. Run relevant E2E tests to verify integration (e.g., `TestCheckpointMetadataDeepValidation` for transcript methods)

Run: `mise run fmt && mise run lint`

**Commit:** Use `/commit` to commit all files. Skip if no files changed.

### Step 11: E2E Tier 6 — Interactive and Rewind Tests

Run these if the agent supports interactive multi-step sessions:

- `TestInteractiveMultiStep` — Multiple prompts in one session
- `TestRewindPreCommit` — Rewind to a checkpoint before committing
- `TestRewindAfterCommit` — Rewind to a checkpoint after committing
- `TestRewindMultipleFiles` — Rewind with multiple files changed

**Cycle:** Same pattern — run, **watch it fail**, `/debug-e2e` on failures, fix, repeat.

Run: `mise run fmt && mise run lint`

**Commit:** Use `/commit` to commit all files. Skip if no files changed.

### Step 12: E2E Tier 7 — Complex Scenarios

Run the remaining edge case and stress tests:

- `TestPartialCommitStashNewPrompt` — Partial commit, stash, new prompt
- `TestStashSecondPromptUnstashCommitAll` — Stash workflow across prompts
- `TestRapidSequentialCommits` — Multiple commits in quick succession
- `TestAgentContinuesAfterCommit` — Agent keeps working after a commit
- `TestSubagentCommitFlow` — If the agent has subagent support
- `TestSingleSessionSubagentCommitInTurn` — Subagent commits during a turn

**Cycle:** Same pattern — **watch it fail**, fix, repeat. Many of these require no new agent code — they exercise strategy-layer behavior.

Run: `mise run fmt && mise run lint`

**Commit:** Use `/commit` to commit all files. Skip if no files changed.

### Step 13: Full E2E Suite Pass

Run the complete E2E suite for the agent to catch any regressions or tests that were skipped in earlier tiers:

```bash
mise run test:e2e --agent $AGENT_SLUG
```

This runs every `ForEachAgent` test, not just the ones targeted in Steps 4-12.

**Important: E2E tests can be flaky when run all at once.** Do NOT run them in parallel — always use sequential execution. If some tests fail when running the full suite, re-run each failing test individually before investigating:

```bash
mise run test:e2e --agent $AGENT_SLUG TestFailingTestName
```

If a test passes when run individually but fails in the full suite, it's a flaky failure — not a real error. Only investigate failures that reproduce consistently when run in isolation.

Fix any real failures before proceeding — the same cycle applies: read the failure, use `/debug-e2e {artifact-dir}`, implement the minimum fix, re-run.

All E2E tests must pass before writing unit tests.

### Step 14: Write Unit Tests

Now that all E2E tiers pass, write unit tests to lock in behavior. Use real data from E2E runs (captured JSON payloads, transcript snippets, config file contents) as golden fixtures.

**Test files to create:**

1. **`hooks_test.go`** — Test `InstallHooks` (creates config, idempotent), `UninstallHooks` (removes hooks), `AreHooksInstalled` (detects presence). Use a temp directory to avoid touching real config.

2. **`lifecycle_test.go`** — Test `ParseHookEvent` for all event types. Use actual JSON payloads from E2E artifacts or `AGENT.md` examples. Test every `EventType` mapping, nil returns for unknown hook names, pass-through hooks, empty input, and malformed JSON. **Important:** Test against `EventType` constants from `event.go`, not native hook names — the agent's native hook verbs (e.g., "stop") map to normalized EventTypes (e.g., `TurnEnd`).

3. **`types_test.go`** — Test hook input struct parsing with actual JSON payloads from E2E artifacts or `AGENT.md` examples.

4. **`transcript_test.go`** — Test `ReadTranscript`, `ChunkTranscript`, `ReassembleTranscript` with sample data in the agent's native format. Test all `TranscriptAnalyzer` methods (from `agent.go`) if implemented. Use transcript snippets from E2E artifact directories as golden test data.

5. **`${AGENT_PACKAGE}_test.go`** — Test agent constructor (`New`), `Name()`, `AgentName()`, and any other agent-level methods. Verify the agent satisfies all expected interfaces using compile-time checks (`var _ agent.Agent = (*${AgentType})(nil)`).

**Where to find golden test data:**

- E2E artifact directories contain captured transcripts, hook payloads, and config files
- `AGENT.md` has example JSON payloads in the "Hook input" sections
- The agent's actual config file format from E2E test repos

Run: `mise run fmt && mise run lint && mise run test`

**Commit:** Use `/commit` to commit all files. Skip if no files changed.

### Step 15: Verify Registration

Verify that registration from Step 3 is correct and complete:

1. The `init()` function in `${AGENT_PACKAGE}.go` calls `agent.Register(agent.AgentName("$AGENT_KEY"), New)`
2. The blank import in `cmd/entire/cli/hooks_cmd.go` is present
3. Run the full test suite: `mise run test:ci`

### Step 16: Final Validation

Run the complete validation:

```bash
mise run fmt      # Format
mise run lint     # Lint
mise run test:ci  # All tests (unit + integration)
```

Check against the integration checklist (`docs/architecture/agent-integration-checklist.md`):

- [ ] Full transcript stored at every checkpoint
- [ ] Native format preserved
- [ ] All mappable hook events implemented
- [ ] Session storage working
- [ ] Hook installation/uninstallation working
- [ ] Tests pass with `t.Parallel()`

**Commit:** Use `/commit` to commit all files. Skip if no files changed.

## E2E Debugging Protocol

At every E2E failure, follow this protocol:

1. **Read the test output** — the assertion message often tells you exactly what's wrong
2. **Find the artifact directory** — E2E tests save artifacts (logs, transcripts, git state) to a temp dir printed in the output
3. **Run `/debug-e2e {artifact-dir}`** — this skill analyzes artifacts and diagnoses the root cause
4. **Implement the minimum fix** — don't over-engineer; fix only what the test demands
5. **Re-run the failing test** — not the whole suite, just the one test

## Key Patterns to Follow

- **Use `agent.ReadAndParseHookInput[T]`** for parsing hook stdin JSON
- **Use `paths.WorktreeRoot()`** not `os.Getwd()` for git-relative paths
- **Preserve unknown config keys** when modifying agent config files (don't clobber user settings)
- **Use `logging.Debug/Info/Warn/Error`** for internal logging, not `fmt.Print`
- **Keep interface implementations minimal** — only implement what's needed
- **Follow Go idioms** from `.golangci.yml` — check before writing code

## Output

Summarize what was implemented:
- Package directory and files created
- Interfaces implemented (core + optional)
- Hook names registered
- E2E tiers passing (list which E2E tests pass)
- Unit test coverage (number of test functions, what they cover — written in Step 14)
- Any gaps or TODOs remaining
- Commands to run full validation
