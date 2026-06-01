# `entire review` Command

`entire review` runs a set of configured review skills inside an agent session. The review session is an immutable fact attached to a checkpoint — no verdict, no status tracking, no empty commits. On the next `git commit`, the review session is condensed into the checkpoint metadata alongside normal sessions, permanently recording that the code was reviewed and which skills were run.

## Command Surface

```
entire review                          # Normal run: load config, run configured agent(s)
entire review --edit                   # Re-open the skills picker before running
entire review --agent <name>           # Force a specific configured agent (skips multi-picker)
entire review attach <session-id>      # Tag an existing agent session as a review (post-hoc)
entire review attach --force           # Skip confirmation
entire review attach --agent <name>    # Agent that created the session
entire review attach --skills <s,...>  # Declare which skills were run
```

When two or more launchable agents are configured and `--agent` is not set, a multi-select picker appears with an optional per-run prompt field (e.g. "focus on security"). Selecting one agent or passing `--agent` runs the single-agent path; selecting two or more runs the N-agent path.

## Settings Schema

Review skills are configured per-agent in `.entire/settings.json`:

```json
{
  "review": {
    "claude-code": {"skills": ["/pr-review-toolkit:review-pr"], "prompt": "Be thorough."},
    "codex": {"skills": ["/codex:adversarial-review"]}
  }
}
```

The key is the agent name. The value is a `ReviewConfig` with `skills` (skill invocations passed verbatim to the agent) and optional `prompt` (an always-prompt appended to the composed prompt). Settings field: `EntireSettings.Review` in `cmd/entire/cli/settings/settings.go`.

## How It Works (env-var handshake)

1. `entire review` selects the configured agent (override → alphabetically first → prompt if multiple), composes the review prompt via `review.ComposeReviewPrompt`, and computes scope (mainline base ref via `review.ComputeScopeStats`, overridable with `--base`).
2. **For launchable agents** (claude-code, codex, gemini-cli): the spawned agent process is given env vars `ENTIRE_REVIEW_{SESSION,AGENT,SKILLS,PROMPT,STARTING_SHA}` that the agent's `UserPromptSubmit` lifecycle hook reads to tag the session as `Kind = "agent_review"` with the configured skills/prompt. Each spawned process has its own env, so multiple worktrees and multi-agent runs are correct by construction (no shared marker file, no race).
3. **For non-launchable agents** (cursor, opencode, factoryai-droid): `RunMarkerFallback` writes a `PendingReviewMarker` file and prints guidance — the user opens the agent themselves and runs the skills. Single shared file (`review/marker_fallback.go`); adding new non-launchable agents is a registry entry, not a new file.
4. The agent runs the review skills; the session ends naturally.
5. On the next `git commit`, the PostCommit hook condenses the review session into the checkpoint on `entire/checkpoints/v1`, with `Kind` and `ReviewSkills` recorded in `CommittedMetadata`.
6. The `CheckpointSummary` sets `HasReview = true` for O(1) lookup. `HasReview` is an umbrella "any review happened" flag — future review kinds (e.g. manual review) should also set it.
7. `entire status` and the re-run guard read `HasReview` from the checkpoint metadata (no commit history walking).

## Checkpoint Metadata

Review metadata is stored at two levels on `entire/checkpoints/v1`:

- **`CommittedMetadata` (per-session)**: `kind: "agent_review"`, `review_skills: ["/skill1", "/skill2"]`, `review_prompt: "..."`
- **`CheckpointSummary` (per-checkpoint)**: `has_review: true` (umbrella; set when any session in the checkpoint has a review-kind `Kind`)

## Architecture

- **`AgentReviewer` interface** (`cmd/entire/cli/review/types/reviewer.go`): per-agent contract with `Name() string` and `Start(ctx, RunConfig) (Process, error)`. Each launchable agent implements this in its own package.
- **`ReviewerTemplate`** (`cmd/entire/cli/review/types/template.go`): shared scaffolding (Spawn → pipe stdout → run parser → forward events → close). Each agent supplies only its `BuildCmd` (argv/env) and `Parser` (stdout-to-Event stream).
- **`Sink` interface**: consumers of the event stream. Production sinks: `DumpSink` (post-run per-agent narrative), `TUISink` (Bubble Tea live dashboard with Ctrl+O drill-in), `SynthesisSink` (opt-in y/N cross-agent verdict). Sinks are composed by `composeMultiAgentSinks` based on TTY detection.
- **`Run(ctx, reviewer, cfg, sinks)`** (`cmd/entire/cli/review/run.go`): single-agent orchestrator. Forwards events to all sinks via `AgentEvent`, calls `RunFinished` once at end with a populated `RunSummary`. Sink dispatch is serialized; sinks need not internally synchronize.
- **`RunMulti(ctx, reviewers, cfg, sinks)`** (`cmd/entire/cli/review/run_multi.go`): N-agent orchestrator. Each agent runs concurrently in its own goroutine; events fan into a single dispatch loop so the serial-dispatch contract is preserved. Per-agent skills/prompts are injected via `perAgentConfiguredReviewer` adapter (each reviewer sees its own `RunConfig` despite the shared API surface).
- **Env-var contract** (`cmd/entire/cli/review/env.go`): single source of truth for `ENTIRE_REVIEW_*` constants used by spawn-side and lifecycle adoption.
- **Scope detection** (`cmd/entire/cli/review/scope.go`): `detectScopeBaseRef` returns the first existing ref from the fallback chain `origin/HEAD → origin/main → origin/master → main → master`. Overridable per-invocation via `--base <ref>` (validated through go-git's `ResolveRevision`). Banner output: "Reviewing feat/X vs main: 3 commits, 7 files changed, 2 uncommitted".

## Multi-Agent UI

When `RunMulti` is dispatched in a TTY, the sink slice is `[TUISink, DumpSink, SynthesisSink?]`:

- **`TUISink` / `reviewTUIModel`** (`cmd/entire/cli/review/tui_sink.go`, `tui_model.go`, `tui_detail.go`): live dashboard with one row per agent (name, status, tokens, last assistant preview, duration). `Ctrl+O` enters drill-in mode on the alt screen showing the full event buffer for the selected agent; `Esc` returns to the dashboard. `Ctrl+C` cancels the run via the shared `CancelFunc`. The model uses `tea.WithoutSignalHandler` so the cobra root retains SIGINT routing. After all agents finish, the user dismisses with any key — `RunFinished` blocks on dismissal so `DumpSink` renders below the TUI rather than overlapping it.
- **`SynthesisSink`** (`cmd/entire/cli/review/synthesis_sink.go`): opt-in y/N prompt offered after the dump. On "y", composes a synthesis prompt covering all agent narratives + per-run user prompt, calls the configured summary provider, and prints the unified verdict. Skipped silently when stdin can't prompt, the run was cancelled, or fewer than 2 agents produced usable output. Provider failures degrade gracefully ("synthesis unavailable: <err>") so the user can still commit.
- **Sink composition** (`composeMultiAgentSinks` in `cmd/entire/cli/review/cmd.go`): pure helper taking explicit `isTTY`/`canPrompt` so tests don't depend on real TTY detection. `findTUISink` picks the TUI out of the slice for `Start`/`Wait` lifecycle hooks.

## Skill Discovery (Claude Code)

`DiscoverReviewSkills` (`cmd/entire/cli/agent/claudecode/discovery.go`) walks three roots: plugin cache (`~/.claude/plugins/cache/<market>/<plugin>/<version>/{skills,commands,agents}`), user skills (`~/.claude/skills`), user commands/agents (`~/.claude/commands`, `~/.claude/agents`).

For the plugin cache, `pickLatestVersion` picks ONE version directory per plugin: highest valid semver wins; if no entries parse as semver, the lexicographic max is picked (handles the `unknown` sentinel some plugins ship). Without this, multiple installed versions of a plugin produced duplicate skill entries in the picker and prompt.

## Anti-Features (do NOT recreate)

The redesign eliminated several constructs from the prior implementation. None should be reintroduced without explicit design:

- `PendingReviewMarker` for launchable agents (env-var handshake makes it unnecessary)
- `WorktreePath` field + worktree-scoping logic (env per process eliminates the multi-tenant problem)
- `AgentEntries` map on the marker (each agent has its own env)
- Marker overwrite tripwire / refuse-attach guard (the bug classes they defended against don't exist)
- `--track-only` flag (intentionally removed by #1009)
- `--postreview` / `--finalize` / empty review commits / `/entire-review:finish` skill installer
- `Launcher` + `HeadlessLauncher` as separate interfaces (single `AgentReviewer`)
- Codex chrome-line filtering or any agent-specific stdout post-processing in shared multi-agent code (per-agent parsers own their format; shared code only sees `Event` variants)
- `sync.Once`-guarded onCancel + parallel `signal.Notify` goroutine (single cancel from start)

## Key Files

- `cmd/entire/cli/review/cmd.go` — `NewCommand()`, `runReview` dispatch fork, `composeMultiAgentSinks`
- `cmd/entire/cli/review/picker.go` / `multipicker.go` — config-edit picker, first-run setup, single- and multi-agent selection
- `cmd/entire/cli/review/attach.go` + `cli/review_helpers.go:newReviewAttachCmd` — `entire review attach` subcommand
- `cmd/entire/cli/review/marker_fallback.go` — non-launchable agent flow (single shared file)
- `cmd/entire/cli/review/prompt.go` / `scope.go` / `run.go` / `dump.go` / `run_multi.go` — core machinery (single-agent + N-agent fan-in)
- `cmd/entire/cli/review/tui_sink.go` / `tui_model.go` / `tui_detail.go` — Bubble Tea TUI sink
- `cmd/entire/cli/review/synthesis_sink.go` / `synthesis_prompt.go` — opt-in cross-agent verdict
- `cmd/entire/cli/review/types/{reviewer,sink,template}.go` — interface contracts (CU2 + CU4 + CU5b)
- `cmd/entire/cli/review/env.go` — `ENTIRE_REVIEW_*` constants + `EncodeSkills`/`DecodeSkills` + `AppendReviewEnv`
- `cmd/entire/cli/agent/{claudecode,codex,geminicli}/reviewer.go` — per-agent `AgentReviewer` implementations (claude-code, codex, gemini-cli)
- `cmd/entire/cli/agent/claudecode/discovery.go` — skill discovery + `pickLatestVersion` plugin-cache dedupe
- `cmd/entire/cli/lifecycle.go` — `adoptReviewEnv` reads `ENTIRE_REVIEW_*` from process env; replaces marker-file adoption
- `cmd/entire/cli/review_bridge.go` / `review_helpers.go` — bridge code in `cli` package for cycle-bound functions (`headHasReviewCheckpoint`, `launchableReviewerFor`, `newReviewAttachCmd`, `lazySynthesisProvider`)
- `cmd/entire/cli/checkpoint/checkpoint.go` — `Kind`, `ReviewSkills`, `ReviewPrompt` on `CommittedMetadata`; `HasReview` on `CheckpointSummary`
- `cmd/entire/cli/settings/settings.go` — `EntireSettings.Review` field
