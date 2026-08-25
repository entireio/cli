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
- [ ] **Use agent's canonical export**: Prefer the agent's native export command (e.g., reading Claude's JSONL file, Gemini's JSON, Cursor's JSONL, Factory AI Droid's JSONL, Copilot CLI's JSONL, OpenCode's `opencode export` JSON, Pi's JSONL session file) over manually reconstructing from events
- [ ] **No custom formats**: Store the agent's native format directly in `NativeData` - do not convert between formats (e.g., JSON to JSONL) or create intermediate representations
- [ ] **Graceful degradation**: If the canonical source is unavailable (e.g., agent shutting down), fall back to best-effort capture with clear documentation of limitations

### Session Storage Abstraction

See Guide: [Step 3 - Core Agent Interface](agent-guide.md#step-3-implement-core-agent-interface-youragentgo)

- [ ] **`WriteSession` implementation**: Agent must implement `WriteSession(AgentSession)` to restore sessions
- [ ] **File-based agents** (Claude, Gemini, Cursor, Factory AI Droid, Copilot CLI, Pi): Write `NativeData` to `SessionRef` path
- [ ] **Database-backed agents** (OpenCode): Write `NativeData` to file, then import into native storage (the native format should be what the agent's import command expects)
- [ ] **Single format per agent**: Store only the agent's native format in `NativeData` - no separate fields for different representations of the same data

### Hook Events

See Guide: [Step 4 - ParseHookEvent](agent-guide.md#step-4-implement-parsehookevent-lifecyclego), [Event Mapping Reference](agent-guide.md#event-mapping-reference)

Map agent-native hooks to these `EventType` constants (see `agent/event.go`):

- [ ] **TurnStart**: Fire when user submits a prompt (for pre-prompt state capture)
- [ ] **TurnEnd**: Fire when agent finishes responding (for checkpoint creation)
- [ ] **SessionStart**: Fire when a new session begins
- [ ] **SessionEnd**: Fire when session is explicitly ended (optional but recommended)
- [ ] **SubagentEnd true-completion signal, not just launch**: If the agent can run
      subagents in the background — a launch-time hook fires immediately with a
      stub, before any work happened, and real completion is notified later out
      of band — map that separate true-completion hook to `SubagentEnd` as well,
      with `agent.Event.Final = true` (never a payload sentinel) as the only thing
      framework code branches on. Claude Code does this: `post-task`
      (PostToolUse[Task]) fires at the launch stub (`Final: false`) and
      `subagent-stop` (the `SubagentStop` hook, wired to `entire hooks
      claude-code subagent-stop`) fires at true completion, per-agent, including
      after the parent's own turn already ended (`Final: true`). Without the
      second hook, everything a background subagent does is invisible — see the
      "Task Records (Subagent Work)" section of [Sessions and
      Checkpoints](sessions-and-checkpoints.md) for the full launch-stub →
      task-record completion → condensation-materializer design this drives.

### Hook Installation

See Guide: [Step 6 - InstallHooks](agent-guide.md)

- [ ] **Hook commands name the `entire` binary**, resolved through PATH — never a
      path that resolves inside the working tree. A repo-relative command runs
      whatever the checked-out branch contains, on every agent turn, and any repo
      could opt its cloners into it. This is why `local_dev` was removed.
- [ ] **Stale Entire hooks are dropped on every install, not just `--force`**, via
      `agent.DropStaleManagedHooks`. Adding the current hook without removing an
      older one leaves both firing. Two agents got this wrong independently, so
      route through the shared helper rather than re-deriving the loop.
- [ ] **Legacy command shapes stay in `entireHookPrefixes`** (including
      `agent.LegacyLocalDevHookScript`) so hooks written by older versions are
      recognized as ours and replaced instead of being left in place.
- [ ] **A committed generated config is drift-guarded** — see
      `testutil.AssertCommittedDogfoodFile` / `AssertCommittedDogfoodConfigStable`
      if this repo commits the agent's config for its own dogfooding.

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
- [ ] **Manual token validation for session-wide aggregate agents**: If an agent emits authoritative token totals only at session end (for example Copilot CLI `session.shutdown`), manually verify checkpoint-scoped metadata and full-session status separately. See [Copilot Token Validation](copilot-token-validation.md).

## User-Level Hook Support (Global Tracking)

`agent.UserHookSupport` (`cmd/entire/cli/agent/user_hooks.go`) is an optional
extension of `HookSupport` for agents whose hooks can also be installed at the
USER level (home-directory config), so they fire in every repository —
including repos with no repo-level Entire setup. This is what lets global
tracking (configured in the user settings file) reaches a repo that was never enabled:
without a user-level hook no agent hook fires there, so the lazy global enable
never gets a chance to run.

Contract:

- User-level installs write ONLY user-scope config files (never a file inside
  a repository), and the hook commands use the plain production `entire`
  binary form — never a repo-local dev script path, which would be
  meaningless outside that repo.
- `UninstallUserHooks` removes only entries recognizable as Entire's and
  preserves every unrelated key in the user config file.
- All three methods are safe to call outside a git repository.
- Implement the interface only for agents with a verified user/repo dedup
  story (both scopes installed must never double-fire hooks); never fake it
  for agents without a real user-level surface.
- Before any THIRD agent gains `UserHookSupport`, user-level setup must grow
  an agent-availability gate — install user-level hooks only for agents whose
  config surface is detected (e.g. `~/.claude`, `~/.gemini` exist), or via a
  picker — and doctor's USER-LEVEL AGENT HOOKS MISSING check must scope
  itself to detected agents. With today's two near-universal agents,
  install-for-all deliberately errs toward coverage; at three-plus agents it
  starts creating home-dir config for tools the user doesn't have, and doctor
  would warn about never-used agents. The availability gate is the recorded
  consent story (trail 968 finding 019fef03-2ad1).

Audit of built-in agents (2026-08), recorded per the global-enable design:

- **claude-code**: IMPLEMENTED. `~/.claude/settings.json` accepts the same
  hooks schema as the repo's `.claude/settings.json`. Claude Code merges
  hooks across settings scopes and deduplicates identical hook commands;
  the user-level entries are byte-identical to the repo-level production
  entries, so a repo with both installed fires each hook once. Caveat: the
  dedup only covers byte-identical commands — a repo whose hooks were
  installed with `--local-dev` (scripts/entire-dev form) or by a legacy
  go-run install uses different command strings, so its hooks double-fire
  alongside the user-level install.
- **gemini**: IMPLEMENTED. `~/.gemini/settings.json` accepts the same hooks
  schema as the repo's `.gemini/settings.json`. Gemini concatenates hook
  event arrays across settings scopes (no per-key override) and its hook
  planner deduplicates entries by name+command
  (`hookPlanner.deduplicateHooks`/`getHookKey`); the user-level entries are
  byte-identical to the repo-level production entries, so a repo with both
  installed fires each hook once. Caveat: the dedup only covers identical
  name+command pairs — a repo whose hooks were installed with `--local-dev`
  (scripts/entire-dev form) uses different command strings, so its hooks
  double-fire alongside the user-level install.
- **cursor**: NOT implemented. Cursor supports a user-level
  `~/.cursor/hooks.json`, but does not document dedup between user- and
  project-level hook entries, so installing both risks double-firing
  every hook; deferred until that contract is verified.
- **codex**: NOT implemented. Entire manages the repo-scoped
  `.codex/hooks.json` only, and Codex requires per-machine trust approval
  (`/hooks`) keyed to each hooks file — a silently installed user-level
  file would sit unapproved and never fire.
- **copilot-cli**: NOT implemented. Copilot CLI discovers hook configs from
  the repository's `.github/hooks` directory; there is no user-level hook
  surface.
- **opencode**: NOT implemented. OpenCode has a global plugin directory
  (`~/.config/opencode/plugin`), but Entire's plugin install contract is
  only verified for the repo-level `.opencode/plugins` directory; deferred.
- **pi**: NOT implemented. Entire's pi extension is auto-discovered from the
  repo's `.pi/extensions` directory; no verified user-level surface.
- **factoryai-droid**: NOT implemented. Droid reads `~/.factory/settings.json`,
  but dedup between user- and repo-level hook entries is undocumented, so
  installing both risks double-firing; deferred.
- **vogon**: test-only agent, excluded from user-level hook management.
