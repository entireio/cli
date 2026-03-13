# E2E Tests

End-to-end tests for the `entire` CLI against real agents (Claude Code, Gemini CLI, OpenCode, Cursor, Copilot CLI).

## Commands

```bash
mise run test:e2e [filter]                          # run filtered (or omit filter for all agents)
mise run test:e2e --agent claude-code [filter]       # Claude Code only
mise run test:e2e --agent gemini-cli [filter]        # Gemini CLI only
mise run test:e2e --agent opencode [filter]          # OpenCode only
mise run test:e2e --agent copilot-cli [filter]       # Copilot CLI only
mise run test:e2e --agent cursor-cli [filter]        # Cursor only
go build ./...                                      # compile check (no agent CLI needed)
```

**Do NOT run E2E tests proactively.** They make real API calls that consume tokens and cost money. Only run when explicitly asked.

## Structure

```
e2e/
├── agents/       # Agent abstraction (Agent interface, tmux sessions, concurrency gates)
├── bootstrap/    # CI pre-test setup (auth config, warmup)
├── entire/       # `entire` CLI wrapper (enable, rewind, etc.)
├── exploratory/  # Experimental tests, not run by CI
├── tests/        # Blessed test files (run by CI)
└── testutil/     # Repo setup, assertions, artifact capture
```

## Key Patterns

- Every test uses `testutil.ForEachAgent` which runs it per registered agent with repo setup, concurrency gating, and timeout scaling.
- All operations go through `RepoState` (`s.RunPrompt`, `s.Git`) so they're logged to `console.log`.
- Use the `entire` package for CLI interactions, not raw `exec.Command`.
- Skip tests pending CLI fixes with `t.Skip("ENT-XXX: reason")`.

## Adding a New Agent

1. Create `agents/<name>.go` implementing the `Agent` interface.
2. Register it in `init()` with `Register(&YourAgent{})`.
3. Add a `Bootstrap()` method for any CI-specific setup (auth config, warmup).
4. Add a `RegisterGate("<name>", N)` call if concurrency needs limiting.
5. Ensure the agent name is accepted by `mise run test:e2e --agent <name>`.
6. Add the agent to `.github/workflows/e2e.yml` matrix and `e2e-isolated.yml` options.

## Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `E2E_AGENT` | Agent to test (`claude-code`, `gemini-cli`, `opencode`, `cursor-cli`, `copilot-cli`) | all registered |
| `E2E_ENTIRE_BIN` | Path to a pre-built `entire` binary | builds from source |
| `E2E_TIMEOUT` | Timeout per prompt | `2m` |
| `E2E_KEEP_REPOS` | Set to `1` to preserve temp repos after test | unset |
| `E2E_ARTIFACT_DIR` | Override artifact output directory | `e2e/artifacts/<timestamp>` |
| `ANTHROPIC_API_KEY` | Required for Claude Code | — |
| `GEMINI_API_KEY` | Required for Gemini CLI | — |
| `COPILOT_GITHUB_TOKEN` | Required for Copilot CLI (or `gh auth login`) | — |

## Debugging Failures

Artifacts are captured to `e2e/artifacts/` on every run (git-log, git-tree, console.log, checkpoint metadata, entire logs). Set `E2E_KEEP_REPOS=1` to preserve the temp repo — a symlink appears in the artifact dir pointing to it.

Use the `debug-e2e` skill (`.claude/skills/debug-e2e/`) for a structured workflow when investigating failures.

### Reading artifacts

- `console.log` — full operation transcript including agent stdout/stderr
- `git-log.txt` — commit history at time of failure
- `git-tree.txt` — working tree state
- `entire-logs/` — internal CLI logs

### Fixing flaky tests

When a test passes on retry but failed once, the problem is usually agent non-determinism, not a CLI bug. Common patterns:

- **Agent asked for confirmation instead of acting**: The model output contains "Does this look right?" or "Should I proceed?". Fix: append "Do not ask for confirmation, just make the change." to the prompt.
- **Agent wrote to wrong path or created extra files**: Fix: be more explicit about exact file paths and what _not_ to do.
- **Agent committed when it shouldn't have**: Fix: add "Do not commit" to the prompt.
- **Checkpoint wait timeout**: `WaitForCheckpoint` or `WaitForCheckpointAdvanceFrom` exceeded deadline. Fix: increase the timeout argument.

To diagnose: read `console.log` in the failing test's artifact directory. Compare what the agent actually did vs what the test expected.

## CI Workflows

- **`.github/workflows/e2e.yml`** — Runs full suite on push to main. Matrix: `[claude-code, opencode, gemini-cli, cursor-cli, copilot-cli]`.
- **`.github/workflows/e2e-isolated.yml`** — Manual dispatch for debugging a single test. Inputs: agent + test name filter.

Both workflows run `go run ./e2e/bootstrap` before tests to handle agent-specific CI setup (auth config, warmup).
