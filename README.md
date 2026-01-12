# Entire CLI

The Entire CLI helps you track and restore your development sessions when working with AI coding tools like Claude Code. It automatically creates checkpoints of your work, allowing you to rewind to any point or resume previous sessions.

## Typical Workflow

### 1. Enable Entire in Your Repository

First, set up Entire in your git repository:

```bash
entire enable
```

This installs Claude Code and git hooks that automatically capture checkpoints whenever Claude Code makes changes. Your code commits stay clean—all session metadata is stored separately.

### 2. Work with Claude Code

Just use Claude Code normally. Entire runs in the background, creating checkpoints automatically. You can check the status anytime:

```bash
entire status
```

### 3. Rewind to a Previous Checkpoint

If you want to undo some changes and go back to an earlier checkpoint:

```bash
entire rewind
```

This shows you all available checkpoints in the current session. Select one to restore your code to that exact state.

### 4. Resume a Previous Session

To see and restore sessions from earlier work:

```bash
entire resume
```

This lists all your past sessions with timestamps. You can view the full conversation history or restore the code from any session.

### 5. Disable Entire (Optional)

If you want to remove Entire from a repository:

```bash
entire disable
```

This removes the git hooks. Your code and commit history remain untouched—only the Entire hooks are removed.

## Other Useful Commands

| Command | Description |
| --- | --- |
| `entire session` | View and manage sessions (list, show details, view logs) |
| `entire explain` | Explain a session or commit |
| `entire version` | Show Entire CLI version |

## How It Works

Entire uses hooks to capture your development sessions automatically:
- **Checkpoints**: Saved automatically when Claude Code makes changes
- **Metadata**: Stored in special git branches (never mixed with your code)
- **Clean History**: Your main branch stays clean—no extra commits from Entire

Use `entire status` to see your current checkpoint strategy and session information.

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

