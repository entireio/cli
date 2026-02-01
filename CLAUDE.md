# Entire - CLI 

This repo contains the CLI for Entire.

## Architecture

- CLI build with github.com/spf13/cobra and github.com/charmbracelet/huh 

## Key Directories

### Commands (`cmd/`)
- `entire/`: Main CLI entry point
- `entire/cli`: CLI utilities and helpers
- `entire/cli/commands`: actual command implementations
- `entire/cli/strategy`: strategy implementations - see section below
- `entire/cli/checkpoint`: checkpoint storage abstractions (temporary and committed)
- `entire/cli/session`: session state management
- `entire/cli/integration_test`: integration tests

## Tech Stack

- Language: Go 1.25.x
- Build tool: mise, go modules
- Linting: golangci-lint

## Development

### Environment Setup (Claude Code Web Sessions)

**If `CLAUDE_CODE_REMOTE=true`** (web/cloud session), run these setup steps at the start of the session:

```bash
# 1. Configure DNS (required in web containers)
echo "nameserver 8.8.8.8" | sudo tee /etc/resolv.conf

# 2. Install mise
curl https://mise.run | sh

# 3. Activate mise and trust config
export PATH="$HOME/.local/bin:$PATH"
eval "$(mise activate bash)"
mise trust

# 4. Install project tools (Go, golangci-lint)
mise install
```

**Local Claude Code sessions** (no `CLAUDE_CODE_REMOTE` env var): Skip this section.

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

Integration tests use the `//go:build integration` build tag and are located in `cmd/entire/cli/integration_test/`.

### Linting and Formatting
```bash
mise run fmt && mise run lint
```

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
if _, err := paths.RepoRoot(); err != nil {
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

### Logging vs User Output

- **Internal/debug logging**: Use `logging.Debug/Info/Warn/Error(ctx, msg, attrs...)` from `cmd/entire/cli/logging/`. Writes to `.entire/logs/`.
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

Git commands like `git status` and `worktree.Status()` return paths relative to the **repository root**, not the current working directory. When Claude runs from a subdirectory (e.g., `/repo/frontend`), using `os.Getwd()` to construct absolute paths will produce incorrect results for files in sibling directories.

```go
// WRONG - breaks when running from subdirectory
cwd, _ := os.Getwd()  // e.g., /repo/frontend
absPath := filepath.Join(cwd, file)  // file="api/src/types.ts" → /repo/frontend/api/src/types.ts (WRONG)

// CORRECT - use repo root
repoRoot, _ := paths.RepoRoot()  // or strategy.GetWorktreePath()
absPath := filepath.Join(repoRoot, file)  // → /repo/api/src/types.ts (CORRECT)
```

This also affects path filtering. The `paths.ToRelativePath()` function rejects paths starting with `..`, so computing relative paths from cwd instead of repo root will filter out files in sibling directories:

```go
// WRONG - filters out sibling directory files
cwd, _ := os.Getwd()  // /repo/frontend
relPath := paths.ToRelativePath("/repo/api/file.ts", cwd)  // returns "" (filtered out as "../api/file.ts")

// CORRECT - keeps all repo files
repoRoot, _ := paths.RepoRoot()
relPath := paths.ToRelativePath("/repo/api/file.ts", repoRoot)  // returns "api/file.ts"
```

**When to use `os.Getwd()`:** Only when you actually need the current directory (e.g., finding agent session directories that are cwd-relative).

**When to use repo root:** Any time you're working with paths from git status, git diff, or any git-relative file list.

Test case in `state_test.go`: `TestFilterAndNormalizePaths_SiblingDirectories` documents this bug pattern.

### Session Strategies (`cmd/entire/cli/strategy/`)

The CLI uses a strategy pattern for managing session data and checkpoints. Each strategy implements the `Strategy` interface defined in `strategy.go`.

#### Core Interface
All strategies implement:
- `SaveChanges()` - Save session checkpoint (code + metadata)
- `SaveTaskCheckpoint()` - Save subagent task checkpoint
- `GetRewindPoints()` / `Rewind()` - List and restore to checkpoints
- `GetSessionLog()` / `GetSessionInfo()` - Retrieve session data
- `ListSessions()` / `GetSession()` - Session discovery

#### Available Strategies

| Strategy | Main Branch | Metadata Storage | Use Case |
|----------|-------------|------------------|----------|
| **manual-commit** (default) | Unchanged (no commits) | `entire/<HEAD-hash>` branches + `entire/sessions` | Recommended for most workflows |
| **auto-commit** | Creates clean commits | Orphan `entire/sessions` branch | Teams that want code commits from sessions |

#### Strategy Details

**Manual-Commit Strategy** (`manual_commit*.go`) - Default
- **Does not modify** the active branch - no commits created on the working branch
- Creates shadow branch `entire/<HEAD-commit-hash[:7]>` per base commit for checkpoints
- **Supports multiple concurrent sessions** - checkpoints from different sessions interleave on the same shadow branch
- Session logs are condensed to permanent `entire/sessions` branch on user commits
- Builds git trees in-memory using go-git plumbing APIs
- Rewind restores files from shadow branch commit tree (does not use `git reset`)
- Tracks session state in `.git/entire-sessions/` (shared across worktrees)
- **Shadow branch migration** - if user does stash/pull/rebase (HEAD changes without commit), shadow branch is automatically moved to new base commit
- **Orphaned branch cleanup** - if a shadow branch exists without a corresponding session state file, it is automatically reset when a new session starts
- PrePush hook can push `entire/sessions` branch alongside user pushes
- `AllowsMainBranch() = true` - safe to use on main/master since it never modifies commit history

**Auto-Commit Strategy** (`auto_commit.go`)
- Code commits to active branch with **clean history** (commits have `Entire-Checkpoint` trailer only)
- Metadata stored on orphan `entire/sessions` branch at sharded paths: `<id[:2]>/<id[2:]>/`
- Uses `checkpoint.WriteCommitted()` for metadata storage
- Checkpoint ID (12-hex-char) links code commits to metadata on `entire/sessions`
- Full rewind allowed if commit is only on current branch (not in main); otherwise logs-only
- Rewind via `git reset --hard`
- PrePush hook can push `entire/sessions` branch alongside user pushes
- `AllowsMainBranch() = true` - creates commits on active branch, safe to use on main/master

#### Key Files

- `strategy.go` - Interface definition and context structs (`SaveContext`, `RewindPoint`, etc.)
- `registry.go` - Strategy registration/discovery (factory pattern with `Get()`, `List()`, `Default()`)
- `common.go` - Shared helpers for metadata extraction, tree building, rewind validation, `ListCheckpoints()`
- `session.go` - Session/checkpoint data structures
- `push_common.go` - Shared PrePush logic for pushing `entire/sessions` branch
- `manual_commit.go` - Manual-commit strategy main implementation
- `manual_commit_types.go` - Type definitions: `SessionState`, `CheckpointInfo`, `CondenseResult`
- `manual_commit_session.go` - Session state management (load/save/list session states)
- `manual_commit_condensation.go` - Condense logic for copying logs to `entire/sessions`
- `manual_commit_rewind.go` - Rewind implementation: file restoration from checkpoint trees
- `manual_commit_git.go` - Git operations: checkpoint commits, tree building
- `manual_commit_logs.go` - Session log retrieval and session listing
- `manual_commit_hooks.go` - Git hook handlers (prepare-commit-msg, pre-push)
- `manual_commit_reset.go` - Shadow branch reset/cleanup functionality
- `auto_commit.go` - Auto-commit strategy implementation
- `hooks.go` - Git hook installation

#### Checkpoint Package (`cmd/entire/cli/checkpoint/`)
- `checkpoint.go` - Data types (`Checkpoint`, `TemporaryCheckpoint`, `CommittedCheckpoint`)
- `store.go` - `GitStore` struct wrapping git repository
- `temporary.go` - Shadow branch operations (`WriteTemporary`, `ReadTemporary`, `ListTemporary`)
- `committed.go` - Metadata branch operations (`WriteCommitted`, `ReadCommitted`, `ListCommitted`)

#### Session Package (`cmd/entire/cli/session/`)
- `session.go` - Session data types and interfaces
- `state.go` - `StateStore` for managing `.git/entire-sessions/` files

#### Metadata Structure

**Shadow Strategy** - Shadow branches (`entire/<commit-hash>`):
```
.entire/metadata/<session-id>/
├── full.jsonl               # Session transcript
├── prompt.txt               # User prompts
├── context.md               # Generated context
└── tasks/<tool-use-id>/     # Task checkpoints
    ├── checkpoint.json      # UUID mapping for rewind
    └── agent-<id>.jsonl     # Subagent transcript
```

**Both Strategies** - Metadata branch (`entire/sessions`) - sharded checkpoint format:
```
<checkpoint-id[:2]>/<checkpoint-id[2:]>/
├── metadata.json            # Checkpoint info (see below)
├── full.jsonl               # Current/latest session transcript
├── prompt.txt               # User prompts
├── context.md               # Generated context
├── content_hash.txt         # SHA256 of transcript (shadow only)
├── tasks/<tool-use-id>/     # Task checkpoints (if applicable)
│   ├── checkpoint.json      # UUID mapping
│   └── agent-<id>.jsonl     # Subagent transcript
└── 1/                       # Archived session (if multiple sessions)
    ├── metadata.json        # Archived session metadata
    ├── full.jsonl           # Archived session transcript
    ├── prompt.txt
    └── ...
```

**Multi-session metadata.json format:**
```json
{
  "checkpoint_id": "abc123def456",
  "session_id": "2026-01-13-uuid",      // Current/latest session
  "session_ids": ["2026-01-13-uuid1", "2026-01-13-uuid2"],  // All sessions
  "session_count": 2,                    // Number of sessions in this checkpoint
  "strategy": "manual-commit",
  "created_at": "2026-01-13T12:00:00Z",
  "files_touched": ["file1.txt", "file2.txt"]  // Merged from all sessions
}
```

When multiple sessions are condensed to the same checkpoint (same base commit):
- Latest session files go at the root level
- Previous sessions are archived to numbered subfolders (`1/`, `2/`, etc.)
- `session_ids` array tracks all sessions, `session_count` increments

**Session State** (filesystem, `.git/entire-sessions/`):
```
<session-id>.json            # Active session state (base_commit, checkpoint_count, etc.)
```

#### Checkpoint ID Linking

Both strategies use a **12-hex-char random checkpoint ID** (e.g., `a3b2c4d5e6f7`) as the stable identifier linking user commits to metadata.

**How checkpoint IDs work:**

1. **Generated once per checkpoint**: Either when saving (auto-commit) or when condensing (manual-commit)

2. **Added to user commits** via `Entire-Checkpoint` trailer:
   - **Auto-commit**: Added programmatically when creating the commit
   - **Manual-commit**: Added via `prepare-commit-msg` hook (user can remove it before committing)

3. **Used for directory sharding** on `entire/sessions` branch:
   - Path format: `<id[:2]>/<id[2:]>/`
   - Example: `a3b2c4d5e6f7` → `a3/b2c4d5e6f7/`
   - Creates 256 shards to avoid directory bloat

4. **Appears in commit subject** on `entire/sessions` commits:
   - Format: `Checkpoint: a3b2c4d5e6f7`
   - Makes `git log entire/sessions` readable and searchable

**Bidirectional linking:**

```
User commit → Metadata (two approaches):
  Approach 1: Extract "Entire-Checkpoint: a3b2c4d5e6f7" trailer
              → Look up a3/b2c4d5e6f7/ directory on entire/sessions branch

  Approach 2: Extract "Entire-Checkpoint: a3b2c4d5e6f7" trailer
              → Search entire/sessions commit history for "Checkpoint: a3b2c4d5e6f7" subject

Metadata → User commits:
  Given checkpoint ID a3b2c4d5e6f7
  → Search user branch history for commits with "Entire-Checkpoint: a3b2c4d5e6f7" trailer
```

**Example:**
```
User's commit (on main branch):
  "Implement login feature

  Entire-Checkpoint: a3b2c4d5e6f7"
       ↓ ↑
       Linked via checkpoint ID
       ↓ ↑
entire/sessions commit:
  Subject: "Checkpoint: a3b2c4d5e6f7"

  Tree: a3/b2c4d5e6f7/
    ├── metadata.json (checkpoint_id: "a3b2c4d5e6f7")
    ├── full.jsonl (session transcript)
    ├── prompt.txt
    └── context.md
```

#### Commit Trailers

**On user's active branch commits (both strategies):**
- `Entire-Checkpoint: <checkpoint-id>` - 12-hex-char ID linking to metadata on `entire/sessions`
  - Auto-commit: Always added when creating commits
  - Manual-commit: Added by hook; user can remove to skip linking

**On shadow branch commits (`entire/<commit-hash>`) - manual-commit only:**
- `Entire-Session: <session-id>` - Session identifier
- `Entire-Metadata: <path>` - Path to metadata directory within the tree
- `Entire-Task-Metadata: <path>` - Path to task metadata directory (for task checkpoints)
- `Entire-Strategy: manual-commit` - Strategy that created the commit

**On metadata branch commits (`entire/sessions`) - both strategies:**

Commit subject: `Checkpoint: <checkpoint-id>` (or custom subject for task checkpoints)

Trailers:
- `Entire-Session: <session-id>` - Session identifier
- `Entire-Strategy: <strategy>` - Strategy name (manual-commit or auto-commit)
- `Entire-Agent: <agent-name>` - Agent name (optional, e.g., "Claude Code")
- `Ephemeral-branch: <branch>` - Shadow branch name (optional, manual-commit only)
- `Entire-Metadata-Task: <path>` - Task metadata path (optional, for task checkpoints)

**Note:** Both strategies keep active branch history **clean** - the only addition to user commits is the single `Entire-Checkpoint` trailer. Manual-commit never creates commits on the active branch (user creates them manually). Auto-commit creates commits but only adds the checkpoint trailer. All detailed session data (transcripts, prompts, context) is stored on the `entire/sessions` orphan branch or shadow branches.

#### Multi-Session Behavior

**Concurrent Sessions:**
- When a second session starts while another has uncommitted checkpoints, a warning is shown
- Both sessions can proceed - their checkpoints interleave on the same shadow branch
- Each session's `RewindPoint` includes `SessionID` and `SessionPrompt` to help identify which checkpoint belongs to which session
- On commit, all sessions are condensed together with archived sessions in numbered subfolders

**Orphaned Shadow Branches:**
- A shadow branch is "orphaned" if it exists but has no corresponding session state file
- This can happen if the state file is manually deleted or lost
- When a new session starts with an orphaned branch, the branch is automatically reset
- If the existing session DOES have a state file (e.g., cross-worktree conflict), a `SessionIDConflictError` is returned

**Shadow Branch Migration (Pull/Rebase):**
- If user does stash → pull → apply (or rebase), HEAD changes but work isn't committed
- The shadow branch would be orphaned at the old commit
- Detection: base commit changed AND old shadow branch still exists (would be deleted if user committed)
- Action: shadow branch is renamed from `entire/<old-hash>` to `entire/<new-hash>`
- Session continues seamlessly with checkpoints preserved

#### When Modifying Strategies
- All strategies must implement the full `Strategy` interface
- Register new strategies in `init()` using `Register()`
- Test with `mise run test` - strategy tests are in `*_test.go` files
- **Update this CLAUDE.md** when adding or modifying strategies to keep documentation current

# Important Notes

- Tests: always run `mise run test` before committing changes
- Integration tests: run `mise run test:integration` when changing integration test code
- Formatting and linting: always run `mise run fmt && mise run lint` before committing changes
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
