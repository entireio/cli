# Agent Instruction Templates

Drop one of these files into your repository root so coding agents automatically know how to use Entire.

## Which file to use

| Agent | File | Where to place |
|-------|------|----------------|
| Any / multiple agents | `AGENTS.md` | Repo root |
| Claude Code | `CLAUDE.md` | Repo root |
| Gemini CLI | `GEMINI.md` | Repo root |
| AMP (Sourcegraph) | `AMP.md` | Repo root |

Most agents respect `AGENTS.md` as a universal instruction file. Use the agent-specific files if you want tailored instructions or if you're only using one agent.

## Usage

```bash
# Copy the appropriate file to your repo root
cp docs/agent-instructions/AGENTS.md .

# Or for a specific agent
cp docs/agent-instructions/CLAUDE.md .
```

Then commit it. The agent will read it automatically on session start.

## Customization

These are templates. Edit them to match your workflow — add project-specific conventions, remove sections that don't apply.
