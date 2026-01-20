# Entire CLI

Entire hooks into your git workflow to capture AI agent sessions on every push. Sessions are indexed alongside commits, creating a searchable record of how code was written. Runs locally, stays in your repo.

## Quick Start

```bash
# Install via Homebrew (requires SSH access)
brew tap entirehq/tap git@github.com:entirehq/homebrew-entire.git
brew install entirehq/tap/entire

# Enable in your project
cd your-project && entire enable

# Check status
entire status
```

## Typical Workflow

### 1. Enable Entire in Your Repository

```bash
entire enable
```

This installs Claude Code and git hooks that automatically capture checkpoints whenever Claude Code makes changes. Your code commits stay clean—all session metadata is stored separately.

### 2. Work with Claude Code

Just use Claude Code normally. Entire runs in the background, creating checkpoints automatically:

```bash
entire status  # Check current session status anytime
```

### 3. Rewind to a Previous Checkpoint

If you want to undo some changes and go back to an earlier checkpoint:

```bash
entire rewind
```

This shows all available checkpoints in the current session. Select one to restore your code to that exact state.

### 4. Resume a Previous Session

To see and restore sessions from earlier work:

```bash
entire resume
```

Lists all past sessions with timestamps. You can view the conversation history or restore the code from any session.

### 5. Disable Entire (Optional)

```bash
entire disable
```

Removes the git hooks. Your code and commit history remain untouched.

## Key Concepts

### Sessions

A **session** represents a complete interaction with your AI agent, from start to finish. Each session captures all prompts, responses, files modified, and timestamps.

**Session ID format:** `YYYY-MM-DD-<UUID>` (e.g., `2026-01-08-abc123de-f456-7890-abcd-ef1234567890`)

Sessions are stored separately from your code commits on the `entire/sessions` branch.

### Checkpoints

A **checkpoint** is a snapshot within a session that you can rewind to—a "save point" in your work.

**When checkpoints are created:**
- **Manual-commit strategy**: When you make a git commit
- **Auto-commit strategy**: After each agent response

**Checkpoint IDs** are 12-character hex strings (e.g., `a3b2c4d5e6f7`).

### Strategies

Entire offers two strategies for capturing your work:

| Aspect | Manual-Commit | Auto-Commit |
|--------|---------------|-------------|
| Code commits | None on your branch | Created automatically after each agent response |
| Safe on main branch | Yes | No - creates commits |
| Rewind | Always possible, non-destructive | Full rewind on feature branches; logs-only on main |
| Best for | Most workflows - keeps git history clean | Teams wanting automatic code commits |

## Commands Reference

| Command | Description |
|---------|-------------|
| `entire enable` | Enable Entire in your repository, install hooks |
| `entire disable` | Remove Entire hooks from repository |
| `entire status` | Show current session and strategy info |
| `entire rewind` | Rewind to a previous checkpoint |
| `entire resume` | Resume a previous session |
| `entire explain` | Explain a session or commit |
| `entire session` | View and manage sessions (list, show details, view logs) |
| `entire version` | Show Entire CLI version |

## Configuration

Entire uses two configuration files in the `.entire/` directory:

### settings.json (Project Settings)

Shared across the team, typically committed to git:

```json
{
  "strategy": "manual-commit",
  "agent": "claude-code",
  "enabled": true
}
```

### settings.local.json (Local Settings)

Personal overrides, gitignored by default:

```json
{
  "enabled": false,
  "log_level": "debug"
}
```

### Configuration Options

| Option | Values | Description |
|--------|--------|-------------|
| `strategy` | `manual-commit`, `auto-commit` | Session capture strategy |
| `enabled` | `true`, `false` | Enable/disable Entire |
| `agent` | `claude-code`, `gemini`, etc. | AI agent to integrate with |
| `log_level` | `debug`, `info`, `warn`, `error` | Logging verbosity |
| `strategy_options.push_sessions` | `true`, `false` | Auto-push `entire/sessions` branch on git push |

### Settings Priority

Local settings override project settings field-by-field. When you run `entire status`, it shows both project and local (effective) settings.

## Troubleshooting

### Common Issues

| Issue | Solution |
|-------|----------|
| "Not a git repository" | Navigate to a git repository first |
| "Entire is disabled" | Run `entire enable` |
| "No rewind points found" | Work with Claude Code and commit (manual-commit) or wait for agent response (auto-commit) |
| "shadow branch conflict" | Run `entire rewind reset --force` |
| "session not found" | Check available sessions with `entire session list` |

### Debug Mode

```bash
# Via environment variable
ENTIRE_LOG_LEVEL=debug entire status

# Or via settings.local.json
{
  "log_level": "debug"
}
```

### Resetting State

```bash
# Reset shadow branch for current commit
entire rewind reset --force

# Disable and re-enable
entire disable && entire enable --force
```

### Accessibility

For screen reader users, enable accessible mode:

```bash
export ACCESSIBLE=1
entire enable
```

This uses simpler text prompts instead of interactive TUI elements.

## Development

This project uses [mise](https://mise.jdx.dev/) for task automation and dependency management.

### Prerequisites

- [mise](https://mise.jdx.dev/) - Install with `curl https://mise.run | sh`

### Getting Started

```bash
# Clone the repository
git clone <repo-url>
cd cli

# Install dependencies (including Go)
mise install

# Build the CLI
mise run build
```

### Common Tasks

```bash
# Run tests
mise run test

# Run integration tests
mise run test:integration

# Run all tests (unit + integration, CI mode)
mise run test:ci

# Lint the code
mise run lint

# Format the code
mise run fmt
```

### Project Structure

- `cmd/entire/` - Main CLI entry point
- `cmd/entire/cli/` - CLI utilities and helpers
- `cmd/entire/cli/commands/` - Command implementations
- `cmd/entire/cli/strategy/` - Session checkpoint strategies
- `cmd/entire/cli/checkpoint/` - Checkpoint storage abstractions
- `cmd/entire/cli/session/` - Session state management
- `cmd/entire/cli/integration_test/` - Integration tests

## Getting Help

```bash
entire --help              # General help
entire <command> --help    # Command-specific help
```

- **GitHub Issues:** Report bugs or request features at https://github.com/entireio/cli/issues
- **Contributing:** See [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines
