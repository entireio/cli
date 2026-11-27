# Copilot Instructions for Entire CLI

## Repository Overview

Entire CLI is a Go CLI tool that captures AI coding agent sessions (Claude Code, Codex, Antigravity, OpenCode, Cursor, Factory AI Droid, Copilot CLI, etc.) as searchable metadata stored separately from code commits. It uses `*strategy.ManualCommitStrategy` (the only strategy; there is no strategy interface or worktree restore path) to track session state and write checkpoint metadata on commit with minimal impact on commit history.

**Tech Stack**: Go (version in `mise.toml`), Cobra (CLI), charm.land/huh/v2 (interactive prompts), go-git/v6 (with caveats)

## Build & Validation Commands

All commands use `mise` as the task runner. Always run these in order before committing:

```bash
mise run fmt           # Format with gofmt (required - CI will fail otherwise)
mise run lint          # Lint entire codebase (golangci-lint, gofmt, go.mod, mise, secretpatterns, shellcheck)
mise run test          # Unit tests
mise run test:ci       # Full tests with race detection (same as CI)
```

**CI runs these checks on every PR** (see `.github/workflows/`):
1. gofmt formatting check (must pass - no unformatted files)
2. golangci-lint
3. go.mod tidy check
4. All tests with race detection (`mise run test:ci`)

## Critical Code Patterns

### Error Handling

The CLI uses `SilentError` to avoid duplicate error output (see `errors.go`):

```go
// When you print a custom error message, return SilentError
fmt.Fprintln(cmd.ErrOrStderr(), "User-friendly error message")
return NewSilentError(errors.New("internal error"))

// For normal errors, just return them - main.go will print
return fmt.Errorf("failed to do X: %w", err)
```

### Forbidden go-git Operations

**NEVER use go-git's `Reset` or `Checkout` directly** - `.golangci.yaml` forbids them (historically go-git v5 deleted `.gitignore`d directories such as `.entire/` and `.worktrees/`). Use the git CLI or CLI wrappers instead (see [Git and subprocess safety](../docs/development/git-safety.md)).

```go
// WRONG - forbidden by forbidigo; use the git CLI instead
worktree.Reset(&git.ResetOptions{Mode: git.HardReset, Commit: hash})
worktree.Checkout(&git.CheckoutOptions{Branch: ref})

// CORRECT - use git CLI wrappers (e.g., CheckoutBranch in git_operations.go) or exec.CommandContext
CheckoutBranch(ctx, branch)
cmd := exec.CommandContext(ctx, "git", "reset", "--hard", hash.String())
```

### Path Handling

**NEVER use `os.Getwd()` for git-relative paths** - breaks when running from subdirectories.

```go
// WRONG - breaks in subdirectories
cwd, _ := os.Getwd()
absPath := filepath.Join(cwd, gitRelativePath)

// CORRECT - always use worktree root
repoRoot, _ := paths.WorktreeRoot(ctx)
absPath := filepath.Join(repoRoot, gitRelativePath)
```

The linter enforces these patterns via `forbidigo` rules in `.golangci.yaml`.

### Accessibility

Interactive prompts must support screen readers:

```go
// In cli package - use NewAccessibleForm()
form := NewAccessibleForm(huh.NewGroup(...))

// Outside cli (including strategy) - use cmd/entire/cli/uiform
form := uiform.New(huh.NewGroup(...))
```

Both helpers apply accessibility and theming centrally. Do not hand-roll
`IsAccessibleMode` / `WithAccessible` wiring at call sites.

### Logging vs User Output

- **Internal/debug logging**: Use `logging.Debug/Info/Warn/Error(ctx, msg, attrs...)` from `cli/logging/`. Writes to `.entire/logs/`.
- **User-facing output**: Use `fmt.Fprint*(cmd.OutOrStdout(), ...)` or `cmd.ErrOrStderr()`.

Don't use `fmt.Print*` for operational messages (checkpoint saves, hook invocations) - use the `logging` package.

**Privacy**: Don't log user content (prompts, file contents, commit messages). Log only operational metadata (IDs, counts, paths, durations).

## Project Structure

```
cmd/entire/
├── main.go           # Entry point
└── cli/              # Main CLI package
    ├── strategy/     # Manual-commit strategy and lifecycle/git hooks
    ├── checkpoint/   # Low-level storage operations
    ├── session/      # Session state management (.git/entire-sessions/)
    ├── agent/        # AI agent abstraction layer
    ├── paths/        # Path utilities (WorktreeRoot)
    └── integration_test/  # Integration tests (//go:build integration)
```

**Key configuration files**:
- `mise.toml` - Task definitions and Go version
- `.golangci.yaml` - Linting rules (forbidigo patterns for unsafe operations)
- `go.mod` - Module dependencies

Use the [task-specific reference map](../CLAUDE.md#read-when-relevant) to find detailed architecture and development documentation.

## Testing Guidelines

Write tests that provide real value, not just coverage:

- **Test behavior, not implementation** - tests should validate what the code does, not how
- **Include edge cases and error conditions** - ensure error paths are properly tested
- **When fixing bugs, write a test that surfaces the bug first** - then write the fix
- **Unit tests**: Same directory as source, `*_test.go` files
- **Integration tests**: `cmd/entire/cli/integration_test/`, require `//go:build integration` tag

Run `mise run test` for quick validation, `mise run test:ci` for full CI parity.

Test accessibility features with `ACCESSIBLE=1` environment variable.

## Linting Notes

The linter is strict (many linters enabled). Key rules:
- All errors must be handled explicitly
- No unused variables/imports
- `wrapcheck` requires wrapping errors from external packages
- Test files are exempt from some linters (gosec, wrapcheck, forbidigo)

Run `mise run lint` to run all linters across the entire codebase.

## When Adding New Commands

1. Add command in `cmd/entire/cli/` following existing patterns (look at similar commands)
2. Register in `root.go`
3. Use `SilentError` for custom error messages
4. Add meaningful tests that verify behavior
5. Consider integration tests if command has complex git interactions

## Trust These Instructions

These instructions are verified and accurate. Only search the codebase if information here is incomplete or found to be incorrect during execution.
