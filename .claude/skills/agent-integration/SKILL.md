---
name: agent-integration
description: >
  Run all three agent integration phases sequentially: research, write-tests,
  and implement using E2E-first TDD (unit tests written last).
  For individual phases, use /agent-integration:research,
  /agent-integration:write-tests, or /agent-integration:implement.
  Use when the user says "integrate agent", "add agent support", or wants
  to run the full agent integration pipeline end-to-end.
---

# Agent Integration — Full Pipeline

Run all three phases of agent integration in a single session. Parameters are collected once and reused across all phases.

## Parameters

Collect these before starting (ask the user if not provided):

| Parameter | Description | How to derive |
|-----------|-------------|---------------|
| `AGENT_NAME` | Human-readable name (e.g., "Gemini CLI") | User provides |
| `AGENT_PACKAGE` | Go package dir name — **no hyphens** | Lowercase, remove hyphens/spaces |
| `AGENT_KEY` | Registry key for `agent.Register()` and `entire enable` | Check existing patterns in `cmd/entire/cli/agent/registry.go` |
| `AGENT_SLUG` | Filesystem/URL-safe slug (kebab-case) used in E2E runner filenames and script names | Kebab-case of agent name; align with existing entries in `e2e/agents/` |
| `AGENT_BIN` | CLI binary name | `command -v <binary>` |
| `LIVE_COMMAND` | Full command to launch agent | User provides |
| `EVENTS_OR_UNKNOWN` | Known hook event names, or "unknown" | From agent docs or "unknown" |

**Note:** These identifiers can differ. Run `grep -r 'AgentName\|func.*Name()' cmd/entire/cli/agent/*/` and `e2e/agents/` to see how existing agents handle the split.

## Architecture References

These documents define the agent integration contract:

- **Implementation guide**: `docs/architecture/agent-guide.md` — Step-by-step code templates, event mapping, testing patterns
- **Integration checklist**: `docs/architecture/agent-integration-checklist.md` — Design principles and validation criteria

## Scope

This skill targets **hook-capable agents** — those that support lifecycle hooks
(implementing `HookSupport` from `agent.go`). Agents that use file-based detection
(implementing `FileWatcher`) require a different integration approach not covered here.
Check `agent.go` for the current interface definitions.

## Core Rule: E2E-First TDD

This skill enforces strict E2E-first test-driven development. The rules:

1. **E2E tests are the spec.** The existing `ForEachAgent` test scenarios define what "working" means. The agent runner makes those tests runnable for the new agent.
2. **Run E2E tests at every step.** Each implementation tier starts by running the E2E test and watching it fail. You implement until it passes. No exceptions.
3. **Unit tests are written last.** After all E2E tiers pass (Step 14), you write unit tests using real data collected from E2E runs as golden fixtures.
4. **If you didn't watch it fail, you don't know if it tests the right thing.** Never write a test you haven't seen fail first.
5. **Minimum viable fix.** At each E2E failure, implement only the code needed to fix that failure. Don't anticipate future tiers.
6. **`/debug-e2e` is your debugger.** When an E2E test fails, use the artifact directory with `/debug-e2e` before guessing at fixes.

## Pipeline

Run these three phases in order. Each phase builds on the previous phase's output.

### Phase 1: Research

Discover the agent's hook mechanism, transcript format, and configuration through binary probing and documentation research. Produces an implementation one-pager at `cmd/entire/cli/agent/$AGENT_PACKAGE/AGENT.md` that the other phases use as their single source of agent-specific information.

Read and follow the research procedure from `.claude/skills/agent-integration/researcher.md`.

**Expected output:** Implementation one-pager at `cmd/entire/cli/agent/$AGENT_PACKAGE/AGENT.md` and a test script at `scripts/test-$AGENT_SLUG-agent-integration.sh`.

**Commit:** After the research phase completes, use `/commit` to commit all files.

**Gate:** If the verdict is INCOMPATIBLE, stop and discuss with the user before proceeding.

### Phase 2: Write E2E Runner

Create the E2E agent runner so existing test scenarios can exercise the new agent. No unit tests are written in this phase — no new test scenarios either (existing `ForEachAgent` tests are the spec).

Read and follow the procedure from `.claude/skills/agent-integration/test-writer.md`.

**Expected output:** E2E agent runner at `e2e/agents/$AGENT_SLUG.go` that compiles and registers with the test framework.

**Commit:** After the E2E runner compiles and registers, use `/commit` to commit all files.

### Phase 3: Implement (E2E-First, Unit Tests Last)

Build the Go agent package using strict E2E-first TDD. E2E tests drive development at every step — run each tier, watch it fail, implement the minimum fix, repeat. Unit tests are written only after all E2E tiers pass, using real data from E2E runs as golden fixtures.

Read and follow the implement procedure from `.claude/skills/agent-integration/implementer.md`.

**Expected output:** Complete agent package at `cmd/entire/cli/agent/$AGENT_PACKAGE/` with all E2E tiers passing and unit tests locking in behavior.

**Note:** `AGENT.md` is a living document — Phases 2 and 3 update it when they discover new information during testing or implementation.

## Final Validation

After all three phases, run the complete validation:

```bash
mise run fmt      # Format
mise run lint     # Lint
mise run test:ci  # All tests (unit + integration)
```

Summarize:
- Compatibility verdict from Phase 1
- Files created in Phases 2 and 3
- Test coverage
- Any remaining TODOs or gaps
