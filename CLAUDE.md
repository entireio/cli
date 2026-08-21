# Entire - CLI

This repo contains the CLI for Entire.

## Architecture

- CLI built with github.com/spf13/cobra and github.com/charmbracelet/huh

## Key Directories

### Commands (`cmd/`)

- `entire/`: Main CLI entry point. Also home to kubectl-style external-command resolution (`entire <name>` → `entire-<name>` on PATH) — see [External Commands](docs/architecture/external-commands.md).
- `entire/cli`: CLI utilities and helpers (Cobra commands, helpers, group roots)
- `entire/cli/commands`: actual command implementations
- `entire/cli/agent`: agent implementations (Claude Code, Gemini CLI, OpenCode, Cursor, Factory AI Droid, Copilot CLI, Pi) - see [Agent Integration Checklist](docs/architecture/agent-integration-checklist.md) and [Agent Implementation Guide](docs/architecture/agent-guide.md)
- `entire/cli/strategy`: strategy implementation (manual-commit) - see section below
- `entire/cli/checkpoint`: checkpoint storage abstractions (ephemeral and persistent)
- `entire/cli/session`: session state management
- `entire/cli/integration_test`: integration tests (simulated hooks)
- `e2e/`: E2E tests with real agent calls (see [e2e/README.md](e2e/README.md))

### Command Layout

The visible CLI is organized around a set of noun groups plus a small set of
top-level verbs. The groups are the canonical home for each verb. Newer
experimental command families are discoverable through `entire labs` and
their canonical paths are always runnable.

Experimental commands are gated by a build-time visibility flag (the
`cmd/entire/cli/experimental` package): they are shown — grouped under an
"Experimental commands:" help section — in developer and nightly builds, and
hidden in stable release builds. Visibility is toggled by `experimental.Visible`
(default `"true"`), which GoReleaser stamps `"false"` only on stable tags
(`.Prerelease` empty); nightly (`vX.Y.Z-nightly.*`) and local builds leave it at
the default. Register a command as experimental with `experimental.Register(parent,
child)` instead of `parent.AddCommand(child)`. Gating only controls visibility —
the commands are always runnable in every build.

- `session` (alias: `sessions`): `list`, `info`, `tokens`, `stop`, `attach`, `adopt`, `resume`, `current`.
  `resume` with a branch arg switches to it and resumes its session; with no arg
  it opens an interactive picker of stopped sessions (across all worktrees),
  resolving each to its branch and pointing at the owning worktree when the
  branch is checked out elsewhere. Resume keeps an existing local session log
  as-is by default (`--force` overwrites it from the checkpoint).
  `adopt` moves an active session from another repo or worktree into the current
  worktree and resets target-local checkpoint bookkeeping so future commits link
  to the adopted session from the new location.
- `checkpoint` (aliases: `cp`, `checkpoints`): `list`, `explain`, `tokens`, `search`.
  `explain` also takes `--repo <owner/name>`, the drill-down for a cross-repo
  `search` hit: it reads the checkpoint from that repo's entire-api cell over
  HTTP (`/repos/{repo_id}/checkpoints/{id}` plus `.../transcript/raw`) rather
  than fetching git objects, so a foreign checkpoint never enters this repo's
  object store, ref namespace, or `tokens profile`. It needs a full checkpoint
  ID and a pushed checkpoint; `--commit`, `--session`, `--search-all`, and
  `--generate` are rejected with it, and naming the current repo is a no-op
  that falls through to the local path. See `checkpoint_api_reader.go`
  (`apiCheckpointReader`, which implements the two checkpoint reader tiers and
  deliberately not `Writer`) and `explain_repo.go`.
- `agent`: bare opens the interactive agent selector, plus `list`, `add`, `remove`
- `configure`: bare prints help and a hint pointing at `entire agent`; flags
  manage non-agent settings (telemetry, git-hook installation mode, strategy
  options, summary provider). Agent CRUD lives under `entire agent`.
- `auth`: `login`, `logout`, `status`, `contexts`, `use`, plus
  `token` (prints the active control-plane bearer to stdout for scripting/curl;
  honors `ENTIRE_TOKEN`, else the refreshed active-context login JWT). `token`
  also takes `--jurisdiction <slug>` (e.g. `us`, `eu`), which instead mints a
  jurisdictional identity token (RFC 8693 exchange, `scope=openid`,
  `aud=<jurisdiction host>`) for that jurisdiction's entire-api cells (e.g.
  `https://aws-us-east-2.api.entire.io/api/v1`), which reject the control-plane
  bearer; it exchanges `ENTIRE_TOKEN` when set (deriving the environment from the
  env token's `aud`), else the active login. `auth status` shows the caller's
  home jurisdiction so the slug is discoverable. `logout`
  takes `--everywhere` (revoke every session on the active core, not just the
  current one) and `--all-contexts` (log out of every saved login)
- `doctor`: bare runs the scan-and-fix flow, plus `trace`, `logs`, `bundle`
- `org`: control-plane organization management — `create`, `list`, `get`, `delete`
- `project`: control-plane project management — `create`, `list`, `get`, `delete`
- `repo`: control-plane repository lifecycle — `create`, `list`, `get`, `delete`,
  `clone`, plus the `mirror` and `visibility` subtrees. Git content operations
  (log, diff, …) are intentionally out of scope. The `mirror` subtree is
  server-side (`create`, `list`, `get`, `remove`, `collaborators`) with one
  exception: `mirror use` repoints the *current clone's* git remote at a mirror
  (local git config only — it creates nothing server-side). Interactively it
  picks among the repo's placements and asks whether to replace the remote
  (preserving the old URL under `--upstream`) or add a separate one;
  non-interactively it repoints `--remote` directly. Both `use` and `clone`
  choose a placement through the shared `selectPlacement` picker.
- `grant`: manage access grants and org membership — `org`, `project`, and `repo`
  each support `add` / `list` / `remove`

Experimental commands (gated by the build-time visibility flag above — visible
and grouped under "Experimental commands:" in developer/nightly builds, hidden
in stable releases, always runnable): `tokens`, `import`, `review`,
`investigate`, `blame`, `why`, `experts`, `runner`, and `checkpoint policy`.
`tokens` is also advertised through `entire labs`.

Top-level lifecycle and standalone commands: `enable`, `disable`, `status`,
`login`, `logout`, `clean`, `version`, `dispatch`, `activity`, `help`,
`configure`, `agent-help`, `api`, `search`. `search` is the canonical
spelling (visible in every build, grouped with Sessions & Checkpoints);
`checkpoint search` stays a working alias of the same command.

`api` is an authenticated passthrough to Entire's HTTP APIs (gh-style): it
attaches the right bearer and dials the right host so callers don't plumb auth
themselves. `--to core` (default) hits the control plane; `--to cell` hits an
entire-api cell. `--jurisdiction <slug>` (e.g. `us`, `eu`) targets a specific
jurisdiction's cell instead of the caller's home cell and implies `--to cell`
(cell routing + identity-token exchange live in `auth.NewEntireAPICellClient`
via `auth.CellTarget`). `{owner}`/`{repo}`/`{repo_id}` in the path are filled
from the current repo's origin remote. It is an escape hatch, so it is absent
from `agent-help`'s curated listing but stays in `entire help` and agent-help's
footer — an agent that needs raw access must find it rather than hand-roll curl
with a token.

`agent-help` renders machine-readable, agent-facing usage live from the Cobra
command tree (so it always matches the installed binary): bare prints a curated
"when to use entire" map; `agent-help <command>` drills into one command's
current flags; `--json` emits structured output. It is the single source of
truth the first-turn context injection and the `--agent-help-skill` skill point
agents at, instead of enumerating a surface that goes stale.

#### Where agent-facing text goes

| What you have | Where it goes |
| --- | --- |
| A new command | `agentHelpClassification` in `agent_help_cmd.go` — one entry, keyed by command path, carrying `audience` and `listed` |
| "When to use this at all" advice for agents | `agentHelpGuidance` — **never** cobra `Short`/`Long` |
| A fact humans need too (e.g. "this output is not stable") | cobra `Long`. Human help is a reference, not a lecture: whoever typed `--help` already chose the command |
| A per-task command recommendation | `agent-help`, which is pulled on demand. **Never** the first-turn injection, which carries only invariants true on every turn |

**Flag it; don't decide it.** Whether a command is `listed`, and whether it is
read-only / task-driven / user-owned, are product judgment calls — they change
what agents do unprompted in every user's repo. Take the safe default
(unlisted, user-owned), then say in the PR what you picked and why so a human
can move it. Never quietly promote a command into the listing or into
read-only.

CI enforces the mechanical parts, so trust these rather than re-deriving them:
every advertised top-level command and every child of a listed group is
classified; a read-only group contains no writing subcommand; guidance text
never appears in a command's `Short`/`Long`.

Hidden commands opt into being advertised here by setting
`Annotations[agentHelpAnnotation] = "true"` (e.g. `trail`). Because `agent-help`
renders live and lists non-hidden commands, the experimental commands appear in
`agent-help` in developer/nightly builds and are absent in stable releases — the
advertised surface is build-dependent, matching what `entire help` shows.
No-channel agents (Cursor, Copilot CLI, Factory Droid, MCP hosts — no
context-injection channel and no agent-help skill template) reach it without an
active push. All of them can discover it passively: it is visible in `entire
help`, the `entire status` footer points at it, and `entire status --json`
exposes it as the `agent_help` field. On top of that, Factory AI Droid (which is
banner-only) gets the pointer appended to its SessionStart hook banner, and
MCP-host agents can launch the hidden `entire mcp` stdio server, which exposes
`agent_help` and `entire_status` as MCP tools using the same live rendering.
Enabling a no-channel agent with `--agent-help-skill` reports the skill
unsupported and points the agent at this passive path instead.

Cobra-native aliases (no hint): `sessions` → `session`, `cp`/`checkpoints` →
`checkpoint`.

Hidden infrastructure commands: `hooks`, `trail`,
`curl-bash-post-install`, `__send_analytics`, `mcp` (MCP stdio server for
MCP-host agents).

Diagnostic subcommands live alongside `doctor.go` as `doctor_logs.go` and
`doctor_bundle.go`. Group roots and noun-group children live in files
named `<noun>_group.go` and `<noun>_<verb>.go` respectively.

## Tech Stack

- Language: Go 1.26.x
- Build tool: mise, go modules
- Linting: golangci-lint

## Development

### Running Tests

```bash
mise run test
```

### Running Integration Tests

```bash
mise run test:integration
```

### Running All Tests (CI)

```bash
mise run test:ci
```

This runs unit tests, integration tests, and the E2E canary (Vogon agent) in sequence. Integration tests use the `//go:build integration` build tag and are located in `cmd/entire/cli/integration_test/`.

### Running E2E Canary Tests (Vogon Agent)

The Vogon agent is a deterministic fake agent that exercises the full E2E test suite without making any API calls.

```bash
mise run test:e2e:canary           # Run all E2E tests with the Vogon agent
mise run test:e2e:canary TestFoo   # Run a specific test
```

- **Runs as part of `test:ci`** — canary failures block merges
- **No API calls, no cost** — safe to run freely, unlike real agent E2E tests
- **If a canary test fails, the bug is in the CLI or test infrastructure**, not in an agent
- Located in `e2e/vogon/` (binary) and `cmd/entire/cli/agent/vogon/` (Agent interface)
- The binary parses prompts via regex, creates/modifies/deletes files, and fires lifecycle hooks
- **IMPORTANT: When changing E2E test prompt wording**, the Vogon binary (`e2e/vogon/main.go`) parses prompts with hardcoded regexes. New phrasing may not match existing patterns — always run `mise run test:e2e:canary` after changing prompt text and fix Vogon's parsing if tests fail.

### Running E2E Tests (Only When Explicitly Requested)

**IMPORTANT: Do NOT run E2E tests proactively.** E2E tests make real API calls to agents, which consume tokens and cost money. Only run them when the user explicitly asks for E2E testing.

```bash
mise run test:e2e [filter]                          # All agents, filtered
mise run test:e2e --agent claude-code [filter]       # Claude Code only as an example here, replace `claude-code` with other agents to run tests for those agents
```

E2E tests:

- Use the `//go:build e2e` build tag
- Located in `e2e/tests/`
- See [`e2e/README.md`](e2e/README.md) for full documentation (structure, debugging, adding agents)
- Test real agent interactions (Claude Code, Gemini CLI, OpenCode, Cursor, Factory AI Droid, Copilot CLI, Pi, or Vogon creating files, committing, etc.)
- Validate checkpoint scenarios documented in `docs/architecture/checkpoint-scenarios.md`
- Support multiple agents via `E2E_AGENT` env var (`claude-code`, `gemini`, `opencode`, `cursor`, `factoryai-droid`, `copilot-cli`, `pi`, `vogon`)

**Environment variables:**

- `E2E_AGENT` - Agent to test with (default: `claude-code`)
- `E2E_CLAUDE_MODEL` - Claude model to use (default: `haiku` for cost efficiency)
- `E2E_TIMEOUT` - Timeout per prompt (default: `2m`)

### Test Parallelization

**Always use `t.Parallel()` in tests.** Every top-level test function and subtest should call `t.Parallel()` unless it modifies process-global state (e.g., `os.Chdir()`).

```go
func TestFeature_Foo(t *testing.T) {
    t.Parallel()
    // ...
}

// Integration tests with TestEnv
func TestFeature_Bar(t *testing.T) {
    t.Parallel()
    env := NewFeatureBranchEnv(t)
    // ...
}
```

**Exception:** Tests that modify process-global state cannot be parallelized. This includes `os.Chdir()`/`t.Chdir()` and `os.Setenv()`/`t.Setenv()` — Go's test framework will panic if these are used after `t.Parallel()`.

### Git in Tests

**Tests that touch git state must use an isolated temp repo — never the real repo CWD.**

Many handlers (lifecycle, strategy, hooks) resolve the git repo from CWD via `OpenRepository`, `GetGitCommonDir`, `DetectFileChanges`, etc. Without isolation, tests can create session state files, shadow branches, or other artifacts in the real `.git/` directory.

Use the `testutil` helpers:

```go
tmpDir := t.TempDir()
testutil.InitRepo(t, tmpDir)                    // git init + user config + disable GPG
testutil.WriteFile(t, tmpDir, "f.txt", "init")  // create a file
testutil.GitAdd(t, tmpDir, "f.txt")             // stage it
testutil.GitCommit(t, tmpDir, "init")           // commit (needs at least one commit for HEAD)
t.Chdir(tmpDir)                                 // redirect CWD-based git resolution
```

`testutil.InitRepo` configures `user.name`, `user.email`, and disables GPG signing — safe for CI environments without global git config.

**Prefer `testutil.InitRepo()` over direct `git.PlainInit()` in tests.** When a test in this repo needs an initialized repository, use `testutil.InitRepo(t, dir)` unless the test specifically needs lower-level initialization behavior that the helper cannot provide. Do not call `git.PlainInit()` directly and then create commits or run CLI git operations without also reproducing the helper's repo-local config.

**Do NOT** shell out to `git init`/`git commit` directly without setting user config and `--no-gpg-sign`, and **do NOT** run lifecycle/strategy handlers from the real repo CWD in tests.

### Config/Cache/Keyring Isolation in Tests

Tests must never read or write the developer's real `~/.config/entire`
(contexts.json, version_check.json), `~/.cache/entire` (nodes.json,
cluster_cores.json, api_discovery.json), or OS keychain. The developer may be
using `entire` for real while tests run.

- **Single resolver**: `internal/entireclient/userdirs` is the only place
  that resolves the per-user config dir (`userdirs.Config()`:
  `$ENTIRE_CONFIG_DIR` else `~/.config/entire`) and cache dir
  (`userdirs.Cache()`: `$XDG_CACHE_HOME/entire` else `~/.cache/entire`).
  Never derive these paths anywhere else.
- **In-process safety net**: `userdirs` and the `tokenstore` default backend
  detect `go test` (via `internal/testdirs`) and fall back to a throwaway
  per-process temp directory when their env override is unset. The fallback
  is shared across tests in one process — for per-test isolation still set
  `t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())` and
  `tokenstore.UseFileBackendForTesting(...)`.
- **Spawned binaries are NOT covered**: `testing.Testing()` is false in a
  subprocess. The integration and e2e TestMains set `ENTIRE_CONFIG_DIR`,
  `XDG_CACHE_HOME`, `ENTIRE_TOKEN_STORE=file`, `ENTIRE_TOKEN_STORE_PATH`, and
  `ENTIRE_TEST_AUTH_STORE_FILE` process-wide so every spawned `entire` (and
  every agent-invoked hook) inherits isolation. Any new harness that spawns
  the real binary must do the same.
- **Legacy auth store**: `auth.NewStore()` talks straight to the zalando
  keyring; packages whose tests can reach it need `keyring.MockInit()` in
  `TestMain` (see `cmd/entire/cli/global_test.go`) — the `testdirs` fallback
  does not cover it in-process.

### Spawning subprocesses in tests (TTY detection)

Tests that spawn the real `entire` or `git` binary need the child to be non-interactive so prompts don't hang on a developer terminal.

`interactive.CanPromptInteractively()` resolves in this order:

1. `ENTIRE_TEST_TTY=1` → force interactive ON (any other non-empty value → force OFF).
2. `testing.Testing()` → false. In-process `go test` runs are non-interactive by default; no per-test `t.Setenv("ENTIRE_TEST_TTY", "0")` is needed.
3. Agent sentinels (`GEMINI_CLI`, `COPILOT_CLI`, `PI_CODING_AGENT`, `GIT_TERMINAL_PROMPT=0`) → false.
4. `CI=<non-empty-non-false>` → false.
5. `/dev/tty` probe, plus its terminal mode → a terminal held in raw mode
   (canonical input off) belongs to a full-screen TUI that spawned us, not to a
   shell we can prompt: TUI git clients (lazygit, gitui, tig) run `git commit`
   as a child while owning the screen, so the hook inherits a `/dev/tty` it
   must not prompt on. Fails open when the mode can't be read. See
   `interactive/rawmode_unix.go` for the rationale.

For subprocesses spawning the real `entire` binary (e2e, integration tests, `entire` calling itself from a hook), prefer `execx.NonInteractive` over env-var plumbing:

```go
import "github.com/entireio/cli/cmd/entire/cli/execx"

cmd := execx.NonInteractive(ctx, getTestBinary(), "status")
cmd.Dir = repoDir
out, err := cmd.CombinedOutput()
```

`execx.NonInteractive` puts the child in a new session with no controlling terminal (`Setsid` on Unix, `DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP` on Windows), so the child's `/dev/tty` probe fails naturally. No env var required.

`interactive.UnderTest()` returns true when `testing.Testing()` or `ENTIRE_TEST_TTY` is set — use it where code needs to skip a real-terminal operation even if `CanPromptInteractively()` returns true (e.g., reading from `/dev/tty` directly inside `askConfirmTTY`).

### Linting and Formatting

```bash
mise run fmt && mise run lint
```

`mise run fmt` can rewrite files. Treat `mise run fmt && mise run lint` as a single verification sequence: if formatting changes anything, run lint again on the formatted tree rather than assuming a previous lint result still applies.

### Before Every Commit (REQUIRED)

**CI will fail if you skip these steps:**

```bash
mise run check
```

Equivalent expanded form:

```bash
mise run fmt      # Format code (CI enforces gofmt)
mise run lint     # Lint check (CI enforces golangci-lint)
mise run test:ci  # Run all tests (unit + integration)
```

`mise run check` runs the three commands above.

Safety note: do not treat a clean `mise run lint` result as final unless it was run after the most recent `mise run fmt` pass.

### Before Any Push Or Remote Code Update (REQUIRED)

Before pushing commits or otherwise sending code changes to any remote, run `mise run lint` on the current tree and ensure it passes. If `mise run fmt` changed files, rerun `mise run lint` on the formatted tree before pushing.

**Common CI failures from skipping this:**

- `gofmt` formatting differences → run `mise run fmt`
- Lint errors → run `mise run lint` and fix issues
- Test failures → run `mise run test` and fix

### Code Duplication Prevention

Before implementing Go code, use `/go:discover-related` to find existing utilities and patterns that might be reusable.

**Check for duplication:**

```bash
mise run dup           # Comprehensive check (threshold 50) with summary
mise run dup:staged    # Check only staged files
mise run lint          # Normal lint includes dupl at threshold 75 (new issues only)
mise run lint:full     # All issues at threshold 75
```

**Tiered thresholds:**

- **75 tokens** (lint/CI) - Blocks on serious duplication (~20+ lines)
- **50 tokens** (dup) - Advisory, catches smaller patterns (~10+ lines)

When duplication is found:

1. Check if a helper already exists in `common.go` or nearby utility files
2. If not, consider extracting the duplicated logic to a shared helper
3. If duplication is intentional (e.g., test setup), add a `//nolint:dupl` comment with explanation

## Code Patterns

### Error Handling

The CLI uses a specific pattern for error output to avoid duplication between Cobra and main.go.

**How it works:**

- `root.go` sets `SilenceErrors: true` globally - Cobra never prints errors
- `main.go` prints errors to stderr, unless the error is a `SilentError`
- Commands return `NewSilentError(err)` when they've already printed a custom message

**When to use `SilentError`:**
Use `NewSilentError()` when you want to print a custom, user-friendly error message instead of the raw error:

```go
// In a command's RunE function:
if _, err := paths.WorktreeRoot(); err != nil {
    cmd.SilenceUsage = true  // Don't show usage for prerequisite errors
    fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Please run 'entire enable' from within a git repository.")
    return NewSilentError(errors.New("not a git repository"))
}
```

**When NOT to use `SilentError`:**
For normal errors where the default error message is sufficient, return the error directly. main.go will print it:

```go
// Normal error - main.go will print "unknown strategy: foo"
return fmt.Errorf("unknown strategy: %s", name)
```

**Key files:**

- `errors.go` - Defines `SilentError` type and `NewSilentError()` constructor
- `root.go` - Sets `SilenceErrors: true` on root command
- `main.go` - Checks for `SilentError` before printing

### Settings

All settings access should go through the `settings` package (`cmd/entire/cli/settings/`).

**Why a separate package:**
The `settings` package exists to avoid import cycles. The `cli` package imports `strategy`, so `strategy` cannot import `cli`. The `settings` package provides shared settings loading that both can use.

**Usage:**

```go
import "github.com/entireio/cli/cmd/entire/cli/settings"

// Load full settings object
s, err := settings.Load()
if err != nil {
    // handle error
}
if s.Enabled {
    // ...
}

// Or use convenience functions
if settings.IsSummarizeEnabled() {
    // ...
}
```

**Do NOT:**

- Read `.entire/settings.json` or `.entire/settings.local.json` directly with `os.ReadFile`
- Duplicate settings parsing logic in other packages
- Create new settings helpers without adding them to the `settings` package

**Key files:**

- `settings/settings.go` - `EntireSettings` struct, `Load()`, and helper methods
- `config.go` - Higher-level config functions that use settings (for `cli` package consumers)

### Logging vs User Output

- **Internal/debug logging**: Use `logging.Debug/Info/Warn/Error(ctx, msg, attrs...)` from `cmd/entire/cli/logging/`. Writes to `.entire/logs/`.
- **Enabling debug/perf logs locally**: Prefer adding `"log_level": "DEBUG"` to `.entire/settings.local.json` when you need detailed hook/perf logs. This file is gitignored. `ENTIRE_LOG_LEVEL=debug` also works and takes precedence.
- **User-facing output**: Use `fmt.Fprint*(cmd.OutOrStdout(), ...)` or `cmd.ErrOrStderr()`.

Don't use `fmt.Print*` for operational messages (checkpoint saves, hook invocations, strategy decisions) - those should use the `logging` package.

**Privacy**: Don't log user content (prompts, file contents, commit messages). Log only operational metadata (IDs, counts, paths, durations).

### Git Operations

We use github.com/go-git/go-git for most git operations, but with important exceptions:

#### Opening Repositories - Always Use `gitrepo`

**Never call `git.Open`, `git.PlainOpen`, or `git.PlainOpenWithOptions` directly.
`cmd/entire/cli/gitrepo` is the single source of truth for opening a
repository.** Use `gitrepo.OpenCurrent(ctx)` for the current worktree or
`gitrepo.OpenPath(root)` for a specific worktree root. Both funnel through
`openPathWithAlternates`, which is the only place that opens a `*git.Repository`.

Routing every open through `gitrepo` guarantees two behaviours no ad-hoc
`git.PlainOpen` call gets right:

- **Object alternates** are rewritten to absolute paths so shared clones resolve
  their objects (`PlainOpen` cannot follow relative/absolute alternates).
- **Reftable repositories** are detected and opened through the git-CLI-backed
  reference storer (`reftableStorer`). A direct `git.PlainOpen` on a reftable
  repo fails outright with `unknown extension: refstorage`, because go-git's
  filesystem storer cannot read the reftable backend. The reftable storer also
  re-approves the `objectformat` (sha1/sha256) and `worktreeconfig` extensions
  that go-git verifies at open time.

If a code path opens a repo with a bare go-git call, it silently breaks on
reftable and sha256 repositories. Reviewers should flag any new
`git.PlainOpen*`/`git.Open` outside `gitrepo`. Key files: `gitrepo/repository.go`
(open entry points) and `gitrepo/reftable.go` (`reftableStorer`).

#### Reading Worktree Status - Always Use `gitrepo.Status`

**Never call go-git's `worktree.Status()` directly.** Use
`gitrepo.Status(ctx, repo)`; a `forbidigo` rule in `.golangci.yaml` enforces
this, and `gitrepo/status.go` is the only sanctioned call site.

`Worktree.Status()` walks the worktree, so its cost scales with working-set size
rather than with the size of the change being inspected, which makes it the most
expensive git read on the hook paths. Avoid calling it more than once per hook.
Do not memoize it either: a context-scoped cache was tried and removed, because
the write-free window it required cost more to maintain than the walk saved (see
`git log` on `gitrepo/status.go` for the measurements). The turn-start hook
currently walks twice — `CapturePrePromptState` and the strategy's prompt
attribution each read their own status.

Agent-hook capture paths must use `gitrepo.StatusWithBudget` instead: it bounds
the walk with a wall-clock budget (`gitrepo.StatusWalkBudget`) because go-git's walk is
not context-cancellable and a pathological worktree (e.g. a stray `git init` in
`$HOME`) otherwise leaves the hook process grinding for hours after the agent's
own hook timeout fires. On breach it returns an error wrapping
`gitrepo.ErrStatusBudgetExceeded` — hook callers warn and continue with
transcript-derived data (capture is fail-open; new-file detection is skipped for
the turn via the pre-prompt/pre-task `UntrackedScanSkipped` marker) — and a
process-local latch makes every later `StatusWithBudget` call in the same hook
process fail fast rather than re-entering the walk. The first-checkpoint
`git status` subprocess in the checkpoint store is bounded by the same
`StatusWalkBudget` (a killed child, not an abandoned goroutine) and reports the
same sentinel; the lifecycle handlers warn-and-skip the checkpoint on it. Turn
end persists whether the turn degraded (`SessionState.CaptureDegradedAt` — set
on breach, cleared by the next healthy turn) so `entire status` surfaces the
degradation instead of it living only in `.entire/logs`. Paths
where a user is actively waiting on a command (review, rewind, and
`session adopt` via `detectFileChangesUnbounded`) keep the unbounded
`gitrepo.Status`.

#### go-git v5 Bugs - Use CLI Instead

**Do NOT use go-git v5 for `checkout` or `reset --hard` operations.**

go-git v5 has a bug where `worktree.Reset()` with `git.HardReset` and `worktree.Checkout()` incorrectly delete untracked directories even when they're listed in `.gitignore`. This would destroy `.entire/` and `.worktrees/` directories.

Use the git CLI instead:

```go
// WRONG - go-git deletes ignored directories
worktree.Reset(&git.ResetOptions{
    Commit: hash,
    Mode:   git.HardReset,
})

// CORRECT - use git CLI
cmd := exec.CommandContext(ctx, "git", "reset", "--hard", hash.String())
```

See `CheckoutBranch()` in `git_operations.go` for an example.

#### Repo Root vs Current Working Directory

**Always use repo root (not `os.Getwd()`) when working with git-relative paths.**

Git commands like `git status` and `worktree.Status()` return paths relative to the **repository root**, not the current working directory. When an agent runs from a subdirectory (e.g., `/repo/frontend`), using `os.Getwd()` to construct absolute paths will produce incorrect results for files in sibling directories.

```go
// WRONG - breaks when running from subdirectory
cwd, _ := os.Getwd()  // e.g., /repo/frontend
absPath := filepath.Join(cwd, file)  // file="api/src/types.ts" → /repo/frontend/api/src/types.ts (WRONG)

// CORRECT - use repo root
repoRoot, _ := paths.WorktreeRoot()
absPath := filepath.Join(repoRoot, file)  // → /repo/api/src/types.ts (CORRECT)
```

This also affects path filtering. The `paths.ToRelativePath()` function rejects paths starting with `..`, so computing relative paths from cwd instead of repo root will filter out files in sibling directories:

```go
// WRONG - filters out sibling directory files
cwd, _ := os.Getwd()  // /repo/frontend
relPath := paths.ToRelativePath("/repo/api/file.ts", cwd)  // returns "" (filtered out as "../api/file.ts")

// CORRECT - keeps all repo files
repoRoot, _ := paths.WorktreeRoot()
relPath := paths.ToRelativePath("/repo/api/file.ts", repoRoot)  // returns "api/file.ts"
```

**When to use `os.Getwd()`:** Only when you actually need the current directory (e.g., finding agent session directories that are cwd-relative).

**When to use repo root:** Any time you're working with paths from git status, git diff, or any git-relative file list.

Test case in `state_test.go`: `TestFilterAndNormalizePaths_SiblingDirectories` documents this bug pattern.

#### Invoking Commands on Windows - Never Put a Dynamic Value on a cmd.exe Line

**When Entire performs the exec itself, do not go through `cmd.exe`.** Pass the
program and its arguments as separate argv elements (`exec.Command(prog, arg)`),
or call the Win32 API directly.

cmd.exe treats `&`, `|`, `<`, `>` as command separators/redirections and expands
`%VAR%`, and **Go's argv escaping will not protect you**: `syscall.EscapeArg`
only quotes an argument containing a space, tab, quote, or backslash. A URL,
a percent-encoded path, or any `&`-bearing string therefore reaches cmd.exe bare
and is silently cut at the first metacharacter. That is exactly how
`entire login` shipped a Windows build that opened
`…/authorize?client_id=entire-cli` and got rejected for a missing
`redirect_uri` — the `cmd /c start "" <url>` launcher lost everything from the
first `&` on, and because the truncation happened inside the released child, the
CLI saw a successful launch and printed no fallback URL.

Escaping is the right tool in exactly one situation: **a third party owns the
exec.** When Entire writes a command into an agent's config file (Cursor/Codex
`hooks.json`) and that agent runs it through cmd.exe, there is no shell to
avoid — use `agent.escapeWindowsCMD`. Reviewers should flag any new
`exec.Command("cmd", …)` / `"cmd.exe"` call site that interpolates a
non-constant value.

Key files: `cmd/entire/cli/browser_open_windows.go` (ShellExecute, the
avoid-the-shell side) and `cmd/entire/cli/agent/hook_command.go`
(`escapeWindowsCMD`, the third-party-exec side). Each doc comment points at the
other.

### Control-Plane Core Resolution (which core am I talking to?)

Control-plane commands dial one of three cores: the active context's
(`coreapi.New`), a specific cluster's (`coreapi.NewForCluster`), or — when
`ENTIRE_TOKEN` is set — the env token's `aud` (the bypass inside `New`/
`NewForCluster`). This precedence lives **only** inside `coreapi`; nothing else
re-derives it.

**To display which core a request uses, ask the client: `client.CoreOrigin()`.**
It returns whatever was actually wired in, so the shown core can never diverge
from where the request goes. **Do NOT** re-resolve with
`auth.ResolveControlPlaneTarget()` for display — it only knows the active
context and silently ignores both `ENTIRE_TOKEN` and the cluster case, so it can
name a core the request never touches (this was a real bug in the `mirror list`
banner; see `repo_mirror.go` and `coreapi.Client.CoreOrigin`).

When a command resolves auth *outside* a `coreapi.Client` (e.g. `entire auth
status`, which builds its own `/me` client), it must apply the same
env-token-first precedence itself — see `resolveAuthStatusTarget` /
`resolveEnvTokenStatusTarget` in `auth.go`, which branch on
`auth.EnvTokenVar` before falling back to the active context. `logout` is the
deliberate exception: it manages a *stored* login session, which an ephemeral
env token has none of, so it stays on the active context.

### Entire-API Cell Routing (which cell does a data-plane request go to?)

The data plane (entire-api) is deployed per jurisdiction; a repo placement
lives in exactly one cell, user `/me/*` activity is consolidated in the
caller's home cell, and no server-side cross-cell aggregator exists. The CLI
therefore has exactly three routing shapes, mirroring the entire.io BFF:

- **Repo-scoped → one cell**: `resolveRepoCellTarget` (`cell_target.go`) maps
  a repo (ULID or owner/repo) to the cell hosting it — via `GetRepo`'s
  `ClusterHost` for a ULID, or via the control plane's consolidated repos
  index (`ListRepos`) for owner/repo, resolved to the repo's PROCESSING
  placement (`primaries.processing`), not just any active mirror, since a
  repo can be mirrored in several regions but only one placement holds its
  actual data. NOT best-effort: any failure (not onboarded, no/failed/
  suspended processing placement, control-plane error, timeout) returns an
  error instead of falling back to home-jurisdiction routing — a wrong-region
  "success" is worse than a command failure for repo-scoped data. Used by
  trails and experts (`NewAuthenticatedEntireAPICellClient` in
  `api_client.go`). `resolveRepoCellPlacement` performs the same lookup for
  callers that also need the placement's id (repo_id) alongside its cell —
  used by cross-repo checkpoint reads (`explain --repo`, `explain_repo.go`).
- **User-scoped `/me` → home cell, never fan out**:
  `auth.NewEntireAPICellClient(ctx, insecure, nil)` routes by the
  `home_jurisdiction` JWT claim; activity/recap use it with a data-API
  fallback (`runAuthenticatedActivityAPI` in `entireapi_client.go`).
- **Repo-set queries → fan out and merge client-side**: `cell_fanout.go` —
  `groupReposByCell` (repo index → per-cell groups; the catalog join key is
  `ClusterSlug`↔`Cluster.Slug`, NOT the cell name, which the catalog does not
  expose), `resolveCellBaseURLs`, and `fanOutCells` (parallel per-cell calls,
  per-cell timeout, partial failures isolated per slot). Merge semantics stay
  with the command. Each repo routes to exactly ONE placement — its home,
  picked by `routedRepoPlacement` (the elected `primaries.processing`, else
  the canonical row-ID convention). Mirrors are never searched: they are
  replicated copies indexed under their own namespaces, so an extra leg
  returns duplicate and stale rows, and diverges from the web
  (ENT-1672/ENT-1776). Unlike the repo-scoped resolver above, this one fails
  SOFT — a home placement that is not ready is skipped and reported
  (`reportableSkippedRepos`: pinned requests always warn, broad ones only
  when the skips left no cell to query), never substituted with a ready
  mirror.

Token rule: identity tokens are **per-jurisdiction, not per-cell**. Multi-cell
callers must build one `auth.CellClientFactory`
(`NewEntireAPICellClientFactory`) per operation — it resolves the login
subject once and mints at most one token per jurisdiction. `fanOutCells` does
this automatically; do not call `NewEntireAPICellClient` in a loop.

### Session Strategy (`cmd/entire/cli/strategy/`)

The CLI uses a manual-commit strategy for managing session data and checkpoints. The strategy implements the `Strategy` interface defined in `strategy.go`.

#### Strategy Interface

The `Strategy` interface provides:

- `SaveStep()` - Save session step checkpoint (code + metadata)
- `SaveTaskStep()` - Save subagent task step checkpoint
- `GetRewindPoints()` / `Rewind()` - List and restore to checkpoints
- `GetSessionLog()` / `GetSessionInfo()` - Retrieve session data

`Rewind()` has **no CLI surface**. The `rewind` commands were removed, so
nothing outside tests calls it; `GetRewindPoints()` is still live, feeding
`checkpoint list --pending`. Treat the restore path as machinery pending
removal rather than as a supported entry point — do not build on it, and do not
re-expose it without deciding whether the removal was meant to be permanent.
Mind the split in its test coverage: `PreviewRewind` tests cover which untracked
files are *planned* for deletion, while
`TestShadowStrategy_Rewind_PreservesIgnoredFiles` covers what survives an actual
execution — that ignored paths such as `.entire/` are still there afterwards.
The second half is where the go-git hazard lives, so keep it covered for as long
as the restore path exists.

#### How It Works

The manual-commit strategy (`manual_commit*.go`) does not modify the active branch - no commits are created on the working branch. Instead it:

- Creates shadow branch `entire/<HEAD-commit-hash[:7]>-<worktreeHash[:6]>` per base commit + worktree
- **Worktree-specific branches** - each git worktree gets its own shadow branch namespace, preventing conflicts
- **Supports multiple concurrent sessions** - checkpoints from different sessions in the same directory interleave on the same shadow branch
- Condenses session logs to permanent `entire/checkpoints/v1` branch on user commits
- Every path that builds a stored transcript sanitizes before redacting; the committed paths also externalize images in between, giving **sanitize → externalize images → redact**. Sanitization is the agent's optional `agent.TranscriptSanitizer` capability, applied via `agent.SanitizeTranscriptForStorage`; it strips non-portable agent state (Codex's encrypted reasoning payloads and compaction blobs, which are bound to the originating session and cannot be replayed out of a checkpoint). The three paths are the Stop/shadow write (`lifecycle.go` sanitizes before `.entire/metadata/<session>/full.jsonl` is written, then the metadata-dir walker redacts it into the shadow tree — **no externalization**, so inline images in a shadow transcript are subject to redaction and assets exist only under committed checkpoints), post-commit condensation (`prepareTranscriptForStorage` in `manual_commit_condensation.go`), and the Stop finalize full-session rewrite (`manual_commit_hooks.go`). Where all three steps run, order is load-bearing at each step: sanitizing first avoids externalizing images out of items about to be discarded (storing an asset whose referencing line disappears) and avoids redacting megabytes of ciphertext only to discard it — base64 is the pathological input for the entropy layer, so a large Codex rollout otherwise costs tens of seconds per Stop *and* per commit; externalizing before redaction is required because redaction would otherwise flag and destroy the high-entropy base64. Sanitization is idempotent, so downstream paths call it without knowing whether an upstream path already did (`checkpoint.sanitizeForAgentType` is the store's belt-and-braces call). The agent's own transcript is never modified. Coupling to respect: `SessionState.CheckpointTranscriptSize` is a growth baseline compared against the shadow transcript blob size in `sessionHasNewContent`, so it must be measured in the same sanitized (pre-externalization) coordinate — that is `CondenseResult.TranscriptSizeBaseline`; using the raw size makes the comparison false forever and the session silently stops condensing.
- **Redaction cost and the two mechanisms that contain it.** Redaction dominates the Stop hook on a large session: it is ~99.7% of the metadata-walk blob write (git object writing is milliseconds), and ~82% of that is the betterleaks regex ruleset. Two things keep it bounded, and both rest on redaction being **per-line and stateless** (`redact.redactJSONLLines`) — a rule that looked at neighbouring lines would silently break both:
  - `jsonlContentImpl` shards the line pass into ~1MiB byte-balanced groups across goroutines. Output is byte-identical to the sequential pass. Sharding is gated on an explicit `concurrencySafe` argument describing the **redactor**, not on which entry point was called: `String` is pure and opts in (including `batch.go`'s OPF-disabled fast path), while the OPF collector closures accumulate into a shared map/slice and pass `concurrencyUnsafeRedactor`. Shards are balanced by bytes rather than line count because transcripts mix short lines with occasional multi-MB tool results.
  - The checkpoint metadata walk reuses the previous checkpoint's redacted blob as a prefix and redacts only appended lines (`checkpoint/redact_cache.go`), turning a per-Stop cost of O(whole transcript) into O(appended). The stored prefix must always end immediately after a `\n`, which is what makes plain byte concatenation reproduce the full result; content with a partial trailing line is therefore never cached. Eligibility is keyed on `paths.TranscriptFileName` (`full.jsonl`), **not** a `.jsonl` suffix — `transcript.jsonl` is regenerated in full each checkpoint and `full.jsonl.001` chunks are not appended, so neither should qualify. Reuse requires the prefix bytes to still hash the same and `redactionFingerprint()` (CLI version + commit + `redact.ConfigFingerprint()`) to match, so a rewritten transcript, changed custom rules, or a CLI upgrade all fall back to a full redaction. Bump `configFingerprintVersion` in `redact/fingerprint.go` whenever the regex layers change behaviour, or stale output can be reused. The cache lives in the git common dir (via the memoized `resolveGitCommonDir`), never under `.entire/`, because anything in the metadata directory would be walked into the checkpoint tree and committed. Only the shadow write is covered: condensation and the Stop finalize rewrite still redact whole transcripts, inheriting the sharding but not the prefix reuse.
- Each committed session stores the (sanitized, redacted) transcript (`full.jsonl`, read by CLI resume/explain) plus a best-effort compact transcript (`transcript.jsonl`, generated via `transcript/compact`). Like `full.jsonl`, `transcript.jsonl` stores the **full compacted session** on every checkpoint (via `compact.FullWithBoundary`), so each checkpoint is self-contained and the session survives a mid-history checkpoint being lost/reverted/rebased. This checkpoint's slice begins at the session metadata's `compact_transcript_start` (a line offset in compact-output coordinates, distinct from `checkpoint_transcript_start` which indexes raw `full.jsonl` lines); a nil/absent marker means a legacy delta-only `transcript.jsonl` (read from line 0). The marker rounds toward inclusion when a streaming message straddles the boundary, so the slice never drops this checkpoint's content but may repeat ≤1 merged line at its head. Compact generation is best-effort and is skipped when the compacted output exceeds the 50MB blob cap (unlike `full.jsonl`, `transcript.jsonl` is not chunked — `full.jsonl` stays authoritative and the compact is regenerable); in the OPF finalize rewrite a failed/skipped regeneration drops the prior `transcript.jsonl` and clears the marker rather than shipping a stale, less-redacted compact. Both files are pushed with the v1 branch. The root `metadata.json` `sessions[].transcript` pointer keeps targeting `full.jsonl`; when the compact transcript was generated the session entry also carries a `compact_transcript` path pointing at `transcript.jsonl` (omitted otherwise) so external readers can locate it next to `full.jsonl`.
- **Subagent task records** - subagent work (Claude Code's Task tool) is captured as durable `session.TaskRecord` entries on session state, a pointer mid-turn (declared transcript path + labels; background launches record at launch, completions attach files/tokens/path exactly-once via `strategy.CompleteTaskRecord`, Factory Droid Workers upsert). Condensation materializes each record — declared path first, agent-layout fallback, same sanitize → externalize → redact pipeline — into `tasks/<toolUseID>/{agent-<agentID>.jsonl, task.json}` inside the parent session's checkpoint (unavailable transcript → `task.json` with a stable path-free reason); live records store transcript-so-far each condensation, completed records are removed after a successful write. `State.HasTaskContent()` is the trigger currency: records-only sessions condense, records never live on the shadow branch, and shadow-branch existence does not imply task content (shadow pinning keys on `StepCount` only). `SaveTaskStep` is incremental-only (post-todo).
- Uses the `post-rewrite` Git hook to keep local session linkage aligned after amend/rebase rewrites
- Builds git trees in-memory using go-git plumbing APIs
- Rewind restores files from shadow branch commit tree (does not use `git reset`)
- **Location-independent transcript resolution** - transcript paths are always computed dynamically from the current repo location (via `agent.GetSessionDir` + `agent.ResolveSessionFile`), never stored in checkpoint metadata. This ensures restore/rewind works after repo relocation or across machines.
- **Token usage scoping** - `SessionState.TokenUsage` is the session-wide total used by `entire status`; `SessionState.CheckpointTokenUsage` is the pending checkpoint delta since the last condensation. Checkpoint metadata must stay scoped to `CheckpointTranscriptStart` or the pending checkpoint delta. Cursor tokens come only from stop-hook payloads, while Copilot CLI can also backfill full-session totals from `session.shutdown`. Condensation's transcript recompute runs with `subagentsDir=""` and so drops `SubagentTokens`; `withSubagentTokensFrom` refills it from the already-rescoped `state.CheckpointTokenUsage`, and the store sums it across a checkpoint's sessions via `types.AddTokenUsage` (the single token-summing primitive — do not hand-roll another; a field-by-field copy is how the nested total came to be dropped in the first place).
- Tracks session state in `.git/entire-sessions/` (shared across worktrees)
- **Reclaiming sessions whose agent vanished** - not every agent fires a session-end hook, and any agent can be killed before its hook runs, so a session can be left un-finalized forever. `SessionState.Owner` fingerprints the owning agent process (PID + start time + boot + host, captured at every turn start by `captureSessionOwner`), and `State.OwnerExited()` reports it gone via `proclive.Check`. `finalizeExitedSessions` sweeps those inside `entire status` (text and `--json`) and `entire doctor`, ending them exactly as a clean stop would. **`OwnerExited` deliberately covers IDLE as well as ACTIVE** — an agent that finishes its last turn and then quits leaves IDLE, so gating on ACTIVE alone missed the common case; only already-finalized sessions are excluded, per the shared `State.IsEnded()` predicate. Liveness is Unknown on Windows and for cross-host state, where behaviour degrades to the `StuckActiveThreshold` timeout. Because the sweep runs inside interactive commands, its eager condensing is capped by `sweepCondenseBudget` across the whole sweep: every candidate is always marked ENDED (a single atomic rename — that is what un-sticks it from `entire status`), while condensing runs only while the budget lasts, so a multi-day backlog drains over successive invocations instead of stalling one. Skipping a condense is the existing fail-open path — PostCommit retries, and `doctor` reports the session as "ended with uncondensed checkpoint data".
- **Session-end hooks that run under a host deadline** - an agent whose session-end hook fires inside its own shutdown may be killed part-way through. Such agents declare `agent.SessionEndBudgeter` (Codex: 3s cap, process tree killed on expiry). The budget bounds **only** the eager condense in `endSessionNow`, so the cheap step that un-sticks the session is never the one given up on; it is set below the host cap with enough headroom for the shell wrapper and binary load, which happen before Entire's own clock starts. Unbounded is not the same as guaranteed, though: mark-ended runs under `MutateSessionState`, whose flock acquire blocks, so a concurrent condense holding the lock can push it past the cap and get the tree killed — the exited-owner sweep above is the backstop. The bound is best-effort (condensation doesn't poll ctx between stages), and being cut off costs duplication rather than data: an incomplete condense leaves `FullyCondensed` false and PostCommit retries, except in the window between the v1 checkpoint write and the state save, where the checkpoint is already committed and PostCommit mints a second one over the same transcript range.
- **Shadow branch migration** - if user does stash/pull/rebase (HEAD changes without commit), shadow branch is automatically moved to new base commit
- **Orphaned branch cleanup** - if a shadow branch exists without a corresponding session state file, it is automatically reset when a new session starts
- PrePush hook can push `entire/checkpoints/v1` branch alongside user pushes
- **Single checkpoint sync remote** - checkpoint data syncs only to the elected checkpoint sync remote (`strategy_options.checkpoint_push_remote` setting → captured election → `origin` → sole remote → first remote in `.git/config` order; fail-closed only when the setting names a missing remote — the captured tier is fail-soft), on both the git-branch and git-refs backends. **Capture**: during pre-push, a push whose target agrees with the branch's declared push destination (`branch.<name>.pushRemote` → `remote.pushDefault` → `branch.<name>.remote`) and differs from the current election persists that remote as the captured election (git common dir, `entire-checkpoint-sync-remotes.json`), announces it on stderr, and the same push carries the checkpoints — see `strategy/checkpoint_sync_capture.go`. Declaration alone is deliberately **not** a tier: electing from tracking config at rest silently drops checkpoint sync on every push to a different remote (`git push <other> HEAD`, a `git clone -o base` pushing checkpoints to a separately added `origin`, any repo with `remote.pushDefault` — the `74e239a9` regression), and a bare push to an undeclared remote never elects (the pre-single-remote transcript leak). Phase-1 capture rules: at most one captured remote, first still-configured capture sticks (no per-push ping-pong in mixed-habit repos; a captured remote that was renamed or removed no longer vetoes the next capture), explicit `checkpoint_push_remote` outranks and disables capture. Pushes to any other remote or to a raw URL never carry checkpoint data; the dedicated `checkpoint_remote` URL mode is exempt. `entire status` shows the sync destination, its source (`observed` renders as "follows your branch's push destination"), and an unpushed-checkpoint count.
- **Checkpoint reads follow the election** - reads consult an ordered candidate chain: the elected sync remote first, then `origin` as a **read-only legacy tier** (pre-single-remote checkpoints live there; `strategy.CheckpointReadRemotes` / `CheckpointReadRemotesWithElection` resolve the chain, failing OPEN to `[origin]` on an election error). Metadata-branch fetches refresh every candidate tracking ref because branch existence does not prove a requested checkpoint exists there; other fetch targets, tracking-ref readers, blob hydration, and checkpoint-policy reads try candidates in order. Git-refs remote discovery ls-remotes every candidate **without** needing a dedicated `checkpoint_remote` and merges the listings. Local-ref writers stay **elected-remote-only**: `EnsurePrimaryRef`, the metadata-fetch advance, `promoteRemoteTrackingPrimary`, and the local checkpoint-policy ref update never act on the legacy tier (a stale origin feeding `SafelyAdvanceLocalRef` is the #1374-class hazard), keyed on the explicit election result, never on the chain's first entry. A remoteless repo keeps its "checkpoint absent" classification, which requires positive evidence (successful empty `git remote` listing, live ctx, readable settings without a `checkpoint_remote` key).
- **Scanner engine selection is a settings-only, fail-closed choice**: `redaction.betterleaks.enabled` (default `true`) and `redaction.goredact.enabled` (default `false`) pick which pattern-matching engine(s) feed layer 2 of `detectAllLayers` (`redact.ConfigureScanners`, `redact/scanners.go`). Both keys are honored from committed `.entire/settings.json` only — a `settings.local.json` copy is ignored with a logged warning, because the choice affects everyone who reads the repo's checkpoints, not just the developer who set it. `validateScannerSettings` fails settings load with `settings.ErrScannerConfig` when both engines are disabled. If goredact is the sole enabled engine and its scan fails at runtime, `redact.ErrScannerDegraded` propagates out of `JSONLBytes`/`JSONLBytesWithPrivacyFilter`; every checkpoint-write call site (`checkpoint/ephemeral.go`, `checkpoint/persistent.go`, `strategy/manual_commit_hooks.go`) must `errors.Is` for it and fail the write rather than fall back to under-scanned content. See `docs/security-and-privacy.md` for user-facing details.
- **OPF (OpenAI Privacy Filter) runs at pre-push, not post-commit**: when `redaction.openai_privacy_filter.enabled` is true, the PrePush hook re-redacts unpushed commits with the OPF 9th layer, builds new commits carrying an `Entire-OPF-Applied: true` trailer, and updates the local refs before pushing. Per-commit condensation stays on the fast 8-layer pipeline. **Both backends are covered, by two rewrites sharing one policy**: `manual_commit_opf_rewrite.go` walks unpushed `entire/checkpoints/v1` commits bounded by the remote tip and CAS-updates the branch; `manual_commit_opf_refs.go` walks the push queue and, per queued ref, rewrites every unpushed commit — stopping at the first commit already carrying the trailer, which makes the trailer its own watermark (steady state stops at the tip's parent) and is bounded by the shared `BootstrapTooLargeError` cap for repos that enable OPF late. Blob policy, byte caps, the single batched shell-out, and the error taxonomy are shared; only discovery and ref update differ. **Fail-closed differs by backend on purpose**: git-branch aborts the user's push, while git-refs withholds the (separately-pushed) checkpoint refs and leaves them queued, so nothing under-redacted ships without blocking the user's own push. See `docs/security-and-privacy.md` for the full flow, including divergence detection, bootstrap caps, and CAS-on-conflict semantics.
- **OPF's `command` is a trust boundary, not a setting**: `redaction.openai_privacy_filter.command` becomes `argv[0]` of an exec during pre-push, and `.entire/settings.json` is version-controlled — so reading it from the project file would let a pull request execute code on every developer who pushes (the prompt is no defense: it never names the command, `prompt_default: "always"` skips it, and non-TTY pushes auto-run). `settings.enforceOPFCommandTrust` (`settings/opf_command_trust.go`) honors `command` only from `.entire/settings.local.json`, and only when that file is untracked in **both** the index and `HEAD` — the filename is not the check, because `.gitignore` does not apply to an already-tracked path. The probe goes through go-git (`gitrepo.OpenPath`), not the git CLI, and is memoized per process: shelling out cost ~15 subprocesses per hook (`settings.Load` is uncached and runs several times per hook) and would fail verification wherever `git` is off `$PATH` — the GUI-git-client population that most needs an explicit `command`. **Two depths**: the layer check reads the index only, because a PR-delivered file is always in the index of a clone that checks it out, and checkout cannot produce a file absent from the index; the `command` check also reads HEAD. That split matters because HEAD is the expensive half — measured 8.3ms → 2.5ms per hook process in this repo, and 39ms → 11ms on reftable, where `gitrepo` routes reference reads back through the git CLI. The cost falls on everyone with a `settings.local.json`, not just OPF users, so keep it off the HEAD path. Rejection is a downgrade to the documented `$PATH` default plus a warning, never a hard error; verification failure (no repo, git missing) counts as untrusted. Do not add other exec-bearing fields to the project settings file without the same gate.
- **A tracked `.entire/settings.local.json` is ignored wholesale**: the local layer's premise is that it is per-clone and per-developer (it is gitignored, `entire enable --local` writes it, and `CheckpointRemoteIsLocalOnly` treats presence there as proof the developer chose it). `.gitignore` does not apply to an already-tracked path, so a committed one arrives by cloning and would override project settings for everyone. `loadMergedSettings` drops the layer when the file is **proven** tracked, records `EntireSettings.LocalLayerRejection()`, and the redaction consumer prints it with the `git rm --cached` fix. It never errors — one committed file must not brick `status`/`doctor`. Two deliberately opposite failure directions, expressed as the three-state `localTrust` (`localUnverifiable` is the zero value so a forgotten assignment fails safe): an *unverifiable* repo keeps the layer (losing all local settings is worse than the risk) but still drops the exec-bearing OPF `command` (being wrong means running someone else's binary); *no* repository counts as proof of locality. `CheckpointRemoteIsLocalOnly` reads the raw file outside the loader, so it repeats the check itself.
- Safe to use on main/master since it never modifies commit history

#### Key Files

- `strategy.go` - Interface definition and context structs (`StepContext`, `TaskStepContext`, `RewindPoint`, etc.)
- `common.go` - Helpers for metadata extraction, tree building, rewind validation, `ListCheckpoints()`
- `manual_commit*.go` - Manual-commit strategy: main impl, types, session state, condensation, rewind, git ops, logs, hook handlers (prepare-commit-msg, post-commit, post-rewrite, pre-push), reset
- `manual_commit_opf_rewrite.go` - Pre-push OPF re-redaction: walks unpushed v1 commits, runs OPF over their blobs, rebuilds commits with `Entire-OPF-Applied: true` trailer, CAS-updates the local ref. Sentinel error types (use `errors.As`): `V1DivergedError`, `BootstrapTooLargeError`, `V1RefMovedError`, `OPFRuntimeFailedError`, `OPFBatchTooLargeError`, `OPFRawBytesTooLargeError`, `OPFNoCategoriesError` (OPF enabled with zero effective categories — the pre-push decision and the rewrite both fail closed instead of stamping the trailer without a scan).
- `manual_commit_opf_refs.go` - The git-refs half of pre-push OPF: `RewriteQueuedCheckpointRefsWithOPF` walks the push queue and rewrites every unpushed commit on each queued ref, reusing the v1 rewrite's blob walk, caps, and error types. See the OPF bullet above for why the two backends fail closed differently.
- `settings/opf_command_trust.go` - Ownership gate for the executed OPF `command` (local-only + untracked); see the OPF trust-boundary bullet above
- `cleanup.go` - Cleanup discovery/deletion for shadow branches, session states, and checkpoint metadata
- `session_state.go` - Package-level session state functions
- `hooks.go` - Git hook installation

Note: `checkpoint/configloader.go` overrides go-git's default config loader with a symlink-following `billy.Basic` (`osSymlinkFS`) — go-git's default reads config via `os.Root`, which rejects absolute symlinks in any path component (e.g. a `~/.config` managed by a dotfile tool), silently dropping global config so author identity fell back to "Unknown" and signing was skipped.

#### Deep-Dive Reference

The phase state machine, metadata directory layout, sharded checkpoint format, multi-session metadata, checkpoint ID linking, commit trailers, and concurrent-session / shadow-branch-migration behavior are documented in:

- [Sessions and Checkpoints](docs/architecture/sessions-and-checkpoints.md) - domain model, storage layout, checkpoint ID linking, commit trailers, package structure
- [Checkpoint Scenarios](docs/architecture/checkpoint-scenarios.md) - phase state machine and worked condensation scenarios
- [Ref-Based Checkpoint Backend](docs/architecture/ref-checkpoint-backend.md) - git-refs backend: primary/mirror taxonomy, ref layout + sharding, push-discovery queue, read routing, config + rollout

#### When Modifying the Strategy

- The strategy must implement the full `Strategy` interface
- Test with `mise run test` - strategy tests are in `*_test.go` files
- Keep this file and `docs/architecture/sessions-and-checkpoints.md` current when changing strategy behavior (`AGENTS.md` is a symlink to this file)

### `entire review` Command

`entire review` runs a configured review profile. Keep documentation brief and user-facing.

See [Review Command](docs/architecture/review-command.md) for usage, minimal profile config, and key files.

### Agent-Safe CLI Fallbacks

When building CLI features, do not make useful output available only through a
TUI, picker, wizard, terminal selection menu, confirmation dialog, or stdin
question. Agents must be able to complete the same read-only workflow from a
non-interactive terminal.

Plain text output is acceptable when it contains the full information needed for
the workflow. JSON is preferred for structured data, following existing patterns
such as `--json` on `status`, `agent-help`, `sessions`, `search`, and trail
finding commands. Long human-readable output may use a pager in TTY mode, but
must provide a bypass like the existing `--no-pager` pattern on `explain`.

For interactive browsing flows, provide one of these non-interactive shapes:

- a list command that prints stable identifiers, plus a show/detail command that
  accepts an identifier
- a flag or positional argument that selects the item directly
- a complete text or JSON fallback when stdout is not a terminal, like existing
  static/text fallbacks for TUI-backed commands

When reviewing CLI changes, inspect terminal-gated paths such as
`IsTerminalWriter`, `CanPromptInteractively`, Bubble Tea, `huh`, direct stdin
reads, terminal selection menus, confirmation dialogs, and wizard flows. Flag
the change if a non-interactive agent can only see a menu, preview, truncated
summary, or cannot select the item whose details matter.

Tests for interactive CLI features should cover the non-interactive path. See
the "Spawning subprocesses in tests (TTY detection)" section above for the
`execx.NonInteractive` pattern when testing a real `entire` command.

Existing good patterns:

- `entire investigate --findings` prints a complete plain-text list and includes
  `view: entire investigate show <run-id>` hints.
- `entire investigate show <run-id>` prints the saved investigation summary and
  findings without needing a TUI.
- `entire repo clone /gh/...` prompts only when several clusters are possible;
  without a TTY it asks for `--cluster`.
- `entire experts --tui` is safe because the TUI is opt-in and non-TTY output
  falls back to deterministic plain text.
- `entire explain --no-pager` is the local pattern for avoiding pager-only long
  text output.
- `entire status --json`, `entire agent-help --json`, `entire sessions list --json`,
  and trail finding commands show the local `--json` convention.

Do not require JSON everywhere. Human-readable text is fine if it contains the
complete information an agent needs. The failure mode is requiring an
interactive terminal to select something or reveal details.

# Important Notes

- **Before committing:** Follow the "Before Every Commit (REQUIRED)" checklist above - CI will fail without it
- Integration tests: run `mise run test:integration` when changing integration test code
- When adding new features, ensure they are well-tested and documented.
- Always check for code duplication and refactor as needed.

## Go Code Style

- Write lint-compliant Go code on the first attempt. Before outputting Go code, mentally verify it passes `golangci-lint` (or your specific linter).
- Follow standard Go idioms: proper error handling, no unused variables/imports, correct formatting (gofmt), meaningful names.
- Handle all errors explicitly—don't leave them unchecked.
- Reference `.golangci.yml` for enabled linters before writing Go code.

## Accessibility

The CLI supports an accessibility mode for users who rely on screen readers. This mode uses simpler text prompts instead of interactive TUI elements.

### Environment Variable

- `ACCESSIBLE=1` (or any non-empty value) enables accessibility mode
- Users can set this in their shell profile (`.bashrc`, `.zshrc`) for persistent use

### Implementation Guidelines

When adding new interactive forms or prompts using `huh`:

**In the `cli` package:**
Use `NewAccessibleForm()` instead of `huh.NewForm()`:

```go
// Good - respects ACCESSIBLE env var
form := NewAccessibleForm(
    huh.NewGroup(
        huh.NewSelect[string]().
            Title("Choose an option").
            Options(...).
            Value(&choice),
    ),
)

// Bad - ignores accessibility setting
form := huh.NewForm(...)
```

**In the `strategy` package:**
Use the `isAccessibleMode()` helper. Note that `WithAccessible()` is only available on forms, not individual fields, so wrap confirmations in a form:

```go
form := huh.NewForm(
    huh.NewGroup(
        huh.NewConfirm().
            Title("Confirm action?").
            Value(&confirmed),
    ),
)
if isAccessibleMode() {
    form = form.WithAccessible(true)
}
if err := form.Run(); err != nil { ... }
```

### Key Points

- Always use the accessibility helpers for any `huh` forms/prompts
- Test new interactive features with `ACCESSIBLE=1` to ensure they work
- The accessible mode is documented in `--help` output
