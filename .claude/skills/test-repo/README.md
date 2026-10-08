# Test Repo Skill

A skill for testing Entire CLI strategy changes against a fresh test repository.

## Files

- `SKILL.md` - Skill definition and documentation
- `test-harness.sh` - Executable test harness script that accepts step commands

## How It Works

The skill separates Go commands (which can be approved easily) from test setup commands:

1. **Go commands**: Run directly via `go build`, `go run`, `mise run`
2. **Test harness**: Shell script for git/filesystem operations requiring single approval
3. **Step-by-step execution**: Run individual test steps for better observability

## Usage

The skill will first build the CLI:

```bash
go build -o /tmp/entire-bin ./cmd/entire
```

Then execute test steps via the harness:

```bash
.claude/skills/test-repo/test-harness.sh setup-repo
.claude/skills/test-repo/test-harness.sh configure-strategy
.claude/skills/test-repo/test-harness.sh start-session
# ... etc
```

## Auto-Approval

To avoid repeated approvals, add this to your Claude Code settings:

```json
{
  "approvedBashCommands": [
    ".claude/skills/test-repo/test-harness.sh*"
  ]
}
```

## Available Steps

See `SKILL.md` for the complete list of available steps and testing procedures.

## Automated Testing Alternative

For comprehensive automated testing, use the existing integration tests (no harness needed):

```bash
mise run test:integration
```

This runs the full test suite and is the recommended approach for validation.

The test harness is most useful for:
- Debugging specific strategy behaviors
- Manual verification of edge cases
- Interactive exploration of the checkpoint workflow
- Understanding how the system works step-by-step
