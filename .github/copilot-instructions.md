# Copilot Instructions for Entire CLI

## Repository Overview

Entire CLI is a Go CLI tool that captures AI coding agent sessions (Claude Code, Cursor, etc.) as searchable metadata stored separately from code commits. It uses a strategy pattern for session/checkpoint management with minimal impact on commit history.

**Tech Stack**: Go (version in `mise.toml`), Cobra (CLI), charmbracelet/huh (interactive prompts), go-git/v5 (with caveats)

## Build & Validation Commands

All commands use `mise` as the task runner. Always run these in order before committing:

```bash
mise run fmt           # Format with gofmt (required - CI will fail otherwise)
mise run lint          # Lint with golangci-lint (only new issues)
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

**NEVER use go-git v5's Reset or Checkout directly** - they delete `.gitignore`d directories.

```go
// WRONG - deletes .entire/ and .worktrees/
worktree.Reset(&git.ResetOptions{Mode: git.HardReset, Commit: hash})
worktree.Checkout(&git.CheckoutOptions{Branch: ref})

// CORRECT - use CLI wrappers from strategy/common.go and git_operations.go
HardResetWithProtection(hash)
CheckoutBranch(branch)
```

### Path Handling

**NEVER use `os.Getwd()` for git-relative paths** - breaks when running from subdirectories.

```go
// WRONG - breaks in subdirectories
cwd, _ := os.Getwd()
absPath := filepath.Join(cwd, gitRelativePath)

// CORRECT - always use repo root
repoRoot, _ := paths.RepoRoot()
absPath := filepath.Join(repoRoot, gitRelativePath)
```

The linter enforces these patterns via `forbidigo` rules in `.golangci.yaml`.

### Accessibility

Interactive prompts must support screen readers:

```go
// In cli package - use NewAccessibleForm()
form := NewAccessibleForm(huh.NewGroup(...))

// In strategy package - check isAccessibleMode()
form := huh.NewForm(huh.NewGroup(...))
if isAccessibleMode() {
    form = form.WithAccessible(true)
}
```

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
    ├── strategy/     # Session checkpoint strategies (strategy pattern)
    ├── checkpoint/   # Low-level storage operations
    ├── session/      # Session state management (.git/entire-sessions/)
    ├── agent/        # AI agent abstraction layer
    ├── paths/        # Path utilities (RepoRoot)
    └── integration_test/  # Integration tests (//go:build integration)
```

**Key configuration files**:
- `mise.toml` - Task definitions and Go version
- `.golangci.yaml` - Linting rules (forbidigo patterns for unsafe operations)
- `go.mod` - Module dependencies

For detailed architecture documentation, see `CLAUDE.md`.

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

Run `mise run lint:full` to check all code (not just new issues).

## When Adding New Commands

1. Add command in `cmd/entire/cli/` following existing patterns (look at similar commands)
2. Register in `root.go`
3. Use `SilentError` for custom error messages
4. Add meaningful tests that verify behavior
5. Consider integration tests if command has complex git interactions

## Trust These Instructions

These instructions are verified and accurate. Only search the codebase if information here is incomplete or found to be incorrect during execution.
