# Entire CLI

Entire hooks into your Git workflow to capture AI agent sessions as you work. Sessions are indexed alongside commits, creating a searchable record of how code was written in your repo.

With Entire, you can:

- **Understand why code changed** — see the full prompt/response transcript and files touched
- **Recover instantly** — resume from a known-good checkpoint when an agent goes sideways
- **Keep Git history clean** — agent context lives outside your branch's history
- **Onboard faster** — show the path from prompt → change → commit
- **Maintain traceability** — support audit and compliance requirements when needed

## AgentCheck

> **"You test your software before shipping it. AgentCheck tests the work your AI coding agent just produced before you trust it."**

AgentCheck is a checkpoint-native quality gate and verification system built
directly into the Entire CLI. While Entire preserves the full context and
decision provenance of an AI coding session, AgentCheck evaluates the resulting
work against explicit developer intent, boundary constraints, quality signals,
and test verification to determine whether the output deserves trust.

### One-Sentence Summary

AgentCheck is an AI coding-agent verification layer for the Entire CLI that
evaluates agent-generated code against developer intent, strict boundaries, and
test runs to provide evidence-backed trust verdicts (**TRUSTED**,
**REVIEW REQUIRED**, or **FAIL**).

### Problem, Intended User, and Why It Matters

As developers increasingly delegate complex coding tasks to AI agents, a critical
trust gap emerges:

- **Silent scope creep and boundary violations:** AI coding agents can introduce unrequested refactors, alter database schemas, add unnecessary dependencies, or touch protected files outside their task scope.
- **Incomplete verification:** Agents can claim a task is complete or that tests pass without actually executing verification suites in the target environment.
- **Loss of context:** Standard code review tools inspect only the final `git diff`, losing the rich context of why choices were made, what prompt constraints were set, and what intermediate iterations failed.

AgentCheck is intended for software developers, tech leads, and DevOps engineers
who use autonomous or interactive AI coding agents and need an automated,
reliable quality gate before merging agent-produced commits.

AgentCheck transforms code review from a manual, high-overhead diff inspection
into an automated, context-aware trust check. By validating that agents stay
within explicit boundaries and produce verifiable work, teams can safely
accelerate AI-driven software development.

### Selected Entire Track and Why Entire is Essential

- **Selected Track:** **Track 1 - Checkpoint-Native Developer Experience**

A standard Git diff shows what changed, but it lacks the prompt constraints,
intermediate agent iterations, and architectural intent behind the change.
AgentCheck relies on Entire checkpoints as an essential input:

1. **Intent recovery:** AgentCheck extracts explicit constraints such as "Do NOT modify the database schema" from preserved Entire checkpoint prompts and session context.
2. **Contextual evaluation:** It evaluates whether code changes violate the original intent captured in the checkpoint, which is not possible from Git diff alone.
3. **Session continuity across interruptions:** Entire checkpoint context lets later sessions reconstruct the development memory and proceed safely.

### Architecture and Main Workflow

The current AgentCheck implementation is deliberately small and contract-driven:

```text
Entire checkpoint
    -> agentcheck.Context
    -> Evaluate()
    -> EvaluationResult
    -> terminal renderer or future CLI integration
```

`agentcheck.Context` is the boundary between Entire checkpoint/session internals
and the evaluation engine. The evaluator consumes only that context, combines
intent/boundary findings with quality/bloat findings, preserves evidence, orders
findings deterministically, and produces one of three verdicts:
**TRUSTED**, **REVIEW REQUIRED**, or **FAIL**.

## Why Entire

- **Understand why code changed, not just what** — Transcripts, prompts, files touched, token usage, tool calls, and more are captured alongside every commit.
- **Resume from any checkpoint** — Go back to any previous agent session and pick up exactly where you or a coworker left off.
- **Full context preserved and searchable** — A versioned record of every AI interaction tied to your git history, with nothing lost.
- **Zero context switching** — Git-native, two-step setup, works with Claude Code, Codex, Gemini, Pi, and more.

## Table of Contents

- [AgentCheck](#agentcheck)
- [Why Entire](#why-entire)
- [Requirements](#requirements)
- [Quick Start](#quick-start)
- [Release Channels](#release-channels)
- [Typical Workflow](#typical-workflow)
- [Key Concepts](#key-concepts)
  - [Sessions](#sessions)
  - [Checkpoints](#checkpoints)
  - [Checkpoint Storage](#checkpoint-storage)
  - [How It Works](#how-it-works)
  - [Strategy](#strategy)
- [Headless & CI Authentication](#headless--ci-authentication)
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
- Go 1.26+ only if you install with `go install` (the packaged installs bundle their own runtime)

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

Install with Windows PowerShell 5.1 or later:

```powershell
irm https://entire.io/install.ps1 | iex  # stable
# or nightly:
# iex "& {$(irm https://entire.io/install.ps1)} -Channel nightly"
```

For stable releases, the PowerShell installer uses Scoop when it is available,
adding the Entire bucket if needed. Without Scoop — and for nightly releases —
it verifies the release checksum and installs both `entire.exe` and
`git-remote-entire.exe` to `%USERPROFILE%\.local\bin`, adding that directory to
your user `PATH` if needed.

Or install the stable release with Scoop:

```powershell
scoop bucket add entire https://github.com/entireio/scoop-bucket.git
scoop install entire/entire
```

#### Migrating an old `cli` Scoop install (package rename)

The Scoop package was renamed from `cli` to `entire`. If your install is still registered as the old `cli` package, run the migration below. It installs the new package before removing the old one, so the old install is only removed once the new one is in place (`scoop reset` re-links the shared `entire.exe` and `git-remote-entire.exe` shims). Run it where `entire` is **not** running — a live `entire.exe` locks its own shim, so Scoop can't relink or uninstall it mid-run:

```powershell
cmd.exe /D /C "scoop install entire/entire && scoop uninstall entire/cli && scoop reset entire"
```

If the first step fails with "couldn't find manifest", your bucket clone predates the renamed package — run `scoop update` to refresh it, then retry the command above. Nothing is removed until the install succeeds, so a failed attempt leaves your existing install working.

### Go (development/manual setup)

```bash
go install github.com/entireio/cli/cmd/entire@latest

# The git remote helper that resolves entire:// URLs, needed for `entire repo clone`
# and `git clone entire://…`. The packaged installs above bundle it.
go install github.com/entireio/cli/cmd/git-remote-entire@latest

# Add Go binaries to PATH (add to ~/.zshrc or ~/.bashrc if not already configured)
export PATH="$HOME/go/bin:$PATH"
```

Install both, or just `entire` if you never clone over `entire://`. Git finds the helper by name on `$PATH`, which is what `go install` produces, so nothing else needs configuring.

One difference from a stable release: a `go install` build leaves [experimental commands](#experimental-commands) visible in `entire help`, the same as a nightly or a local build.

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
- `install.ps1` stable (uses Scoop when available): `irm https://entire.io/install.ps1 | iex`
- `install.ps1` nightly: `iex "& {$(irm https://entire.io/install.ps1)} -Channel nightly"`
- Scoop: currently supports `stable` only via `scoop install entire/entire`

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

The hooks capture session data as you work. Checkpoints are created when you or the agent make a git commit. Your code commits stay clean, Entire never creates commits on your active branch. Session metadata is stored outside your branch's history, in the checkpoint storage described under [Checkpoint Storage](#checkpoint-storage).

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

The session ID is the unique identifier the agent itself provides — Entire never mints its own — so an ID you see in `entire session list` is the same one the agent uses. The format is the agent's to choose: most supply a UUID (e.g. `019efea2-b46a-7cbc-be01-4c13460f5019`), while OpenCode uses `ses_`-prefixed IDs.

Sessions are stored separately from your code commits, in the repo's checkpoint storage.

### Checkpoints

A **checkpoint** is a snapshot within a session—a "save point" in your work.

Checkpoints are created when you or the agent make a git commit, and the commit carries an `Entire-Checkpoint: <id>` trailer linking the two.

**Checkpoint IDs** are 26-character ULIDs (e.g. `01K9TQ8ZP7X3F5M2WVJ4CNRB6D`), which sort by creation time. Checkpoints written by older versions of Entire carry a legacy 12-character hex ID (e.g. `a3b2c4d5e6f7`); both formats stay readable in the same repo, and you can pass either to any command that takes a checkpoint ID.

### Checkpoint Storage

Checkpoints live in your repository's own git object store, never in your branch's history. Each checkpoint is its own git ref:

```
refs/entire/checkpoints/<shard>/<id>
```

The ref points at a commit whose tree *is* that checkpoint — `metadata.json`, the per-session transcript files, and any subagent task records. `<shard>` is the last two characters of the ID, which keeps the refs evenly distributed.

Because checkpoints are independent refs, they are written, pushed, and fetched independently. There is no shared branch tip for concurrent sessions to contend on, and a reader can fetch exactly the one checkpoint it needs instead of a whole history. A checkpoint written on another machine is fetched on demand the first time you read it.

`entire enable` sets this up; there is nothing to configure. It is recorded in `.entire/settings.json` as:

```json
{
  "checkpoints": {
    "primary": { "type": "git-refs" }
  }
}
```

Inspect what a repo has locally with plain git, or through Entire:

```bash
git for-each-ref refs/entire/checkpoints    # the raw refs
entire checkpoint list                      # checkpoints on this branch
entire checkpoint explain <id>              # one checkpoint in full
```

### How It Works

```
Your Branch                      Checkpoint storage
     │                                  │
     ▼                                  │
[Base Commit]                           │
     │                                  │
     │  ┌─── Agent works ───┐           │
     │  │  Turn 1           │           │
     │  │  Turn 2           │           │
     │  │  Turn 3           │           │
     │  └───────────────────┘           │
     │                                  │
     ▼                                  ▼
[Your Commit] ─────────────────► [Checkpoint]
  Entire-Checkpoint: <id>          (transcript, prompts,
     │                              files touched, tokens)
     ▼
```

Work in progress is held on a short-lived shadow branch as you go. When you commit, that work is condensed into a permanent checkpoint and linked to your commit by an `Entire-Checkpoint` trailer.

### Strategy

Entire uses a manual-commit strategy that keeps your git history clean:

- **No commits on your branch** — Entire never creates commits on the active branch
- **Safe on any branch** — works on main, master, and feature branches alike
- **Metadata stored separately** — session data lives in per-checkpoint refs, never in your branch's history

### Git Worktrees

Entire works seamlessly with [git worktrees](https://git-scm.com/docs/git-worktree). Each worktree has independent session tracking, so you can run multiple AI sessions in different worktrees without conflicts.

### Concurrent Sessions

Multiple AI sessions can run on the same commit. If you start a second session while another has uncommitted work, Entire warns you and tracks them separately. Both sessions' checkpoints are preserved, and a commit condenses every session with pending work in that worktree.

## Headless & CI Authentication

By default `entire login` opens a browser to sign in and stores tokens in the OS keyring (macOS Keychain, Linux Secret Service, Windows Credential Manager). Machines without a usable browser or keyring — headless servers, containers, minimal VMs, CI runners — have two supported paths:

### Interactive login on a headless machine

Sign-in itself already handles this: with no interactive terminal, or over SSH, `entire login` switches to the device-code flow on its own and prints an approval URL you can open on any machine. `entire login --device` forces that flow explicitly. Only token *storage* needs an override — use the file-backed store:

```bash
ENTIRE_TOKEN_STORE=file entire login
```

Tokens are written with `0600` permissions to `tokens.json` in your Entire config directory (`~/.config/entire` by default). Override the location with `ENTIRE_TOKEN_STORE_PATH`. Set `ENTIRE_TOKEN_STORE=file` persistently (e.g. in your shell profile) so later commands read from the same store.

### Non-interactive automation (CI, workload identity)

Skip login and storage entirely by injecting a token per invocation:

```bash
ENTIRE_TOKEN=<login-or-sa-session-JWT> entire ...
```

`ENTIRE_TOKEN` bypasses stored credentials; the CLI derives the control-plane endpoint from the token itself. Nothing is written to disk. This is the right path for CI pipelines and service accounts.

> **Seeing `save login` / `failed to unlock correct collection` errors from `entire login`?** That's the OS keyring being unavailable — use one of the two paths above.

## Commands Reference

Descriptions below are the commands' own summaries. `entire help` always reflects the installed binary; `entire agent-help` is the machine-readable version agents read.

### Setup

| Command            | Description                                                                        |
| ------------------ | ---------------------------------------------------------------------------------- |
| `entire enable`    | Enable Entire in current repository                                                |
| `entire disable`   | Disable Entire in current repository                                               |
| `entire status`    | Show Entire status (`--json` for structured output)                                |
| `entire agent`     | Manage agent integrations (`add`, `remove`, `list`)                                |
| `entire configure` | Update non-agent Entire settings in the current repository                          |
| `entire doctor`    | Diagnose and fix session issues (`trace`, `logs`, `bundle`, `migrate-checkpoints`)  |
| `entire clean`     | Clean up Entire session data (`--all` for repo-wide cleanup)                       |
| `entire plugin`    | Manage Entire plugins (see [Plugins](#plugins))                                    |

### Sessions & Checkpoints

| Command                       | Description                                                                              |
| ----------------------------- | ---------------------------------------------------------------------------------------- |
| `entire session`              | Manage agent sessions (`list`, `info`, `current`, `stop`, `attach`, `adopt`, `resume`, `tokens`) |
| `entire session resume`       | Resume a stopped session — interactive picker, or by branch                               |
| `entire checkpoint`           | Inspect and search checkpoints (`list`, `explain`, `tokens`, `search`)                    |
| `entire checkpoint explain`   | Explain a checkpoint, commit, or session                                                 |
| `entire search`               | Search checkpoints, commits, and sessions using semantic and keyword matching            |
| `entire activity`             | Show your activity overview                                                              |
| `entire recap`                | Summarize recent checkpoint activity                                                     |
| `entire dispatch`             | Generate a dispatch summarizing recent agent work                                         |

### Account

| Command          | Description                                                                          |
| ---------------- | ------------------------------------------------------------------------------------ |
| `entire login`   | Log in to Entire (browser by default; `--device` for the device-code flow)            |
| `entire logout`  | Log out of Entire                                                                    |
| `entire auth`    | Manage authentication (`status`, `contexts`, `use`, `token`, `login`, `logout`)       |

### Control Plane

| Command          | Description                                                                       |
| ---------------- | --------------------------------------------------------------------------------- |
| `entire org`     | Manage Entire organizations (`create`, `list`, `get`, `delete`)                    |
| `entire project` | Manage Entire projects (`create`, `list`, `get`, `delete`)                         |
| `entire repo`    | Manage Entire repositories (`create`, `list`, `get`, `delete`, `clone`, `mirror`, `visibility`) |
| `entire grant`   | Manage Entire access grants and org membership (`org`, `project`, `repo`)          |
| `entire api`     | Make an authenticated request to an Entire API and print the response              |

### Other

| Command             | Description                                                                    |
| ------------------- | ------------------------------------------------------------------------------ |
| `entire agent-help` | Machine-readable usage for coding agents (always matches the installed CLI)     |
| `entire labs`       | Explore experimental Entire workflows                                          |
| `entire version`    | Show build information                                                         |

### Experimental Commands

These are visible in developer and nightly builds and hidden in stable releases, but always runnable in every build. Run `entire labs` to discover them.

| Command              | Description                                              |
| -------------------- | -------------------------------------------------------- |
| `entire review`      | Run a multi-agent review against a branch                |
| `entire investigate` | Run a multi-agent investigation against the current branch |
| `entire tokens`      | Analyze token usage across sessions and checkpoints       |
| `entire blame`       | Show which lines came from Entire checkpoints             |
| `entire why`         | Show why a line exists                                   |
| `entire experts`     | Rank agent provenance for code scopes                    |
| `entire import`      | Import pre-existing agent history into Entire            |
| `entire runner`      | Set up and tune trail runners for this repository        |

`entire blame <file>` shows which current file lines came from an Entire checkpoint (`--long` for the full agent, model, author, and session table), and `entire why <file>:<line>` jumps from a specific line back to the prompt, session, and checkpoint that created it.

### `entire enable` Flags

| Flag                                        | Description                                                                                                       |
| ------------------------------------------- | ----------------------------------------------------------------------------------------------------------------- |
| `--agent <name>`                            | Agent to set up hooks for: `claude-code`, `codex`, `copilot-cli`, `cursor`, `factoryai-droid`, `gemini`, `opencode`, `pi` (external agents on `$PATH` also work). Enables non-interactive mode |
| `--yes`, `-y`                               | Accept all defaults without prompting                                                                             |
| `--force`, `-f`                             | Force reinstall hooks (removes existing Entire hooks first)                                                       |
| `--checkpoint-remote <provider:owner/repo>` | Push checkpoint data to a separate repo (e.g., `github:org/checkpoints-repo`)                                     |
| `--skip-push-sessions`                      | Disable automatic pushing of checkpoint data on git push                                                           |
| `--local`                                   | Write settings to `.entire/settings.local.json` instead of `.entire/settings.json`                                |
| `--project`                                 | Write settings to `.entire/settings.json` even if it already exists                                               |
| `--absolute-git-hook-path`                  | Embed the full binary path in git hooks (for GUI git clients that don't source shell profiles)                    |
| `--import-history`                          | During first-time setup, import the selected agents' existing session history (last 30 days) without prompting     |
| `--search-skill`                            | Install the optional Entire search skill for the selected agent(s)                                                |
| `--agent-help-skill`                        | Install the Entire agent-help skill (points agents at `entire agent-help`) for the selected agent(s)              |
| `--telemetry=false`                         | Disable anonymous usage analytics                                                                                 |

Run in a directory that is not a git repository, `entire enable` offers to initialize one and (optionally) create a matching GitHub repo via the `gh` CLI. That path is driven by `--init-repo` / `--no-init-repo`, `--no-github`, `--repo-name`, `--repo-owner`, `--repo-visibility`, `--push`, `--skip-initial-commit`, and `--initial-commit-message`. See `entire enable --help` for the full list.

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
- Pick the provider, model, and timeout for `entire checkpoint explain --generate` (`--summarize-provider`, `--summarize-model`, `--summarize-timeout-seconds`)

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

# Choose who generates summaries
entire configure --summarize-provider claude-code
```

Adding or removing an agent lives under `entire agent`:

```bash
entire agent add claude-code
entire agent remove claude-code
```

## Plugins

Plugins extend the CLI with new verbs: any executable named `entire-<name>` on `$PATH` runs as `entire <name>`, kubectl-style — stdio passes through, exit codes propagate, no SDK or protocol required.

```sh
entire plugin search                                    # search the plugin index
entire plugin browse                                    # browse the index interactively and install
entire plugin install run                               # install by name (index lookup)
entire plugin install https://github.com/you/entire-x   # install from any git host
entire plugin install ./dist/entire-x                   # link a local build
entire plugin list                                      # list what's installed
entire run                                              # run an installed plugin
entire plugin upgrade --all                             # update remote-installed plugins
entire plugin doctor                                    # check for broken installs and missing dependencies
entire plugin remove run
```

Remote installs are forge-agnostic: the newest stable semver tag is resolved over the git protocol (prereleases need an explicit `--pin`), and the platform's release asset is downloaded over HTTPS and verified against the release's `checksums.txt`. A release that publishes no checksums is refused unless you pass `--allow-unverified` — installing means making those bytes executable. `entire plugin doctor` re-checks installed binaries against the digests recorded at install time. Plugins can declare dependencies on other plugins in an `entire-plugin.yml`; missing ones are installed after a single confirmation.

Discovery uses a git-synced catalog, [entireio/plugin-index](https://github.com/entireio/plugin-index) by default. Organizations can point the CLI at an internal catalog with the `ENTIRE_PLUGIN_INDEX_URL` environment variable or `--index`. It is deliberately not settable from a repository's committed settings: an index-listed plugin installs without a prompt, so a checked-out repo must not be able to choose the catalog.

For the full contract — resolution rules, environment filtering, release-asset conventions, and how to author a plugin — see [External Commands](docs/architecture/external-commands.md).

## Configuration

Entire uses two configuration files in the `.entire/` directory:

### settings.json (Project Settings)

Shared across the team, typically committed to git. This is what `entire enable` writes for a new repo:

```json
{
  "enabled": true,
  "checkpoints": {
    "primary": { "type": "git-refs" }
  }
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

| Option                                    | Values                                       | Description                                                                       |
| ----------------------------------------- | -------------------------------------------- | --------------------------------------------------------------------------------- |
| `enabled`                                 | `true`, `false`                              | Enable/disable Entire                                                             |
| `log_level`                               | `debug`, `info`, `warn`, `error`             | Logging verbosity                                                                 |
| `checkpoints.primary.type`                | `git-refs`                                   | Checkpoint storage backend — written by `entire enable`, see [Checkpoint Storage](#checkpoint-storage) |
| `telemetry`                               | `true`, `false`                              | Send anonymous usage statistics to Posthog                                        |
| `absolute_git_hook_path`                  | `true`, `false`                              | Embed the full binary path in git hooks, for GUI git clients that don't source shell profiles |
| `commit_linking`                          | `always`, `prompt`                           | Link commits to sessions automatically, or ask each time (default `prompt`)        |
| `sign_checkpoint_commits`                 | `true`, `false`                              | Sign checkpoint commits (default: on). See [checkpoint signing](docs/architecture/checkpoint-signing.md) |
| `strategy_options.push_sessions`          | `true`, `false`                              | Auto-push checkpoint data on git push (default `true`)                            |
| `strategy_options.checkpoint_remote`      | `{"provider": "github", "repo": "org/repo"}` | Push checkpoint data to a separate repo (see below)                               |
| `strategy_options.checkpoint_push_remote` | remote name, e.g. `"upstream"`               | Pin which single remote carries checkpoint data (see below)                       |
| `strategy_options.filtered_fetches`       | `true`, `false`                              | Use `--filter=blob:none` on checkpoint fetches                                    |
| `strategy_options.summarize.enabled`      | `true`, `false`                              | Auto-generate AI summaries at commit time                                         |
| `summary_generation.provider`             | e.g. `claude-code`, `codex`, `gemini`        | Which agent generates summaries (defaults to Claude)                              |
| `summary_generation.model`                | provider-specific model hint                 | Model hint for summary generation (requires `provider`)                            |
| `summary_timeout_seconds`                 | seconds                                      | Hard deadline for `entire checkpoint explain --generate`. Unset or `0` means **no deadline** |
| `redaction.*`                             | nested object                                | PII redaction, custom secret patterns, scanner engines, and the OpenAI Privacy Filter — documented in [docs/security-and-privacy.md](docs/security-and-privacy.md) |

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

By default, checkpoint data rides along with your own pushes — but only to **one** remote, the elected checkpoint sync remote. Entire picks it in this order:

1. `strategy_options.checkpoint_push_remote`, if set. This is fail-closed: if it names a remote that isn't configured, checkpoints don't sync.
2. A remote captured from your own habits: the first push whose target matches the branch's declared push destination elects that remote, announces it on stderr, and carries the checkpoints. The first capture sticks.
3. `origin`
4. The sole remote, if the repo has exactly one
5. The first remote in `.git/config` order

A push to any *other* remote carries no checkpoint data. `entire status` shows the current destination, where it came from, and how many checkpoints are unpushed. This matters if you push code to several remotes: checkpoints go to exactly one of them.

If instead you want checkpoint data in a separate repo (e.g., a private repo for a public project), configure `checkpoint_remote` with a structured provider and repo. A dedicated `checkpoint_remote` is addressed directly and is exempt from the single-remote election above:

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

- Fetch existing checkpoint data locally if the remote has it and you don't (one-time)
- Push checkpoint data to the checkpoint repo instead of your default push remote
- Ignore the setting if it looks inherited rather than yours, and fall back to `origin` (or, failing that, the remote you are pushing to). `checkpoint_remote` is normally committed in `.entire/settings.json`, so cloning or forking a project inherits it — without this, a contributor's session data would be pushed into the upstream project's checkpoint repo. A setting is treated as yours when it lives in the gitignored `.entire/settings.local.json`, or when your `origin` **and every push URL** of the remote you are pushing to are owned by the same account or org as the checkpoint repo. Requiring the push destination too covers contributors who cloned the upstream repo and added their own fork, where `origin` belongs to the upstream project rather than to them; requiring every push URL covers mirror-style remotes that fan out to several repositories
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

Summaries are also generated on demand, with or without this setting, by `entire checkpoint explain --generate`.

**Which agent writes them.** By default Claude Code (`claude` on your `PATH`, model `sonnet`). Set a different one with `summary_generation.provider` — `claude-code`, `codex`, `copilot-cli`, `cursor`, `gemini`, or `pi`, plus an optional `summary_generation.model` hint:

```bash
entire configure --summarize-provider codex
```

`opencode` and `factoryai-droid` cannot generate summaries. Whichever provider you pick must be installed and authenticated.

**Requirements:**

- The configured provider's CLI installed and authenticated
- Summary generation is non-blocking: failures are logged but don't prevent commits

### Settings Priority

Local settings override project settings field-by-field. `entire status --detailed` shows the state of each settings file.

Two exceptions to field-by-field merging:

- The `checkpoints` block is **replaced wholesale** by the local one, not deep-merged — it selects a backend, so a half-merged block would be meaningless.
- A `settings.local.json` that is **tracked in git** is ignored entirely, and Entire tells you to `git rm --cached` it. `.gitignore` doesn't apply to an already-tracked path, so a committed local file would otherwise override project settings for everyone who clones.

### Agent-Specific Steps & Limitations

- Codex hooks are enabled by default (codex-cli 0.124.0+), so enabling Entire for Codex only installs `.codex/hooks.json` — no `config.toml` is needed and Entire never creates one. If an older Entire version left a `.codex/config.toml` behind and your repo lives inside `~/.codex/agents`, delete that file to stop Codex's "malformed agent role definition" startup warning.
- Entire supports Cursor IDE and Cursor Agent CLI tool. Commands (`doctor`, `status` etc.) work the same as all other agents.
- Entire supports Copilot CLI, but not Copilot in VS Code, in other IDEs, or on github.com.
- Entire supports Pi coding agent (Preview). Pi uses a TypeScript extension instead of a JSON hook config. Subagent capture is not currently available.

## Security & Privacy

**Your session transcripts are stored in your git repository**, in the [per-checkpoint refs](#checkpoint-storage) described above. If your repository is public, this data is visible to anyone.

Entire automatically redacts detected secrets (API keys, tokens, credentials) from transcripts and metadata before writing a checkpoint, but redaction is best-effort.

The temporary shadow branches used during a session get the same redaction for transcripts and metadata, but their **code-file snapshots are raw blobs of your working tree**, so a secret hardcoded in your source appears unredacted there. Entire never pushes shadow branches — don't push them manually. See [docs/security-and-privacy.md](docs/security-and-privacy.md) for the full picture, including the configurable scanner layers, opt-in PII redaction, and the OpenAI Privacy Filter pass.

## Troubleshooting

### Common Issues

| Issue                              | Solution                                                          |
| ---------------------------------- | ----------------------------------------------------------------- |
| "Not a git repository"             | Navigate to a Git repository first                                |
| "Entire is disabled"               | Run `entire enable`                                               |
| A session stuck ACTIVE, or leftover session state | Run `entire doctor`, then `entire clean --force` if it persists |
| Anything else                      | Run `entire doctor`; `entire doctor bundle` produces a redacted diagnostic bundle for a bug report |

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

The container's `postCreateCommand` runs `.devcontainer/post-create.sh`, which does `mise trust --yes && mise install`, so Go, `golangci-lint`, `gotestsum`, `shellcheck`, and the canary E2E helper binaries are ready after creation. Use `.devcontainer/run-with-keyring.sh <command>` for commands that touch the Linux keyring, including `mise run test:ci`.

If `ENTIRE_DEVCONTAINER_KEYRING_PASSWORD` is set in the environment, `.devcontainer/run-with-keyring.sh` uses that value to unlock the keyring non-interactively. If it is unset, the script generates a random password for the session automatically.

### Common Tasks

```
# Run tests
mise run test

# Run integration tests
mise run test:integration

# Run all tests: unit + integration under -race, then the E2E canary
mise run test:ci

# Lint the code
mise run lint

# Format the code
mise run fmt
```

### Local Device Auth Testing

If you're working on the CLI login flow against a locally running Entire API, use the smoke script. It defaults to `http://localhost:8787` for both the data API and the login server, and passes the hidden `--insecure-http-auth` flag that a plain-HTTP login server requires:

```bash
./scripts/local-device-auth-smoke.sh
```

Override either endpoint with `ENTIRE_API_BASE_URL` and `ENTIRE_LOGIN_SERVER`:

```bash
ENTIRE_LOGIN_SERVER=http://localhost:8180 ./scripts/local-device-auth-smoke.sh
```

The script starts a login, opens the approval URL, waits for the CLI to finish, and then verifies a matching context was written to `contexts.json`.

To drive the flow by hand, or to run the focused integration coverage:

```bash
# Login against a local server (--device forces the device-code flow)
go run ./cmd/entire login --server http://localhost:8787 --insecure-http-auth --device

# Focused integration coverage for login
go test -tags=integration ./cmd/entire/cli/integration_test -run TestLogin
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
