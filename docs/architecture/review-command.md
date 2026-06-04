# `entire review` Command

`entire review` runs a set of configured review skills inside an agent session. The review session is an immutable fact attached to a checkpoint — no verdict, no status tracking, no empty commits. On the next `git commit`, the review session is condensed into the checkpoint metadata alongside normal sessions, permanently recording that the code was reviewed and which skills were run.

## Command Surface

```
entire review                          # Normal run: dispatch to configured Reviewers
entire review setup                    # Two-step role + skills picker (canonical configuration entrypoint)
entire review --edit                   # Alias for `entire review setup`
entire review --prompt "<text>"        # Per-run extra context (bypasses inline ask)
entire review --reviewers <list>       # One-off override: agents to run as Reviewers (comma-separated)
entire review --fixer <name>           # One-off override: agent to use as Fixer
entire review --agent <name>           # Force a specific configured Reviewer
entire review --base <ref>             # Scope against <ref> instead of mainline
entire review fix [session-id]         # Apply review findings via the configured Fixer
entire review fix --all                # Apply all findings without selectors
entire review fix --prompt "<text>"    # Per-run extra context for the fix run
entire review --findings               # Browse local review findings (read-only)
entire review attach <session-id>      # Tag an existing agent session as a review (post-hoc)
entire review attach --force           # Skip confirmation
entire review attach --agent <name>    # Agent that created the session
entire review attach --skills <s,...>  # Declare which skills were run
```

`entire review` no longer auto-opens a picker. Resolution is role-driven:

1. `--reviewers` / `--fixer` flags → one-off override (NOT persisted).
2. Saved roles (`ReviewersOf(s)` / `FixerOf(s)`) → use them.
3. Interactive, no saved roles → print `Run: entire review setup`; exit.
4. Non-interactive, no saved roles, invoking agent detected (`CLAUDE_CODE`, `CODEX`, `GEMINI_CLI`, `COPILOT_CLI`, `PI_CODING_AGENT`) → use that agent as Role `Both`; print a one-line note (NOT persisted).
5. Non-interactive, no roles, no invoker → hard error pointing at setup.

When two or more agents have `RoleReviewer` (or `RoleBoth`), all of them run concurrently via `RunMulti`. There is no spawn-time multi-select picker — `entire review setup` is the only place where reviewer membership is decided.

`ENTIRE_REVIEW_SESSION` in env triggers a recursion guard on both `entire review` and `entire review fix` — refuses to start a nested review (prevents reviewer-prompted loops).

## Pre-launch staging + post-review Fix UX

- **Staging view** (`stagePerRunContext` in `post_review.go`, replaces the old `askPerRunPrompt`): after scope detection and BEFORE fan-out, the optional per-run prompt is collected together with the scope summary in one styled huh form — the "Add context for this run?" input on top, and a Note underneath listing the scope line plus the **itemised checkpoints (short id + commit summary) and in-progress sessions (short id + agent) in scope**. The user sees exactly what's about to be reviewed before agents launch. `--prompt` or non-interactive: the same summary is printed plainly and the ask is skipped. (`ContextResult` carries structured `CheckpointItems` / `SessionItems`; backticks are sanitised for huh's markdown renderer.)
- After the manifest is written, `RunPostReviewFixPrompt` (in `post_review.go`) drives the next step:
  - `FixAfterReview == "always"` → launch the Fixer with `--all`, skip the prompt.
  - Non-TTY → print the `Run:` footer (`entire review fix` / `--all` / `entire review findings`).
  - TTY → prompt `[Y]es · [s]elect fixes · [n]o · [A]lways`. `[A]` persists `FixAfterReview = "always"`. Gated on a resolvable Fixer (`FixerOf(s)`); no fixer → footer hint instead.
- **`[s]` / `entire review fix` selection** (`fix.go`): for the interactive multi-source case, source-selection and findings-selection are ONE navigable huh form (two groups; findings recomputed from the selected sources via `OptionsFunc`), so the user can `shift+tab` back to change the source — e.g. to the **Aggregate** source — without restarting. Single-source / `--all` / non-interactive keep the linear path.
- `userExplicitlyOmittedFixer` (true when `--reviewers` was used WITHOUT `--fixer`) swaps the no-fixer footer from "Run: entire review setup" to a one-off `--fixer <agent>` hint.

## Settings Schema

Review configuration is role-first; each entry in `.entire/settings.json` declares the agent's `role` plus per-agent `skills`, `prompt`, and optional per-spawn `model` / `reasoning_effort`:

```json
{
  "review": {
    "claude-code": {"role": "reviewer", "skills": ["/pr-review-toolkit:review-pr"], "prompt": "Be thorough."},
    "codex": {"role": "both", "skills": ["$code-reviewer"], "model": "gpt-5-mini", "reasoning_effort": "low"}
  },
  "fix_after_review": "always"
}
```

Roles: `reviewer` (runs on `entire review`), `fixer` (runs on `entire review fix`), `both` (counts as the Fixer AND reviews), `skip` (configured but unused). The empty role on legacy entries is upgraded to `reviewer` in-memory by `settings.MigrateLegacyRoles` on load (the upgrade is idempotent and only persists on the next settings write).

**Skill invocation form is per-agent.** Claude skills are slash-prefixed (`/review`, `/plugin:name`); **codex skills are dollar-prefixed (`$code-reviewer`, `$plugin:name`)** — the literal token a user types to invoke a skill in that CLI. Discovery emits each agent's `Name` already in its own form (see Skill Discovery below).

**`model` / `reasoning_effort`** (`ReviewConfig.Model` / `ReviewConfig.ReasoningEffort`) are optional per-spawn overrides threaded into `reviewtypes.RunConfig` by `applyReviewConfig` and emitted by the codex reviewer as `-m <model>` / `-c model_reasoning_effort=<level>`. Empty values defer to the agent's own config. Agents without these knobs ignore them.

**`entire review setup` defaults new agents to `skip`** (reviewing is opt-in): every hook-installed agent is listed, but the user explicitly picks Reviewer/Both; a "≥1 reviewer" guard forces an explicit choice. Saved roles are never silently rewritten.

**`fix_after_review`** lives on both `EntireSettings` and `ClonePreferences`. Values: `""` (unset — default, behaves as Ask and, clone-side, means "no override"), `"ask"` (explicit Ask — the non-empty token a clone writes to override a project-level `"always"` back to Ask), `"always"` (auto-run the Fixer). `settings.Load` validates `role` and `fix_after_review` after merge+migration, rejecting hand-edited unknown values.

Settings field: `EntireSettings.Review` and `EntireSettings.FixAfterReview` in `cmd/entire/cli/settings/settings.go`. `RoleReviewer` / `RoleFixer` / `RoleBoth` / `RoleSkip` are exported constants.

## How It Works (env-var handshake)

1. `entire review` selects reviewers via `ReviewersOf(s)`, composes the review prompt via `review.ComposeReviewPrompt`, and computes scope (mainline base ref via `review.ComputeScopeStats`, overridable with `--base`).
2. **For launchable agents** (claude-code, codex, gemini-cli): the spawned agent process is given env vars `ENTIRE_REVIEW_{SESSION,AGENT,SKILLS,PROMPT,STARTING_SHA}` that the agent's `UserPromptSubmit` lifecycle hook reads to tag the session as `Kind = "agent_review"` with the configured skills/prompt. Each spawned process has its own env, so multiple worktrees and multi-agent runs are correct by construction (no shared marker file, no race).
3. **For non-launchable agents** (cursor, opencode, factoryai-droid): `RunMarkerFallback` writes a `PendingReviewMarker` file and prints guidance — the user opens the agent themselves and runs the skills. Single shared file (`review/marker_fallback.go`); adding new non-launchable agents is a registry entry, not a new file.
4. The agent runs the review skills; the session ends naturally.
5. On the next `git commit`, the PostCommit hook condenses the review session into the checkpoint on `entire/checkpoints/v1`, with `Kind` and `ReviewSkills` recorded in `CommittedMetadata`.
6. The `CheckpointSummary` sets `HasReview = true` for O(1) lookup. `HasReview` is an umbrella "any review happened" flag — future review kinds (e.g. manual review) should also set it.
7. `entire status` and the re-run guard read `HasReview` from the checkpoint metadata (no commit history walking).

Env adoption is generalized: `tryAdoptEnv` in `cmd/entire/cli/lifecycle.go` runs the shared env-present + agent-match + STARTING_SHA gate, and `adoptReviewEnv` / `adoptInvestigateEnv` supply the kind-specific `envAdoptionSpec` (env keys + `apply`). The session env value is always `"1"`.

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
- **Scope detection** (`cmd/entire/cli/review/scope.go`): `detectScopeBaseRef` returns the first existing ref from the fallback chain `origin/HEAD → origin/main → origin/master → main → master`. Overridable per-invocation via `--base <ref>` (validated through go-git's `ResolveRevision`). `detectScope` (in `cmd.go`) returns the base ref plus a human-facing banner string ("Reviewing feat/X vs main: 3 commits, 7 files changed, 2 uncommitted") for the staging step to fold into the styled prompt.

## Multi-Agent UI

When `RunMulti` is dispatched in a TTY, the sink slice is `[TUISink, DumpSink, SynthesisSink?]`:

- **`TUISink` / `reviewTUIModel`** (`cmd/entire/cli/review/tui_sink.go`, `tui_model.go`, `tui_detail.go`): live dashboard with one row per agent (name, status, tokens, last assistant preview, duration). `Ctrl+O` enters drill-in mode on the alt screen showing the full event buffer for the selected agent; `Esc` returns to the dashboard. `Ctrl+C` cancels the run via the shared `CancelFunc`. The model uses `tea.WithoutSignalHandler` so the cobra root retains SIGINT routing. After all agents finish, the user dismisses with any key — `RunFinished` blocks on dismissal so `DumpSink` renders below the TUI rather than overlapping it.
- **`SynthesisSink`** (`cmd/entire/cli/review/synthesis_sink.go`): opt-in y/N prompt offered after the dump. On "y", composes a synthesis prompt covering all agent narratives + per-run user prompt, calls the configured summary provider, and prints the unified verdict. Skipped silently when stdin can't prompt, the run was cancelled, or fewer than 2 agents produced usable output. Provider failures degrade gracefully ("synthesis unavailable: <err>") so the user can still commit.
- **Sink composition** (`composeMultiAgentSinks` in `cmd/entire/cli/review/cmd.go`): pure helper taking explicit `isTTY`/`canPrompt` so tests don't depend on real TTY detection. `findTUISink` picks the TUI out of the slice for `Start`/`Wait` lifecycle hooks.
- **Output styling**: `DumpSink` (per-agent findings) and `SynthesisSink` (aggregate verdict) render through `mdrender.RenderMutedForWriter` — a low-chroma palette (`mdrender.mutedStyles`) that keeps coloured+bold headings for structure but neutralises the high-frequency inline noise (inline-code highlight blocks, coloured bullets/links) that makes markdown-dense findings read as a wall of colour. The shared palette (dispatch / explain / status) is untouched. The live TUI dashboard adds no per-cell colour; only its column header is dimmed (`reviewHeaderStyle`, `Faint`).

## Skill Discovery (Claude Code + Codex)

The generic SKILL.md/markdown scanners, version dedupe (`PickLatestVersion`), and frontmatter parsing live in the shared `skilldiscovery` package (`scan.go`), parameterized by an **invocation-form builder** (`SlashForm` for Claude, `DollarForm` for codex) — the only per-agent difference. Each agent's `DiscoverReviewSkills` supplies its roots + form; discovery emits `DiscoveredSkill.Name` already in that agent's invocation form so the shared prompt composer joins it verbatim.

- **Claude Code** (`claudecode/discovery.go`) walks: plugin cache (`~/.claude/plugins/cache/<market>/<plugin>/<version>/{skills,commands,agents}`), user skills (`~/.claude/skills`), user commands/agents (`~/.claude/commands`, `~/.claude/agents`). Emits `/name` / `/plugin:name`.
- **Codex** (`codex/discovery.go`) walks: `~/.codex/skills/<name>`, plugin cache `~/.codex/plugins/cache/<market>/<plugin>/<version>/skills/<name>`, and `~/.codex/superpowers/skills/<name>`. Emits `$name` / `$plugin:name`. Codex has **no curated built-ins** in `skilldiscovery/registry.go` (its review skills are discovered on disk, not bundled in the binary; `/review` is a TUI-only built-in that doesn't fire in `codex exec`).

For the plugin cache, `PickLatestVersion` picks ONE version directory per plugin: highest valid semver wins; if no entries parse as semver, the lexicographic max is picked (handles the `unknown` sentinel some plugins ship, and codex's opaque content-hash version dirs). Without this, multiple installed versions of a plugin produced duplicate skill entries in the picker and prompt.

When an agent has no saved skills and no per-run skill flags, `seedDefaultSkills` (`setup.go`) supplies a default: the agent's curated built-ins, or — for an agent with none (codex) — on-disk discovered skills whose name signals a review skill (contains "review"), so codex still invokes e.g. `$code-reviewer` instead of a generic scope-only prompt. Used by the invoker-only fallback and the `--reviewers` / `--fixer` override.

## Codex specifics (exec fires no hooks)

`codex exec` fires **no lifecycle hooks**, which drives several codex-only behaviors:

- **Skill invocation**: configured skills are passed to codex **verbatim** (no paraphrase) — codex's skill system injects its installed-skill catalog into every exec session and loads the matching `SKILL.md`. Native `codex exec review` is intentionally NOT used (it rejects a prompt under a scope flag and can't carry Entire's scope/per-run/checkpoint context). `codex/reviewer.go` argv: `codex exec --skip-git-repo-check --json [-m <model>] [-c model_reasoning_effort=<level>] -`.
- **Live tokens**: `codex exec --json` carries `usage` only on the terminal `turn.completed`, and a review is a single turn — so the TUI would show no tokens until the end. `codex/review_tokens.go` resolves codex's rollout transcript by `thread_id` (from the `thread.started` envelope), **tails it** (via `os.File.Read` — bufio is sticky on EOF), and emits cumulative `Tokens` per `token_count` event. The parser launches the tailer on `thread.started` and uses a `stop` channel + `WaitGroup` so it can't send on a closed channel.
- **Fix manifest sourcing**: because no hook tags a codex review session, `buildLocalReviewManifestFromSummary` includes an agent as a fix source when it has **live output OR a matched session** (not session-match-only) — codex's review narrative is captured in `run.Buffer`. Without this, codex was silently dropped from `entire review fix`. The completion footer prints for a session-less codex-only manifest too; `entire review fix` with no handle resolves to the most-recent run.
- **Skill verification**: `VerifyConfiguredSkillsInstalled` is **advisory for codex** (logs, doesn't block) — codex resolves skills by loose description match, and legacy saved configs may carry pre-`$`form invocations.

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
- Spawn-time multi-select agent picker / `PickAgents` / `PickedAgents` / `multipicker.go` (roles answer "who reviews" up front via `entire review setup`)
- Spawn-time "Choose fix agent" prompt (`FixerOf(s)` is the single source of truth)
- First-run auto-picker on bare `entire review` (replaced by `Run: entire review setup` pointer + invoker-only non-interactive fallback)
- `ConfirmFirstRunSetup` / `RunReviewConfigPicker` (use `RunSetup` instead)
- `MultiPickerFn` on `review.Deps` and the runtime multi-agent picker dispatch fork

NOTE — the unified source⇄findings picker in `entire review fix` (`fix.go`) is **not** a reintroduction of the spawn-time agent picker: it selects which findings to *fix* after a review, not which agents *review*. Reviewer membership is still decided only by roles / `entire review setup`.

## Key Files

- `cmd/entire/cli/review/cmd.go` — `NewCommand()`, `runReview` dispatch fork, `detectScope`, `composeMultiAgentSinks`, post-review fix prompt wiring
- `cmd/entire/cli/review/setup.go` — `entire review setup` two-step picker + `DetectInvokingAgent` / `invokerOnlyReviewConfig` / `seedDefaultSkills` for the non-interactive fallback
- `cmd/entire/cli/review/picker.go` — `PromptForAgent` single-select, `ComputeEligibleConfigured`, `SelectReviewAgent`, `VerifyConfiguredSkillsInstalled`, `SaveReviewConfig`
- `cmd/entire/cli/review/flags.go` — `resolveRolesFromFlags` + `mergeFlagOverrideWithSavedSkills` for `--reviewers` / `--fixer` one-off overrides
- `cmd/entire/cli/review/inline_prompt.go` — `ReadSingleKey` helper used by the `[Y]/[s]/[n]/[A]` post-review prompt
- `cmd/entire/cli/review/post_review.go` — `stagePerRunContext` (pre-launch staging) + `RunPostReviewFixPrompt` + `launchFixFromManifest` + `printFindingsFooter`
- `cmd/entire/cli/review/banner.go` — `formatContextBanner` (itemised checkpoints/sessions scope) + `reviewerSkillSuffix` (setup banner skill counts)
- `cmd/entire/cli/review/fix.go` — `entire review fix` subcommand, `runReviewFix`, `selectReviewFixInteractive`, `extractSourceFindings`/`reviewFindingTitle`, `resolveReviewFixAgent` (delegates to `FixerOf(s)`); launches via `agentlaunch.LaunchFixAgent`
- `cmd/entire/cli/review/manifest.go` — local fix manifest; `buildLocalReviewManifestFromSummary` includes live-output agents (codex) even without a tagged review session
- `cmd/entire/cli/review/roles.go` — `NormalizeRoles`, `ReviewersOf`, `FixerOf`, `MigrateLegacyRoles` thin-wrap
- `cmd/entire/cli/review/attach.go` + `cli/review_helpers.go:newReviewAttachCmd` — `entire review attach` subcommand
- `cmd/entire/cli/review/marker_fallback.go` — non-launchable agent flow (single shared file)
- `cmd/entire/cli/review/prompt.go` / `scope.go` / `run.go` / `dump.go` / `run_multi.go` — core machinery (single-agent + N-agent fan-in)
- `cmd/entire/cli/review/tui_sink.go` / `tui_model.go` / `tui_detail.go` — Bubble Tea TUI sink
- `cmd/entire/cli/review/synthesis_sink.go` / `synthesis_prompt.go` — opt-in cross-agent verdict
- `cmd/entire/cli/review/types/{reviewer,sink,template}.go` — interface contracts
- `cmd/entire/cli/review/env.go` — `ENTIRE_REVIEW_*` constants + `EncodeSkills`/`DecodeSkills` + `AppendReviewEnv`; the recursion guard reads `EnvSession` here
- `cmd/entire/cli/agent/{claudecode,codex,geminicli}/reviewer.go` — per-agent `AgentReviewer` implementations
- `cmd/entire/cli/agent/skilldiscovery/scan.go` — shared SKILL.md scanners, `PickLatestVersion`, frontmatter parse, `SlashForm`/`DollarForm` invocation builders
- `cmd/entire/cli/agent/claudecode/discovery.go` + `cmd/entire/cli/agent/codex/discovery.go` — per-agent skill discovery (slash / `$name` form) over each agent's roots
- `cmd/entire/cli/agent/codex/review_tokens.go` — rollout-transcript token tailing for live codex tokens
- `cmd/entire/cli/mdrender/mdrender.go` — `RenderMuted` / `RenderMutedForWriter` low-chroma palette for dense review output
- `cmd/entire/cli/review_context.go` — `reviewCheckpointContext` builds scope context (agent prompt text + structured `ContextResult` checkpoint/session items for the staging banner)
- `cmd/entire/cli/lifecycle.go` — `tryAdoptEnv` + `adoptReviewEnv` / `adoptInvestigateEnv` read `ENTIRE_REVIEW_*` / `ENTIRE_INVESTIGATE_*` from process env; replaces marker-file adoption
- `cmd/entire/cli/review_bridge.go` / `review_helpers.go` — bridge code in `cli` package for cycle-bound functions (`headHasReviewCheckpoint`, `launchableReviewerFor`, `newReviewAttachCmd`, `lazySynthesisProvider`)
- `cmd/entire/cli/checkpoint/checkpoint.go` — `Kind`, `ReviewSkills`, `ReviewPrompt` on `CommittedMetadata`; `HasReview` on `CheckpointSummary`
- `cmd/entire/cli/settings/settings.go` — `EntireSettings.Review`, `EntireSettings.FixAfterReview`, `Role` / `RoleReviewer` / `RoleFixer` / `RoleBoth` / `RoleSkip`
