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
- **Zero context switching** — Git-native, two-step setup, works with Claude Code, Codex, Gemini, and more.

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
- [Supported agent](#agent-hook-configuration) installed and authenticated

## Quick Start

```bash
# Install stable via Homebrew
brew tap entireio/tap
brew install --cask entire

# Or install nightly via Homebrew
brew tap entireio/tap
brew install --cask entire@nightly

# Or install stable via install.sh
curl -fsSL https://entire.io/install.sh | bash

# Or install nightly via install.sh
curl -fsSL https://entire.io/install.sh | bash -s -- --channel nightly

# Or install stable via Scoop (Windows)
scoop bucket add entire https://github.com/entireio/scoop-bucket.git
scoop install entire/cli

# Or install via Go (development/manual setup)
go install github.com/entireio/cli/cmd/entire@latest

# Linux: Add Go binaries to PATH (add to ~/.zshrc or ~/.bashrc if not already configured)
export PATH="$HOME/go/bin:$PATH"

# Enable in your project
cd your-project && entire enable

# Check status
entire status
```

After the initial setup, use `entire configure` to add/remove agents or update setup-related options, and use `entire enable` / `entire disable` to toggle Entire on or off.

## Release Channels

Entire currently ships two release channels:

- `stable`: recommended for most users. Stable releases change less often and are the default for Homebrew, Scoop, and `install.sh`.
- `nightly`: prerelease builds for users who want the latest changes earlier. Nightlies are published more frequently and may include newer, less-proven changes than stable.

How to use each channel:

- Homebrew stable: `brew install --cask entire`
- Homebrew nightly: `brew install --cask entire@nightly`
- `install.sh` stable: `curl -fsSL https://entire.io/install.sh | bash`
- `install.sh` nightly: `curl -fsSL https://entire.io/install.sh | bash -s -- --channel nightly`
- Scoop: currently supports `stable` only via `scoop install entire/cli`

## Typical Workflow

### 1. Enable Entire in Your Repository

```
entire enable
```

On a repo that has not been enabled yet, `entire enable` runs the initial enable flow: it creates Entire settings, installs git hooks, and prompts you to choose which agent hooks to install. To enable a specific agent non-interactively, use `entire enable --agent <name>` (for example, `entire enable --agent cursor`).

After setup:

- Use `entire enable` to turn Entire back on if the repo is currently disabled.
- Use `entire configure` to change which agents are installed or to update setup-related settings.

The hooks capture session data as you work. Checkpoints are created when you or the agent make a git commit. Your code commits stay clean, Entire never creates commits on your active branch. All session metadata is stored on a separate `entire/checkpoints/v1` branch.

### 2. Work with Your AI Agent

Just use one of your AI agents as before. Entire runs in the background, tracking your session:

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
| `entire clean`   | Clean up session data and orphaned Entire data (use `--all` for repo-wide cleanup)                |
| `entire configure` | Configure agents and setup options for the current repository                                  |
| `entire disable` | Remove Entire hooks from repository                                                               |
| `entire doctor`  | Fix or clean up stuck sessions                                                                    |
| `entire enable`  | Enable Entire in your repository                                                                  |
| `entire explain` | Explain a session or commit                                                                       |
| `entire login`   | Authenticate the CLI with Entire device auth                                                      |
| `entire resume`  | Switch to a branch, restore latest checkpointed session metadata, and show command(s) to continue |
| `entire rewind`  | Rewind to a previous checkpoint                                                                   |
| `entire status`  | Show current session info                                                                         |
| `entire sessions stop` | Mark one or more active sessions as ended                                                   |
| `entire version` | Show Entire CLI version                                                                           |

### `entire enable` Flags

| Flag                                        | Description                                                                                                       |
| ------------------------------------------- | ----------------------------------------------------------------------------------------------------------------- |
| `--agent <name>`                            | AI agent to install hooks for: `claude-code`, `codex`, `gemini`, `opencode`, `cursor`, `factoryai-droid`, or `copilot-cli` |
| `--force`, `-f`                             | Force reinstall hooks (removes existing Entire hooks first)                                                       |
| `--local`                                   | Write settings to `settings.local.json` instead of `settings.json`                                                |
| `--project`                                 | Write settings to `settings.json` even if it already exists                                                       |
| `--skip-push-sessions`                      | Disable automatic pushing of session logs on git push                                                             |
| `--checkpoint-remote <provider:owner/repo>` | Push checkpoint branches to a separate repo (e.g., `github:org/checkpoints-repo`)                                 |
| `--telemetry=false`                         | Disable anonymous usage analytics                                                                                 |

**Examples:**

```
# First-time setup with a specific agent
entire enable --agent claude-code

# Re-enable a disabled repo
entire enable

# Re-enable and refresh hooks
entire enable --force

# Save settings locally (not committed to git)
entire enable --local
```

`entire enable` is primarily for turning Entire on. On an unconfigured repo it will also bootstrap setup, but once the repo is already configured, `entire configure` is the clearer command for managing agents and setup options.

### `entire configure`

Use `entire configure` after the repo is already set up and you want to change the configuration without framing the action as an enable/disable toggle.

Typical uses:

- Add another agent
- Remove an agent
- Reinstall hooks for selected agents
- Update settings such as `--checkpoint-remote` or `--skip-push-sessions`

**Examples:**

```bash
# Add or remove agents interactively
entire configure

# Install or refresh hooks for one agent non-interactively
entire configure --agent claude-code --force

# Update setup settings on an existing repo
entire configure --checkpoint-remote github:myorg/checkpoints-private

# Remove one agent's hooks
entire configure --remove claude-code
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
| Codex            | `.codex/hooks.json`           | JSON hooks config |
| Copilot CLI      | `.github/hooks/entire.json`   | JSON hooks config |
| Cursor           | `.cursor/hooks.json`          | JSON hooks config |
| Factory AI Droid | `.factory/settings.json`      | JSON hooks config |
| Gemini CLI       | `.gemini/settings.json`       | JSON hooks config |
| OpenCode         | `.opencode/plugins/entire.ts` | TypeScript plugin |

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

### Agent-Specific Steps & Limitations

- When enabling Entire for Codex, the command will also create or update `.codex/config.toml` with `codex_hooks = true` to enable Codex hooks. If you configure Codex manually, make sure this flag is set in your `.codex/config.toml`. Or select Codex from the interactive agent picker when running `entire enable`.
- Entire supports Cursor IDE and Cursor Agent CLI tool, but `entire rewind` is not available at this time. Other commands (`doctor`, `status` etc.) work the same as all other agents.
- Entire supports Copilot CLI, but not Copilot in VS Code, in other IDEs, or on github.com.

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
| "shadow branch conflict" | Run `entire clean --force`                              |

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

### Cleaning Up State

```
# Clean session data for current commit
entire clean --force

# Clean all orphaned data across the repository
entire clean --all --force

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

### Dev Container

The repo includes a `.devcontainer/` configuration that installs the system packages used by local development and CI (`git`, `tmux`, `gnome-keyring`, etc) and then bootstraps the repo's `mise` toolchain.

Open the folder in a Dev Container, or start it from the `devcontainer` CLI as follows:

```bash
devcontainer up --workspace-folder .
devcontainer exec --workspace-folder . bash -lc '.devcontainer/run-with-keyring.sh'
```

The container's `postCreateCommand` runs `mise trust --yes && mise install`, so Go, `golangci-lint`, `gotestsum`, `shellcheck`, and the canary E2E helper binaries are ready after creation. Use `.devcontainer/run-with-keyring.sh <command>` for commands that touch the Linux keyring, including `mise run test:ci`.

If `ENTIRE_DEVCONTAINER_KEYRING_PASSWORD` is set in the environment, `.devcontainer/run-with-keyring.sh` uses that value to unlock the keyring non-interactively. If it is unset, the script generates a random password for the session automatically.

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
