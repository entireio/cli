# Testing and development tools

Test harness safety, platform-specific testing, and source guards. Repository-wide verification requirements live in [CLAUDE.md](../../CLAUDE.md#verification).

Repository paths in code spans are relative to the repository root unless stated otherwise.

### Instruction documentation guard

`go test ./docs/development` (also included in unit/CI tests) enforces the 20 KiB
`CLAUDE.md` budget and checks local inline Markdown links and heading anchors in
`CLAUDE.md`, `CONTRIBUTING.md`, and `docs/development/*.md`, including unstaged files.
It ignores fenced examples and does not fetch external URLs. Keep links in these
instruction docs inline and use ATX (`#`) headings; reference-style links and HTML
IDs are outside this small guard's scope. It does not validate prose pointers in
Go comments or error strings: audit inbound references separately when moving docs.

### Running Tests

```bash
mise run test
```

### Running Integration Tests

```bash
mise run test:integration
```

### Running the Windows Installer Tests

`scripts/install.ps1` has its own Pester suite and PSScriptAnalyzer pass under
`scripts/test/`; `.github/workflows/install-ps1-e2e.yml` runs it in both
Windows PowerShell 5.1 and pwsh on PRs that touch the installer or its tests,
followed by a real install on a Windows runner, and `mise run test:ps1` runs
the suite locally when `pwsh` is installed (it skips cleanly otherwise; it is
not part of `check`). On a fresh machine run
`scripts/test/init.ps1` once first: it installs Pester and PSScriptAnalyzer for
the current user and, on Windows PowerShell 5.1, the NuGet package provider
that `Install-Module` needs there.

### Running All Tests (CI)

```bash
mise run test:ci
```

This runs unit tests, integration tests, and the E2E canary (Vogon agent) in sequence. Integration tests use the `//go:build integration` build tag and are located in `cmd/entire/cli/integration_test/`.

### Running E2E Canary Tests (Vogon Agent)

The Vogon agent is a deterministic fake agent that exercises the full E2E test suite without making any API calls.

```bash
mise run test:e2e:canary           # Run all E2E tests with the Vogon agent
mise run test:e2e:canary TestFoo   # Run a specific test
```

- **Runs as part of `test:ci`** — canary failures block merges
- **No API calls, no cost** — safe to run freely, unlike real agent E2E tests
- **If a canary test fails, the bug is in the CLI or test infrastructure**, not in an agent
- Located in `e2e/vogon/` (binary) and `cmd/entire/cli/agent/vogon/` (Agent interface)
- The binary parses prompts via regex, creates/modifies/deletes files, and fires lifecycle hooks
- **IMPORTANT: When changing E2E test prompt wording**, the Vogon binary (`e2e/vogon/main.go`) parses prompts with hardcoded regexes. New phrasing may not match existing patterns — always run `mise run test:e2e:canary` after changing prompt text and fix Vogon's parsing if tests fail.

### Running E2E Tests (Only When Explicitly Requested)

**IMPORTANT: Do NOT run E2E tests proactively.** E2E tests make real API calls to agents, which consume tokens and cost money. Only run them when the user explicitly asks for E2E testing.

```bash
mise run test:e2e [filter]                          # All agents, filtered
mise run test:e2e --agent claude-code [filter]       # Claude Code only as an example here, replace `claude-code` with other agents to run tests for those agents
```

E2E tests:

- Use the `//go:build e2e` build tag
- Located in `e2e/tests/`
- See [`e2e/README.md`](../../e2e/README.md) for full documentation (structure, debugging, adding agents)
- Test agent interactions (creating files, committing, etc.); Vogon and Roger Roger are deterministic canaries rather than real-agent API calls.
- Validate checkpoint scenarios documented in `docs/architecture/checkpoint-scenarios.md`
- Select a runner via `E2E_AGENT`: `claude-code`, `gemini-cli`, `opencode`, `codex`, `cursor-cli`, `factoryai-droid`, `copilot-cli`, `pi`, `vogon`, or `roger-roger`. These are the filter names used by registration in `e2e/agents/`, not necessarily the CLI's agent identifiers.

**Environment variables:**

- `E2E_AGENT` - Runner filter (unset/empty: all registered agents)
- `E2E_CLAUDE_MODEL` / `E2E_CODEX_MODEL` / `E2E_GEMINI_MODEL` / `E2E_OPENCODE_MODEL` / `E2E_COPILOT_MODEL` / `E2E_CURSOR_MODEL` - pin the model per paid runner
- `E2E_TIMEOUT` - Per-prompt timeout, overriding each runner's own default (e.g. `E2E_TIMEOUT=4m`)

An unpinned runner re-routes between runs, so turn duration varies for reasons unrelated to the change under test and a single slow measurement cannot be read. Five of the seven paid runners therefore pin a cheap default in code *and* take an `E2E_*_MODEL` override — claude-code, codex, gemini, opencode and copilot-cli; `.github/workflows/e2e.yml` pins codex and gemini to cheaper ones still. The other two each have one half:

- **Cursor defaults to nothing**, so it runs on `auto` — Cursor's own listed default, which re-routes per run — until `E2E_CURSOR_MODEL` is set. `e2e.yml` and `nightly-e2e.yml` pin `gpt-5.4-nano-medium`, the cheapest id measured at $0.0047 per 3-commit prompt against `composer-2.5`'s $0.0169 — composer is the fastest (18.3s vs 26.8s) but bills cache reads at $0.20/M against nano's $0.02/M, which dominates an agentic loop that re-sends the conversation every turn. Nano is the weakest tier offered, so a capability-shaped failure should be met by bumping to `gpt-5-mini` (same cost, one tier up) before assuming the CLI is at fault. There is deliberately no in-code default: `agent --list-models` is account-scoped (an id this org is entitled to need not exist for a contributor running the leg locally) and Cursor offers no haiku-tier model to be the obvious cheap choice. Not a silent-failure risk either way — an unknown id exits 1 listing every valid one rather than falling back to `auto` — so refreshing the pin from `agent --list-models` is safe.
- **Droid has a default but no override.** That default (`custom:claude-haiku-4-5-20251001`) is a BYOK custom model that `Bootstrap()` declares in `~/.factory/settings.json`, so the flag and that file have to change together — an env knob that moved only the flag would name a model the settings do not declare.

**How a model reaches the interactive path differs by runner, and it is not always `--model`.** `StartSession` passes the flag for gemini, opencode, copilot-cli and cursor-cli; codex and droid instead pin through a settings file their setup writes (`config.toml` via `seedCodexHome`, `~/.factory/settings.json` via `Bootstrap`) and pass no model flag at all. `Claude.StartSession` is the one that pins nothing by either route, so interactive claude tests run on the account's default model rather than on the `haiku` its own `RunPrompt` pins.

The per-prompt default is the runner's, not a single number: codex, copilot-cli and gemini use 60s, cursor 90s, opencode 2m, and claude-code, droid, pi, vogon and roger-roger impose no per-prompt bound at all — for those the scenario timeout passed to `ForEachAgent` is the only deadline. `E2E_TIMEOUT` sets a bound for every runner including those, and a per-test `agents.WithPromptTimeout(...)` overrides it. All ten resolve through `promptTimeout` in `e2e/agents/agent.go`; a runner that resolves its own is a build failure (`TestEveryRunPromptResolvesThroughPromptTimeout`), as is one that passes anything but its own receiver as the scaler.

**A per-test `WithPromptTimeout` is scaled by the runner's `TimeoutMultiplier`, upward only.** The same factor `runForAgents` already applies to the scenario timeout: a duration written in a test file is authored for the work, not for whichever of the ten runners draws the subtest, so left unscaled it binds hardest on exactly the agents the multiplier exists to give room to (`TestRapidSequentialCommits` asked three commits of gemini in 120s while granting it a 10m scenario budget for being 2.5× slow). It never scales *down*, which is the one asymmetry with the scenario timeout — vogon and roger-roger at 0.5× are local and deterministic, so a tighter per-prompt threshold there could only fire on a loaded machine. The runner's own default and `E2E_TIMEOUT` are deliberately left verbatim: the first is already agent-specific and would be double-counted, and the second is typed by a human for one particular run. A malformed value is an error rather than a silent fall back to the default.

**A deadline the harness writes about itself is not scaled either, and that distinction is enforced.** `withExactPromptTimeout` (unexported) is the option for those; `WithPromptTimeout` is for tests. opencode's `Bootstrap` warmup is why they are separate: it bounds attempts at 90s then 30s, with `openCodeWarmupRetryBudget` existing precisely because "3 × 90s is over four minutes of blocked CI", and scaling by opencode's 2.0× turned that sequence into 180s + 60s + 60s — breaking the bound the constant was written to enforce, without failing anything. `TestHarnessDeadlinesAreNotScaled` fails the build on any non-test file in the package that reaches for the scaling option.

### Test Parallelization

**Always use `t.Parallel()` in tests.** Every top-level test function and subtest should call `t.Parallel()` unless it modifies process-global state (e.g., `os.Chdir()`).

```go
func TestFeature_Foo(t *testing.T) {
    t.Parallel()
    // ...
}

// Integration tests with TestEnv
func TestFeature_Bar(t *testing.T) {
    t.Parallel()
    env := NewFeatureBranchEnv(t)
    // ...
}
```

**Exception:** Tests that modify process-global state must not run in parallel.
`t.Chdir()` and `t.Setenv()` enforce this: they panic when the test or an ancestor
is parallel, and prevent a later `t.Parallel()` call. Raw `os.Chdir()` and
`os.Setenv()` do not enforce it; they still mutate process-global state and are
unsafe in parallel tests. Prefer the `t.*` helpers for enforcement and cleanup.

### Git in Tests

**Tests that touch git state must use an isolated temp repo — never the real repo CWD.**

Many handlers (lifecycle, strategy, hooks) resolve the git repo from CWD via `OpenRepository`, `GetGitCommonDir`, `DetectFileChanges`, etc. Without isolation, tests can create session state files, shadow branches, or other artifacts in the real `.git/` directory.

Use the `testutil` helpers:

```go
tmpDir := t.TempDir()
testutil.InitRepo(t, tmpDir)                    // git init + user config + disable GPG
testutil.WriteFile(t, tmpDir, "f.txt", "init")  // create a file
testutil.GitAdd(t, tmpDir, "f.txt")             // stage it
testutil.GitCommit(t, tmpDir, "init")           // commit (needs at least one commit for HEAD)
t.Chdir(tmpDir)                                 // redirect CWD-based git resolution
```

`testutil.InitRepo` configures `user.name`, `user.email`, and disables GPG signing — safe for CI environments without global git config.

**Prefer `testutil.InitRepo()` over direct `git.PlainInit()` in tests.** When a test in this repo needs an initialized repository, use `testutil.InitRepo(t, dir)` unless the test specifically needs lower-level initialization behavior that the helper cannot provide. Do not call `git.PlainInit()` directly and then create commits or run CLI git operations without also reproducing the helper's repo-local config.

**Do NOT** shell out to `git init`/`git commit` directly without setting user config and `--no-gpg-sign`, and **do NOT** run lifecycle/strategy handlers from the real repo CWD in tests.

### Config/Cache/Keyring Isolation in Tests

Tests must never read or write the developer's real `~/.config/entire`
(contexts.json, version_check.json), `~/.cache/entire` (nodes.json,
cluster_cores.json, api_discovery.json), or OS keychain. The developer may be
using `entire` for real while tests run.

- **Single resolver**: `internal/entireclient/userdirs` is the only place
  that resolves the per-user config dir (`userdirs.Config()`:
  `$ENTIRE_CONFIG_DIR` else `~/.config/entire`) and cache dir
  (`userdirs.Cache()`: `$XDG_CACHE_HOME/entire` else `~/.cache/entire`).
  Never derive these paths anywhere else.
- **In-process safety net**: `userdirs` and the `tokenstore` default backend
  detect `go test` (via `internal/testdirs`) and fall back to a throwaway
  per-process temp directory when their env override is unset. The fallback
  is shared across tests in one process — for per-test isolation still set
  `t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())` and
  `tokenstore.UseFileBackendForTesting(...)`.
- **Spawned binaries are NOT covered**: `testing.Testing()` is false in a
  subprocess. The integration and e2e TestMains set `ENTIRE_CONFIG_DIR`,
  `XDG_CACHE_HOME`, `ENTIRE_TOKEN_STORE=file`, `ENTIRE_TOKEN_STORE_PATH`, and
  `ENTIRE_TEST_AUTH_STORE_FILE` process-wide so every spawned `entire` (and
  every agent-invoked hook) inherits isolation. Any new harness that spawns
  the real binary must do the same.
- **OS keyring**: packages whose tests can reach the zalando keyring need
  `keyring.MockInit()` in `TestMain` (see `cmd/entire/cli/global_test.go`) —
  the `testdirs` fallback does not isolate keyring access in-process.

### Spawning subprocesses in tests (TTY detection)

Tests that spawn the real `entire` or `git` binary need the child to be non-interactive so prompts don't hang on a developer terminal.

`interactive.CanPromptInteractively()` resolves in this order:

1. `ENTIRE_TEST_TTY=1` → force interactive ON (any other non-empty value → force OFF).
2. `testing.Testing()` → false. In-process `go test` runs are non-interactive by default; no per-test `t.Setenv("ENTIRE_TEST_TTY", "0")` is needed.
3. Agent sentinels → false. `interactive.agentSubprocessEnvVars` is the single
   source of truth for the presence-checked ones (`COPILOT_CLI`, `CURSOR_AGENT`,
   `GEMINI_CLI`, `OPENCODE`, `PI_CODING_AGENT`) — the function reads it and the
   tests both enumerate and clear it, so adding a vendor is a one-line change.
   `GIT_TERMINAL_PROMPT=0` stays separate because only that exact value counts.
   `CLAUDECODE` is deliberately not a sentinel: Claude Code sets it, but adding
   it withdraws prompts from the largest agent population at once, which is a
   product decision rather than a detection fix.
4. `CI=<non-empty-non-false>` → false.
5. Controlling-terminal probe — `/dev/tty` on Unix, `CONIN$` + `CONOUT$` on
   Windows. A terminal held in raw mode (canonical/line input off) belongs to a
   full-screen TUI that spawned us, not to a shell we can prompt: TUI git clients
   (lazygit, gitui, tig) run `git commit` as a child while owning the screen, so
   the hook inherits the same terminal it must not prompt on. The mode check
   fails open when it cannot read the mode. See `interactive/tty_*.go` and
   `interactive/rawmode_{unix,windows}.go` for the platform split and rationale.

For subprocesses spawning the real `entire` binary (e2e, integration tests, `entire` calling itself from a hook), prefer `execx.NonInteractive` over env-var plumbing:

```go
import "github.com/entireio/cli/cmd/entire/cli/execx"

cmd := execx.NonInteractive(ctx, getTestBinary(), "status")
cmd.Dir = repoDir
out, err := cmd.CombinedOutput()
```

`execx.NonInteractive` puts the child in a new session with no controlling terminal (`Setsid` on Unix, `DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP` on Windows), so the child's platform terminal probe fails naturally. No env var required.

`interactive.UnderTest()` returns true when `testing.Testing()` or `ENTIRE_TEST_TTY` is set — use it where code needs to skip a real-terminal operation even if `CanPromptInteractively()` returns true (e.g., opening `interactive.OpenPromptTTY()` directly inside a prompt reader).

A prompt that runs Bubble Tea on a separately opened terminal (plugin confirmations, the login key prompt) must open it with `interactive.OpenPromptTTY()` and release it with `PromptTTY.Close()`, never `tea.OpenTTY()` plus a bare `Close`. Bubble Tea only gets a cancellable console reader for `os.Stdin`; on any other handle its reader loop leaves a read pending after the answer, and Go's `os.File.Close` on Windows waits for that read, which a console completes only on a keypress — the user had to press Enter twice. `PromptTTY.Close` cancels the pending read first (`CancelIoEx`, `tty_release_windows.go`); the reader then sees `io.EOF`, so the close must come after the form has returned.

### Source-Level Guard Tests

Source-level guards scan this repo's own source with `git grep` to enforce
invariants the compiler cannot. Examples include `TestRootBasesAreTrusted` (root bases are trusted paths),
`TestTranscriptReadsOnlyShrink` (the unconfined transcript-read ratchet),
`TestGitStatusCallSitesPassNoOptionalLocks`, and
`TestAllHookConfigRelPaths_CoversEveryWorktreeConfigAgent`.

**They all go through `testutil.GitGrepGuard`, which owns three flags.** Each was
missing from at least one guard, and the same one was missing from three:

- `--untracked`. `git grep` searches the INDEX. Every one of these guards has
  "someone just added a file" as its subject, so the file it most needs to see is
  the one not yet staged. Two failure shapes were worse than a miss: the
  hook-config guard compares two sets built from the same blind grep, so a new
  agent package calling `OpenHookConfig` without declaring `HookConfigRelPath`
  was absent from both and the comparison *passed*; and the root-base guard's
  staleness half reported a just-added entry as STALE, telling the author to
  delete the entry that legitimised their new root.
- `--no-color`. `color.ui`/`color.grep` set to `always` colorizes into a pipe and
  the escapes land in the **filename** field. True of `-l` too, which looks
  immune. Guards then either misparse (read as staleness, #2248) or compare
  garbage to garbage while their "did we match anything" assertion still passes.
- Repo-selector scrubbing. Git exports `GIT_DIR`/`GIT_WORK_TREE` to hooks and
  they outrank `cmd.Dir`, so a `go test` under a hook or `git rebase --exec`
  scanned a different repository. `RunGit`'s isolation does **not** cover this:
  it filters `GIT_CONFIG_*` only.

Two rules for writing one:

- **A pathspec restricted to `*.go`, so an unparseable path can be fatal.** The
  `git status` guard passed a bare `cmd internal`, which also matched testdata
  `.jsonl` and a `.md`, so its non-`.go` branch had to `continue` — and that
  skip was the only thing between colorized output and a guard that silently
  checked nothing.
- **Fail on zero matches.** A detection pattern that goes stale otherwise passes
  forever. `GitGrepGuard` does this itself; the `checked == 0` tallies are the
  second half of the same idea.

### Code Duplication Prevention

Before implementing Go code, search nearby packages and utilities for reusable patterns. Use `/go:discover-related` when available; otherwise use `rg` and inspect related implementations and tests.

**Check for duplication:**

```bash
mise run dup           # Comprehensive check (threshold 50) with summary
mise run dup:staged    # Check only staged files
mise run lint          # Normal lint includes dupl at threshold 75 (new issues only)
mise run lint:full     # All issues at threshold 75
```

**Tiered thresholds:**

- **75 tokens** (lint/CI) - Blocks on serious duplication (~20+ lines)
- **50 tokens** (dup) - Advisory, catches smaller patterns (~10+ lines)

When duplication is found:

1. Check if a helper already exists in `common.go` or nearby utility files
2. If not, consider extracting the duplicated logic to a shared helper
3. If duplication is intentional (e.g., test setup), add a `//nolint:dupl` comment with explanation
