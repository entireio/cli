# Windows Support

## Prerequisites

- **Windows 10 1809+** (required for ConPTY support in E2E tests)
- **Git for Windows** — provides `git.exe` and bundled bash for git hooks
- **Go 1.26+** — for building from source

## Building

Cross-compile from macOS/Linux:

```bash
mise run build:windows          # amd64
mise run build:windows-arm64    # arm64
```

Or build natively on Windows:

```bash
go build -o entire.exe ./cmd/entire/
```

## Installation

Copy `entire.exe` to a directory in your `PATH`. Git for Windows must be installed and available in `PATH`.

## How It Works

The CLI is pure Go with no CGO dependencies, so cross-compilation produces a fully static binary.

### Git Hooks

Git hooks use `#!/bin/sh` shebangs with POSIX shell syntax. Git for Windows executes hooks through its bundled MSYS2 bash, so they work as-is. No batch file wrappers are needed.

### Agent Hooks

Agent-specific hooks (Claude Code, Cursor, Gemini, OpenCode) are JSON configuration — the agents themselves handle execution. The hooks call `entire.exe` directly via `exec.Command`, not through a shell.

## Testing

### Unit + Integration Tests

```bash
go test ./...                                          # unit tests
go test -tags=integration ./cmd/entire/cli/integration_test/...  # integration tests
go test -tags=integration -race ./...                  # all (CI equivalent)
```

### E2E Tests

E2E tests require the agent binary (e.g., `claude`) to be installed and available in `PATH`.

```bash
# Set required env vars
set E2E_ENTIRE_BIN=entire.exe
set E2E_AGENT=claude-code        # or gemini-cli, opencode

# Run all E2E tests
go test -tags=e2e -count=1 -timeout=30m ./e2e/tests/...

# Run a specific test
go test -tags=e2e -count=1 -timeout=30m -run TestSingleSessionManualCommit ./e2e/tests/...
```

The E2E test infrastructure uses native PTY (ConPTY on Windows, creack/pty on Unix) instead of tmux, so no tmux installation is needed.

## Known Limitations

- **Interactive integration tests** that use PTY (`resume_interactive_test.go`) are skipped on Windows (guarded with `//go:build unix`).
- **File permissions** (0o755, 0o644) are set but ignored by Windows — Windows uses ACLs instead.
- **Symlinks** in E2E tests require Windows Developer Mode or admin privileges.
- **OpenCode plugin** uses `Bun.spawnSync(["sh", "-c", ...])` which won't work on Windows unless Bun supports it. OpenCode Windows support is pending.

## Smoke Test Checklist

After building, verify these work on a Windows machine:

1. `entire.exe version`
2. `entire.exe enable` — in a git repo with an agent installed
3. Start an agent session, make file changes
4. `git add . && git commit -m "test"` — hooks should fire (prepare-commit-msg, post-commit)
5. `entire.exe rewind --list` — should show checkpoint(s)
6. `entire.exe explain` — pager should use `more` by default

## Architecture Notes

### Platform-Specific Files

| File | Purpose |
|------|---------|
| `cmd/entire/cli/telemetry/detached_unix.go` | Telemetry subprocess via `Setpgid` |
| `cmd/entire/cli/telemetry/detached_windows.go` | Telemetry subprocess via `CREATE_NEW_PROCESS_GROUP` |
| `cmd/entire/cli/integration_test/procattr_unix.go` | Test process detachment via `Setsid` |
| `cmd/entire/cli/integration_test/procattr_windows.go` | Test process detachment via `CREATE_NEW_PROCESS_GROUP` |
| `e2e/agents/procattr_unix.go` | E2E process groups via `Setpgid` + `SIGKILL` |
| `e2e/agents/procattr_windows.go` | E2E process groups via `CREATE_NEW_PROCESS_GROUP` |
| `e2e/agents/pty_session_unix.go` | Interactive E2E sessions via `creack/pty` |
| `e2e/agents/pty_session_windows.go` | Interactive E2E sessions via ConPTY |

### Cross-Platform Patterns Used

- `os.Interrupt` only (no `syscall.SIGTERM`) for signal handling
- `filepath.FromSlash()` on git CLI output paths
- `strings.ReplaceAll(s, "\r\n", "\n")` for CRLF-safe git output parsing
- `os.DevNull` instead of hardcoded `/dev/null`
- `runtime.GOOS == "windows"` for pager selection (`more` vs `less`)
- Build-tagged `_unix.go` / `_windows.go` files for syscall differences
