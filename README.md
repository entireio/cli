# Entire CLI

Entire hooks into your Git workflow to capture AI agent sessions as you work. Sessions are indexed alongside commits, creating a searchable record of how code was written in your repo.

With Entire, you can:

- **Understand why code changed** — see the full prompt/response transcript and files touched
- **Recover instantly** — resume from a known-good checkpoint when an agent goes sideways
- **Keep Git history clean** — preserve agent context on a separate branch
- **Onboard faster** — show the path from prompt → change → commit
- **Maintain traceability** — support audit and compliance requirements when needed

## Why Entire

- **Understand why code changed, not just what** — Transcripts, prompts, files touched, token usage, tool calls, and more are captured alongside every commit.
- **Resume from any checkpoint** — Go back to any previous agent session and pick up exactly where you or a coworker left off.
- **Full context preserved and searchable** — A versioned record of every AI interaction tied to your git history, with nothing lost.
- **Zero context switching** — Git-native, two-step setup, works with Claude Code, Codex, Gemini, Pi, and more.

## Table of Contents

- [Why Entire](#why-entire)
- [Quick Start](#quick-start)
- [Typical Workflow](#typical-workflow)
- [Key Concepts](#key-concepts)
  - [How It Works](#how-it-works)
  - [Strategy](#strategy)
- [Headless & CI Authentication](#headless--ci-authentication)
- [Local Device Auth Testing](#local-device-auth-testing)
- [Commands Reference](#commands-reference)
- [Plugins](#plugins)
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

### macOS and Linux

Install with Homebrew:

```bash
brew tap entireio/tap
brew trust entireio/tap
brew install --cask entire            # stable
# brew install --cask entire@nightly  # or nightly
```

Or with the install script:

```bash
curl -fsSL https://entire.io/install.sh | bash                          # stable
# curl -fsSL https://entire.io/install.sh | bash -s -- --channel nightly  # or nightly
```

### Windows

Install with Scoop:

```powershell
scoop bucket add entire https://github.com/entireio/scoop-bucket.git
scoop install entire/cli
```

### Go (development/manual setup)

```bash
go install github.com/entireio/cli/cmd/entire@latest

# Add Go binaries to PATH (add to ~/.zshrc or ~/.bashrc if not already configured)
export PATH="$HOME/go/bin:$PATH"
```

### Enable in your project

```bash
cd your-project && entire enable

# Check status
entire status
```

After the initial setup, use `entire agent` to add or remove agents, `entire configure` to update non-agent settings, and `entire enable` / `entire disable` to toggle Entire on or off.

## Release Channels

Entire currently ships two release channels:

- `stable`: recommended for most users. Stable releases change less often and are the default for Homebrew, Scoop, and `install.sh`.
- `nightly`: prerelease builds for users who want the latest changes earlier. Nightlies are published more frequently and may include newer, less-proven changes than stable.

How to use each channel:

- Homebrew (one-time setup): `brew tap entireio/tap && brew trust entireio/tap`
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
- Use `entire agent` to add or remove agents.
- Use `entire configure` to update non-agent settings (telemetry, hooks, checkpoint remote, summary provider).

The hooks capture session data as you work. Checkpoints are created when you or the agent make a git commit. Your code commits stay clean, Entire never creates commits on your active branch. All session metadata is stored on a separate `entire/checkpoints/v1` branch.

### 2. Work with Your AI Agent

Just use one of your AI agents as before. Entire runs in the background, tracking your session:

```
entire status  # Check current session status anytime
```

### 3. Resume a Previous Session

To restore the latest checkpointed session metadata for a branch:

```
entire session resume <branch>
```

Entire checks out the branch, restores the latest checkpointed session metadata (one or more sessions), and prints command(s) to continue.

### 4. Disable Entire (Optional)

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

A **checkpoint** is a snapshot within a session—a "save point" in your work.

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
- **Metadata stored separately** — all session data lives on the `entire/checkpoints/v1` branch

### Git Worktrees

Entire works seamlessly with [git worktrees](https://git-scm.com/docs/git-worktree). Each worktree has independent session tracking, so you can run multiple AI sessions in different worktrees without conflicts.

### Concurrent Sessions

Multiple AI sessions can run on the same commit. If you start a second session while another has uncommitted work, Entire warns you and tracks them separately. Both sessions' checkpoints are preserved and can be rewound independently.

## Headless & CI Authentication

By default `entire login` stores tokens in the OS keyring (macOS Keychain,
Linux Secret Service, Windows Credential Manager). Machines without a usable
keyring — headless servers, containers, minimal VMs, CI runners — have two
supported paths:

### Interactive login on a headless machine

Use the file-backed token store. The device-auth flow already works without a
local browser (the CLI prints an approval URL you can open on any machine);
only token storage needs the override:

```bash
ENTIRE_TOKEN_STORE=file entire login
```

Tokens are written with `0600` permissions to `tokens.json` in your Entire
config directory (`~/.config/entire` by default). Override the location with
`ENTIRE_TOKEN_STORE_PATH`. Set `ENTIRE_TOKEN_STORE=file` persistently (e.g. in
your shell profile) so later commands read from the same store.

### Non-interactive automation (CI, workload identity)

Skip login and storage entirely by injecting a token per invocation:

```bash
ENTIRE_TOKEN=<login-or-sa-session-JWT> entire ...
```

`ENTIRE_TOKEN` bypasses stored credentials; the CLI derives the control-plane
endpoint from the token itself. Nothing is written to disk. This is the right
path for CI pipelines and service accounts.

> **Seeing `save login` / `failed to unlock correct collection` errors from
> `entire login`?** That's the OS keyring being unavailable — use one of the
> two paths above.

## Local Device Auth Testing

If you're working on the CLI device auth flow against a local `entire.io` checkout:

```bash
# In your app repo
cd ../entire.io-1
mise run dev

# In this repo, point the CLI at the local API. The login flow targets
# the local server via --server (the default is the production
# us.auth.entire.io).
cd ../cli
export ENTIRE_API_BASE_URL=http://localhost:8787
entire login --server https://localhost:8180

# Run the smoke test
./scripts/local-device-auth-smoke.sh
```

Useful commands while developing:

```bash
# Run the login flow against a local server (prompts to press Enter before opening the browser)
go run ./cmd/entire login --server https://localhost:8180

# Run the focused integration coverage for login
go test -tags=integration ./cmd/entire/cli/integration_test -run TestLogin
```

## Commands Reference

| Command          | Description                                                                                       |
| ---------------- | ------------------------------------------------------------------------------------------------- |
| `entire clean`   | Clean up session data and orphaned Entire data (use `--all` for repo-wide cleanup)                |
| `entire agent`   | Add, remove, or list agent integrations for the current repository                                |
| `entire configure` | Update non-agent settings (telemetry, git hook, strategy options, summary provider)            |
| `entire disable` | Remove Entire hooks from repository                                                               |
| `entire doctor`  | Fix or clean up stuck sessions                                                                    |
| `entire enable`  | Enable Entire in your repository                                                                  |
| `entire checkpoint`        | List, explain, and search checkpoints                                                   |
| `entire checkpoint explain` | Explain a session, commit, or checkpoint                                               |
| `entire login`   | Authenticate the CLI with Entire device auth                                                      |
| `entire org`     | Manage Entire organizations (create, list, get, delete)                                           |
| `entire plugin`  | Discover, install, upgrade, and remove plugins (see [Plugins](#plugins))                          |
| `entire project` | Manage Entire projects (create, list, get, delete)                                                |
| `entire repo`    | Manage Entire repositories (create, list, get, delete, clone, mirror, visibility)                 |
| `entire grant`   | Manage access grants and org membership (org, project, repo)                                      |
| `entire session` | View and manage agent sessions tracked by Entire                                                  |
| `entire session resume`    | Switch to a branch, restore latest checkpointed session metadata, and show command(s) |
| `entire session attach`    | Attach to a previously detached session                                                |
| `entire status`  | Show current session info                                                                         |
| `entire doctor trace` | Show hook performance traces                                                                 |
| `entire version` | Show Entire CLI version                                                                           |

`entire blame` and `entire why` are experimental Labs commands. Run `entire
labs` to discover them. `entire blame <file>` shows which current file lines
came from an Entire checkpoint, and `entire why <file>:<line>` jumps from a
specific line back to the prompt, session, and checkpoint that created it. Use
`entire blame <file> --long` for the full agent, model, author, and session
table.

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

`entire enable` is primarily for turning Entire on. On an unconfigured repo it will also bootstrap setup. Use `entire agent` for adding or removing agents, and `entire configure` for non-agent settings.

### `entire configure`

Use `entire configure` to update non-agent settings on a repo that's already set up. Agent installation lives under `entire agent`.

Typical uses:

- Toggle telemetry
- Reinstall the Entire git hook (`--force`, `--absolute-git-hook-path`)
- Update strategy options such as `--checkpoint-remote` or `--skip-push-sessions`
- Pick a summary provider for `entire explain --generate`

**Examples:**

```bash
# Show help and the hint pointing to 'entire agent'
entire configure

# Opt out of telemetry
entire configure --telemetry=false

# Reinstall the Entire git hook with an absolute binary path
entire configure --absolute-git-hook-path

# Update strategy options on an existing repo
entire configure --checkpoint-remote github:myorg/checkpoints-private

# Add or remove an agent
entire agent add claude-code
entire agent remove claude-code
```

## Plugins

Plugins extend the CLI with new verbs: any executable named `entire-<name>` on `$PATH` runs as `entire <name>`, kubectl-style — stdio passes through, exit codes propagate, no SDK or protocol required.

```sh
entire plugin search                                    # browse the plugin index
entire plugin install upgrade                           # install by name (index lookup)
entire plugin install https://github.com/you/entire-x   # install from any git host
entire plugin install ./dist/entire-x                   # link a local build
entire upgrade                                          # run an installed plugin
entire plugin upgrade --all                             # update remote-installed plugins
entire plugin doctor                                    # check for broken installs and missing dependencies
entire plugin remove x
```

Remote installs are forge-agnostic: the newest stable semver tag is resolved over the git protocol (prereleases need an explicit `--pin`), and the platform's release asset is downloaded over HTTPS and verified against the release's `checksums.txt`. A release that publishes no checksums is refused unless you pass `--allow-unverified` — installing means making those bytes executable. `entire plugin doctor` re-checks installed binaries against the digests recorded at install time. Plugins can declare dependencies on other plugins in an `entire-plugin.yml`; missing ones are installed after a single confirmation.

Discovery uses a git-synced catalog, [entireio/plugin-index](https://github.com/entireio/plugin-index) by default. Organizations can point the CLI at an internal catalog with the `ENTIRE_PLUGIN_INDEX_URL` environment variable or `--index`. It is deliberately not settable from a repository's committed settings: an index-listed plugin installs without a prompt, so a checked-out repo must not be able to choose the catalog.

For the full contract — resolution rules, environment filtering, release-asset conventions, and how to author a plugin — see [External Commands](docs/architecture/external-commands.md).

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
| Pi               | `.pi/extensions/entire/index.ts` | TypeScript extension |

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
- Ignore the setting if it looks inherited rather than yours, and push checkpoints to your own push remote instead. `checkpoint_remote` is normally committed in `.entire/settings.json`, so forking a project inherits it — without this, a contributor's session data would be pushed into the upstream project's checkpoint repo. A setting is treated as yours when it lives in the gitignored `.entire/settings.local.json`, or when your `origin` remote is owned by the same account or org as the checkpoint repo
- If your checkpoint repo is owned by a different account or org than `origin`, configure it in `.entire/settings.local.json` so it is always honored
- If the remote is unreachable, warn and continue without blocking your main push

#### `ENTIRE_CHECKPOINT_TOKEN`

`ENTIRE_CHECKPOINT_TOKEN` allows you to provide a dedicated token for checkpoint repository operations, without modifying the credentials used for your primary repository.

When this environment variable is set, Entire behaves as follows:

- Injects the token into HTTPS Git operations used for checkpoint fetch and push
- If `checkpoint_remote` is configured:
  - Prefers an HTTPS URL for the checkpoint remote when a token is present, even if the repository’s `origin` uses SSH
- If `checkpoint_remote` is not configured:
  - Falls back to using the default `origin` remote
- If `checkpoint_remote` configuration cannot be loaded:
  - Falls back to `origin`
  - If `origin` is a valid SSH or HTTPS Git remote, Entire converts it to an HTTPS URL to enable token-based authentication

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

- Codex hooks are enabled by default (codex-cli 0.124.0+), so enabling Entire for Codex only installs `.codex/hooks.json` — no `config.toml` is needed and Entire never creates one. If an older Entire version left a `.codex/config.toml` behind and your repo lives inside `~/.codex/agents`, delete that file to stop Codex's "malformed agent role definition" startup warning.
- Entire supports Cursor IDE and Cursor Agent CLI tool. Commands (`doctor`, `status` etc.) work the same as all other agents.
- Entire supports Copilot CLI, but not Copilot in VS Code, in other IDEs, or on github.com.
- Entire supports Pi coding agent (Preview). Pi uses a TypeScript extension instead of a JSON hook config. Subagent capture is not currently available.

## Security & Privacy

**Your session transcripts are stored in your git repository** on the `entire/checkpoints/v1` branch. If your repository is public, this data is visible to anyone.

Entire automatically redacts detected secrets (API keys, tokens, credentials) when writing to `entire/checkpoints/v1`, but redaction is best-effort. Temporary shadow branches used during a session may contain unredacted data and should not be pushed. See [docs/security-and-privacy.md](docs/security-and-privacy.md) for details.

## Troubleshooting

### Common Issues

| Issue                    | Solution                                                |
| ------------------------ | ------------------------------------------------------- |
| "Not a git repository"   | Navigate to a Git repository first                      |
| "Entire is disabled"     | Run `entire enable`                                     |
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
