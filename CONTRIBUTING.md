# Contributing to the Entire CLI

Thank you for your interest in contributing to Entire! We welcome contributions from everyone.

Please read our [Code of Conduct](CODE_OF_CONDUCT.md) before participating.

> **New to Entire?** See the [README](README.md) for setup and usage documentation.

---

## First-Time Contributors

New to the project? Welcome! Here's how to get started:

### Good First Issues

We recommend starting with:
- **Documentation improvements** - Fix typos, clarify explanations, add examples
- **Test contributions** - Add test cases, improve coverage
- **Small bug fixes** - Issues labeled `good-first-issue`

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
- [GitHub Discussions](https://github.com/entireio/cli/discussions) - Questions and general conversation
- [Discord](https://discord.gg/4WXDu2Ph) - Real-time chat and support

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

- **Go 1.25.x** - Check with `go version`
- **mise** - Task runner and version manager. Install with `curl https://mise.run | sh`

### Clone and Install

```bash
# Clone the repository
git clone https://github.com/entireio/cli.git
cd cli

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

These are Go implementations that integrate Entire with different AI coding tools (Claude, Cursor, Aider, etc.) using the Agent abstraction layer.

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

### Prerequisites

1. **Install and enable Entire** - Install the latest version of the Entire CLI (see [installation docs](https://docs.entire.io/cli/installation)), verify with `entire version`, then run `entire enable` in the repository
2. **Pass linting** - Run `mise run lint` (includes golangci-lint, gofmt, gomod, shellcheck)
3. **Pass tests** - Run `mise run test` to verify your changes
4. **Add tests** - Include tests for new Go code and functionality
5. **Include checkpoint trailers** - All commits must have Entire checkpoint trailers from your sessions

### Submitting

1. **Push** your branch to your fork
2. **Open a PR** against the `main` branch
3. **Fill out** the PR template with:
   - Clear description of changes
   - Related issue numbers
   - Testing done
4. **Wait for review** - maintainers will provide feedback

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
- **GitHub Discussions** - [Join the conversation][discussions]

[discord]: https://discord.gg/4WXDu2Ph
[discussions]: https://github.com/entireio/cli/discussions

---

## Additional Resources

- [README](README.md) - Setup and usage documentation
- [CLAUDE.md](CLAUDE.md) - Architecture and development reference
- [Code of Conduct](CODE_OF_CONDUCT.md) - Community guidelines
- [Security Policy](SECURITY.md) - Reporting security vulnerabilities

---

Thank you for contributing!