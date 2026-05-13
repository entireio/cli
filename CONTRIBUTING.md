# Contributing to the Entire CLI

Thank you for your interest in contributing to Entire! We welcome contributions from everyone.

Please read our [Code of Conduct](CODE_OF_CONDUCT.md) before participating.

> **New to Entire?** See the [README](README.md) for setup and usage documentation.

---

## Before You Code: Discuss First

The fastest way to get a contribution merged is to align with maintainers before writing code. Please **open an issue first** using our [issue templates](https://github.com/entireio/cli/issues/new/choose) and wait for maintainer feedback before starting implementation.

### Contribution Workflow

1. **Open an issue** describing the problem or feature
2. **Wait for maintainer feedback** -- we may have relevant context or plans
3. **Get approval** before starting implementation
4. **Submit your PR** referencing the approved issue
5. **Address all feedback** including automated Copilot comments
6. **Maintainer review and merge**

---

## First-Time Contributors

**New to open source or to Entire?** Start with the [First-Time Contributors Guide](docs/first-time-contributors.md). It walks through the full path: finding an issue, forking, setting up your dev environment, opening a PR, and what to expect afterwards.

The rest of this document is the reference for ongoing contributors.

---

## Submitting Issues

All feature requests, bug reports, and general issues should be submitted through [GitHub Issues](https://github.com/entireio/cli/issues). Please search for existing issues before opening a new one.

For security-related issues, see the Security section below.

---

## Security

If you discover a security vulnerability, **do not report it through GitHub Issues**. Instead, please follow the instructions in our [SECURITY.md](SECURITY.md) file for responsible disclosure. All security reports are kept confidential as described in SECURITY.md.

---

## Contributions & Communication

Contributions and communications are expected to occur through:

- [GitHub Issues](https://github.com/entireio/cli/issues) - Bug reports and feature requests
- [Discord](https://discord.gg/jZJs3Tue4S) - Questions, general conversation, and real-time support

Please represent the project and community respectfully in all public and private interactions.


## How to Contribute


There are many ways to contribute:

- **Feature requests** - Open a [GitHub Issue](https://github.com/entireio/cli/issues) to discuss your idea
- **Bug reports** - Report issues via [GitHub Issues](https://github.com/entireio/cli/issues) (see [Reporting Bugs](#reporting-bugs))
- **Code contributions** - Fix bugs, add features, improve tests
- **Documentation** - Improve guides, fix typos, add examples
- **Community** - Help others, answer questions, share knowledge


## Reporting Bugs

Good bug reports help us fix issues quickly. When reporting a bug, please include:

### Required Information

1. **Entire CLI version** - run `entire version`
2. **Operating system**
3. **Go version** - run `go version`

### What to Include

Please answer these questions in your bug report:

1. **What did you do?** - Include the exact commands you ran
2. **What did you expect to happen?**
3. **What actually happened?** - Include the full error message or unexpected output
4. **Can you reproduce it?** - Does it happen every time or intermittently?
5. **Any additional context?** - Logs, screenshots, or related issues

---

## Local Setup

### Prerequisites

- **Go 1.26.x** - Check with `go version`
- **mise** - Task runner and version manager. Install with `curl https://mise.run | sh`

### Clone and Install

```bash
# Clone the repository
git clone https://github.com/entireio/cli.git
cd cli

# Trust the mise configuration (required on first setup)
mise trust

# Install dependencies (mise will install the correct Go version)
mise install

# Download Go modules
go mod download

# Build the CLI
mise run build

# Verify setup by running tests
mise run test
```

> See [CLAUDE.md](CLAUDE.md) for detailed architecture and development reference.

---

## Making Changes

1. **Create a branch** for your changes:
   ```bash
   git checkout -b feature/your-feature-name
   ```

2. **Make your changes** - follow the [Code Style](#code-style) guidelines

3. **Test your changes** - see [Testing](#testing)

4. **Commit** with clear, descriptive messages:
   ```bash
   git commit -m "Add feature: description of what you added"
   ```

---

## Code Style

Follow standard Go idioms and conventions. For detailed guidance, see the **Go Code Style** section in [CLAUDE.md](CLAUDE.md).

### Key Points

- **Error handling**: Handle all errors explicitly - don't leave them unchecked
- **Formatting**: Code must pass `gofmt` (run `mise run fmt`)
- **Linting**: Code must pass `golangci-lint` (run `mise run lint`)
- **Naming**: Use meaningful, descriptive names following Go conventions

---

## Testing

> See [CLAUDE.md](CLAUDE.md) for complete testing documentation.

```bash
# Unit tests - always run before committing
mise run test

# Integration tests
mise run test:integration

# Full CI suite
mise run test:ci
```

Integration tests use the `//go:build integration` build tag and are located in `cmd/entire/cli/integration_test/`.

---

## Creating an Agent


Entire supports two ways to create agents:

### 1. Claude Code Agent Personas (Markdown)

These are markdown files that define specialized behaviors for Claude Code (e.g., developer, reviewer, etc.).

- **Location:** `.claude/agents/`
- **Structure:**
   ```markdown
   ---
   name: my-agent
   description: What this agent does
   model: opus
   color: blue
   ---

   # Agent Name
   You are a **[Role]** with expertise in [domain].

   ## Core Principles
   - Principle 1
   - Principle 2

   ## Process
   1. Step 1
   2. Step 2

   ## Output Format
   How to structure responses...
   ```
- **To invoke:** Create a matching command in `.claude/commands/` that spawns the agent via the Task tool.
- **Examples:**
   - `.claude/agents/dev.md` - TDD Developer
   - `.claude/agents/reviewer.md` - Code Reviewer

### 2. Coding Agent Integrations (Go)

These are Go implementations that integrate Entire with different AI coding tools (Claude Code, Gemini CLI, OpenCode, Cursor, Factory AI Droid, Copilot CLI, etc.) using the Agent abstraction layer.

- **Location:** `cmd/entire/cli/agent/`
- **Steps:**
   1. Implement the `Agent` interface in `agent/agent.go`
   2. Register your agent in the agent registry
   3. Add setup and hook configuration as needed
   4. Ensure session and checkpoint tracking is handled per the abstraction
- **Reference:** See [CLAUDE.md](CLAUDE.md) for architecture and code examples.

---

**Which should I use?**

- Use a persona markdown agent if you want to create a new role or workflow for Claude Code.
- Use a coding agent integration if you want to add support for a new AI coding tool or extend agent capabilities in the CLI.

---

## Submitting a Pull Request

### Before You Submit

- **Related issue exists and is approved** -- Your PR references an issue where a maintainer has acknowledged the approach. (Exceptions: documentation fixes, typo corrections, and `good-first-issue` items.)
- **Linting passes** -- Run `mise run lint` (includes golangci-lint, gofmt, gomod, shellcheck)
- **Tests pass** -- Run `mise run test` to verify your changes
- **Tests included** -- New Go code and functionality should have accompanying tests
- **Entire checkpoint trailers included** -- See [Using Entire While Contributing](#using-entire-while-contributing) below

PRs that skip these steps are likely to be closed without merge.

### Submitting

1. **Push** your branch to your fork
2. **Open a PR** against the `main` branch
3. **Describe your changes** -- Link the related issue, summarize what changed and what testing you did
4. **Address Copilot feedback** -- See [Responding to Automated Review](#responding-to-automated-review)
5. **Wait for maintainer review**

---

## Responding to Automated Review

Co-pilot agent reviews every PR and provides feedback on code quality, potential bugs, and project conventions.

**Read and respond to every Copilot comment.** PRs with unaddressed Copilot feedback will not move to maintainer review.

- **Fixed** -- Push a commit addressing the issue.
- **Disagree** -- Reply explaining your reasoning. The Copilot isn't always right.
- **Question** -- Ask for clarification. We're happy to help.

Addressing Copilot feedback upfront is the fastest path to maintainer review.

---

## Working with agents

Entire exists to help you work with AI coding agents, so it would be odd if you weren't using one to contribute. There's no need to tell us you did. Our general thinking: use whatever agent and methodology you like, but until the robot revolution comes, you are responsible for the final code. Before submitting a PR for review, make sure you have reviewed it yourself. We'll close PRs that obviously skipped this step.

Entire supports Claude Code, Gemini CLI, OpenCode, Cursor, Factory AI Droid, Copilot CLI, and Pi, so feel free to use whichever one you're most comfortable with.

One thing to watch out for is LLM eagerness. Agents like to please and they're in a hurry. A few common failure modes to push back on:

- **Think first:** Agents tend to jump straight to writing code. Explain the architecture you want first, based on your own understanding, or have the agent explore the code and propose approaches before any edits happen. If the first implementation doesn't look right, just start over and use what you learned to do better next time. Re-rolling is cheaper than untangling.
- **Spot the laziness:** LLMs will make their own job easy. They write trivial tests, make types wide and optional so the compiler doesn't complain, catch exceptions and log instead of handling errors, and copy local patterns whether or not they fit. When you notice this happening, push back and ask the agent to do the work properly.
- **Spot the uncertainty:** As much as the bots declare "I see the issue now clearly," they often don't. Call them on it if you see the agent flailing. Another telltale sign: the agent starts listing the many ways it fixed an issue, or starts writing overly defensive code.
- **Spot the bloat:** Agents like to insert redundant comments, or worse, comments that describe the change at hand rather than the resulting code. They write loads of tests that don't really test anything, and when they do, they test the implementation rather than the intention. They also like to log anything, just in case. When you see this in the diff, trim it back before opening your PR.

---

## Using Entire While Contributing

We use Entire on Entire. When contributing, install the Entire CLI and let it capture your coding sessions -- this gives us valuable dogfooding data and helps improve the tool.

### Setup

Install the latest version of the Entire CLI (see [installation docs](https://docs.entire.io/cli/installation)) and verify with `entire version`. Entire is already configured in this repository, so there's no need to run `entire enable`.

### Checkpoint Trailers

All commits should include `Entire-Checkpoint` trailers from your sessions. These are added automatically by the `prepare-commit-msg` hook when Entire is enabled. The trailers link your commits to session metadata on the `entire/checkpoints/v1` branch.

### Sessions Branch

When you push your PR branch, Entire can automatically push the `entire/checkpoints/v1` branch alongside it (if `push_sessions` is enabled in your settings). Include this in your PR so maintainers can review the session context behind your changes.

---

## Troubleshooting

### Common Setup Issues

**`go mod download` fails with timeout**
```bash
# Try using direct mode
GOPROXY=direct go mod download
```

**`mise install` fails**
```bash
# Ensure mise is properly installed
curl https://mise.run | sh

# Reload your shell
source ~/.zshrc  # or ~/.bashrc
```

**Binary not updating after rebuild**
```bash
# Check which binary is being used
which entire
type -a entire

# You may have multiple installations - update the correct path
```

---

## Community

Join the Entire community:

- **Discord** - [Join our server][discord] for discussions and support

[discord]: https://discord.gg/jZJs3Tue4S

---

## Additional Resources

- [README](README.md) - Setup and usage documentation
- [CLAUDE.md](CLAUDE.md) - Architecture and development reference (Claude Code)
- [AGENTS.md](AGENTS.md) - Architecture and development reference (Gemini CLI, OpenCode, Cursor, Factory AI Droid, Copilot CLI)
- [Code of Conduct](CODE_OF_CONDUCT.md) - Community guidelines
- [Security Policy](SECURITY.md) - Reporting security vulnerabilities

---

Thank you for contributing!
