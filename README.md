# Entire CLI

Entire hooks into your Git workflow to capture AI agent sessions as you work. Sessions are indexed alongside commits, creating a searchable record of how code was written in your repo.

With Entire, you can:

- **Understand why code changed** — see the full prompt/response transcript and files touched
- **Recover instantly** — rewind to a known-good checkpoint when an agent goes sideways and resume seamlessly
- **Keep Git history clean** — preserve agent context on a separate branch
- **Onboard faster** — show the path from prompt → change → commit
- **Maintain traceability** — support audit and compliance requirements when needed

## Why Entire

- **Understand why code changed, not just what** — Transcripts, prompts, files touched, token usage, tool calls, and more are captured alongside every commit.
- **Rewind and resume from any checkpoint** — Go back to any previous agent session and pick up exactly where you or a coworker left off.
- **Full context preserved and searchable** — A versioned record of every AI interaction tied to your git history, with nothing lost.
- **Zero context switching** — Git-native, two-step setup, works with Claude Code, Gemini, and more.

## Table of Contents

- [Why Entire](#why-entire)
- [Quick Start](#quick-start)
- [Typical Workflow](#typical-workflow)
- [Key Concepts](#key-concepts)
  - [How It Works](#how-it-works)
  - [Strategy](#strategy)
- [Local Device Auth Testing](#local-device-auth-testing)
- [Commands Reference](#commands-reference)
- [Configuration](#configuration)
- [Security & Privacy](#security--privacy)
- [Troubleshooting](#troubleshooting)
- [Development](#development)
- [Getting Help](#getting-help)
- [License](#license)

## Requirements

- Git
- macOS, Linux or Windows
- [Claude Code](https://docs.anthropic.com/en/docs/claude-code), [Gemini CLI](https://github.com/google-gemini/gemini-cli), [OpenCode](https://opencode.ai/docs/cli/), [Cursor](https://www.cursor.com/), [Factory AI Droid](https://www.factory.ai/), or [GitHub Copilot CLI](https://docs.github.com/en/copilot) installed and authenticated

## Quick Start

```bash
# Install via Homebrew
brew tap entireio/tap
brew install entireio/tap/entire

# Or install via Scoop (Windows)
scoop bucket add entire https://github.com/entireio/scoop-bucket.git
scoop install entire/cli

# Or install via Go
go install github.com/entireio/cli/cmd/entire@latest

# Linux: Add Go binaries to PATH (add to ~/.zshrc or ~/.bashrc if not already configured)
export PATH="$HOME/go/bin:$PATH"

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

This installs agent and git hooks to work with your AI agent (Claude Code, Gemini CLI, OpenCode, Cursor, Factory AI Droid, or Copilot CLI). You'll be prompted to select which agents to enable. To enable a specific agent non-interactively, use `entire enable --agent <name>` (e.g., `entire enable --agent cursor`).

The hooks capture session data as you work. Checkpoints are created when you or the agent make a git commit. Your code commits stay clean, Entire never creates commits on your active branch. All session metadata is stored on a separate `entire/checkpoints/v1` branch.

### 2. Work with Your AI Agent

Just use Claude Code, Gemini CLI, OpenCode, Cursor, Factory AI Droid, or Copilot CLI normally. Entire runs in the background, tracking your session:

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

Checkpoints are created when you or the agent make a git commit. **Checkpoint IDs** are 12-character hex strings (e.g., `a3b2c4d5e6f7`).

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

### Strategy

Entire uses a manual-commit strategy that keeps your git history clean:

- **No commits on your branch** — Entire never creates commits on the active branch
- **Safe on any branch** — works on main, master, and feature branches alike
- **Non-destructive rewind** — restore files from any checkpoint without altering commit history
- **Metadata stored separately** — all session data lives on the `entire/checkpoints/v1` branch

### Git Worktrees

Entire works seamlessly with [git worktrees](https://git-scm.com/docs/git-worktree). Each worktree has independent session tracking, so you can run multiple AI sessions in different worktrees without conflicts.

### Concurrent Sessions

Multiple AI sessions can run on the same commit. If you start a second session while another has uncommitted work, Entire warns you and tracks them separately. Both sessions' checkpoints are preserved and can be rewound independently.

## Local Device Auth Testing

If you're working on the CLI device auth flow against a local `entire.io` checkout:

```bash
# In your app repo
cd ../entire.io-1
mise run dev

# In this repo, point the CLI at the local API
cd ../cli
export ENTIRE_API_BASE_URL=http://localhost:8787

# Run the smoke test
./scripts/local-device-auth-smoke.sh
```

Useful commands while developing:

```bash
# Run the login flow against a local server (prompts to press Enter before opening the browser)
go run ./cmd/entire login --insecure-http-auth

# Run the focused integration coverage for login
go test -tags=integration ./cmd/entire/cli/integration_test -run TestLogin
```

## Commands Reference

| Command          | Description                                                                                       |
| ---------------- | ------------------------------------------------------------------------------------------------- |
| `entire clean`   | Clean up orphaned Entire data                                                                     |
| `entire disable` | Remove Entire hooks from repository                                                               |
| `entire doctor`  | Fix or clean up stuck sessions                                                                    |
| `entire enable`  | Enable Entire in your repository                                                                  |
| `entire explain` | Explain a session or commit                                                                       |
| `entire login`   | Authenticate the CLI with Entire device auth                                                      |
| `entire reset`   | Delete the shadow branch and session state for the current HEAD commit                            |
| `entire resume`  | Switch to a branch, restore latest checkpointed session metadata, and show command(s) to continue |
| `entire rewind`  | Rewind to a previous checkpoint                                                                   |
| `entire status`  | Show current session info                                                                         |
| `entire version` | Show Entire CLI version                                                                           |

### `entire enable` Flags

| Flag                                        | Description                                                                                                       |
| ------------------------------------------- | ----------------------------------------------------------------------------------------------------------------- |
| `--agent <name>`                            | AI agent to install hooks for: `claude-code`, `gemini`, `opencode`, `cursor`, `factoryai-droid`, or `copilot-cli` |
| `--force`, `-f`                             | Force reinstall hooks (removes existing Entire hooks first)                                                       |
| `--local`                                   | Write settings to `settings.local.json` instead of `settings.json`                                                |
| `--project`                                 | Write settings to `settings.json` even if it already exists                                                       |
| `--skip-push-sessions`                      | Disable automatic pushing of session logs on git push                                                             |
| `--checkpoint-remote <provider:owner/repo>` | Push checkpoint branches to a separate repo (e.g., `github:org/checkpoints-repo`)                                 |
| `--telemetry=false`                         | Disable anonymous usage analytics                                                                                 |

**Examples:**

```
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

| Option                               | Values                                       | Description                                             |
| ------------------------------------ | -------------------------------------------- | ------------------------------------------------------- |
| `enabled`                            | `true`, `false`                              | Enable/disable Entire                                   |
| `log_level`                          | `debug`, `info`, `warn`, `error`             | Logging verbosity                                       |
| `strategy_options.push_sessions`     | `true`, `false`                              | Auto-push `entire/checkpoints/v1` branch on git push    |
| `strategy_options.checkpoint_remote` | `{"provider": "github", "repo": "org/repo"}` | Push checkpoint branches to a separate repo (see below) |
| `strategy_options.summarize.enabled` | `true`, `false`                              | Auto-generate AI summaries at commit time               |
| `telemetry`                          | `true`, `false`                              | Send anonymous usage statistics to Posthog              |

### Agent Hook Configuration

Each agent stores its hook configuration in its own directory. When you run `entire enable`, hooks are installed in the appropriate location for each selected agent:

| Agent            | Hook Location                 | Format            |
| ---------------- | ----------------------------- | ----------------- |
| Claude Code      | `.claude/settings.json`       | JSON hooks config |
| Gemini CLI       | `.gemini/settings.json`       | JSON hooks config |
| OpenCode         | `.opencode/plugins/entire.ts` | TypeScript plugin |
| Cursor           | `.cursor/hooks.json`          | JSON hooks config |
| Factory AI Droid | `.factory/settings.json`      | JSON hooks config |
| Copilot CLI      | `.github/hooks/entire.json`   | JSON hooks config |

You can enable multiple agents at the same time — each agent's hooks are independent. Entire detects which agents are active by checking for installed hooks, not by a setting in `settings.json`.

### Checkpoint Remote

By default, Entire pushes `entire/checkpoints/v1` to the same remote as your code. If you want to push checkpoint data to a separate repo (e.g., a private repo for public projects), configure `checkpoint_remote` with a structured provider and repo:

```json
{
  "strategy_options": {
    "checkpoint_remote": {
      "provider": "github",
      "repo": "myorg/checkpoints-private"
    }
  }
}
```

Or via the CLI:

```bash
entire enable --checkpoint-remote github:myorg/checkpoints-private
```

Entire derives the git URL automatically using the same protocol (SSH or HTTPS) as your push remote. It will:

- Fetch the checkpoint branch locally if it exists on the remote but not locally (one-time)
- Push `entire/checkpoints/v1` to the checkpoint repo instead of your default push remote
- Skip pushing if a fork is detected (push remote owner differs from checkpoint repo owner)
- If the remote is unreachable, warn and continue without blocking your main push

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

### Gemini CLI

Gemini CLI support is currently in preview. Entire can work with [Gemini CLI](https://github.com/google-gemini/gemini-cli) as an alternative to Claude Code, or alongside it — you can have multiple agents' hooks enabled at the same time.

To enable:

```bash
entire enable --agent gemini
```

All commands (`rewind`, `status`, `doctor`, etc.) work the same regardless of which agent is configured.

If you run into any issues with Gemini CLI integration, please [open an issue](https://github.com/entireio/cli/issues).

### OpenCode

OpenCode support is currently in preview. Entire can work with [OpenCode](https://opencode.ai/docs/cli/) as an alternative to Claude Code, or alongside it — you can have multiple agents' hooks enabled at the same time.

To enable:

```bash
entire enable --agent opencode
```

Or select OpenCode from the interactive agent picker when running `entire enable`.

All commands (`rewind`, `status`, `doctor`, etc.) work the same regardless of which agent is configured.

If you run into any issues with OpenCode integration, please [open an issue](https://github.com/entireio/cli/issues).

### Cursor

Cursor support is currently in preview. Entire can work with [Cursor](https://www.cursor.com/) as an alternative to Claude Code, or alongside it — you can have multiple agents' hooks enabled at the same time.

Entire supports Cursor IDE and Cursor Agent CLI tool.

To enable:

```bash
entire enable --agent cursor
```

Or select Cursor IDE from the interactive agent picker when running `entire enable`.

Rewind is not available at this time, but other commands (`doctor`, `status` etc.) work the same as all other agents.

If you run into any issues with Cursor integration, please [open an issue](https://github.com/entireio/cli/issues).

### Factory AI Droid

Factory AI Droid support is currently in preview. Entire can work with [Factory AI Droid](https://www.factory.ai/) as an alternative to Claude Code, or alongside it — you can have multiple agents' hooks enabled at the same time.

To enable:

```bash
entire enable --agent factoryai-droid
```

Or select Factory AI Droid from the interactive agent picker when running `entire enable`.

All commands (`rewind`, `resume`, `status`, `doctor`, etc.) work the same regardless of which agent is configured.

If you run into any issues with Factory AI Droid integration, please [open an issue](https://github.com/entireio/cli/issues).

### Copilot CLI

GitHub Copilot CLI support is currently in preview. Entire can work with [GitHub Copilot CLI](https://docs.github.com/en/copilot) as an alternative to Claude Code, or alongside it — you can have multiple agents' hooks enabled at the same time.

To enable:

```bash
entire enable --agent copilot-cli
```

Or select Copilot CLI from the interactive agent picker when running `entire enable`.

All commands (`rewind`, `resume`, `status`, `doctor`, etc.) work the same regardless of which agent is configured.

If you run into any issues with Copilot CLI integration, please [open an issue](https://github.com/entireio/cli/issues).

## Security & Privacy

**Your session transcripts are stored in your git repository** on the `entire/checkpoints/v1` branch. If your repository is public, this data is visible to anyone.

Entire automatically redacts detected secrets (API keys, tokens, credentials) when writing to `entire/checkpoints/v1`, but redaction is best-effort. Temporary shadow branches used during a session may contain unredacted data and should not be pushed. See [docs/security-and-privacy.md](docs/security-and-privacy.md) for details.

## Troubleshooting

### Common Issues

| Issue                    | Solution                                                |
| ------------------------ | ------------------------------------------------------- |
| "Not a git repository"   | Navigate to a Git repository first                      |
| "Entire is disabled"     | Run `entire enable`                                     |
| "No rewind points found" | Work with your configured agent and commit your changes |
| "shadow branch conflict" | Run `entire reset --force`                              |

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
