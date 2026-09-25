# E2E Tests

End-to-end tests for the `entire` CLI against real agents (Claude Code, Antigravity, OpenCode, Codex, Cursor, Factory AI Droid, Copilot CLI).

## Commands

```bash
mise run test:e2e [filter]                          # run filtered (or omit filter for all agents)
mise run test:e2e --agent claude-code [filter]       # Claude Code only
mise run test:e2e --agent antigravity [filter]       # Antigravity only
mise run test:e2e --agent opencode [filter]          # OpenCode only
mise run test:e2e --agent codex [filter]             # Codex only
mise run test:e2e --agent cursor [filter]            # Cursor only
mise run test:e2e --agent factoryai-droid [filter]   # Factory AI Droid only
mise run test:e2e --agent copilot-cli [filter]       # Copilot CLI only
mise run test:e2e:controlplane [filter]              # control-plane commands against production (no agent)
go build ./...                                      # compile check (no agent CLI needed)
```

**Do NOT run E2E tests proactively.** They make real API calls that consume tokens and cost money. Only run when explicitly asked.

## Structure

```
e2e/
├── agents/       # Agent abstraction (Agent interface, tmux sessions, concurrency gates)
├── bootstrap/    # CI pre-test setup (auth config, warmup)
├── controlplane/ # Control-plane tests (login, org/project/repo) with their own TestMain
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

## Control-Plane Tests

`controlplane/` runs the `entire` binary against the production control plane with no coding agent involved. Its `TestMain` logs in once per run with `entire login --device`, completing GitHub sign-in and the device approval in headless Chromium (playwright-go) as the GitHub test user named by `E2E_GH_USERNAME` / `E2E_GH_PASSWORD` / `E2E_GH_TOTP_SECRET`; every test then starts from that session. The account has authenticator-app 2FA enabled on purpose: GitHub skips its emailed new-device verification for 2FA accounts, and the test computes the one-time code from the secret. The CLI's config and token store live in a temp dir outside `e2e/artifacts/`, which CI uploads.

Tests create real resources named `e2e-cp-<timestamp>` and delete them in reverse order (repo, project, org) through `t.Cleanup`. The native-mirror lifecycle test also places a real mirror and removes it; seeding one took 9 seconds when measured against production, so it needs no extra time budget. The test account may own at most three orgs, so a run that is killed before cleanup (package timeout, cancelled job, lost runner) would block later ones; each run therefore starts by sweeping `e2e-cp-*` orgs older than 30 minutes, and everything under them.

Run it with `mise run test:e2e:controlplane [filter]`; the task installs the Playwright driver and Chromium on first use.

## Adding a New Agent

1. Create `agents/<name>.go` implementing the `Agent` interface.
2. Register it in `init()` with `Register(&YourAgent{})`.
3. Add a `Bootstrap()` method for any CI-specific setup (auth config, warmup).
4. Add a `RegisterGate("<name>", N)` call if concurrency needs limiting.
5. Ensure the agent name is accepted by `mise run test:e2e --agent <name>`.
6. Add the agent to `.github/workflows/e2e.yml` matrix and dispatch options.

## Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `E2E_AGENT` | Runner filter (`claude-code`, `antigravity`, `opencode`, `codex`, `cursor-cli`, `factoryai-droid`, `copilot-cli`, `pi`, `vogon`, `roger-roger`) | all registered |
| `E2E_ENTIRE_BIN` | Path to a pre-built `entire` binary | builds from source |
| `E2E_TIMEOUT` | Per-prompt timeout, overriding every runner's own default. A per-test `agents.WithPromptTimeout(...)` still wins over it, and a malformed value is a hard error rather than a silent fall back. | per runner: 60s (codex, copilot-cli), 90s (cursor), 2m (opencode), none (claude-code, droid, pi, vogon, roger-roger — bounded only by the scenario timeout) |
| `E2E_ANTIGRAVITY_MODEL` | Optional Antigravity model override (slug from `agy models`) | `gemini-3.8-flash-low` |
| `E2E_KEEP_REPOS` | Set to `1` to preserve temp repos after test | unset |
| `E2E_CHECKPOINT_STORE` | Checkpoint backend to run the suite against (`git-branch`, `git-refs`). Maps to the `ENTIRE_CHECKPOINTS_PRIMARY` override that every spawned binary/hook honors. | `git-branch` |
| `E2E_ARTIFACT_DIR` | Override artifact output directory | `e2e/artifacts/<timestamp>` |
| `ANTHROPIC_API_KEY` | Required for Claude Code | — |
| `GEMINI_API_KEY` | Enables Antigravity's API-key mode (agy ≥ 1.1.13; hooks execute on that route from agy 1.1.25); the default in CI | — |
| `GOOGLE_APPLICATION_CREDENTIALS` | Optional Antigravity ADC (service account); takes precedence over `GEMINI_API_KEY` when set | — |
| `GOOGLE_CLOUD_PROJECT` | Optional Antigravity ADC project override | ADC JSON `project_id` |
| `E2E_ANTIGRAVITY_PROJECT` | Optional Antigravity E2E project override; takes precedence over `GOOGLE_CLOUD_PROJECT` | ADC JSON `project_id` |
| `OPENAI_API_KEY` | Required for Codex | — |
| `COPILOT_GITHUB_TOKEN` | Required for Copilot CLI, unless a `copilot login` credential is already stored. `GH_TOKEN` and `GITHUB_TOKEN` also work — Copilot reads all three, in that order of precedence. A `gh auth login` alone is not enough: Copilot does not read gh's config. | — |
| `E2E_KEEP_AGENT_HOME` | Set to `1` to preserve the isolated `COPILOT_HOME` a session ran under (holds Copilot's own logs) | unset |
| `E2E_GH_USERNAME` | GitHub test user for the control-plane tests' `entire login --device` | — |
| `E2E_GH_PASSWORD` | Password of that GitHub test user | — |
| `E2E_GH_TOTP_SECRET` | The account's authenticator-app setup key (base32, as GitHub displays it) | — |

### Antigravity credentials

Antigravity E2E resolves its auth mode from the environment, in this order:

1. **ADC** — `GOOGLE_APPLICATION_CREDENTIALS` set: agy runs against its default
   (`cloudcode-pa`) backend with `USE_ADC=1`, an isolated per-repo `HOME`, and
   `--project` from `E2E_ANTIGRAVITY_PROJECT`, `GOOGLE_CLOUD_PROJECT`, or the ADC
   JSON `project_id`. This path needs a Gemini Code Assist entitlement on the
   project (see caveat below). In GitHub Actions it is enabled by the optional
   `ANTIGRAVITY_GOOGLE_APPLICATION_CREDENTIALS_JSON` secret.
2. **Gemini API key** — `GEMINI_API_KEY` set: agy ≥ 1.1.13 talks
   to the Gemini API directly with no account session. The harness isolates
   `HOME` per repo, writes `{"modelProvider":"gemini"}` into that home's
   `~/.gemini/antigravity-cli/settings.json`, passes `GEMINI_API_KEY` (and
   `GOOGLE_GEMINI_BASE_URL` if set) through, and scrubs `GOOGLE_API_KEY` — agy
   prefers it over `GEMINI_API_KEY` when both are set. **Needs agy ≥ 1.1.25:**
   up to 1.1.24 agy loaded `.agents/hooks.json` on this route but never executed
   the hooks (google-antigravity/antigravity-cli#893, still open upstream; the
   fix shipped silently in 1.1.25 — verified by bisecting the release binaries
   with a probe hook), so Entire recorded nothing. CI installs the latest
   release, so this is the default CI mode. The hook probe
   (`E2E_ANTIGRAVITY_HOOK_PROBE=1`, log at `.entire/logs/agy-hook-probe.log` in
   artifacts) is how to tell "hooks not executed" from "hooks not loaded".

Both isolated modes also pre-trust the test repo (`trustedWorkspaces` in that
`settings.json`, agy's equivalent of `GEMINI_CLI_TRUST_WORKSPACE=true`): agy only
loads a workspace's `.agents/hooks.json` for a trusted workspace, so in a fresh
`HOME` a headless run would otherwise do the work with no Entire hooks firing.
They also seed `cache/onboarding.json` as already complete: interactive agy in a
never-run `HOME` shows a color-scheme chooser and a Terms of Service consent
before the prompt (headless `-p` skips both), and the tmux-driven
`TestInteractive*` tests otherwise time out with agy sat on the Terms screen.
The runner can also click through both screens if the seed stops being honoured.
3. **OAuth** — neither set: the developer's real `HOME` (existing `agy` login) is
   used for local runs.

The harness fails fast (no scenario restart) on the walls that don't clear on
retry: `Individual quota reached` / `SERVICE_DISABLED` / `AUTH_PERMISSION_DENIED`
(ADC/OAuth entitlement), `GEMINI_API_KEY environment variable is not set`
(API-key misconfiguration) and `API_KEY_INVALID`.

> **ADC entitlement caveat:** agy's default backend (`cloudcode-pa.googleapis.com`)
> is a gated private API that cannot be enabled with `gcloud services enable`
> (fails with `AUTH_PERMISSION_DENIED`, subject 110002) — project Owner is not
> sufficient. ADC only works once a Gemini Code Assist Standard/Enterprise
> subscription is provisioned on the project; Code Assist licenses attach to user
> identities, so plain service accounts are not entitled. Prefer the API-key mode
> unless you specifically need the Code Assist backend.

For local runs, either authenticate `agy` with OAuth, or:

```bash
export GEMINI_API_KEY=...            # API-key mode (agy >= 1.1.13)
mise run test:e2e --agent antigravity TestSingleSessionManualCommit
```

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

- **`.github/workflows/e2e.yml`** runs the standard agent suite on pushes to main. Antigravity runs in the standard suite in agy's Gemini API-key mode (see [Antigravity credentials](#antigravity-credentials)).
- For debugging a single test, dispatch **`.github/workflows/e2e.yml`** with an agent and the optional `test` regex. An empty regex keeps the normal suite. The filter also reaches Windows when running Claude; selecting another agent skips the Windows Claude job.
- **`.github/workflows/e2e-controlplane.yml`** runs the control-plane tests on pushes to main and on manual dispatch. Runs are serialized and never cancelled mid-flight, because the shared test account's resources are cleaned up by the test itself.

The E2E workflow bootstraps agents before testing. `ci.yml` covers both checkpoint backends with the free canary; `nightly-e2e.yml` checks the published nightly installation.
