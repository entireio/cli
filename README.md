# Entire CLI

Entire hooks into your git workflow to capture AI agent sessions on every push. Sessions are indexed alongside commits, creating a searchable record of how code was written in your repo.

## Why Entire

- **Understand why code changed, not just what** — Transcripts, prompts, files touched, token usage, tool calls, and more are captured alongside every commit.
- **Rewind and resume from any checkpoint** — Go back to any previous agent session and pick up exactly where you left off.
- **Full context preserved and searchable** — A versioned record of every AI interaction tied to your git history, with nothing lost.
- **Zero context switching** — Git-native, two-step setup, works with Claude Code, Gemini, and more.

## Table of Contents

- [Why Entire](#why-entire)
- [Quick Start](#quick-start)
- [Typical Workflow](#typical-workflow)
- [Key Concepts](#key-concepts)
  - [How It Works](#how-it-works)
  - [Strategies](#strategies)
- [Commands Reference](#commands-reference)
- [Configuration](#configuration)
- [Troubleshooting](#troubleshooting)
- [Development](#development)
- [Getting Help](#getting-help)
- [License](#license)

## Requirements

- Git
- macOS or Linux (Windows via WSL)
- [Claude Code](https://docs.anthropic.com/en/docs/claude-code) or [Gemini CLI](https://github.com/google-gemini/gemini-cli) installed and authenticated

## Quick Start

```bash
# Install via Homebrew
brew tap entireio/tap
brew install entireio/tap/entire

# Or install via Go
go install github.com/entireio/cli/cmd/entire@latest

# Enable in your project
cd your-project && entire enable

# Check status
entire status
```

## Typical Workflow

### 1. Enable Entire in Your Repository

```
entire enable
```

This installs agent and git hooks to work with your AI agent (Claude Code or Gemini CLI). The hooks capture session data at specific points in your workflow. Your code commits stay clean—all session metadata is stored on a separate `entire/checkpoints/v1` branch.

**When checkpoints are created** depends on your chosen strategy (default is `manual-commit`):
- **Manual-commit**: Checkpoints are created when you or the agent make a git commit
- **Auto-commit**: Checkpoints are created after each agent response

### 2. Work with Your AI Agent

Just use Claude Code or Gemini CLI normally. Entire runs in the background, tracking your session:

```
entire status  # Check current session status anytime
```

### 3. Rewind to a Previous Checkpoint

If you want to undo some changes and go back to an earlier checkpoint:

```
entire rewind
```

This shows all available checkpoints in the current session. Select one to restore your code to that exact state.

### 4. Resume a Previous Session

To restore the latest checkpointed session metadata for a branch:

```
entire resume <branch>
```

Entire checks out the branch, restores the latest checkpointed session metadata (one or more sessions), and prints command(s) to continue.

### 5. Disable Entire (Optional)

```
entire disable
```

Removes the git hooks. Your code and commit history remain untouched.

## Key Concepts

### Sessions

A **session** represents a complete interaction with your AI agent, from start to finish. Each session captures all prompts, responses, files modified, and timestamps.

**Session ID format:** `YYYY-MM-DD-<UUID>` (e.g., `2026-01-08-abc123de-f456-7890-abcd-ef1234567890`)

Sessions are stored separately from your code commits on the `entire/checkpoints/v1` branch.

### Checkpoints

A **checkpoint** is a snapshot within a session that you can rewind to—a "save point" in your work.

**When checkpoints are created:**

- **Manual-commit strategy**: When you or the agent make a git commit
- **Auto-commit strategy**: After each agent response

**Checkpoint IDs** are 12-character hex strings (e.g., `a3b2c4d5e6f7`).

### How It Works

```
Your Branch                    entire/checkpoints/v1
     │                                  │
     ▼                                  │
[Base Commit]                           │
     │                                  │
     │  ┌─── Agent works ───┐           │
     │  │  Step 1           │           │
     │  │  Step 2           │           │ 
     │  │  Step 3           │           │
     │  └───────────────────┘           │
     │                                  │
     ▼                                  ▼
[Your Commit] ─────────────────► [Session Metadata]
     │                           (transcript, prompts,
     │                            files touched)
     ▼
```

Checkpoints are saved as you work. When you commit, session metadata is permanently stored on the `entire/checkpoints/v1` branch and linked to your commit.

### Strategies

Entire offers two strategies for capturing your work:

| Aspect              | Manual-Commit                            | Auto-Commit                                        |
| ------------------- | ---------------------------------------- | -------------------------------------------------- |
| Code commits        | None on your branch                      | Created automatically after each agent response    |
| Safe on main branch | Yes                                      | Use caution - creates commits on active branch     |
| Rewind              | Always possible, non-destructive         | Full rewind on feature branches; logs-only on main |
| Best for            | Most workflows - keeps git history clean | Teams wanting automatic code commits               |

### Git Worktrees

Entire works seamlessly with [git worktrees](https://git-scm.com/docs/git-worktree). Each worktree has independent session tracking, so you can run multiple AI sessions in different worktrees without conflicts.

### Concurrent Sessions

Multiple AI sessions can run on the same commit. If you start a second session while another has uncommitted work, Entire warns you and tracks them separately. Both sessions' checkpoints are preserved and can be rewound independently.

## Commands Reference

| Command          | Description                                                                   |
| ---------------- | ----------------------------------------------------------------------------- |
| `entire clean`   | Clean up orphaned Entire data                                                 |
| `entire disable` | Remove Entire hooks from repository                                           |
| `entire doctor`  | Fix or clean up stuck sessions                                                |
| `entire enable`  | Enable Entire in your repository (uses `manual-commit` by default)            |
| `entire explain` | Explain a session or commit                                                   |
| `entire reset`   | Delete the shadow branch and session state for the current HEAD commit        |
| `entire resume`  | Switch to a branch, restore latest checkpointed session metadata, and show command(s) to continue |
| `entire rewind`  | Rewind to a previous checkpoint                                               |
| `entire status`  | Show current session and strategy info                                        |
| `entire version` | Show Entire CLI version                                                       |

### `entire enable` Flags

| Flag                   | Description                                                        |
|------------------------|--------------------------------------------------------------------|
| `--agent <name>`       | AI agent to setup hooks for: `claude-code` (default) or `gemini`   |
| `--force`, `-f`        | Force reinstall hooks (removes existing Entire hooks first)        |
| `--local`              | Write settings to `settings.local.json` instead of `settings.json` |
| `--project`            | Write settings to `settings.json` even if it already exists        |
| `--skip-push-sessions` | Disable automatic pushing of session logs on git push              |
| `--strategy <name>`    | Strategy to use: `manual-commit` (default) or `auto-commit`        |
| `--telemetry=false`    | Disable anonymous usage analytics                                  |

**Examples:**

```
# Use auto-commit strategy
entire enable --strategy auto-commit

# Force reinstall hooks
entire enable --force

# Save settings locally (not committed to git)
entire enable --local
```

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

| Option                               | Values                           | Description                                          |
|--------------------------------------|----------------------------------|------------------------------------------------------|
| `enabled`                            | `true`, `false`                  | Enable/disable Entire                                |
| `log_level`                          | `debug`, `info`, `warn`, `error` | Logging verbosity                                    |
| `strategy`                           | `manual-commit`, `auto-commit`   | Session capture strategy                             |
| `strategy_options.push_sessions`     | `true`, `false`                  | Auto-push `entire/checkpoints/v1` branch on git push |
| `strategy_options.summarize.enabled` | `true`, `false`                  | Auto-generate AI summaries at commit time            |
| `telemetry`                          | `true`, `false`                  | Send anonymous usage statistics to Posthog           |

### Auto-Summarization

When enabled, Entire automatically generates AI summaries for checkpoints at commit time. Summaries capture intent, outcome, learnings, friction points, and open items from the session.

```json
{
  "strategy_options": {
    "summarize": {
      "enabled": true
    }
  }
}
```

**Requirements:**
- Claude CLI must be installed and authenticated (`claude` command available in PATH)
- Summary generation is non-blocking: failures are logged but don't prevent commits

**Note:** Currently uses Claude CLI for summary generation. Other AI backends may be supported in future versions.

### Settings Priority

Local settings override project settings field-by-field. When you run `entire status`, it shows both project and local (effective) settings.

### Gemini CLI (Preview)

Gemini CLI support is currently in preview. Entire can work with [Gemini CLI](https://github.com/google-gemini/gemini-cli) as an alternative to Claude Code, or alongside it — you can have both agents' hooks enabled at the same time.

To enable:

```bash
entire enable --agent gemini
```

All commands (`rewind`, `status`, `doctor`, etc.) work the same regardless of which agent is configured.

If you run into any issues with Gemini CLI integration, please [open an issue](https://github.com/entireio/cli/issues).

## Troubleshooting

### Common Issues

| Issue                    | Solution                                                                                  |
|--------------------------|-------------------------------------------------------------------------------------------|
| "Not a git repository"   | Navigate to a Git repository first                                                        |
| "Entire is disabled"     | Run `entire enable`                                                                       |
| "No rewind points found" | Work with Claude Code and commit (manual-commit) or wait for agent response (auto-commit) |
| "shadow branch conflict" | Run `entire reset --force`                                                                |

### SSH Authentication Errors

If you see an error like this when running `entire resume`:

```
Failed to fetch metadata: failed to fetch entire/checkpoints/v1 from origin: ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey], no supported methods remain
```

This is a [known issue with go-git's SSH handling](https://github.com/go-git/go-git/issues/411). Fix it by adding GitHub's host keys to your known_hosts file:

```
ssh-keyscan -t rsa github.com >> ~/.ssh/known_hosts
ssh-keyscan -t ecdsa github.com >> ~/.ssh/known_hosts
```

### Debug Mode

```
# Via environment variable
ENTIRE_LOG_LEVEL=debug entire status

# Or via settings.local.json
{
  "log_level": "debug"
}
```

### Resetting State

```
# Reset shadow branch for current commit
entire reset --force

# Disable and re-enable
entire disable && entire enable --force
```

### Accessibility

For screen reader users, enable accessible mode:

```
export ACCESSIBLE=1
entire enable
```

This uses simpler text prompts instead of interactive TUI elements.

## Development

This project uses [mise](https://mise.jdx.dev/) for task automation and dependency management.

### Prerequisites

- [mise](https://mise.jdx.dev/) - Install with `curl https://mise.run | sh`

### Getting Started

```
# Clone the repository
git clone <repo-url>
cd cli

# Install dependencies (including Go)
mise install

# Trust the mise configuration (required on first setup)
mise trust

# Build the CLI
mise run build
```

### Common Tasks

```
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

## Getting Help

```
entire --help              # General help
entire <command> --help    # Command-specific help
```

- **GitHub Issues:** Report bugs or request features at https://github.com/entireio/cli/issues
- **Contributing:** See [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines

## License

MIT License - see [LICENSE](LICENSE) for details.
