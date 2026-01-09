# Entire CLI User Guide

Entire hooks into your git workflow to capture AI agent sessions on every push. Sessions are indexed alongside commits, creating a searchable record of how code was written. Runs locally, stays in your repo.

Entire guide for setup, commands, and workflows for using the Entire CLI.


---

## Table of Contents

1. [Installation & Setup](#installation--setup)
2. [Configuration](#configuration)
3. [Core Concepts](#core-concepts)
   - [Sessions](#sessions)
   - [Checkpoints](#checkpoints)
   - [Strategies](#strategies)
   - [Hooks](#hooks)
4. [Commands Reference](#commands-reference)
5. [Workflow Examples](#workflow-examples)
6. [Collaboration](#collaboration)
7. [Troubleshooting](#troubleshooting)
8. [Quick Reference](#quick-reference)
9. [Getting Help](#getting-help)

---

## Installation & Setup

### Prerequisites

- Git 2.x or higher
- A git repository to work in
- Claude Code (for AI agent integration)

### Installation

```bash
# Option 1: Install script (recommended)
curl -fsSL https://entire.io/install.sh | bash

# Option 2: Build from source
git clone https://github.com/entireio/cli.git
cd cli
go build -o entire ./cmd/entire
sudo mv entire /usr/local/bin/
```

### Initial Setup

1. Navigate to your git repository:
   ```bash
   cd /path/to/your/project
   ```

2. Enable Entire:
   ```bash
   entire enable
   ```

3. Select a strategy when prompted:
   ```
   > manual-commit  Sessions are only captured when you commit
     auto-commit    Automatically capture sessions after agent response completion
   ```

4. If project settings already exist, choose where to save:
   ```
   > Update project settings (settings.json)
     Use local settings (settings.local.json, gitignored)
   ```

**Expected output:**
```
✓ Claude Code hooks installed
✓ .entire directory created
✓ Project settings saved (.entire/settings.json)
✓ Git hooks installed

✓ manual-commit strategy enabled
```

---

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
| `agent` | `claude-code` | AI agent to integrate with |
| `log_level` | `debug`, `info`, `warn`, `error` | Logging verbosity |

### Settings Priority

Local settings override project settings. When you run `entire status`:

```
Project, enabled (manual-commit)
Local, disabled (auto-commit)
```

The effective setting is what's shown in "Local" (disabled, auto-commit in this example).

### Shell Autocompletion

Generate shell completions with `entire completion <shell>`:

```bash
# Load in current session
source <(entire completion zsh)   # Zsh
source <(entire completion bash)  # Bash

# Or install permanently (Zsh)
entire completion zsh > "${fpath[1]}/_entire"
```

---

## Core Concepts

### Sessions

A **session** represents a complete interaction with your AI agent, from start to finish. Each session captures:

- All prompts you sent to the agent
- All responses from the agent
- Files created, modified, or deleted
- Timestamps and metadata

**Session properties:**
- **ID**: Unique identifier (e.g., `2025-01-08-abc123de-f456-7890-abcd-ef1234567890`)
- **Strategy**: Which strategy created this session (`manual-commit` or `auto-commit`)
- **Description**: Human-readable summary (typically derived from your first prompt)
- **Checkpoints**: List of save points within the session

Sessions are stored separately from your code commits on the `entire/sessions` branch.

### Checkpoints

A **checkpoint** is a snapshot within a session that you can rewind to. Think of it as a "save point" in your work.

**When checkpoints are created:**
- **Manual-commit strategy**: When you make a git commit
- **Auto-commit strategy**: After each agent response

**What checkpoints contain:**
- Current file state
- Session transcript up to that point
- Metadata (timestamp, checkpoint ID, etc.)

**Checkpoints enable:**
- **Rewinding**: Restore code to any previous checkpoint state
- **Cross-machine restoration**: Resume sessions on different machines by fetching the `entire/sessions` branch
- **PR-ready commits**: Squash checkpoint history into clean commits for pull requests

### Strategies

Entire offers two strategies for capturing your work:

#### Manual-Commit

| Aspect | Behavior |
|--------|----------|
| Code commits | None on your branch - you control when to commit |
| Checkpoint storage | Shadow branches (`entire/<hash>`) |
| Safe on main branch | Yes |
| Rewind | Always possible, non-destructive |

**Best for:** Most workflows. Keeps your git history clean while allowing full rewind capability.

#### Auto-Commit

| Aspect | Behavior |
|--------|----------|
| Code commits | Created automatically after each agent response |
| Checkpoint storage | `entire/sessions` branch |
| Safe on main branch | No - creates commits |
| Rewind | Full rewind on feature branches; logs-only on main |

**Best for:** Teams wanting automatic code commits from sessions.

### Hooks

Hooks are how Entire integrates with Claude Code. When Claude Code runs, it triggers hooks that allow Entire to:

- Start tracking a new session
- Create checkpoints at appropriate times
- Save session transcripts
- Handle subagent (Task tool) checkpoints

Hooks are automatically configured in `.claude/settings.json` when you run `entire enable`.

---

## Commands Reference

### entire enable

Initialize Entire in your repository.

```bash
entire enable                        # Interactive setup
entire enable --strategy manual-commit  # Skip strategy prompt
entire enable --local                # Save to settings.local.json
entire enable --project              # Save to settings.json
entire enable --force                # Reinstall hooks
```

**Example output:**
```
✓ Claude Code hooks installed
✓ Project settings saved (.entire/settings.json)
✓ Git hooks installed

✓ manual-commit strategy enabled
```

### entire disable

Temporarily disable Entire. Hooks will exit silently.

```bash
entire disable                       # Writes to settings.local.json
entire disable --project             # Writes to settings.json
```

**Example output:**
```
Entire is now disabled.
```

### entire status

Show current Entire status and configuration.

```bash
entire status
```

**Example output (both settings files exist):**
```
Project, enabled (manual-commit)
Local, disabled (manual-commit)
```


**Example output (not set up):**
```
○ not set up (run `entire enable` to get started)
```

### entire rewind

Restore code to a previous checkpoint.

```bash
entire rewind                        # Interactive selection
entire rewind --list                 # List all rewind points
entire rewind --to <checkpoint-id>   # Rewind to specific checkpoint
entire rewind --logs-only            # Restore logs only (not files)
```

**Example output (--list):**
```json
[
  {
    "id": "abc123def456",
    "message": "Session checkpoint",
    "date": "2024-01-08T10:30:00Z",
    "files": ["src/main.go", "README.md"]
  }
]
```

### entire rewind reset

Reset the shadow branch for the current commit (manual-commit strategy).

```bash
entire rewind reset                  # Prompts for confirmation
entire rewind reset --force          # Skip confirmation
```

**When to use:** If you see a shadow branch conflict error, this gives you a clean start.

### entire session

Manage and view sessions. All session-related commands are subcommands of `entire session`.

#### entire session list

List all sessions stored by the current strategy.

```bash
entire session list
```

#### entire session current

Show details of the current session.

```bash
entire session current
```

#### entire session raw

Output the raw session transcript for a commit.

```bash
entire session raw <commit-sha>
```

#### entire session resume

Resume a session and restore agent memory. Can be interactive or specify a session ID.

```bash
entire session resume                # Interactive picker
entire session resume <session-id>   # Resume specific session (prefix match supported)
```

#### entire session cleanup

Remove orphaned session data that wasn't cleaned up automatically. This finds and removes:

- **Shadow branches** (`entire/<commit-hash>`) - Created by manual-commit strategy
- **Session state files** (`.git/entire-sessions/`) - Track active sessions
- **Checkpoint metadata** (`entire/sessions` branch) - For auto-commit checkpoints

```bash
entire session cleanup               # Dry run, shows what would be removed
entire session cleanup --force       # Actually delete orphaned items
```

### entire resume

Switch to a branch and resume its session with agent memory.

```bash
entire resume <branch>
```

**Example:**
```bash
entire resume feature/new-thing
```

**Example output:**
```
Switched to branch 'feature/new-thing'
Session restored to: .claude/projects/.../2025-01-08-abc123.jsonl
Session: 2025-01-08-abc123def456

To continue this session, run:
  claude --resume
```

### entire explain

Get a human-readable explanation of sessions or commits.

```bash
entire explain                       # Explain current session
entire explain --session <id>        # Explain specific session
entire explain --commit <sha>        # Explain specific commit
entire explain --no-pager            # Don't use pager for output
```

### entire version

Show version and build information.

```bash
entire version
```

---

## Workflow Examples

### Basic Workflow

```bash
# 1. Set up Entire in your project
cd my-project
entire enable

# 2. Work with Claude Code normally
# Entire captures your sessions automatically

# 3. Check your status
entire status

# 4. View available rewind points
entire rewind --list

# 5. If you need to go back
entire rewind --to <checkpoint-id>

# 6. When done, commit as normal
git add .
git commit -m "Implement feature"
```

### Feature Branch Workflow

```bash
# Start a new feature
git checkout -b feature/new-thing
entire enable  # If not already enabled

# Work with Claude Code...

# Need to switch branches temporarily
git stash
git checkout main

# Come back and resume
entire resume feature/new-thing

# Continue where you left off with Claude Code
```

### Reviewing Session History

```bash
# See what happened in a session
entire explain --session <session-id>

# View raw transcript
entire session raw <commit-sha>

# List all sessions
entire session list
```

---

## Collaboration

### Team Setup

1. **Commit project settings:** The `.entire/settings.json` file should be committed so the team shares the same strategy.

2. **Local overrides:** Individual developers can use `.entire/settings.local.json` for personal preferences (this file is gitignored).

3. **Session sharing:** Session data is stored on the `entire/sessions` git branch. Team members can:
   - Fetch this branch to see session history
   - Use `entire explain` to understand what happened in commits
   - Resume sessions started by others (on the same branch)

### Fetching Team Sessions

```bash
git fetch origin entire/sessions:entire/sessions
entire session list
entire explain --session <session-id>
```

---

## Troubleshooting

### Common Issues

| Issue | Cause | Solution |
|-------|-------|----------|
| "Not a git repository" | Running `entire` outside a git repo | Navigate to a git repository first |
| "Entire is disabled" | `enabled: false` in settings | Run `entire enable` |
| "No rewind points found" | No checkpoints created yet | Work with Claude Code and commit (manual-commit) or wait for agent response (auto-commit) |
| "shadow branch conflict" | Another session using same base commit | Run `entire rewind reset --force` |

### Debug Mode

Enable debug logging to troubleshoot issues:

```bash
# Via settings.local.json
{
  "log_level": "debug"
}

# Or via environment variable
ENTIRE_LOG_LEVEL=debug entire status
```

### Resetting State

If things get into a bad state:

```bash
# Reset shadow branch for current commit
entire rewind reset --force

# Clean up orphaned data
entire session cleanup --force

# Disable and re-enable
entire disable
entire enable
```

### Accessibility

For screen reader users, enable accessible mode:

```bash
export ACCESSIBLE=1
entire enable
```

This uses simpler text prompts instead of interactive TUI elements.

---

## Quick Reference

| Command | Description |
|---------|-------------|
| `entire enable` | Set up Entire in repository |
| `entire disable` | Temporarily disable Entire |
| `entire status` | Show current status |
| `entire rewind` | Rewind for session management |
| `entire session list` | List all sessions |
| `entire session current` | Show current session |
| `entire session resume` | Resume a session and restore agent memory |
| `entire session cleanup` | Remove orphaned session data |
| `entire resume <branch>` | Switch to branch and resume its session |
| `entire explain` | Explain current session |
| `entire version` | Show version info |

### Flags Quick Reference

| Flag | Commands | Description |
|------|----------|-------------|
| `--project` | enable, disable | Write to settings.json |
| `--local` | enable | Write to settings.local.json |
| `--force, -f` | enable, rewind reset, session cleanup | Skip confirmations |
| `--list` | rewind | List rewind points as JSON |
| `--to <id>` | rewind | Specify checkpoint ID |
| `--logs-only` | rewind | Restore logs only, not files |
| `--session <id>` | explain | Explain specific session |
| `--commit <sha>` | explain | Explain specific commit |

---

## Getting Help

### In-CLI Help

```bash
entire --help              # General help
entire <command> --help    # Command-specific help
```

### Resources

- **GitHub Issues:** Report bugs or request features at https://github.com/entireio/cli/issues
- **Documentation:** See the project [README](../README.md) and [CONTRIBUTING.md](../CONTRIBUTING.md)
- **Source Code:** https://github.com/entireio/cli

### Reporting Issues

For detailed bug reporting guidelines, see the [Reporting Bugs section in CONTRIBUTING.md](../CONTRIBUTING.md#reporting-bugs).

When reporting issues, please include:

1. Entire version (`entire version`)
2. Operating system and Go version
3. Steps to reproduce (exact commands)
4. Expected vs actual behavior
5. Debug logs if applicable (`ENTIRE_LOG_LEVEL=debug`)
