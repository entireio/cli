# E2E Tests

End-to-end tests for the `entire` CLI against real agents (Claude Code, Gemini CLI, OpenCode, Codex, Cursor, Factory AI Droid, Copilot CLI).

## Commands

```bash
mise run test:e2e [filter]                          # run filtered (or omit filter for all agents)
mise run test:e2e --agent claude-code [filter]       # Claude Code only
mise run test:e2e --agent gemini-cli [filter]        # Gemini CLI only
mise run test:e2e --agent opencode [filter]          # OpenCode only
mise run test:e2e --agent codex [filter]             # Codex only
mise run test:e2e --agent cursor [filter]            # Cursor only
mise run test:e2e --agent factoryai-droid [filter]   # Factory AI Droid only
mise run test:e2e --agent copilot-cli [filter]       # Copilot CLI only
go build ./...                                      # compile check (no agent CLI needed)
```

**Do NOT run E2E tests proactively.** They make real API calls that consume tokens and cost money. Only run when explicitly asked.

## Shared runner

`sh scripts/e2e-run.sh <agent> [test-regex]` runs one agent against the matching
Go tests. The mise E2E tasks use this same runner. Supply credentials through
the agent's native environment variables, such as `ANTHROPIC_API_KEY` for
Claude Code. Tokens are not command-line arguments.

The runner builds Entire unless `E2E_ENTIRE_BIN` is set, writes reports, fails
when no parent tests ran, and always prints the artifact path after reporting.
Named tests that all skip remain successful. A subtest regex that matches a
parent but no children is not detected as empty. Real agents get one gotestsum
retry; vogon and roger-roger get none. `E2E_BOOTSTRAP=1` also runs the selected
agent's bootstrap before testing; existing local invocations leave this off.

For CI, call the reusable agent job:

```yaml
jobs:
  smoke:
    uses: ./.github/workflows/e2e-agent.yml
    with:
      agent: claude-code
      test-regex: '^TestMultiSessionSequential$'
    secrets:
      ANTHROPIC_API_KEY: ${{ secrets.ANTHROPIC_API_KEY }}
```

Each agent has a small caller job that passes only its named provider secret.
The shared job exposes these as native environment variables, and the runner
checks the selected agent's required key before building or running tests.
Copilot passes `github.token` as `COPILOT_GITHUB_TOKEN`. Tokenless agents
omit secrets. Local runs can still use stored agent logins. Droid uses
Factory-managed Claude Haiku with only the Factory key. Roger-roger needs no secret.
Vogon runs through the local canary task in `ci.yml`. The shared job installs
and bootstraps only the selected agent and uploads
artifacts even when tests fail. It accepts an optional `artifact-name` for
callers that run the same agent and OS more than once. Optional inputs include
`runner` (default `ubuntu-latest`), `cli-source` (`source` or `nightly`), and
`test-timeout-minutes`. This job always uses `git-refs`; CI's free canary covers
backend compatibility.

For nightly runs, the job installs the published CLI through the OS-specific
installer and runs the current tests against that binary. Agent configuration
and reporting therefore stay consistent with source runs. Tests for features
newer than the nightly binary may need a narrower `test-regex`. Copilot callers must grant `copilot-requests:
write` when passing `github.token`; the shared job inherits caller permissions.

## Structure

```
e2e/
├── agents/       # Agent abstraction (Agent interface, tmux sessions, concurrency gates)
├── bootstrap/    # CI pre-test setup (auth config, warmup)
├── entire/       # `entire` CLI wrapper (enable, explain, etc.)
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
6. Add installation support in `.github/workflows/e2e-agent.yml` and selections
   in the workflow matrices and dispatch choices in `.github/workflows/e2e.yml`
   and `.github/workflows/nightly-e2e.yml`.

## Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `E2E_AGENT` | Agent to test (`claude-code`, `gemini-cli`, `opencode`, `codex`, `cursor`, `factoryai-droid`, `copilot-cli`) | all registered |
| `E2E_ENTIRE_BIN` | Path to a pre-built `entire` binary | builds from source |
| `E2E_TIMEOUT` | Per-prompt timeout, overriding every runner's own default. A per-test `agents.WithPromptTimeout(...)` still wins over it, and a malformed value is a hard error rather than a silent fall back. | per runner: 60s (codex, copilot-cli, gemini), 90s (cursor), 2m (opencode), none (claude-code, droid, pi, vogon, roger-roger — bounded only by the scenario timeout) |
| `E2E_KEEP_REPOS` | Set to `1` to preserve temp repos after test | unset |
| `E2E_CHECKPOINT_STORE` | Checkpoint backend (`git-refs` or legacy `git-branch`). Maps to the `ENTIRE_CHECKPOINTS_PRIMARY` override that every spawned binary/hook honors. | `git-refs` |
| `E2E_ARTIFACT_DIR` | Override artifact output directory | `e2e/artifacts/<timestamp>` |
| `ANTHROPIC_API_KEY` | Required for Claude Code | — |
| `GEMINI_API_KEY` | Required for Gemini CLI | — |
| `OPENAI_API_KEY` | Required for Codex | — |
| `COPILOT_GITHUB_TOKEN` | Required for Copilot CLI, unless a `copilot login` credential is already stored. `GH_TOKEN` and `GITHUB_TOKEN` also work — Copilot reads all three, in that order of precedence. A `gh auth login` alone is not enough: Copilot does not read gh's config. | — |
| `E2E_KEEP_AGENT_HOME` | Set to `1` to preserve the isolated `COPILOT_HOME` a session ran under (holds Copilot's own logs) | unset |

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

- **`e2e.yml`** runs the full source-built suite on pushes to main: seven Linux
  agents plus Windows/Claude, using `git-refs`. Manual dispatch accepts `agent`,
  `test` (regex), and `runner`. Selecting an agent selects one job; the default
  runner is Linux. Selecting Windows without an agent selects Claude.
  Gemini is available explicitly but excluded from the default matrix.
- **`nightly-e2e.yml`** runs an installed-nightly smoke test on Linux, macOS, and
  Windows with `git-refs`. Its YAML matrix excludes Cursor and Factory on
  Windows and caps concurrency at three agent jobs. Each agent has
  a separate job, result summary, and artifact bundle.
- **`e2e-agent.yml`** is the reusable job both workflows call for installation,
  bootstrap, execution, reporting, and artifact upload.
- **`ci.yml`** retains the deterministic canary on PRs for both `git-refs` and
  legacy `git-branch`, alongside the unit and integration checks. It also tests the E2E
  external-agent integration with roger-roger. This free backend matrix is the
  pre-merge gate; it does not require paid agent credentials.

The former isolated and Windows dispatches are inputs to `e2e.yml`. Backend
coverage lives in `ci.yml`, replacing the checkpoint-store workflow. The obsolete
checkpoints-v2 workflow was removed: its `E2E_CHECKPOINTS_MODE` variable was no
longer read by the harness. For local legacy-backend testing, set
`E2E_CHECKPOINT_STORE=git-branch` when running the canary task.

Example manual dispatch:

```bash
gh workflow run e2e.yml -f agent=claude-code -f runner=windows-latest \
  -f test='^TestMultiSessionSequential$'
```
