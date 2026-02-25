# PI Integration Completion Plan

This plan finalizes PI integration on the new lifecycle architecture while preserving PI-specific behavior and minimizing non-PI impact.

## Goals

- Keep PI on the same lifecycle contract as Claude/Gemini: `ParseHookEvent() -> Event -> DispatchLifecycleEvent()`.
- Preserve PI-specific transcript behavior (tree/leaf-aware parsing, compaction-aware context, branch handling).
- Keep changes PI-scoped whenever possible; allow only minimal core seams when PI parity cannot be achieved otherwise.
- Avoid deleting or refactoring unrelated non-PI code.

## Non-Goals

- No rewrite of hook transport to `--input <file>` in this scope.
- No broad cleanup outside PI-owned paths unless required to unblock PI correctness.
- No changes to non-PI agent behavior.

## Constraints

- PI hook integration remains extension-based (`.pi/extensions/entire/index.ts`), not JSON settings-based.
- `GetHookConfigPath()` remains empty for PI.
- Keep uninstall behavior safe: remove only Entire-managed scaffold, preserve user-managed extensions.

## Integration Policy (Hard Requirement)

- [x] Treat PI as an integration, not a core architecture rewrite.
- [x] Default all work to PI-owned files first.
- [x] Keep non-PI behavior unchanged unless explicitly required for PI correctness.
- [x] Allowed core touchpoints are narrowly scoped to lifecycle leaf propagation and condensation/finalization leaf usage.
- [ ] Any additional core changes outside approved touchpoints require explicit sign-off before implementation.

## Strict Checklist Status ([x]/[ ])

Legend: checked means complete; unchecked means incomplete or only partially complete.

### Core Principle: Full Transcript Storage

- [ ] Each checkpoint always stores the full session transcript (mid-turn finalize path is best-effort today).

### Core Principle: Native Format Preservation

- [x] PI transcript is stored as native JSONL bytes (`NativeData`) without conversion for storage.

### Transcript Capture

- [x] Full transcript on every turn at checkpoint time.
- [x] Resumed session handling includes full historical messages.
- [x] Use canonical source (read PI transcript directly from session file path).
- [x] No custom intermediate transcript format for storage.
- [x] Graceful degradation when canonical source is unavailable (PI stop hook now ensures fallback transcript path/file).

### Session Storage Abstraction

- [x] `WriteSession(AgentSession)` implemented for PI.
- [x] File-based write path persists `NativeData` directly to `SessionRef`.
- [x] Single native format in `NativeData`.
- [x] Database-backed import path not applicable to PI.

### Hook Events

- [x] `TurnStart` mapped from PI prompt submit hook.
- [x] `TurnEnd` mapped from PI stop hook.
- [x] `SessionStart` mapped from PI session start hook.
- [x] `SessionEnd` mapped from PI session end hook.

### Rewind/Resume Support

- [x] Rewind restores full PI state with validated branch/leaf correctness.
- [x] Resume command is session-specific and unambiguous for PI.
- [x] Session ID preservation path exists (`TransformSessionID` / `ExtractAgentSessionID` passthrough).

### Testing

- [x] New session test verifies full transcript at each checkpoint for PI.
- [x] Resumed session test verifies checkpoint includes historical messages for PI.
- [x] Rewind test verifies PI can continue from restored point with correct context.
- [x] Agent shutdown test verifies graceful handling during checkpointing for PI.

## Evidence Anchors (Current Branch)

- `cmd/entire/cli/agent/pi/lifecycle.go`
- `cmd/entire/cli/agent/pi/pi.go`
- `cmd/entire/cli/agent/pi/transcript.go`
- `cmd/entire/cli/lifecycle.go`
- `cmd/entire/cli/strategy/manual_commit_condensation.go`
- `cmd/entire/cli/strategy/manual_commit_hooks.go`
- `cmd/entire/cli/integration_test/setup_pi_hooks_test.go`
- `cmd/entire/cli/integration_test/pi_before_compact_hook_test.go`
- `cmd/entire/cli/integration_test/pi_leaf_persistence_test.go`
- `cmd/entire/cli/integration_test/pi_session_lifecycle_test.go`
- `cmd/entire/cli/integration_test/pi_transcript_capture_test.go`
- `cmd/entire/cli/integration_test/resume_test.go`
- `cmd/entire/cli/integration_test/rewind_test.go`

## Phase 0: Baseline And Guardrails

### Actions

- Build a PI feature parity checklist against `~/projects/pi-mono` covering:
  - tree/leaf traversal semantics
  - prompt extraction
  - modified-file extraction
  - token usage extraction
  - compaction/branch summary behavior
  - session switch/fork lifecycle flow
- Freeze current expected behavior with tests before touching core seams.

### Files

- `cmd/entire/cli/agent/pi/lifecycle_test.go`
- `cmd/entire/cli/agent/pi/transcript_test.go`
- `cmd/entire/cli/agent/pi/hooks_test.go`
- `cmd/entire/cli/integration_test/hooks.go`

### Exit Criteria

- Existing PI tests pass unchanged.
- Gaps are explicitly enumerated and mapped to phases below.

## Phase 1 (P1): Leaf Propagation At Turn End (Minimal Core)

### Why

PI emits `leaf_id` on `stop`, but lifecycle step save currently does not persist it into `StepContext.TranscriptLeafID`. That blocks deterministic branch scoping later.

### Actions

- In turn-end lifecycle handling, read `leaf_id` from `event.Metadata`.
- Populate `strategy.StepContext.TranscriptLeafID` when saving the step.
- Keep fallback behavior unchanged for non-PI agents.

### Files

- `cmd/entire/cli/lifecycle.go`

### Exit Criteria

- `state.TranscriptLeafID` is updated from PI stop events through existing strategy state persistence.
- No behavior change for Claude/Gemini/OpenCode.

## Phase 2 (P2, Highest Priority): Leaf-Aware Condensation And Finalization

### Why

Post-hoc condensation and finalize paths currently use generic prompt/token extractors and can lose PI branch fidelity on tree transcripts.

### Actions

- Thread `TranscriptLeafID` into condensation/finalization extraction paths.
- For PI sessions, use leaf-aware prompt and token extraction.
- Preserve generic code paths for non-PI agents.
- Keep changes minimal and scoped to existing extraction call sites.

### Files

- `cmd/entire/cli/strategy/manual_commit_condensation.go`
- `cmd/entire/cli/strategy/manual_commit_hooks.go`

### Exit Criteria

- Condensation/finalize use active PI branch context, not stale/abandoned branches.
- Commit message prompt selection remains correct after tree navigation.
- No regression in Claude/Gemini condensation tests.

## Phase 3: Rewind/Resume/Switch/Fork Validation

### Why

PI extension emits lifecycle transitions for session changes; we need end-to-end correctness checks in Entire integration flow.

### Actions

- Add/extend integration tests to cover:
  - `session_switch` behavior (`session-end` old -> `session-start` new)
  - `session_fork` behavior (`session-end` parent -> `session-start` child)
  - tree navigation leaf updates before `stop`
  - resume/rewind continuity using persisted transcript + leaf context

### Files

- `cmd/entire/cli/integration_test/hooks.go`
- `cmd/entire/cli/integration_test/` (PI-specific test files as needed)

### Exit Criteria

- Fork/switch/resume flows behave as expected with PI hooks enabled.
- Rewind continues from correct branch context.

## Phase 4: PI ReadSession Enrichment (PI-Only)

### Why

`ReadSession()` currently returns minimal fields. PI transcripts carry richer metadata that is useful for checkpoints, debugability, and future UI surfaces.

### Actions

- Extend PI transcript parsing helpers to extract structured entries for:
  - assistant model/provider
  - usage totals and cost fields
  - tool call input/result details
  - compaction and branch summary entries
  - bash execution entries
- Populate `AgentSession.Entries` and derived metadata from PI native data.
- Keep this PI-local; do not require non-PI schema changes.

### Files

- `cmd/entire/cli/agent/pi/pi.go`
- `cmd/entire/cli/agent/pi/transcript.go`
- `cmd/entire/cli/agent/pi/types.go`
- `cmd/entire/cli/agent/pi/*_test.go`

### Exit Criteria

- `ReadSession()` returns richer session structure without breaking existing callers.
- Existing PI token/file extraction remains correct.

## Phase 5: PI-Only Cleanup (No Unrelated Removals)

### Actions

- Remove PI-only stale code/tests that no longer match chosen integration behavior.
- Keep compatibility only where intentional.
- Do not remove non-PI code even if stale-looking.

### Candidate Areas

- `cmd/entire/cli/agent/pi/hooks.go`
- `cmd/entire/cli/agent/pi/hooks_test.go`

### Exit Criteria

- No dead PI code paths remain for replaced behavior.
- Non-PI files are untouched unless required by Phases 1-3.

## Documentation And PR Update

### Actions

- Update PR body to explicitly document:
  - why minimal core seams were required (leaf propagation + condensation correctness)
  - what would break without them
  - what remains intentionally PI-specific (extension hooks, no JSON hook config)
  - deferred item: optional `--input <file>` hook transport
- Sync docs only where behavior changed materially.

### Files

- `docs/architecture/agent-guide.md` (if needed)
- `docs/architecture/agent-integration-checklist.md` (if needed)

## Test Matrix (Per Phase)

- `go test ./cmd/entire/cli/agent/pi/...`
- `go test ./cmd/entire/cli/strategy/...`
- `go test ./cmd/entire/cli/integration_test/...`
- `go test ./cmd/entire/cli/...`

## Execution Order

1. Phase 0
2. Phase 1
3. Phase 2
4. Phase 3
5. Phase 4
6. Phase 5
7. Documentation and PR update

## Risk Controls

- Keep core diffs narrowly scoped and PI-driven.
- Add tests before widening extraction behavior.
- Preserve fallback behavior for non-PI agents at each seam.
