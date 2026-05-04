# Entire - CLI

This repo contains the CLI for Entire.

## Architecture

- CLI built with github.com/spf13/cobra and github.com/charmbracelet/huh

## Key Directories

### Commands (`cmd/`)

- `entire/`: Main CLI entry point
- `entire/cli`: CLI utilities and helpers (Cobra commands, helpers, group roots)
- `entire/cli/commands`: actual command implementations
- `entire/cli/agent`: agent implementations (Claude Code, Gemini CLI, OpenCode, Cursor, Factory AI Droid, Copilot CLI) - see [Agent Integration Checklist](docs/architecture/agent-integration-checklist.md) and [Agent Implementation Guide](docs/architecture/agent-guide.md)
- `entire/cli/strategy`: strategy implementation (manual-commit) - see section below
- `entire/cli/checkpoint`: checkpoint storage abstractions (temporary and committed)
- `entire/cli/session`: session state management
- `entire/cli/integration_test`: integration tests (simulated hooks)
- `e2e/`: E2E tests with real agent calls (see [e2e/README.md](e2e/README.md))

### Command Layout

The CLI is organized around five noun groups plus a small set of top-level
verbs. The groups are the canonical home for each verb; legacy top-level
shortcuts remain functional but hidden, and emit a deprecation hint pointing
at the canonical group form.

- `session` (alias: `sessions`): `list`, `info`, `stop`, `attach`, `resume`, `current`
- `checkpoint` (aliases: `cp`, `checkpoints`): `list`, `explain`, `rewind`, `search`
- `agent`: bare opens the interactive agent selector, plus `list`, `add`, `remove`
- `configure`: bare prints help and a hint pointing at `entire agent`; flags
  manage non-agent settings (telemetry, git-hook installation mode, strategy
  options, summary provider). Agent CRUD lives under `entire agent`.
- `auth`: `login`, `logout`, `status`, `list`, `revoke`
- `doctor`: bare runs the scan-and-fix flow, plus `trace`, `logs`, `bundle`

Top-level lifecycle and standalone commands: `enable`, `disable`, `status`,
`login`, `logout`, `clean`, `version`, `dispatch`, `activity`, `help`,
`configure`.

Hidden top-level shortcuts (functional, emit a one-line deprecation hint):
`rewind` → `checkpoint rewind`, `resume` → `session resume`, `attach` →
`session attach`, `explain` → `checkpoint explain`, `trace` → `doctor trace`.
Cobra-native aliases (no hint): `sessions` → `session`, `cp`/`checkpoints` →
`checkpoint`. The `search` top-level remains hidden without a hint.

Deprecated top-level alias (functional, prints cobra deprecation message):
`reset` → `clean`.

Hidden infrastructure commands: `hooks`, `migrate`, `trail`,
`curl-bash-post-install`, `__send_analytics`.

The `hideAsAlias(cmd, canonical)` helper in `cmd/entire/cli/aliascmd.go`
marks a command Hidden and sets cobra's `Deprecated` field so the hint
renders to stderr on every invocation while the command stays functional.
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

The Vogon agent is a deterministic fake agent that exercises the full E2E test suite without making any API calls. Named after the Vogons from The Hitchhiker's Guide to the Galaxy — bureaucratic, procedural, and deterministic to a fault.

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
mise run test:e2e --agent claude-code [filter]       # Claude Code only
mise run test:e2e --agent gemini-cli [filter]        # Gemini CLI only
mise run test:e2e --agent opencode [filter]          # OpenCode only
mise run test:e2e --agent cursor [filter]            # Cursor only
mise run test:e2e --agent factoryai-droid [filter]   # Factory AI Droid only
mise run test:e2e --agent copilot-cli [filter]       # Copilot CLI only
```

E2E tests:

- Use the `//go:build e2e` build tag
- Located in `e2e/tests/`
- See [`e2e/README.md`](e2e/README.md) for full documentation (structure, debugging, adding agents)
- Test real agent interactions (Claude Code, Gemini CLI, OpenCode, Cursor, Factory AI Droid, Copilot CLI, or Vogon creating files, committing, etc.)
- Validate checkpoint scenarios documented in `docs/architecture/checkpoint-scenarios.md`
- Support multiple agents via `E2E_AGENT` env var (`claude-code`, `gemini`, `opencode`, `cursor`, `factoryai-droid`, `copilot-cli`, `vogon`)

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

### Spawning subprocesses in tests (TTY detection)

Tests that spawn the real `entire` or `git` binary need the child to be non-interactive so prompts don't hang on a developer terminal.

`interactive.CanPromptInteractively()` resolves in this order:

1. `ENTIRE_TEST_TTY=1` → force interactive ON (any other non-empty value → force OFF).
2. `testing.Testing()` → false. In-process `go test` runs are non-interactive by default; no per-test `t.Setenv("ENTIRE_TEST_TTY", "0")` is needed.
3. Agent sentinels (`GEMINI_CLI`, `COPILOT_CLI`, `PI_CODING_AGENT`, `GIT_TERMINAL_PROMPT=0`) → false.
4. `CI=<non-empty-non-false>` → false.
5. `/dev/tty` probe.

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
For normal errors where the default error message is sufficient, just return the error directly. main.go will print it:

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
- **Enabling debug/perf logs locally**: Prefer adding `"log_level": "DEBUG"` to `.entire/settings.local.json` when you need detailed hook/perf logs. This file is gitignored, so it is a low-risk local-only change. `ENTIRE_LOG_LEVEL=debug` also works and takes precedence.
- **User-facing output**: Use `fmt.Fprint*(cmd.OutOrStdout(), ...)` or `cmd.ErrOrStderr()`.

Don't use `fmt.Print*` for operational messages (checkpoint saves, hook invocations, strategy decisions) - those should use the `logging` package.

**Privacy**: Don't log user content (prompts, file contents, commit messages). Log only operational metadata (IDs, counts, paths, durations).

### Git Operations

We use github.com/go-git/go-git for most git operations, but with important exceptions:

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

See `HardResetWithProtection()` in `common.go` and `CheckoutBranch()` in `git_operations.go` for examples.

Regression tests in `hard_reset_test.go` verify this behavior - if go-git v6 fixes this issue, those tests can be used to validate switching back.

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

### Session Strategy (`cmd/entire/cli/strategy/`)

The CLI uses a manual-commit strategy for managing session data and checkpoints. The strategy implements the `Strategy` interface defined in `strategy.go`.

#### Strategy Interface

The `Strategy` interface provides:

- `SaveStep()` - Save session step checkpoint (code + metadata)
- `SaveTaskStep()` - Save subagent task step checkpoint
- `GetRewindPoints()` / `Rewind()` - List and restore to checkpoints
- `GetSessionLog()` / `GetSessionInfo()` - Retrieve session data
- `ListSessions()` / `GetSession()` - Session discovery

#### How It Works

The manual-commit strategy (`manual_commit*.go`) does not modify the active branch - no commits are created on the working branch. Instead it:

- Creates shadow branch `entire/<HEAD-commit-hash[:7]>-<worktreeHash[:6]>` per base commit + worktree
- **Worktree-specific branches** - each git worktree gets its own shadow branch namespace, preventing conflicts
- **Supports multiple concurrent sessions** - checkpoints from different sessions in the same directory interleave on the same shadow branch
- Condenses session logs to permanent `entire/checkpoints/v1` branch on user commits
- Uses the `post-rewrite` Git hook to keep local session linkage aligned after amend/rebase rewrites
- Builds git trees in-memory using go-git plumbing APIs
- Rewind restores files from shadow branch commit tree (does not use `git reset`)
- **Location-independent transcript resolution** - transcript paths are always computed dynamically from the current repo location (via `agent.GetSessionDir` + `agent.ResolveSessionFile`), never stored in checkpoint metadata. This ensures restore/rewind works after repo relocation or across machines.
- **Copilot token scoping** - Copilot CLI `session.shutdown` contains session-wide token aggregates. Checkpoint metadata must stay scoped to `CheckpointTranscriptStart`; condensation may separately backfill full-session Copilot totals into session state for `entire status`.
- Tracks session state in `.git/entire-sessions/` (shared across worktrees)
- **Shadow branch migration** - if user does stash/pull/rebase (HEAD changes without commit), shadow branch is automatically moved to new base commit
- **Orphaned branch cleanup** - if a shadow branch exists without a corresponding session state file, it is automatically reset when a new session starts
- PrePush hook can push `entire/checkpoints/v1` branch alongside user pushes
- Safe to use on main/master since it never modifies commit history

#### Key Files

- `strategy.go` - Interface definition and context structs (`StepContext`, `TaskStepContext`, `RewindPoint`, etc.)
- `common.go` - Helpers for metadata extraction, tree building, rewind validation, `ListCheckpoints()`
- `session.go` - Session/checkpoint data structures
- `push_common.go` - PrePush logic for pushing `entire/checkpoints/v1` branch
- `manual_commit.go` - Manual-commit strategy main implementation
- `manual_commit_types.go` - Type definitions: `SessionState`, `CheckpointInfo`, `CondenseResult`
- `manual_commit_session.go` - Session state management (load/save/list session states)
- `manual_commit_condensation.go` - Condense logic for copying logs to `entire/checkpoints/v1`
- `manual_commit_rewind.go` - Rewind implementation: file restoration from checkpoint trees
- `manual_commit_git.go` - Git operations: checkpoint commits, tree building
- `manual_commit_logs.go` - Session log retrieval and session listing
- `manual_commit_hooks.go` - Git hook handlers (prepare-commit-msg, post-commit, post-rewrite, pre-push)
- `manual_commit_reset.go` - Shadow branch reset/cleanup functionality
- `cleanup.go` - Cleanup discovery/deletion, including archived v2 generation retention
- `generation_repair.go` - Archived v2 generation metadata repair from raw transcript timestamps
- `session_state.go` - Package-level session state functions (`LoadSessionState`, `SaveSessionState`, `ListSessionStates`, `FindMostRecentSession`)
- `hooks.go` - Git hook installation

#### Checkpoint Package (`cmd/entire/cli/checkpoint/`)

- `checkpoint.go` - Data types (`Checkpoint`, `TemporaryCheckpoint`, `CommittedCheckpoint`)
- `store.go` - `GitStore` struct wrapping git repository
- `temporary.go` - Shadow branch operations (`WriteTemporary`, `ReadTemporary`, `ListTemporary`)
- `committed.go` - Metadata branch operations (`WriteCommitted`, `ReadCommitted`, `ListCommitted`)

#### Session Package (`cmd/entire/cli/session/`)

- `session.go` - Session data types and interfaces
- `state.go` - `StateStore` for managing `.git/entire-sessions/` files
- `phase.go` - Session phase state machine (phases, events, transitions, actions)

#### Session Phase State Machine

Sessions track their lifecycle through phases managed by a state machine in `session/phase.go`:

**Phases:** `ACTIVE`, `IDLE`, `ENDED`

**Events:**

- `TurnStart` - Agent begins a turn (UserPromptSubmit hook)
- `TurnEnd` - Agent finishes a turn (Stop hook)
- `GitCommit` - A git commit was made (PostCommit hook)
- `SessionStart` - New session started
- `SessionStop` - Session explicitly stopped

**Key transitions:**

- `IDLE + TurnStart → ACTIVE` - Agent starts working
- `ACTIVE + TurnEnd → IDLE` - Agent finishes turn
- `ACTIVE + GitCommit → ACTIVE` - User commits while agent is working (condense immediately)
- `IDLE + GitCommit → IDLE` - User commits between turns (condense immediately)
- `ENDED + GitCommit → ENDED` - Post-session commit (condense if files touched)

The state machine emits **actions** (e.g., `ActionCondense`, `ActionUpdateLastInteraction`) that hook handlers dispatch to strategy-specific implementations.

#### Metadata Structure

**Shadow branches** (`entire/<commit-hash[:7]>-<worktreeHash[:6]>`):

```
.entire/metadata/<session-id>/
├── full.jsonl               # Session transcript
├── prompt.txt               # Checkpoint-scoped user prompts
└── tasks/<tool-use-id>/     # Task checkpoints
    ├── checkpoint.json      # UUID mapping for rewind
    └── agent-<id>.jsonl     # Subagent transcript
```

**Metadata branch** (`entire/checkpoints/v1`) - sharded checkpoint format:

```
<checkpoint-id[:2]>/<checkpoint-id[2:]>/
├── metadata.json            # CheckpointSummary (aggregated stats)
├── 0/                       # First session (0-based indexing)
│   ├── metadata.json        # Session-specific metadata
│   ├── full.jsonl           # Session transcript
│   ├── prompt.txt           # Checkpoint-scoped user prompts
│   ├── content_hash.txt     # SHA256 of transcript
│   └── tasks/<tool-use-id>/ # Task checkpoints (if applicable)
│       ├── checkpoint.json  # UUID mapping
│       └── agent-<id>.jsonl # Subagent transcript
├── 1/                       # Second session (if multiple sessions)
│   ├── metadata.json
│   ├── full.jsonl
│   └── ...
└── ...
```

**Multi-session metadata.json format:**

```json
{
  "checkpoint_id": "abc123def456",
  "session_id": "2026-01-13-uuid", // Current/latest session
  "session_ids": ["2026-01-13-uuid1", "2026-01-13-uuid2"], // All sessions
  "session_count": 2, // Number of sessions in this checkpoint
  "strategy": "manual-commit",
  "created_at": "2026-01-13T12:00:00Z",
  "files_touched": ["file1.txt", "file2.txt"] // Merged from all sessions
}
```

When multiple sessions are condensed to the same checkpoint (same base commit):

- Sessions are stored in numbered subfolders using 0-based indexing (`0/`, `1/`, `2/`, etc.)
- Latest session is always in the highest-numbered folder
- `session_ids` array tracks all sessions, `session_count` increments

**Session State** (filesystem, `.git/entire-sessions/`):

```
<session-id>.json            # Active session state (base_commit, checkpoint_count, etc.)
```

#### Checkpoint ID Linking

The strategy uses a **12-hex-char random checkpoint ID** (e.g., `a3b2c4d5e6f7`) as the stable identifier linking user commits to metadata.

**How checkpoint IDs work:**

1. **Generated once per checkpoint**: When condensing session metadata to the metadata branch

2. **Added to user commits** via `Entire-Checkpoint` trailer:
   - **Manual-commit**: Added via `prepare-commit-msg` hook (user can remove it before committing)

3. **Used for directory sharding** on `entire/checkpoints/v1` branch:
   - Path format: `<id[:2]>/<id[2:]>/`
   - Example: `a3b2c4d5e6f7` → `a3/b2c4d5e6f7/`
   - Creates 256 shards to avoid directory bloat

4. **Appears in commit subject** on `entire/checkpoints/v1` commits:
   - Format: `Checkpoint: a3b2c4d5e6f7`
   - Makes `git log entire/checkpoints/v1` readable and searchable

**Bidirectional linking:**

```
User commit → Metadata:
  Extract "Entire-Checkpoint: a3b2c4d5e6f7" trailer
  → Read a3/b2c4d5e6f7/ directory from entire/checkpoints/v1 tree at HEAD

Metadata → User commits:
  Given checkpoint ID a3b2c4d5e6f7
  → Search user branch history for commits with "Entire-Checkpoint: a3b2c4d5e6f7" trailer
```

Note: Commit subjects on `entire/checkpoints/v1` (e.g., `Checkpoint: a3b2c4d5e6f7`) are
for human readability in `git log` only. The CLI always reads from the tree at HEAD.

**Example:**

```
User's commit (on main branch):
  "Implement login feature

  Entire-Checkpoint: a3b2c4d5e6f7"
       ↓ ↑
       Linked via checkpoint ID
       ↓ ↑
entire/checkpoints/v1 commit:
  Subject: "Checkpoint: a3b2c4d5e6f7"

  Tree: a3/b2c4d5e6f7/
    ├── metadata.json (checkpoint_id: "a3b2c4d5e6f7")
    ├── full.jsonl (session transcript)
    └── prompt.txt
```

#### Commit Trailers

**On user's active branch commits:**

- `Entire-Checkpoint: <checkpoint-id>` - 12-hex-char ID linking to metadata on `entire/checkpoints/v1`
  - Added via `prepare-commit-msg` hook; user can remove it before committing to skip linking

**On shadow branch commits (`entire/<commit-hash[:7]>-<worktreeHash[:6]>`):**

- `Entire-Session: <session-id>` - Session identifier
- `Entire-Metadata: <path>` - Path to metadata directory within the tree
- `Entire-Task-Metadata: <path>` - Path to task metadata directory (for task checkpoints)
- `Entire-Strategy: manual-commit` - Strategy that created the commit

**On metadata branch commits (`entire/checkpoints/v1`):**

Commit subject: `Checkpoint: <checkpoint-id>` (or custom subject for task checkpoints)

Trailers:

- `Entire-Session: <session-id>` - Session identifier
- `Entire-Strategy: <strategy>` - Strategy name (manual-commit)
- `Entire-Agent: <agent-name>` - Agent name (optional, e.g., "Claude Code")
- `Ephemeral-branch: <branch>` - Shadow branch name (optional)
- `Entire-Metadata-Task: <path>` - Task metadata path (optional, for task checkpoints)

**Note:** The strategy keeps active branch history clean - the only addition to user commits is the single `Entire-Checkpoint` trailer. It never creates commits on the active branch (the user creates them manually). All detailed session data (transcripts, prompts, context) is stored on the `entire/checkpoints/v1` orphan branch or shadow branches.

#### Multi-Session Behavior

**Concurrent Sessions:**

- When a second session starts in the same directory while another has uncommitted checkpoints, a warning is shown
- Both sessions can proceed - their checkpoints interleave on the same shadow branch
- Each session's `RewindPoint` includes `SessionID` and `SessionPrompt` to help identify which checkpoint belongs to which session
- On commit, all sessions are condensed together with archived sessions in numbered subfolders
- Note: Different git worktrees have separate shadow branches (worktree-specific naming), so concurrent sessions in different worktrees do not conflict

**Orphaned Shadow Branches:**

- A shadow branch is "orphaned" if it exists but has no corresponding session state file
- This can happen if the state file is manually deleted or lost
- When a new session starts with an orphaned branch, the branch is automatically reset
- If the existing session DOES have a state file (concurrent session in same directory), a `SessionIDConflictError` is returned

**Shadow Branch Migration (Pull/Rebase):**

- If user does stash → pull → apply (or rebase), HEAD changes but work isn't committed
- The shadow branch would be orphaned at the old commit
- Detection: base commit changed AND old shadow branch still exists (would be deleted if user committed)
- Action: shadow branch is renamed from `entire/<old-hash>-<worktreeHash>` to `entire/<new-hash>-<worktreeHash>`
- Session continues seamlessly with checkpoints preserved

#### When Modifying the Strategy

- The strategy must implement the full `Strategy` interface
- Test with `mise run test` - strategy tests are in `*_test.go` files
- **Update both CLAUDE.md and AGENTS.md** when modifying the strategy to keep documentation current

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
