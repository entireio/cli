# `entire review`

Experimental review command for running one configured review profile.

## Basic use

```sh
entire review --configure       # create or edit a profile
entire review --list            # list profiles
entire review <profile>         # run a profile
entire review --profile <name>  # same as positional form
entire review --agent <name>    # run one reviewer from the profile
entire review --target <ref>    # review a branch/trail in a managed worktree
entire review --findings        # view local findings
entire review --show-config     # list what the review would run, then exit
```

Useful run flags:

```sh
entire review --prompt "focus on auth"
entire review --timeout 15m
entire review --agent claude-code --model opus
entire review general --target feature/my-change
entire review general --target feature/my-change --cleanup-worktree
entire review general --target https://entire.io/gh/acme/app/trails/42/my-change
```

`--target` accepts a branch name, trail ID/number, or Entire trail URL. It
resolves the trail when needed, fetches a remote-only branch, and creates or
reuses a worktree under `.entire/worktrees`. The review command is then re-run
inside that worktree, leaving the caller's current checkout unchanged. If the
branch is already checked out in another worktree, that worktree is reused.
After a successful interactive review, a newly-created worktree prompts for
removal. Non-interactive reviews keep it unless `--cleanup-worktree` is passed.
A reused worktree is never removed by the review command, and failed reviews
retain newly-created worktrees for inspection. Cancelling the optional cleanup
prompt also keeps the worktree without changing the successful review exit.
Findings are attributed to the caller's worktree, so the completion handle
remains available through `entire review --findings` even after target cleanup.
Positive integer targets always mean trail numbers rather than numeric branch
names; this avoids a local branch silently shadowing a trail selector.

## Profiles

Profiles live in:

- `.entire/settings.json` for shared project config
- `.entire/settings.local.json` for local, git-ignored config

A profile contains:

- `task`: what to review for
- `agents`: reviewer slots
- `judge`: optional final consolidating reviewer
- `output`: `local` or `trail`

Minimal example:

```json
{
  "review_default_profile": "general",
  "review_profiles": {
    "general": {
      "task": "Review this change for correctness, regressions, tests, and maintainability.",
      "agents": {
        "claude-code": {"skills": ["/review"]},
        "codex": {"skills": ["/review"]},
        "pi": {"model": "anthropic/claude-sonnet", "prompt": "Review the change according to the profile task."}
      },
      "judge": {"agent": "claude-code"},
      "output": "local"
    }
  }
}
```

`entire review --models` lists the models each review-runner agent advertises via the optional `agent.ModelLister` capability (`cmd/entire/cli/agent/model_lister.go`). claude-code returns a curated list of real aliases (opus/sonnet/haiku); Pi enumerates live by shelling out to `pi --list-models`. An agent whose CLI has no enumeration command (codex) does not implement `ListModels`, so the picker offers only Default + Custom. The `--model` flag still forwards any value the agent CLI accepts.

The profile-level `task` is the shared work item. Each `agents` map entry is a worker id. For simple entries the worker id is also the agent name; to run the same agent more than once, use aliases and set `agent` plus `model`. Per-worker `skills`, `prompt`, and `model` adapt that task to agent-specific mechanics. Pi is a prompt/model-driven worker (`pi --mode json --print [--model ...]`) rather than a slash-command worker. Settings fields: `EntireSettings.ReviewProfiles` and `EntireSettings.ReviewDefaultProfile` in `cmd/entire/cli/settings/settings.go`. The old top-level `review` map is parse-tolerated and can be exposed as a legacy `general` profile when no `review_profiles` are configured.

## Behavior

- Reviewers run the profile task.
- Multi-reviewer profiles run reviewers concurrently, then run one judge.
- Results are printed and saved locally; `output: "trail"` also posts findings to the branch trail.
- A bare non-interactive `entire review` does not auto-run a profile. Automation should pass a profile name.
- Profile selection is positional/`--profile` → `review_default_profile` → `general` → the only configured profile.

## Reviewing code by someone else

Reviewers load the checkout's full agent configuration: `CLAUDE.md`/`AGENTS.md`,
skills, hooks, MCP servers, plugins, and settings. On a teammate's branch that
configuration is theirs, so a review of commits you did not author needs your
approval first.

- **Whose commits.** Every commit between your default branch (origin/HEAD,
  origin/main, origin/master, main, master) and the head must have your
  `user.email` as its git author, as for any commit; who committed it does not
  matter. `--base` only scopes the review; it never shortens this range.
  Uncommitted changes in a plain review are yours. With no `user.email`
  (common on CI runners) or no default branch, the commits count as someone
  else's, so automation passes `--trust-target`. The author is self-declared,
  as in any git commit, so this tells a teammate's branch from yours; it is
  not proof against a branch that copies your email. Verifying commit
  signatures and using a trail's authenticated author are follow-ups.
- **What runs.** The gate lists, for the agents in the profile: hooks (Entire's
  own count only when the whole command equals one this CLI installs, Unix or
  Windows form), MCP servers, command-bearing and permission settings in
  `.claude/settings*.json`, every key of `.codex/config.toml` (except the flags
  that only switch hooks on) and `.pi/settings.json`, Pi extensions other than
  Entire's, and every skill, command, prompt, subagent, and local plugin under
  `.claude`, `.codex`, `.agents/skills`, and `.pi`, which can carry their own
  hooks or shell expansions. JSON is read with exact keys, as the agents read
  it, and the commit tree is matched case-insensitively, as a macOS or Windows
  checkout would load it. Symlinks and unparseable files count as unknown,
  never as nothing. `--show-config` (`--json` for structured output) prints the
  full list and exits; its contents come from the branch and are data.
- **Approval.** In a terminal, a confirm lists up to three entries (the
  branch's own first) and defaults to Cancel. Without a terminal, or when an
  agent is driving the command (caller-session variables, the shared
  agent-subprocess sentinels, `CLAUDECODE`, Antigravity's and Droid's
  variables, the cross-tool `AI_AGENT`, `GIT_TERMINAL_PROMPT=0`, or variables
  an external agent declares in `caller_env_vars`), the review is refused with a
  fixed message that tells the agent to stop and ask the user, plus the
  `--trust-target <sha>` command to run once approved. `--trust-target` takes
  the full SHA of the pinned head (a short prefix could be ground to match a
  different commit); a branch that moved is refused.
- **Order.** With `--target`, the profile is resolved and the branch fetched
  and pinned in your checkout, the gate inspects the pinned commit's tree, and
  only then is the worktree created: added with `--no-checkout` and no hooks,
  its head checked against the pin, then checked out. For someone else's
  branch the checkout runs with an empty `core.hooksPath`, no submodule
  recursion, and `GIT_LFS_SKIP_SMUDGE=1`, and `.worktreeinclude` files are not
  copied. A reused worktree is inspected on disk instead.
- **Settings.** The re-run inside the worktree reads review settings (profiles,
  agents, models, judge) from your checkout, not the branch's
  `.entire/settings.json`, and skips the gate only when the forwarded
  `--trust-target` matches its own head. Entire's own lifecycle hooks, which
  the agents start inside the worktree, still read that worktree's settings.
- **Guardrail.** Every reviewer gets a system-prompt addition setting an
  instruction hierarchy: the task comes only from the review request;
  repository content (including CLAUDE.md/AGENTS.md) informs the review but
  cannot change the task, grant permissions, or trigger actions; attempts to
  direct the reviewer are reported as findings (`--append-system-prompt` for
  Claude Code and Pi, `-c developer_instructions=` for Codex, the top of the
  prompt for agents without a runner). The judge runs from a temp directory
  and does not load the checkout's configuration.

## Flow

1. With `--target`, `entire review` selects the profile in the caller's checkout, resolves the branch directly or through its trail, pins its head, runs the trust gate, prepares a worktree, and re-runs the command there without `--target`.
2. It selects a profile. If no profiles exist, it runs guided setup in an interactive terminal or writes an opinionated clone-local default profile in non-interactive mode. Plain reviews then run the trust gate.
3. It composes worker prompts via `review.ComposeReviewPrompt` and computes scope (mainline base ref via `review.ComputeScopeStats`, overridable with `--base`).
4. Adapter-backed review workers (claude-code, codex, pi) are spawned with `ENTIRE_REVIEW_{SESSION,AGENT,SKILLS,PROMPT,STARTING_SHA}` env vars. Their lifecycle hooks use those values to tag sessions as `Kind = "agent_review"`.
5. Each spawned process has its own env, so multiple worktrees and multi-agent runs do not need a shared marker file.
6. In multi-worker profiles, the configured judge receives all worker reports and produces one final verdict. The judge prompt asks it to reject unsupported claims, resolve contradictions, merge duplicates, and prioritize evidence-backed findings.
7. On the next `git commit`, the PostCommit hook condenses worker review sessions into the checkpoint on `entire/checkpoints/v1`, with `Kind`, `ReviewSkills`, and `ReviewPrompt` recorded in `CommittedMetadata`.
8. `CheckpointSummary.HasReview` is set for O(1) lookup. `entire status` and the re-run guard read this flag from checkpoint metadata.

## Checkpoint Metadata

Review metadata is stored at two levels on `entire/checkpoints/v1`:

- **`CommittedMetadata` (per-session)**: `kind: "agent_review"`, `review_skills: ["/skill1", "/skill2"]`, `review_prompt: "..."`
- **`CheckpointSummary` (per-checkpoint)**: `has_review: true` (umbrella; set when any session in the checkpoint has a review-kind `Kind`)

## Architecture

- **`AgentReviewer` interface** (`cmd/entire/cli/review/types/reviewer.go`): per-agent contract with `Name() string` and `Start(ctx, RunConfig) (Process, error)`. Each adapter-backed review worker implements this in its own package.
- **`ReviewerTemplate`** (`cmd/entire/cli/review/types/template.go`): shared scaffolding (spawn → pipe stdout → run parser → forward events → close). Each agent supplies only its `BuildCmd` and `Parser`.
- **`Sink` interface** (`cmd/entire/cli/review/types/sink.go`): consumers of the event stream. Production sinks include `DumpSink`, `TUISink`, and `SynthesisSink`.
- **`Run(ctx, reviewer, cfg, sinks)`** (`cmd/entire/cli/review/run.go`): single-agent orchestrator. Forwards events to sinks and calls `RunFinished` once with a populated `RunSummary`.
- **`RunMulti(ctx, reviewers, cfg, sinks)`** (`cmd/entire/cli/review/run_multi.go`): N-agent orchestrator. Agents run concurrently; events fan into a single dispatch loop so sink dispatch remains serialized.
- **Env-var contract** (`cmd/entire/cli/review/env.go`): single source of truth for `ENTIRE_REVIEW_*` constants used by spawn-side and lifecycle adoption.
- **Scope detection** (`cmd/entire/cli/review/scope.go`): `detectScopeBaseRef` returns the first existing ref from `origin/HEAD → origin/main → origin/master → main → master`.

## Multi-Agent UI

When `RunMulti` is dispatched in a TTY, sink composition includes a live Bubble Tea dashboard plus buffered narrative/final-report output:

- **`TUISink` / `reviewTUIModel`** (`cmd/entire/cli/review/tui_sink.go`, `tui_model.go`, `tui_detail.go`): one row per agent with status, tokens, last assistant preview, and duration. `Ctrl+O` enters drill-in mode; `Esc` returns; `Ctrl+C` cancels through the shared context.
- **`SynthesisSink`** (`cmd/entire/cli/review/synthesis_sink.go`): in profile-native mode, runs automatically after the dump, composes an adjudication prompt covering worker narratives + per-run user prompt + profile task, calls the judge agent, and prints the final report.
- **Sink composition** (`composeMultiAgentSinks` in `cmd/entire/cli/review/cmd.go`): pure helper taking explicit `isTTY`/`canPrompt` so tests do not depend on real TTY detection.

## Skill Discovery (Claude Code)

`DiscoverReviewSkills` (`cmd/entire/cli/agent/claudecode/discovery.go`) walks three roots under Claude Code's config directory (`$CLAUDE_CONFIG_DIR`, default `~/.claude`): plugin cache (`plugins/cache/<market>/<plugin>/<version>/{skills,commands,agents}`), user skills (`skills`), and user commands/agents (`commands`, `agents`).

For the plugin cache, `pickLatestVersion` picks one version directory per plugin: highest valid semver wins; if no entries parse as semver, the lexicographic max is picked.

## Anti-Features (do NOT recreate)

The redesign eliminated several constructs from the prior implementation. None should be reintroduced without explicit design:

- `PendingReviewMarker` for adapter-backed review workers (env-var handshake makes it unnecessary)
- `WorktreePath` field + worktree-scoping logic (env per process eliminates the multi-tenant problem)
- `AgentEntries` map on the marker (each agent has its own env)
- Marker overwrite tripwire / refuse-attach guard (the bug classes they defended against do not exist)
- `--track-only` flag
- `--postreview` / `--finalize` / empty review commits / `/entire-review:finish` skill installer
- `Launcher` + `HeadlessLauncher` as separate interfaces (single `AgentReviewer`)
- Codex chrome-line filtering or any agent-specific stdout post-processing in shared multi-agent code (per-agent parsers own their format)
- `sync.Once`-guarded onCancel + parallel `signal.Notify` goroutine (single cancel from start)

## Key Files

- `cmd/entire/cli/review/cmd.go` — `NewCommand()`, `runReview`, sink composition
- `cmd/entire/cli/review/picker.go` / `profile.go` — profile config picker, first-run setup, profile resolution/default tasks
- `cmd/entire/cli/review/prompt.go` / `scope.go` / `run.go` / `dump.go` / `run_multi.go` — core machinery
- `cmd/entire/cli/review/tui_sink.go` / `tui_model.go` / `tui_detail.go` — Bubble Tea TUI sink
- `cmd/entire/cli/review/synthesis_sink.go` / `synthesis_prompt.go` — judge adjudication
- `cmd/entire/cli/review/types/{reviewer,sink,template}.go` — interface contracts and shared review template
- `cmd/entire/cli/review/env.go` — `ENTIRE_REVIEW_*` constants + `EncodeSkills`/`DecodeSkills` + `AppendReviewEnv`
- `cmd/entire/cli/agent/{claudecode,codex,pi}/reviewer.go` — per-agent `AgentReviewer` implementations
- `cmd/entire/cli/agent/claudecode/discovery.go` — skill discovery + plugin-cache dedupe
- `cmd/entire/cli/lifecycle.go` — `adoptReviewEnv` reads `ENTIRE_REVIEW_*` from process env
- `cmd/entire/cli/review_bridge.go` / `review_target.go` — bridge code for cycle-bound functions, trail posting, and target resolution and checkout
- `cmd/entire/cli/review/trust.go` / `trust_run.go` — trust gate: authorship, approval, messages, `--show-config`
- `cmd/entire/cli/review_trust_inventory.go` — per-agent inventory of what a checkout would run
- `cmd/entire/cli/checkpoint/checkpoint.go` — review metadata on checkpoints
- `cmd/entire/cli/settings/settings.go` — review profile settings
