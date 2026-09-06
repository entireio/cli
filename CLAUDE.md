# Entire - CLI

This repo contains the CLI for Entire.

## Architecture

- CLI built with github.com/spf13/cobra and github.com/charmbracelet/huh

## Key Directories

### Commands (`cmd/`)

- `entire/`: Main CLI entry point. Also home to kubectl-style external-command resolution (`entire <name>` → `entire-<name>` on PATH) — see [External Commands](docs/architecture/external-commands.md).
- `entire/cli`: CLI utilities and helpers (Cobra commands, helpers, group roots)
- `entire/cli/commands`: actual command implementations
- `entire/cli/agent`: agent implementations (Claude Code, Gemini CLI, OpenCode, Cursor, Factory AI Droid, Copilot CLI, Pi) - see [Agent Integration Checklist](docs/architecture/agent-integration-checklist.md) and [Agent Implementation Guide](docs/architecture/agent-guide.md)
- `entire/cli/strategy`: strategy implementation (manual-commit) - see section below
- `entire/cli/checkpoint`: checkpoint storage abstractions (ephemeral and persistent)
- `entire/cli/session`: session state management
- `entire/cli/integration_test`: integration tests (simulated hooks)
- `e2e/`: E2E tests with real agent calls (see [e2e/README.md](e2e/README.md))

### Command Layout

The visible CLI is organized around a set of noun groups plus a small set of
top-level verbs. The groups are the canonical home for each verb. Newer
experimental command families are discoverable through `entire labs` and
their canonical paths are always runnable.

Experimental commands are gated by a build-time visibility flag (the
`cmd/entire/cli/experimental` package): they are shown — grouped under an
"Experimental commands:" help section — in developer and nightly builds, and
hidden in stable release builds. Visibility is toggled by `experimental.Visible`
(default `"true"`), which GoReleaser stamps `"false"` only on stable tags
(`.Prerelease` empty); nightly (`vX.Y.Z-nightly.*`) and local builds leave it at
the default. Register a command as experimental with `experimental.Register(parent,
child)` instead of `parent.AddCommand(child)`. Gating only controls visibility —
the commands are always runnable in every build.

- `session` (alias: `sessions`): `list`, `info`, `tokens`, `stop`, `attach`, `adopt`, `resume`, `current`.
  `resume` with a branch arg switches to it and resumes its session; with no arg
  it opens an interactive picker of stopped sessions (across all worktrees),
  resolving each to its branch and pointing at the owning worktree when the
  branch is checked out elsewhere. Resume keeps an existing local session log
  as-is by default (`--force` overwrites it from the checkpoint).
  `adopt` moves an active session from another repo or worktree into the current
  worktree and resets target-local checkpoint bookkeeping so future commits link
  to the adopted session from the new location.
- `checkpoint` (aliases: `cp`, `checkpoints`): `list`, `explain`, `tokens`, `search`.
  `explain` also takes `--repo <owner/name>`, the drill-down for a cross-repo
  `search` hit: it reads the checkpoint from that repo's entire-api cell over
  HTTP (`/repos/{repo_id}/checkpoints/{id}` plus `.../transcript/raw`) rather
  than fetching git objects, so a foreign checkpoint never enters this repo's
  object store, ref namespace, or `tokens profile`. It needs a full checkpoint
  ID and a pushed checkpoint; `--commit`, `--session`, `--search-all`, and
  `--generate` are rejected with it, and naming the current repo is a no-op
  that falls through to the local path. See `checkpoint_api_reader.go`
  (`apiCheckpointReader`, which implements the two checkpoint reader tiers and
  deliberately not `Writer`) and `explain_repo.go`.
- `agent`: bare opens the interactive agent selector, plus `list`, `add`, `remove`
- `configure`: bare prints help and a hint pointing at `entire agent`; flags
  manage non-agent settings (telemetry, git-hook installation mode, strategy
  options, summary provider). Agent CRUD lives under `entire agent`.
- `auth`: `login`, `logout`, `status`, `contexts`, `use`, plus
  `token` (prints the active control-plane bearer to stdout for scripting/curl;
  honors `ENTIRE_TOKEN`, else the refreshed active-context login JWT). `token`
  also takes `--jurisdiction <slug>` (e.g. `us`, `eu`), which instead mints a
  jurisdictional identity token (RFC 8693 exchange, `scope=openid`,
  `aud=<jurisdiction host>`) for that jurisdiction's entire-api cells (e.g.
  `https://aws-us-east-2.api.entire.io/api/v1`), which reject the control-plane
  bearer; it exchanges `ENTIRE_TOKEN` when set (deriving the environment from the
  env token's `aud`), else the active login. `auth status` shows the caller's
  home jurisdiction so the slug is discoverable. `logout`
  takes `--everywhere` (revoke every session on the active core, not just the
  current one) and `--all-contexts` (log out of every saved login)
- `doctor`: bare runs the scan-and-fix flow, plus `trace`, `logs`, `bundle`
- `org`: control-plane organization management — `create`, `list`, `get`, `delete`
- `project`: control-plane project management — `create`, `list`, `get`, `delete`
- `repo`: control-plane repository lifecycle — `create`, `list`, `get`, `delete`,
  `clone`, plus the `mirror` and `visibility` subtrees. Git content operations
  (log, diff, …) are intentionally out of scope. The `mirror` subtree is
  server-side (`create`, `list`, `get`, `remove`, `collaborators`) with one
  exception: `mirror use` repoints the *current clone's* git remote at a mirror
  (local git config only — it creates nothing server-side). Interactively it
  picks among the repo's placements and asks whether to replace the remote
  (preserving the old URL under `--upstream`) or add a separate one;
  non-interactively it repoints `--remote` directly. Both `use` and `clone`
  choose a placement through the shared `selectPlacement` picker. `clone`
  accepts a native `/et/<project>/<repo>` ref, a mirror `/gh/<owner>/<repo>`
  ref, or a full `entire://` URL passed through verbatim. **Every ref names its
  forge**: the leading token alone decides which grammar is tried, and the bare
  `<project>/<repo>` shorthand was removed because both forges take that shape
  and nothing in it says which was meant (#2252).
  Requiring the prefix is a **namesquatting** guard, not tidiness: without it,
  whichever namespace the CLI defaulted to could shadow the other, and
  `TestCloneRefAlwaysRequiresItsForgePrefix` pins that no forge-less pair
  resolves in either parser or in the command. It holds only for *intent* —
  lookups are already unambiguous because native rows are stored prefixed in the
  same `full_name` index (`et/<project>/<repo>`), which is why the bare-pair
  `--repo` filters on `search`/`experts`/`explain` cannot cross namespaces
  either.
  Native names are validated client-side against the server's own rules
  (`nativeProjectRe`/`nativeRepoRe`, mirroring `normalizeName` in entiredb
  `core/resource/project_name.go`); those bounds are server parity only and buy
  a local error instead of a control-plane round trip, so a failure is phrased
  as what the server accepts rather than as a rule of ours — drift is
  one-directional and only a *looser* server would make us wrong. A ref matching
  no grammar gets a targeted error (`invalidCloneRefError`) in descending
  confidence: a ref naming `github.com` is pointed at its `/gh/` form, a ref
  that declared a forge token keeps its own parser's reason, a bare pair is
  offered the forge-qualified readings that would actually parse
  (`bareRefSuggestions`), and anything left lists the accepted shapes.
  A native ref resolves project → repo ULID → `GetRepo`, whose response is the
  only one carrying both `clusterHost` and `path`, and clones
  `entire://<clusterHost><path>` from the repo's home cluster (`--cluster` is
  rejected on native refs).
  A trailing `.git` is never part of a repo name, on **either** backend
  (`gitDirSuffix` documents the mechanics): every ref parser drops it and `repo
  create` refuses a name ending in it. This is a deliberate client-side
  narrowing — GitHub rejects such a name outright, but the server accepts a
  native `foo.git` (interior dot, same rule that makes `entire-trails.el` legal)
  and strips the suffix for `/gh/` paths only. `gitremote.splitOwnerRepo` trims
  unconditionally when reading a remote back, so such a repo is unaddressable by
  name once cloned regardless; dropping it everywhere makes the CLI agree with
  itself instead of leaving `repo clone` the one path that keeps it. Escape
  hatches: the repo's ULID, or a full `entire://` URL. Two consequences worth
  knowing — a `foo.git` created through the API or web UI *aliases* onto `foo`
  in `resolveRepoRef`, and the durable fix is a server-side rule in
  `normalizeName`, not this check.
- `grant`: manage access grants and org membership — `org`, `project`, and `repo`
  each support `add` / `list` / `remove`

Forge tokens (`gh`, `et`) are the path segments of an `entire://` URL, and
`gitremote.pathForges` owns the *set* — `IsForgePathToken` answers "is this a
forge token", `ForgePathLabels` gives the placeholder spelling of the segments
after it (`<owner>/<repo>` vs `<project>/<repo>`) so messages read correctly for
both. The token strings are still spelled in a dozen call sites; only the set
lives in one place. It is deliberately **not** `hostToForge`, which maps an
*upstream* git host to its id: a native repo has no upstream host, so `et` is
absent there, and conflating the two made `entire://et/<project>/<repo>` dial a
cluster named `et` while `entire://gh/...` got an actionable message.
`CanonicalHost` still reads `forgeToHost`, so a native remote falls back to its
cluster host rather than inventing a forge host. The legacy `/git/` prefix is
excluded because `repo clone` cannot act on such a ref.

**Being a forge token says nothing about which APIs accept it** — the name says
syntax on purpose. Trails are the current example: entire-api takes `et` in the
path but cannot resolve it, so `entire trail` refuses it locally with the real
reason (`errTrailsNativeUnsupported`). That refusal has to cover *both* ways a
forge reaches the API — named in `--repo` and inferred from the origin remote —
and the inferred one is the common path.

Experimental commands (gated by the build-time visibility flag above — visible
and grouped under "Experimental commands:" in developer/nightly builds, hidden
in stable releases, always runnable): `tokens`, `import`, `review`,
`investigate`, `blame`, `why`, `experts`, `runner`, `checkpoint policy`, and
`trust` (per-repo checkpoint egress consent once global tracking is on). `tokens` is also
advertised through `entire labs`.

Top-level lifecycle and standalone commands: `enable`, `disable`, `status`,
`login`, `logout`, `clean`, `version`, `dispatch`, `activity`, `help`,
`configure`, `agent-help`, `api`, `search`. `search` is the canonical
spelling (visible in every build, grouped with Sessions & Checkpoints);
`checkpoint search` stays a working alias of the same command.

`api` is an authenticated passthrough to Entire's HTTP APIs (gh-style): it
attaches the right bearer and dials the right host so callers don't plumb auth
themselves. `--to core` (default) hits the control plane; `--to cell` hits an
entire-api cell. `--jurisdiction <slug>` (e.g. `us`, `eu`) targets a specific
jurisdiction's cell instead of the caller's home cell and implies `--to cell`
(cell routing + identity-token exchange live in `auth.NewEntireAPICellClient`
via `auth.CellTarget`). `{owner}`/`{repo}`/`{repo_id}` in the path are filled
from the current repo's origin remote. It is an escape hatch, so it is absent
from `agent-help`'s curated listing but stays in `entire help` and agent-help's
footer — an agent that needs raw access must find it rather than hand-roll curl
with a token.

`agent-help` renders machine-readable, agent-facing usage live from the Cobra
command tree (so it always matches the installed binary): bare prints a curated
"when to use entire" map; `agent-help <command>` drills into one command's
current flags; `--json` emits structured output. It is the single source of
truth the first-turn context injection and the `--agent-help-skill` skill point
agents at, instead of enumerating a surface that goes stale.

#### Where agent-facing text goes

| What you have | Where it goes |
| --- | --- |
| A new command | `agentHelpClassification` in `agent_help_cmd.go` — one entry, keyed by command path, carrying `audience` and `listed` |
| "When to use this at all" advice for agents | `agentHelpGuidance` — **never** cobra `Short`/`Long` |
| A fact humans need too (e.g. "this output is not stable") | cobra `Long`. Human help is a reference, not a lecture: whoever typed `--help` already chose the command |
| A per-task command recommendation | `agent-help`, which is pulled on demand. **Never** the first-turn injection, which carries only invariants true on every turn |

**Flag it; don't decide it.** Whether a command is `listed`, and whether it is
read-only / task-driven / user-owned, are product judgment calls — they change
what agents do unprompted in every user's repo. Take the safe default
(unlisted, user-owned), then say in the PR what you picked and why so a human
can move it. Never quietly promote a command into the listing or into
read-only.

CI enforces the mechanical parts, so trust these rather than re-deriving them:
every advertised top-level command and every child of a listed group is
classified; a read-only group contains no writing subcommand; guidance text
never appears in a command's `Short`/`Long`.

Hidden commands opt into being advertised here by setting
`Annotations[agentHelpAnnotation] = "true"` (e.g. `trail`). Because `agent-help`
renders live and lists non-hidden commands, the experimental commands appear in
`agent-help` in developer/nightly builds and are absent in stable releases — the
advertised surface is build-dependent, matching what `entire help` shows.
No-channel agents (Cursor, Copilot CLI, Factory Droid, MCP hosts — no
context-injection channel and no agent-help skill template) reach it without an
active push. All of them can discover it passively: it is visible in `entire
help`, the `entire status` footer points at it, and `entire status --json`
exposes it as the `agent_help` field. On top of that, Factory AI Droid (which is
banner-only) gets the pointer appended to its SessionStart hook banner, and
MCP-host agents can launch the hidden `entire mcp` stdio server, which exposes
`agent_help` and `entire_status` as MCP tools using the same live rendering.
Enabling a no-channel agent with `--agent-help-skill` reports the skill
unsupported and points the agent at this passive path instead.

Cobra-native aliases (no hint): `sessions` → `session`, `cp`/`checkpoints` →
`checkpoint`.

Hidden infrastructure commands: `hooks`, `trail`,
`curl-bash-post-install`, `__send_analytics`, `__sweep_sessions`, `mcp` (MCP
stdio server for MCP-host agents).

Diagnostic subcommands live alongside `doctor.go` as `doctor_logs.go` and
`doctor_bundle.go`. Group roots and noun-group children live in files
named `<noun>_group.go` and `<noun>_<verb>.go` respectively.

## Tech Stack

- Language: Go 1.26.x
- Build tool: mise, go modules
- Linting: golangci-lint

## Development

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
- See [`e2e/README.md`](e2e/README.md) for full documentation (structure, debugging, adding agents)
- Test real agent interactions (Claude Code, Gemini CLI, OpenCode, Cursor, Factory AI Droid, Copilot CLI, Pi, or Vogon creating files, committing, etc.)
- Validate checkpoint scenarios documented in `docs/architecture/checkpoint-scenarios.md`
- Support multiple agents via `E2E_AGENT` env var (`claude-code`, `gemini`, `opencode`, `cursor`, `factoryai-droid`, `copilot-cli`, `pi`, `vogon`)

**Environment variables:**

- `E2E_AGENT` - Agent to test with (default: `claude-code`)
- `E2E_CLAUDE_MODEL` - Claude model to use (default: `haiku` for cost efficiency)
- `E2E_TIMEOUT` - Timeout per prompt (default: `2m`)

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

**Exception:** Tests that modify process-global state cannot be parallelized. This includes `os.Chdir()`/`t.Chdir()` and `os.Setenv()`/`t.Setenv()` — Go's test framework will panic if these are used after `t.Parallel()`.

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
- **Legacy auth store**: `auth.NewStore()` talks straight to the zalando
  keyring; packages whose tests can reach it need `keyring.MockInit()` in
  `TestMain` (see `cmd/entire/cli/global_test.go`) — the `testdirs` fallback
  does not cover it in-process.

### Spawning subprocesses in tests (TTY detection)

Tests that spawn the real `entire` or `git` binary need the child to be non-interactive so prompts don't hang on a developer terminal.

`interactive.CanPromptInteractively()` resolves in this order:

1. `ENTIRE_TEST_TTY=1` → force interactive ON (any other non-empty value → force OFF).
2. `testing.Testing()` → false. In-process `go test` runs are non-interactive by default; no per-test `t.Setenv("ENTIRE_TEST_TTY", "0")` is needed.
3. Agent sentinels (`GEMINI_CLI`, `COPILOT_CLI`, `PI_CODING_AGENT`, `GIT_TERMINAL_PROMPT=0`) → false.
4. `CI=<non-empty-non-false>` → false.
5. `/dev/tty` probe, plus its terminal mode → a terminal held in raw mode
   (canonical input off) belongs to a full-screen TUI that spawned us, not to a
   shell we can prompt: TUI git clients (lazygit, gitui, tig) run `git commit`
   as a child while owning the screen, so the hook inherits a `/dev/tty` it
   must not prompt on. Fails open when the mode can't be read. See
   `interactive/rawmode_unix.go` for the rationale.

For subprocesses spawning the real `entire` binary (e2e, integration tests, `entire` calling itself from a hook), prefer `execx.NonInteractive` over env-var plumbing:

```go
import "github.com/entireio/cli/cmd/entire/cli/execx"

cmd := execx.NonInteractive(ctx, getTestBinary(), "status")
cmd.Dir = repoDir
out, err := cmd.CombinedOutput()
```

`execx.NonInteractive` puts the child in a new session with no controlling terminal (`Setsid` on Unix, `DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP` on Windows), so the child's `/dev/tty` probe fails naturally. No env var required.

`interactive.UnderTest()` returns true when `testing.Testing()` or `ENTIRE_TEST_TTY` is set — use it where code needs to skip a real-terminal operation even if `CanPromptInteractively()` returns true (e.g., reading from `/dev/tty` directly inside `askConfirmTTY`).

### Linting and Formatting

```bash
mise run fmt && mise run lint
```

`mise run fmt` can rewrite files. Treat `mise run fmt && mise run lint` as a single verification sequence: if formatting changes anything, run lint again on the formatted tree rather than assuming a previous lint result still applies.

### Before Every Commit (REQUIRED)

**CI will fail if you skip these steps:**

```bash
mise run check
```

Equivalent expanded form:

```bash
mise run fmt      # Format code (CI enforces gofmt)
mise run lint     # Lint check (CI enforces golangci-lint)
mise run test:ci  # Run all tests (unit + integration)
```

`mise run check` runs the three commands above.

Safety note: do not treat a clean `mise run lint` result as final unless it was run after the most recent `mise run fmt` pass.

### Before Any Push Or Remote Code Update (REQUIRED)

Before pushing commits or otherwise sending code changes to any remote, run `mise run lint` on the current tree and ensure it passes. If `mise run fmt` changed files, rerun `mise run lint` on the formatted tree before pushing.

**Common CI failures from skipping this:**

- `gofmt` formatting differences → run `mise run fmt`
- Lint errors → run `mise run lint` and fix issues
- Test failures → run `mise run test` and fix

### Code Duplication Prevention

Before implementing Go code, use `/go:discover-related` to find existing utilities and patterns that might be reusable.

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

## Code Patterns

### Error Handling

The CLI uses a specific pattern for error output to avoid duplication between Cobra and main.go.

**How it works:**

- `root.go` sets `SilenceErrors: true` globally - Cobra never prints errors
- `main.go` prints errors to stderr, unless the error is a `SilentError`
- Commands return `NewSilentError(err)` when they've already printed a custom message

**When to use `SilentError`:**
Use `NewSilentError()` when you want to print a custom, user-friendly error message instead of the raw error:

```go
// In a command's RunE function:
if _, err := paths.WorktreeRoot(); err != nil {
    cmd.SilenceUsage = true  // Don't show usage for prerequisite errors
    fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Please run 'entire enable' from within a git repository.")
    return NewSilentError(errors.New("not a git repository"))
}
```

**When NOT to use `SilentError`:**
For normal errors where the default error message is sufficient, return the error directly. main.go will print it:

```go
// Normal error - main.go will print "unknown strategy: foo"
return fmt.Errorf("unknown strategy: %s", name)
```

**Key files:**

- `errors.go` - Defines `SilentError` type and `NewSilentError()` constructor
- `root.go` - Sets `SilenceErrors: true` on root command
- `main.go` - Checks for `SilentError` before printing

### `.entire` Must Be a Directory

`<worktree-root>/.entire` is either absent or a real directory. A regular file, a
symlink — **including a symlink pointing at a perfectly good directory** — a
FIFO, a socket, or a device is a broken repo, and a command that would read or
write through the path stops instead.

**The entries directly inside it must be regular files or directories.** That is
an allowlist, not a list of known-bad types: Entire only ever creates files and
directories under `.entire`, so anything else arrived some other way and a mode
bit nobody has considered yet is refused by default. A symlinked
`.entire/metadata` redirects transcripts; a symlinked `.entire/settings.local.json`
redirects the file that names the command Entire executes at pre-push; a FIFO in
either place hangs the read instead. The scan is one level deep, and
`fs.ModeIrregular` is its one deliberate exception — see the entry-scan
mechanics below.

`paths.ValidateEntireDirAt(worktreeRoot)` / `paths.RequireEntireDir(ctx)`
(`paths/entiredir.go`) are the only implementation. The stat is `Lstat`, not
`Stat`, which is the whole point: `.entire` holds session metadata, transcripts,
and the redaction settings that decide what may be committed, so a path someone
else owns the far end of is not one we write through. Absent is fine (Entire is
not enabled yet, or `enable` is about to create it). A stat error other than
"not exist" is a failure — it is not evidence the invariant is violated, but it
is not evidence it holds either, and the caller's next move is to write there.
Not memoized, deliberately: the `Lstat` and the one-level listing are free next
to the `git rev-parse` that precedes them, and a cached "it was fine" is stale in
a long-lived `entire mcp`.

**Four failure conditions, each identified positively.** `ErrEntireDirNotDirectory`
(the path exists and is the wrong type), `ErrEntireDirUnsupportedEntry` (an entry
directly inside it is neither a regular file nor a directory), `ErrEntireDirUnreadable` (`Lstat` or the
directory listing failed, so nothing is known about the path), and
`ErrRepositoryUnresolved` (the worktree root would not resolve, so there is no
path to inspect yet). Callers print a remedy, and the remedies are different
things: replace the path, replace the entry, fix ownership/permissions, fix git.
Match them with `errors.Is` and give an unmatched error **no** remedy — an `else`
branch is how a filesystem `EACCES` came to be answered with advice about
`safe.directory`, printed directly under a line that already said "permission
denied". `writeEntireDirRemedy` and `writeEntireDirDiagnosis` (doctor's
labelled variant: BROKEN / UNREADABLE / UNVERIFIED) both take the error as a
parameter so every branch is reachable in a test; staging a genuinely
unreadable `.entire` is impractical, since removing execute permission on the
repo root breaks worktree-root discovery first and exercises the wrong branch.
An unsupported entry and a wrong-typed `.entire` share doctor's BROKEN heading:
to the reader they are one condition — something replaced a path Entire owns —
differing only in which path and what to put back.

**`ErrEntireDirNotDirectory` is not reused for an unsupported entry**, even though
both remedies are "replace it". `.entire/settings.json` is not required to be a
directory, so telling someone it is not one names the wrong problem. The entry
remedy says "replace it with a real file or directory"; the `.entire` remedy says
"replace it with a real directory", and `TestEntireDirRemedyMatchesTheCondition`
asserts neither branch prints the other's phrase.

**Entry-scan mechanics.** `validateEntireDirEntries` does one `os.ReadDir` and
passes each `DirEntry.Type()` to `unsupportedEntryType`, with **no `Lstat` of its
own** — the type comes from the directory read where the platform reports one,
and where it does not, `os.ReadDir` does the `Lstat` internally, *skipping an
entry that vanished between the read and the stat*. `DirEntry.Type()` is
therefore never unresolved, and `Type() == 0` means a regular file, not unknown
(`direntType` returns `^FileMode(0)` for unknown, which sets every bit including
`ModeSymlink`, so even a leak would fail closed). Adding an `Lstat` here
reintroduces that race, which matters because `.entire/tmp` and
`.entire/metadata/<session>` churn under concurrent hooks. The entries are
checked **before** `ReadDir`'s error, because `os.ReadDir` returns what it
managed to read alongside a partial-read error and an unsupported entry among
those is a positive finding — a stronger statement than "the listing failed". One
error names the first offender in `ReadDir`'s sorted order (so the message is
deterministic) and counts the rest, rather than one error per entry: the remedy
is identical for all of them, and a user who reruns the command once per planted
entry is paying for our formatting.

**`fs.ModeIrregular` is tolerated, and that is the one place the allowlist
bends.** Windows overloads the bit: Go maps every reparse tag it has no category
for onto it (the `default` arm of `fileStat.mode` in `os/types_windows.go`),
which lands NTFS directory junctions *and* OneDrive Files On-Demand placeholders
in the same bucket, indistinguishable from a `DirEntry`. Refusing the bucket
would hard-fail every command in a repo inside a synced folder, with a remedy the
user cannot act on, and the placeholder arrives with nobody attacking anything.
The junction it would also catch cannot arrive by checkout — git has no
tree-object mode for one — so planting it already requires local code execution,
at which point this check is not what stands in the way. **The bit is masked
out of the type, not matched against it** — `mode.Type() &^ fs.ModeIrregular`
must equal `0` or `fs.ModeDir` — because Windows does not hand it over alone:
`ModeDir` is withheld only for a *name-surrogate* reparse tag, and the cloud
tags are not surrogates, so a placeholder **directory** arrives as
`ModeDir|ModeIrregular` while a junction (a surrogate) arrives as
`ModeIrregular` by itself. `.entire`'s own entries are mostly directories, so an
exact match on the bare bit would reject `metadata`, `logs`, and `tmp` in exactly
the synced folder the tolerance exists for. Masking does not soften the rest of
the field: anything carrying a rejected type is rejected whatever else it
carries, `ModeIrregular` included.

**The comparison is against the whole type field, never `IsRegular`/`IsDir`.**
Those examine single bits (`IsDir` is `mode&ModeDir != 0`), so an allowlist
keyed on them lets a rejected type in by *also* setting an accepted bit:
`ModeDir|ModeSymlink` and `ModeDir|ModeNamedPipe` both satisfy `IsDir`, and so
does the all-bits-set unknown mode above — which is what made the "even a leak
would fail closed" claim false until it was fixed. An allowlist a rejected type
can enter by setting an extra bit is not an allowlist.
`TestUnsupportedEntryType` pins each combination. Distinguishing junction from
placeholder would mean reading the reparse tag through a
Windows-only syscall (`FindFirstFile`, then `Reserved0 & 0x20000000` for the
name-surrogate tags); that is the upgrade path if junctions ever become worth
catching.

**A settings file is never read through a link, and the settings reader enforces
that itself.** `readConfined` (`settings/settings.go`) — the chokepoint every
settings read funnels through, including `LoadFromFile`, `LoadProjectRaw`,
`LoadLocalRaw`, and clone preferences — `Lstat`s the entry through its own
`os.Root` handle and refuses a symlink outright, wrapping
`paths.ErrEntireDirUnsupportedEntry` via the shared `paths.SymlinkedEntryError`
(the symlink-specific message builder, which names the link target; the entry
scan reaches it through `unsupportedEntryError` and describes other types with
`describeMode`).

This is deliberately redundant with the `.entire` entry scan, because the two
cover different callers: the scan hangs off the root pre-run and
`LoadEntireSettings`, while **eighteen files call `settings.Load` directly** —
`strategy/hooks.go`, `manual_commit_hooks.go`, `checkpoint/remote/*`, `review/*`,
`investigate/*` — and reach settings without ever passing the pre-run.

**`os.Root` confinement is not the invariant, and was not sufficient.** Measured
against `readConfined` before the change: an absolute target (even one pointing
inside `.entire`) and an escaping relative target were refused, but as `path
escapes from parent`, naming neither cause nor fix; and two shapes got through:

| link | before | after |
| --- | --- | --- |
| `settings.local.json -> planted.json` (relative, stays inside) | **followed** | refused |
| `settings.json -> missing.json` (dangling) | **ENOENT → silently default settings** | refused |

The dangling case was the worse of the two: every caller reads ENOENT as
"absent", so a planted link made Entire ignore the project's settings without
saying anything. Do not "simplify" the `Lstat` away on the grounds that
`os.OpenRoot` already confines the read — it confines it, which is a different
property from refusing a link.

Writes are already safe and need no equivalent: `jsonutil.WriteFileAtomic`
finishes with `os.Rename` over the target, which *replaces* a symlink rather
than writing through it.

**Non-goals, deliberately.** The scan is *not* recursive — walking deeper would
traverse every session's transcripts on every command, and the checkpoint writer
already skips symlinks as it walks the metadata directory. It does *not* look at
permissions or ownership, only at type. Relocating `.entire/logs` and
`.entire/tmp` out of the worktree is a separate change; until then, redirecting
them with a symlink is refused rather than supported, and the remedy text says
so.

Cost of the second phase: measured 8.2µs against 1.0µs for the `Lstat` alone, on
a `.entire` holding six subdirectories, three files, and 51 session directories
it does not descend into. That is ~0.1% of the `git rev-parse` subprocess that
`WorktreeRoot` runs immediately before it, which is why the deliberate
non-memoization below still holds.

**Guarded is the default.** The root `PersistentPreRunE` runs the check for every
command, above both `settings.IsSetUpAny` and `ensureLogger` because each of
those already touches the path. A command opts out with
`exemptFromEntireDirCheck(cmd)` (`entiredir_guard.go`), which sets an annotation
that `skipsEntireDirCheck` inherits down the parent chain, so annotating a group
root covers its children. Exemption is registered at the `AddCommand` call in
`root.go`, so the whole set reads as one list.

Exempt means "this command needs nothing under `.entire`" — control-plane and
account commands, `version`/`labs`/`completion`, and `doctor`. It does **not**
mean "write through it anyway": `checkEntireDirBeforeRun` returns
`safe == false` for an exempt command in a broken repo, which is what keeps
`ensureLogger` from creating `.entire/logs` through the symlink. `newLogger`
repeats the check for the callers that build a logger outside the pre-run.

The pre-run is not the only enforcement point. `LoadEntireSettings` repeats the
check, because the pre-run does not cover everything: external plugins are
dispatched from `main.go` before cobra runs at all, and exempt commands still
reach settings through the post-run telemetry path. Settings are read *from* the
directory in question, so loading them is the one operation those callers have
in common — the duplicated `Lstat` on the ordinary path buys the guarantee that
the check happens at least once on the unusual ones.

Outside a git repository there is no worktree root and so nothing to validate,
and the check is skipped rather than failing. Commands that need a repository
report its absence themselves, with a message about the repository rather than
about `.entire`.

**That skip requires git's positive verdict, not merely a failed lookup.**
`WorktreeRoot` classifies its own failure and wraps `paths.ErrNotARepository`
only when git ran, exited non-zero, and said "not a git repository"; exit code
128 alone is not the signal, since git also uses it for dubious ownership and
permission failures, both of which happen *inside* a repository. Locale
variables are pinned to C for that subprocess so the message is recognisable on
a translated machine. Every other outcome — git missing from `PATH`, a cancelled
context, a killed child, success with empty output — fails closed.

The reason is that "we could not find out" is not the same as "there is nothing
here", and guessing costs more than a skipped check: `settingsAbsPaths` falls
back to a path relative to the *current directory* when the root will not
resolve, so a wrong guess reads `./.entire/settings.json` — through the very
symlink the guard exists to reject. Refusing to run on a machine whose git is
broken is the cheaper mistake. Do not "simplify" `RequireEntireDir` back to
treating any `WorktreeRoot` error as absence.

Every exemption needs an entry in `entireDirCheckExemptions`
(`entiredir_guard_test.go`) giving the reason; `TestEntireDirCheckExemptions`
fails both on an unlisted exemption and on a stale entry, so an exemption added
to silence a failing test does not pass for a considered one. `help` and
`agent-help` are deliberately guarded — someone asking what they can do in this
repo is told the repo is broken rather than handed a working command list.
`entire <command> --help` is unaffected in every case, because cobra returns
`flag.ErrHelp` before it runs any `PersistentPreRunE`; that and `doctor` are the
escape hatches.

`doctor` is exempt so that it can run **on** a broken repo, which is only worth
doing if it says what is wrong: `reportBrokenEntireDir` runs in the doctor
group's `PersistentPreRunE` — ahead of doctor's own `PreRunE`, which loads
redaction settings from `.entire/settings.json`, and ahead of `doctor logs` /
`doctor bundle`, which read `.entire/logs` — prints the diagnosis, and stops. It
does not auto-fix: what occupies the path may be someone's data.

### Settings

All settings access should go through the `settings` package (`cmd/entire/cli/settings/`).

**Why a separate package:**
The `settings` package exists to avoid import cycles. The `cli` package imports `strategy`, so `strategy` cannot import `cli`. The `settings` package provides shared settings loading that both can use.

**Usage:**

```go
import "github.com/entireio/cli/cmd/entire/cli/settings"

// Load full settings object
s, err := settings.Load()
if err != nil {
    // handle error
}
if s.Enabled {
    // ...
}

// Or use convenience functions
if settings.IsSummarizeEnabled() {
    // ...
}
```

**Do NOT:**

- Decide whether Entire is *active* in a repo from settings-file presence alone (`IsSetUp*`). The full question is **"the repo's own settings enable it OR global tracking covers it"**, each half with a qualifier: a file saying `enabled: false` is a veto, a `settings.local.json` counts only when it is the developer's own untracked file, and the global tier counts only when the repo is not excluded. `repopolicy.ClassifyRepoPolicy` computes exactly that; use `settings.IsActiveForRepo(WithReason)` for the current repo and `settings.IsActiveAtRoot` for another root instead of hand-rolling the OR. A `forbidigo` rule rejects `settings.IsSetUpAndEnabled` / `IsSetUpAtRoot` callers with that message. `IsSetUpAny`/`RepoActivationConfigured` remain for *setup routing* (does repo-level config exist?), never for capture gates.
- Read `.entire/settings.json` or `.entire/settings.local.json` directly with `os.ReadFile`
- Duplicate settings parsing logic in other packages
- Create new settings helpers without adding them to the `settings` package

**Key files:**

- `settings/settings.go` - `EntireSettings` struct, `Load()`, and helper methods
- `config.go` - Higher-level config functions that use settings (for `cli` package consumers)

### Logging vs User Output

- **Internal/debug logging**: Use `logging.Debug/Info/Warn/Error(ctx, msg, attrs...)` from `cmd/entire/cli/logging/`. Writes to `.entire/logs/`.
- **Enabling debug/perf logs locally**: Prefer adding `"log_level": "DEBUG"` to `.entire/settings.local.json` when you need detailed hook/perf logs. This file is gitignored. `ENTIRE_LOG_LEVEL=debug` also works and takes precedence.
- **User-facing output**: Use `fmt.Fprint*(cmd.OutOrStdout(), ...)` or `cmd.ErrOrStderr()`.

Don't use `fmt.Print*` for operational messages (checkpoint saves, hook invocations, strategy decisions) - those should use the `logging` package.

**Privacy**: Don't log user content (prompts, file contents, commit messages). Log only operational metadata (IDs, counts, paths, durations).

### The Root Anchors

Entire does filesystem I/O in seven trees, and each has one package that owns a
shared `*os.Root` over it. **Never assemble a path into one of these and hand it
to `os.ReadFile`/`os.WriteFile`/`os.MkdirAll`/`os.ReadDir`/`filepath.Walk`.**

| Tree | Owner | Anchored on |
| --- | --- | --- |
| `.entire` | `entiredir` | worktree root (`paths.WorktreeRoot`), cwd only when there is provably no repo |
| git common dir | `gitdir` | `git rev-parse --git-common-dir`, absolutized |
| the working tree | `worktreedir` | worktree root |
| an agent's hook config | `agent.HookConfigFile` | worktree root (`.claude/`, `.cursor/`, `.gemini/`, `.github/hooks/`, `.factory/`, `.codex/`, `.opencode/plugins/`, `.pi/extensions/entire/`) |
| an agent's session store | `agent.SessionStore` | the agent's own `GetSessionDir` |
| per-user config / cache | `userdirs.ConfigRoot` / `CacheRoot` | `$ENTIRE_CONFIG_DIR` else `~/.config/entire`; `$XDG_CACHE_HOME/entire` else `~/.cache/entire` |
| managed plugin tree | `pluginRoot` (`plugin_store.go`) | `pluginParentDir()` — `$ENTIRE_PLUGIN_DIR`, `%LOCALAPPDATA%`, or `$XDG_DATA_HOME` |

**An `os.Root`'s base directory must always be a trusted path — one a resolver
produced — never `filepath.Dir` of the file being opened, and never a path that
arrived as data.** This is the rule the table above encodes, and it is the one
that is easy to get wrong because the wrong version *looks* like the fix:

```go
// WRONG — the root contains exactly the one name it was handed
root, _ := osroot.Shared(filepath.Dir(target))
data, _ := osroot.ReadFile(root, filepath.Base(target))

// RIGHT — the base is what a resolver answered; the rest is a name inside it
root, _ := worktreedir.OpenAt(worktreeRoot)
name, _ := worktreedir.Name(worktreeRoot, target)
data, _ := osroot.ReadFile(root, name)
```

Anchoring on the target's own parent puts every component the caller resolved
*above* the root, so containment covers only the final component and enforces
nothing the `filepath.Join` had not already decided. A symlink at `.claude`, at
`.entire`, or at `entire-investigations` is resolved before the root exists.
Anchoring one level up makes those components **names inside** the root, which is
what `os.Root` and `osroot.MkdirAllNoSymlink` can actually refuse. The same
reasoning kills a containment *check* built on a derived base:
`settings.clonePreferencesRoot` used to compute its common dir as
`filepath.Dir(filepath.Dir(abs))` and hand both to `gitdir.OpenPathIn`, so the
relative path was correct by construction and the check could never fire.

`TestRootBasesAreTrusted` (`osroot/rootbase_guard_test.go`) enforces this: a file
that opens a root without being in `allowedRootBases` fails the build, and an
entry whose file no longer opens one fails too, so the allowlist cannot outlive
its reasons. **Prefer an existing anchor over a new root** — that is what the
anchors are for. Two entries are genuine exceptions, both on a path the *caller*
named, where the file's parent IS the caller's choice and no other base exists:
`tokenstore` (`$ENTIRE_TOKEN_STORE_PATH`) and `settings.readConfinedOutsideEntire`'s
explicit-path fallback. Each says so at the call site.

All of them memoize through **one** registry, `osroot.Shared(dir)` — a directory
is opened at most once per process. `osroot.ResetShared` clears every anchor;
`osroot.Forget(dir)` drops a single one, which is what a caller about to delete
and recreate one directory needs (the plugin index cache is a git clone this
process may `RemoveAll` mid-run — a root cached across that is a handle to an
unlinked inode). There were three copies of that map before; do not add another.

Pair the roots with `osroot` (`ReadFile`, `WriteFile`, `MkdirAllNoSymlink`,
`Remove`, `ReadDir`) and `jsonutil.WriteFileAtomicIn` / `CreateTempIn`.

**A subdirectory of an anchor is opened with `osroot.SharedChild` (memoized,
registry-owned) or `osroot.OpenChild` (short-lived, caller closes), never a bare
`parent.OpenRoot(name)`.** `os.Root` refuses a symlink that escapes the parent
but follows one pointing elsewhere *inside* it, so the bare call silently
accepts a redirected `.git/entire-sessions` or `.entire`. Both helpers `Lstat`
before and `SameFile` after, which also closes the Lstat/OpenRoot race. Neither
appears in `rootOpeners`, deliberately: their base is an already-open root, so
it is trusted by construction and flagging their callers would defeat the point
of having them.

**An atomic write leaves its temp file in the same directory as its target, and
one of those directories is walked wholesale into every checkpoint tree.**
`jsonutil.CreateTempIn` writes `<base>.<16 hex>.tmp` beside the file it is
replacing; `.entire/metadata/<session>` holds `full.jsonl` and is copied into
the tree by `addDirectoryToChanges` (ephemeral) and `copyMetadataDir`
(persistent). A hook killed between the create and the rename — an agent hook
timeout, Codex's session-end process-tree kill, a crash — leaves the temp
behind, and without a filter it is redacted, committed, and pushed on every
later checkpoint. Both walks therefore skip `jsonutil.IsTempName`. Keep that
predicate matching `CreateTempIn`'s output exactly
(`TestIsTempName_MatchesWhatCreateTempInProduces` pins it): a naming change that
outruns it turns both filters into no-ops silently. `agent.ParseChunkIndex` is
the second half of the same defence — it requires the suffix to be *entirely*
digits, because `fmt.Sscanf("%03d")` stopped at the first non-digit and so read
`full.jsonl.123abc….tmp` back as chunk 123 and reassembled it into the
transcript.

**The test is "is there a containment boundary?", not "is traversal reachable
today?"** A root is cheap and it is what keeps a *future* change safe: the names
under several of these directories are fixed constants right now, and that is not
a reason to skip the root. Two places already carried hand-written comments
saying validation was the only thing keeping a path inside its tree —
`PluginDataDir` ("guarantees ENTIRE_PLUGIN_DATA_DIR always points inside the
managed data subtree") and `investigate.RunDir` ("an unvalidated id would be a
path-traversal sink") — which is the argument for the primitive, not against it.

**What each anchor is actually protecting.** These are not uniform, and the
comments at each site say which case applies:

- `.entire` and the git common dir hold names built from agent-supplied session
  IDs, tool-use IDs, and investigation run IDs. Several call sites used to carry
  hand-written comments explaining that an unvalidated ID would be a traversal
  sink feeding `os.RemoveAll`. The root makes that structural.
- The working tree holds names from `git status` and from checkpoint **tree
  entries**, which may have been fetched from a remote. Rewind's restore half
  already opened a root for that reason; its reads did not.
- An agent's session store is where a hook payload's session ID becomes a path,
  via the agent's own `ResolveSessionFile`. `SessionStore.SessionFile` converts
  the result back to a name inside the store and **rejects** an ID that left it.

**Rules that are load-bearing rather than stylistic:**

- **`entiredir.Open` creates, `entiredir.OpenForRead` does not.** A command that
  only looks must leave an untouched repo untouched — which is why
  `logging.Config.Root` takes a *function*: the log file is created by the first
  line actually written, so a command that logs nothing leaves no `.entire`.
- **No symlinked directories.** Every create goes through
  `osroot.MkdirAllNoSymlink`, which refuses when a component already exists as a
  symlink (`osroot.ErrSymlinkedPath`). `os.Root` alone is not enough: it blocks a
  symlink that *escapes* a root but follows one pointing elsewhere *inside* it,
  and an escaping one otherwise fails later with an opaque errno far from the
  cause. `entire doctor` reports what is already there
  (`checkEntireDirSymlinks` for `.entire`, `checkAgentDirSymlinks` for every path
  Entire creates or writes on an agent's behalf — derived from
  `agent.HookConfigLocator` and the scaffold templates, deliberately not from
  `ProtectedDirs`, which both misses the levels Entire creates under an agent's
  directory and includes directories it never writes to). `.entire` **itself is
  refused too**: `entiredir`
  opens it as a checked child of the worktree root (`osroot.SharedChild`), which
  `Lstat`s before and `SameFile`s after. An earlier revision allowed it on the
  grounds that `os.OpenRoot` follows a symlinked root and an existing setup
  should keep working; that was reversed, because `.entire` holds the redaction
  settings deciding what may be committed, and "we follow it, so your data lands
  somewhere else" is not a property to grandfather. `doctor` is exempt from the
  pre-run guard so it still runs on such a repo, and its message says the repo is
  stopped rather than that the setup is fine.
  The agent hook-config directories get the same treatment via
  `agent.HookConfigFile`: a symlinked `.claude` / `.cursor` / `.gemini` /
  `.codex` / `.pi` is refused at the create, because a working tree arrives by
  clone and `entire enable` must not create directories and write JSON through a
  link the repository supplied. Pi was the last agent still joining its path onto
  the repo root and calling `os.ReadFile` / `os.MkdirAll` / `os.WriteFile` /
  `os.RemoveAll` on the result, which was worse there than for the settings-file
  agents because pi is **auto-detected**: `DetectPresence` stats `.pi`, which
  follows the link, so a repository shipping one had `entire enable` install
  through it — and uninstall `RemoveAll` through it — without the user ever
  naming pi. Its uninstall is also the one caller of
  `HookConfigFile.RemoveDir`, because pi discovers extensions by directory, so
  removing only the file would leave a half-uninstalled extension behind.

  The config FILE is refused too, not just its parents: the merge READ would
  otherwise pull the link target's contents into
  what Entire then writes, and the write is a rename, which replaces the link
  with a regular file rather than following it. Both happen silently, so
  refusing is the legible version of the same outcome. Pointing
  `.claude/settings.json` at a dotfile repo is still a real setup — it just has
  to stay out of the repository (`git rm --cached` plus a .gitignore entry),
  which is what makes it the developer's rather than the checkout's.
  `checkAgentDirSymlinks` reports every one of these paths, so the condition is
  named rather than showing up as hooks that mysteriously will not install.

  **A symlinked agent DIRECTORY is refused by every operation, not just the ones
  that create.** `os.Root` blocks a link that escapes the worktree and follows
  one pointing elsewhere inside it, so `.claude -> vendor/x` was previously read
  by `Read`/`GeneratedState`, reported present by `Exists`, and had `Remove`
  delete the file at the far end; only `Write` checked, because
  `MkdirAllNoSymlink` was the only check there was. `osroot.NoSymlinkedParent` is
  that function's read-only counterpart, and every `HookConfigFile` method calls
  it. `HookConfigFile.Root()` hands over the raw primitives, so its one caller
  (Codex's `hooksDocumentRoot`) makes the check itself.

  **`writeManagedScaffold` is the same rule for the skill scaffolds** —
  `.claude/skills/`, `.claude/agents/`, `.codex/agents/`, `.gemini/agents/`. It
  was not one of the seven call sites `HookConfigFile` replaced, and until it was
  anchored it did `os.MkdirAll` two levels and `os.WriteFile` through a symlinked
  `.claude`, landing files outside the repository and reporting Created. It now
  takes a worktree root rather than an assembled absolute path, and its callers
  no longer fall back to `os.Getwd()` when `WorktreeRoot` fails.
- **Call `Reset()` before deleting a rooted directory** (see
  `removeEntireDirectory`). A root that outlives its directory is a handle to an
  unlinked inode: writes succeed and land nowhere.
- **Two coordinates, one directory.** Repo-relative constants
  (`paths.EntireTmpDir`, `settings.EntireSettingsFile`, `logging.LogsDir`,
  `session.SessionStateDirName`) stay as they are — git tree paths, commit
  trailers, gitignore entries and messages all need them. `entiredir.Name` /
  `MustName` is the one bridge to the root-relative name used for I/O. Do not
  introduce an absolute twin of a path that already has a repo-relative spelling:
  `checkpoint.WriteOptions.MetadataDirAbs` existed alongside `MetadataDir` and
  was deleted for exactly that reason. Guard tests pin the pairs that must agree.
- **Absolute paths stay absolute when they cross a process boundary** —
  `opencode export` takes a path, `entire investigate` hands the agent its
  `state.json` path, a transcript path becomes a checkpoint's `SessionRef`.
  Those keep an absolute spelling; the reads and writes around them still go
  through a root.
- **Anything outside the CLI packages takes an `fs.FS`, not a path.**
  `redact.LoadPacks(fsys, dir, logger)` is handed `root.FS()`, which is how pack
  discovery stays confined without `redact` depending on the CLI.
- **A directory cannot be created, statted, or removed through its own root.**
  Those operations (`setupEntireDirectory`, `removeEntireDirectory`, the one
  `MkdirAll` of an agent's session dir in `resume.go`) legitimately use plain
  `os` calls.

**Deliberately not rooted**, with the reason:

- **`.git/hooks`** — `GetHooksDir` honours `core.hooksPath`, so the directory is
  often not `.git`-resident at all, and the hook filenames are compile-time
  constants. No untrusted name to contain.
- **Global/system git config** — `checkpoint/configloader.go` installs a
  *symlink-following* `billy.Basic` on purpose. `os.Root` documents that
  "symbolic links must not be absolute" unconditionally, so go-git's default
  (`osfs.Default`, a boundOS over `os.Root` anchored at `/`) silently dropped the
  global config of anyone whose `~/.config` is a symlink — author identity fell
  back to "Unknown" and signing was skipped. Scope is global + system only;
  `.git/config` is served by `r.Storer.Config()` and never reaches this. Its
  mutating methods fail closed.
- **Agent `ReadTranscript(path)` / `ReadSession(input)`** — neither carries a
  repo path, so neither can build its own store, and the path they receive was
  already resolved through one (`ResolveTranscriptPath`, `resolveTranscriptPath`).
  Containment is applied at resolution. Rooting them properly needs `RepoPath` on
  `HookInput`, which is part of the external-plugin protocol.

  **This is the one place the rooting is knowingly asymmetric, and it is the
  largest gap left**, so do not read it as settled. The WRITE half is contained
  — every agent's `WriteSession` goes through `agent.WriteSessionFile` and
  `SessionStore`, which rejects a `SessionRef` outside the agent's session
  directory — while the eight `os.ReadFile(sessionRef)` reads are not, and the
  read is what pulls transcript content into checkpoints. Closing it is a
  protocol change rather than a refactor, which is why it is scoped separately;
  the shape it wants is `HookInput.RepoPath` plus the same
  `sessionStoreForWrite` resolution the write half already uses. Do not "fix"
  it by anchoring a root on `filepath.Dir(sessionRef)` — that is the derived
  base the rule above refuses, and it would contain nothing while looking like
  it did.
- **Global/system git config** — see the config-loader bullet above; that is the
  one place rooting is actively wrong.
- **A directory the caller is about to create, replace, or delete** — creating,
  statting, or removing a directory is an operation on it from the outside, which
  a root over it cannot perform. `setupEntireDirectory`, `removeEntireDirectory`,
  the `MkdirAll` behind each anchor, and the plugin index clone are all this case.
- **Paths the user named** (`doctor bundle --out`, `api --input`) and the two
  single fixed files `/dev/tty` and `/proc/<pid>/*`. No boundary exists to
  enforce.

### Git Operations

We use github.com/go-git/go-git for most git operations, but with important exceptions:

#### Opening Repositories - Always Use `gitrepo`

**Never call `git.Open`, `git.PlainOpen`, or `git.PlainOpenWithOptions` directly.
`cmd/entire/cli/gitrepo` is the single source of truth for opening a
repository.** Use `gitrepo.OpenCurrent(ctx)` for the current worktree or
`gitrepo.OpenPath(root)` for a specific worktree root. Both funnel through
`openPathWithAlternates`, which is the only place that opens a `*git.Repository`.

Routing every open through `gitrepo` guarantees two behaviours no ad-hoc
`git.PlainOpen` call gets right:

- **Object alternates** are rewritten to absolute paths so shared clones resolve
  their objects (`PlainOpen` cannot follow relative/absolute alternates).
- **Reftable repositories** are detected and opened through the git-CLI-backed
  reference storer (`reftableStorer`). A direct `git.PlainOpen` on a reftable
  repo fails outright with `unknown extension: refstorage`, because go-git's
  filesystem storer cannot read the reftable backend. The reftable storer also
  re-approves the `objectformat` (sha1/sha256) and `worktreeconfig` extensions
  that go-git verifies at open time.

If a code path opens a repo with a bare go-git call, it silently breaks on
reftable and sha256 repositories. Reviewers should flag any new
`git.PlainOpen*`/`git.Open` outside `gitrepo`.

**`OpenCurrent` fails rather than opening the current directory.** It used to
fall back to `OpenPath(".")` when `paths.WorktreeRoot` could not resolve, and
"." is a *different repository* whenever git and the process's directory
disagree — which is precisely what the cases that break the resolution look
like. Git exports `GIT_DIR`/`GIT_WORK_TREE` to the hooks it runs and
`WorktreeRoot` honours them, while `OpenPath(".")` cannot see them, so a hook
running for repo A opened repo B; and go-git applies neither git's
`safe.directory` ownership check nor its `.git` parse, so the fallback opened
repositories the user's own git refuses. Use `gitrepo.OpenCurrentOrCwd` only for
`WarnCheckpointPolicyIfNeeded`, whose three properties do not generalise: it is
dispatched from `main.go` after cobra, so it is the one repository open with no
pre-run guard ahead of it; it only reads a policy ref; and it discards every
error. Anything that writes must stop instead — that is what "we could not
find out which repository this is" means. Key files: `gitrepo/repository.go`
(open entry points) and `gitrepo/reftable.go` (`reftableStorer`).

#### Reading Worktree Status - Always Use `gitrepo.Status`

**Never call go-git's `worktree.Status()` directly.** Use
`gitrepo.Status(ctx, repo)`; a `forbidigo` rule in `.golangci.yaml` enforces
this, and `gitrepo/status.go` is the only sanctioned call site.

`Worktree.Status()` walks the worktree, so its cost scales with working-set size
rather than with the size of the change being inspected, which makes it the most
expensive git read on the hook paths. Avoid calling it more than once per hook.
Do not memoize it either: a context-scoped cache was tried and removed, because
the write-free window it required cost more to maintain than the walk saved (see
`git log` on `gitrepo/status.go` for the measurements). The turn-start hook
currently walks twice — `CapturePrePromptState` and the strategy's prompt
attribution each read their own status.

Agent-hook capture paths must use `gitrepo.StatusWithBudget` instead: it bounds
the walk with a wall-clock budget (`gitrepo.StatusWalkBudget`) because go-git's walk is
not context-cancellable and a pathological worktree (e.g. a stray `git init` in
`$HOME`) otherwise leaves the hook process grinding for hours after the agent's
own hook timeout fires. On breach it returns an error wrapping
`gitrepo.ErrStatusBudgetExceeded` — hook callers warn and continue with
transcript-derived data (capture is fail-open; new-file detection is skipped for
the turn via the pre-prompt/pre-task `UntrackedScanSkipped` marker) — and a
process-local latch makes every later `StatusWithBudget` call in the same hook
process fail fast rather than re-entering the walk. The first-checkpoint
`git status` subprocess in the checkpoint store is bounded by the same
`StatusWalkBudget` (a killed child, not an abandoned goroutine) and reports the
same sentinel; the lifecycle handlers warn-and-skip the checkpoint on it. Turn
end persists whether the turn degraded (`SessionState.CaptureDegradedAt` — set
on breach, cleared by the next healthy turn) so `entire status` surfaces the
degradation instead of it living only in `.entire/logs`. Paths
where a user is actively waiting on a command (review, and `session adopt`
via `detectFileChangesUnbounded`) keep the unbounded
`gitrepo.Status`.

#### `git status` Is a Write - Always Pass `--no-optional-locks`

**Every `git status` Entire runs must pass `--no-optional-locks`.** A guard test
(`TestGitStatusCallSitesPassNoOptionalLocks` in `cmd/entire/cli/gitrepo/`) fails the build on
any call site that omits it.

`git status` is not a read. It refreshes the index's stat cache and, whenever any
entry is stale, writes the result back: `builtin/commit.c` takes
`.git/index.lock` for the duration of the *whole worktree walk*, then renames a
fresh index over `.git/index` (`tempfile.c`, `rename(2)`). The gate is
`use_optional_locks()`, and `--no-optional-locks` is literally `setenv(
GIT_OPTIONAL_LOCKS, "0")` — so the flag also propagates to child git processes,
and `GIT_OPTIONAL_LOCKS=0` in the environment is an equivalent user-side
mitigation. Output is byte-identical either way. The write fires on
mtime-moved-but-content-identical files — the ordinary aftermath of an agent
turn, a formatter, or an editor save — not on content edits.

That refresh is git working as designed, and running `git status` is not itself
a mistake. The reason we always drop the write is that **Entire never benefits
from it**: every call site reads the porcelain output once and discards it, so
the stat-cache update is a cost with no return.

Three consequences, all observed in the field (issue #2111):

- **A repo-deleting commit.** The rename replaces `.git/index` with a new inode.
  On a filesystem where rename-over-existing is not atomic against a concurrent
  lookup — Docker Desktop / virtiofs bind mounts, measured at 9.9% of opens
  during continuous replacement versus 0 on ext4 — a concurrent reader gets
  ENOENT. Git silently treats ENOENT on the index, **and only ENOENT**, as an
  *empty* index (every other errno calls `die_errno`), so a `git commit` landing
  in that window records the empty tree with exit code 0 and no warning: a commit
  that deletes every tracked file. Recovery is `git reset --mixed HEAD~1`.
- **The user's own `git add` failing** with `Unable to create '.git/index.lock':
  File exists` while Entire holds the lock across its walk.
- **A permanently stale `index.lock`** when a budget (`StatusWalkBudget`)
  SIGKILLs the child mid-walk, breaking every later `git add`/`git commit` until
  someone removes the file by hand.

**Passing the flag does not make the hazard go away, and must not be described
as if it did.** On an affected filesystem *any* concurrent `git status` opens the
same window: the user's own, another tool's, a file watcher's — and in
particular **N agents working the same repo**, each running its own hooks, which
is precisely the workflow Entire encourages. Our share is the one write that is
both unnecessary and asynchronous to the human's terminal, so it is the one that
can land between someone's `git add` and their `git commit`. For the writers we
do not control, the mitigation is `GIT_OPTIONAL_LOCKS=0` in the environment
(devcontainers: `containerEnv`), which covers every git process in the session.

Related: any git subprocess that can run inside a git hook and names its target
with `cmd.Dir` or `-C` must also set `cmd.Env = gitrepo.EnvWithoutRepoOverrides()`
(`gitrepo/env.go`). Git exports `GIT_DIR` / `GIT_WORK_TREE` / `GIT_INDEX_FILE` to
hooks and those take precedence over `cmd.Dir`, so a bare `exec.Command`
silently operates on the hook's repo. Deliberately *not* applied to user-invoked
commands that act on the current directory (`status`, `doctor`, `review`): there
a `GIT_DIR` the user exported is an instruction, not contamination.

This exact producer was diagnosed once before (ENT-242, Feb 2026) and lost: the
fix was closed unmerged on the premise that `git status --porcelain -z` "reads
without rewriting", which is false, and it would have grown the number of
index-rewriting call sites from one to eleven. That is why the guard test exists
rather than a comment.

#### go-git v5 Bugs - Use CLI Instead

**Do NOT use go-git v5 for `checkout` or `reset --hard` operations.**

go-git v5 has a bug where `worktree.Reset()` with `git.HardReset` and `worktree.Checkout()` incorrectly delete untracked directories even when they're listed in `.gitignore`. This would destroy `.entire/` and `.worktrees/` directories.

Use the git CLI instead:

```go
// WRONG - go-git deletes ignored directories
worktree.Reset(&git.ResetOptions{
    Commit: hash,
    Mode:   git.HardReset,
})

// CORRECT - use git CLI
cmd := exec.CommandContext(ctx, "git", "reset", "--hard", hash.String())
```

See `CheckoutBranch()` in `git_operations.go` for an example.

#### Repo Root vs Current Working Directory

**Always use repo root (not `os.Getwd()`) when working with git-relative paths.**

Git commands like `git status` and `worktree.Status()` return paths relative to the **repository root**, not the current working directory. When an agent runs from a subdirectory (e.g., `/repo/frontend`), using `os.Getwd()` to construct absolute paths will produce incorrect results for files in sibling directories.

```go
// WRONG - breaks when running from subdirectory
cwd, _ := os.Getwd()  // e.g., /repo/frontend
absPath := filepath.Join(cwd, file)  // file="api/src/types.ts" → /repo/frontend/api/src/types.ts (WRONG)

// CORRECT - use repo root
repoRoot, _ := paths.WorktreeRoot()
absPath := filepath.Join(repoRoot, file)  // → /repo/api/src/types.ts (CORRECT)
```

This also affects path filtering. The `paths.ToRelativePath()` function rejects paths starting with `..`, so computing relative paths from cwd instead of repo root will filter out files in sibling directories:

```go
// WRONG - filters out sibling directory files
cwd, _ := os.Getwd()  // /repo/frontend
relPath := paths.ToRelativePath("/repo/api/file.ts", cwd)  // returns "" (filtered out as "../api/file.ts")

// CORRECT - keeps all repo files
repoRoot, _ := paths.WorktreeRoot()
relPath := paths.ToRelativePath("/repo/api/file.ts", repoRoot)  // returns "api/file.ts"
```

**When to use `os.Getwd()`:** Only when you actually need the current directory (e.g., finding agent session directories that are cwd-relative).

**When to use repo root:** Any time you're working with paths from git status, git diff, or any git-relative file list.

Test case in `state_test.go`: `TestFilterAndNormalizePaths_SiblingDirectories` documents this bug pattern.

#### Executable Resolution - Absolute Paths Only

Two rules, each with exactly one implementation, because Go's own protections
do not reach far enough on their own.

**Every `$PATH` scanner goes through `execx.PathScanDirs()`** (`execx/pathscan.go`),
which returns only absolute entries. `exec.LookPath` reports `ErrDot` for a
match found through a "." entry and `exec.Command` re-checks a separator-free
`Path`, but neither protection reaches a scanner that resolves by
`filepath.Glob`: a globbed match never passes through `LookPath`, and it arrives
with separators in it. The external-agent scanner
(`agent/external/discovery.go`) is exactly that shape and *executes* what it
finds, so a relative entry would make a file committed to the caller's
repository a binary Entire runs. `findInaccessiblePlugin` (`plugin.go`) carries
the same rule; they are one helper because two copies is how they drifted.

**`external.Agent.run` refuses a non-absolute `binaryPath` before spawning.**
`run` sets `cmd.Dir` to the worktree root and `os/exec` resolves a relative
`Path` against `Dir`, so the file `registerExternalAgent` statted is not
necessarily the file that executes. The check lives at the exec, not at the
caller, so it also covers the exported `New`.

`pluginParentDir` (`plugin_store.go`) applies the same absoluteness rule to
every directory override it reads — `ENTIRE_PLUGIN_DIR`, `XDG_DATA_HOME`,
`LOCALAPPDATA` — via `requireAbsPluginParent`, rather than leaving the platform
two to `osroot` and to `main.go`'s `PATH` restore. Those backstops hold, but
they state the invariant two layers from where it is decided.

**The OPF `command` is the deliberate exception, and stays one.**
`redaction.openai_privacy_filter.command` becomes `argv[0]` of an
`exec.CommandContext` in `redact/opf.go` with no `cmd.Dir` and no absoluteness
check, so `"command": "./tools/opf"` resolves against the pre-push process's
working directory. That is allowed because the field is not repo-supplied: the
ownership gate above honors it only from an untracked, index-and-HEAD-verified
`.entire/settings.local.json`, so a relative path there is the developer's own
choice about their own machine, and a bare name is still covered by
`exec.Command`'s `ErrDot` re-check. The scanners are the opposite case — they
resolve names nobody chose, out of a `$PATH` the repository can reach — which
is why the rule is theirs and not this field's. Do not "finish the job" by
rejecting a relative OPF `command`: it would break the GUI-git-client setups
the explicit `command` exists to serve, without closing anything the trust gate
leaves open.

#### Invoking Commands on Windows - Never Put a Dynamic Value on a cmd.exe Line

**When Entire performs the exec itself, do not go through `cmd.exe`.** Pass the
program and its arguments as separate argv elements (`exec.Command(prog, arg)`),
or call the Win32 API directly.

cmd.exe treats `&`, `|`, `<`, `>` as command separators/redirections and expands
`%VAR%`, and **Go's argv escaping will not protect you**: `syscall.EscapeArg`
only quotes an argument containing a space, tab, quote, or backslash. A URL,
a percent-encoded path, or any `&`-bearing string therefore reaches cmd.exe bare
and is silently cut at the first metacharacter. That is exactly how
`entire login` shipped a Windows build that opened
`…/authorize?client_id=entire-cli` and got rejected for a missing
`redirect_uri` — the `cmd /c start "" <url>` launcher lost everything from the
first `&` on, and because the truncation happened inside the released child, the
CLI saw a successful launch and printed no fallback URL.

Escaping is the right tool in exactly one situation: **a third party owns the
exec.** When Entire writes a command into an agent's config file (Cursor/Codex
`hooks.json`) and that agent runs it through cmd.exe, there is no shell to
avoid — use `agent.escapeWindowsCMD`. Reviewers should flag any new
`exec.Command("cmd", …)` / `"cmd.exe"` call site that interpolates a
non-constant value.

Key files: `cmd/entire/cli/browser_open_windows.go` (ShellExecute, the
avoid-the-shell side) and `cmd/entire/cli/agent/hook_command.go`
(`escapeWindowsCMD`, the third-party-exec side). Each doc comment points at the
other.

### Control-Plane Core Resolution (which core am I talking to?)

Control-plane commands dial one of three cores: the active context's
(`coreapi.New`), a specific cluster's (`coreapi.NewForCluster`), or — when
`ENTIRE_TOKEN` is set — the env token's `aud` (the bypass inside `New`/
`NewForCluster`). This precedence lives **only** inside `coreapi`; nothing else
re-derives it.

**To display which core a request uses, ask the client: `client.CoreOrigin()`.**
It returns whatever was actually wired in, so the shown core can never diverge
from where the request goes. **Do NOT** re-resolve with
`auth.ResolveControlPlaneTarget()` for display — it only knows the active
context and silently ignores both `ENTIRE_TOKEN` and the cluster case, so it can
name a core the request never touches (this was a real bug in the `mirror list`
banner; see `repo_mirror.go` and `coreapi.Client.CoreOrigin`).

When a command resolves auth *outside* a `coreapi.Client` (e.g. `entire auth
status`, which builds its own `/me` client), it must apply the same
env-token-first precedence itself — see `resolveAuthStatusTarget` /
`resolveEnvTokenStatusTarget` in `auth.go`, which branch on
`auth.EnvTokenVar` before falling back to the active context. `logout` is the
deliberate exception: it manages a *stored* login session, which an ephemeral
env token has none of, so it stays on the active context.

### Entire-API Cell Routing (which cell does a data-plane request go to?)

The data plane (entire-api) is deployed per jurisdiction; a repo placement
lives in exactly one cell, user `/me/*` activity is consolidated in the
caller's home cell, and no server-side cross-cell aggregator exists. The CLI
therefore has exactly three routing shapes, mirroring the entire.io BFF:

- **Repo-scoped → one cell**: `resolveRepoCellTarget` (`cell_target.go`) maps
  a repo (ULID or owner/repo) to the cell hosting it — via `GetRepo`'s
  `ClusterHost` for a ULID, or via the control plane's consolidated repos
  index (`ListRepos`) for owner/repo, resolved to the repo's PROCESSING
  placement (`primaries.processing`), not just any active mirror, since a
  repo can be mirrored in several regions but only one placement holds its
  actual data. NOT best-effort: any failure (not onboarded, no/failed/
  suspended processing placement, control-plane error, timeout) returns an
  error instead of falling back to home-jurisdiction routing — a wrong-region
  "success" is worse than a command failure for repo-scoped data. Used by
  trails (`NewAuthenticatedEntireAPICellClient` in `api_client.go`) and by
  `experts --repo <ulid>`. `resolveRepoCellPlacement` performs the same lookup
  for callers that also need the placement's id (repo_id) alongside its cell —
  used by cross-repo checkpoint reads (`explain --repo`, `explain_repo.go`) and
  by `experts --repo owner/repo`, which sends that placement id to entire-api
  instead of re-deriving it from a data-plane repo listing.
- **User-scoped `/me` → home cell, never fan out**:
  `auth.NewEntireAPICellClient(ctx, insecure, nil)` routes by the
  `home_jurisdiction` JWT claim; activity/recap use it with a data-API
  fallback (`runAuthenticatedActivityAPI` in `entireapi_client.go`).
- **Repo-set queries → fan out and merge client-side**: `cell_fanout.go` —
  `groupReposByCell` (repo index → per-cell groups; the catalog join key is
  `ClusterSlug`↔`Cluster.Slug`, NOT the cell name, which the catalog does not
  expose), `resolveCellBaseURLs`, and `fanOutCells` (parallel per-cell calls,
  per-cell timeout, partial failures isolated per slot). Merge semantics stay
  with the command. Each repo routes to exactly ONE placement — its home,
  picked by `routedRepoPlacement` (the elected `primaries.processing`, else
  the canonical row-ID convention). Mirrors are never searched: they are
  replicated copies indexed under their own namespaces, so an extra leg
  returns duplicate and stale rows, and diverges from the web
  (ENT-1672/ENT-1776). Unlike the repo-scoped resolver above, this one fails
  SOFT — a home placement that is not ready is skipped and reported
  (`reportableSkippedRepos`: pinned requests always warn, broad ones only
  when the skips left no cell to query), never substituted with a ready
  mirror.

Token rule: identity tokens are **per-jurisdiction, not per-cell**. Multi-cell
callers must build one `auth.CellClientFactory`
(`NewEntireAPICellClientFactory`) per operation — it resolves the login
subject once and mints at most one token per jurisdiction. `fanOutCells` does
this automatically; do not call `NewEntireAPICellClient` in a loop.

### Session Strategy (`cmd/entire/cli/strategy/`)

The CLI uses a manual-commit strategy for managing session data and checkpoints. There is no `Strategy` interface: `*ManualCommitStrategy` (`manual_commit.go`, constructed by `NewManualCommitStrategy()`) is the only implementation and callers hold it concretely. `strategy.go` holds the types its methods take and return.

#### Strategy Methods

`*ManualCommitStrategy`'s main entry points:

- `SaveStep()` - Save session step checkpoint (code + metadata)
- `SaveTaskStep()` - Save subagent task step checkpoint
- `ListPendingCheckpoints()` - List pending checkpoints (see the note below: there is no restore path)
- `GetSessionLog()` / `GetSessionInfo()` - Retrieve session data

**There is no restore path.** `Rewind()`, `PreviewRewind()`, `CanRewind()` and
their helpers were removed once the `rewind` commands went and nothing outside
tests still called them. Nothing in the strategy writes checkpoint contents back
over the worktree, and re-adding that is a product decision, not a refactor.

What survives is everything that reads a checkpoint without touching working
files. `ListPendingCheckpoints()` / `ListLogsOnlyPendingCheckpoints()` feed
`checkpoint list --pending`. `RestoreLogsOnly()` writes a checkpoint's session
logs into the agent's session directory — logs only, never worktree files — and
feeds `entire resume` and `entire trail resume`. The type they return is
`PendingCheckpoint`, and it covers both shapes that listing contains: a live
checkpoint on the session's shadow branch, not yet condensed onto
`entire/checkpoints/v1`, **or** a logs-only resume point — a commit on the
current branch whose logs *are* already condensed there, listed so the
transcript can be restored. "Pending" names the listing, not a promise that the
work is un-condensed. Either way you can list it and resume from it, but the
CLI cannot restore working files from it. The `--pending` flag, the command
paths, and the `--json` shape are unchanged by that rename.

#### How It Works

The manual-commit strategy (`manual_commit*.go`) does not modify the active branch - no commits are created on the working branch. Instead it:

- Creates shadow branch `entire/<HEAD-commit-hash[:7]>-<worktreeHash[:6]>` per base commit + worktree
- **Worktree-specific branches** - each git worktree gets its own shadow branch namespace, preventing conflicts
- **Supports multiple concurrent sessions** - checkpoints from different sessions in the same directory interleave on the same shadow branch
- Condenses session logs to permanent `entire/checkpoints/v1` branch on user commits
- Every path that builds a stored transcript sanitizes before redacting; the committed paths also externalize images in between, giving **sanitize → externalize images → redact**. Sanitization is the agent's optional `agent.TranscriptSanitizer` capability, applied via `agent.SanitizeTranscriptForStorage`; it strips non-portable agent state (Codex's encrypted reasoning payloads and compaction blobs, which are bound to the originating session and cannot be replayed out of a checkpoint). The three paths are the Stop/shadow write (`lifecycle.go` sanitizes before `.entire/metadata/<session>/full.jsonl` is written, then the metadata-dir walker redacts it into the shadow tree — **no externalization**, so inline images in a shadow transcript are subject to redaction and assets exist only under committed checkpoints), post-commit condensation (`prepareTranscriptForStorage` in `manual_commit_condensation.go`), and the Stop finalize full-session rewrite (`manual_commit_hooks.go`). Where all three steps run, order is load-bearing at each step: sanitizing first avoids externalizing images out of items about to be discarded (storing an asset whose referencing line disappears) and avoids redacting megabytes of ciphertext only to discard it — base64 is the pathological input for the entropy layer, so a large Codex rollout otherwise costs tens of seconds per Stop *and* per commit; externalizing before redaction is required because redaction would otherwise flag and destroy the high-entropy base64. Sanitization is idempotent, so downstream paths call it without knowing whether an upstream path already did (`checkpoint.sanitizeForAgentType` is the store's belt-and-braces call). The agent's own transcript is never modified. Coupling to respect: `SessionState.CheckpointTranscriptSize` is a growth baseline compared against the shadow transcript blob size in `sessionHasNewContent`, so it must be measured in the same sanitized (pre-externalization) coordinate — that is `CondenseResult.TranscriptSizeBaseline`; using the raw size makes the comparison false forever and the session silently stops condensing.
- **Redaction cost and the two mechanisms that contain it.** Redaction dominates the Stop hook on a large session: it is ~99.7% of the metadata-walk blob write (git object writing is milliseconds), and ~82% of that is the betterleaks regex ruleset. Two things keep it bounded, and both rest on redaction being **per-line and stateless** (`redact.redactJSONLLines`) — a rule that looked at neighbouring lines would silently break both:
  - `jsonlContentImpl` shards the line pass into ~1MiB byte-balanced groups across goroutines. Output is byte-identical to the sequential pass. Sharding is gated on an explicit `concurrencySafe` argument describing the **redactor**, not on which entry point was called: `String` is pure and opts in (including `batch.go`'s OPF-disabled fast path), while the OPF collector closures accumulate into a shared map/slice and pass `concurrencyUnsafeRedactor`. Shards are balanced by bytes rather than line count because transcripts mix short lines with occasional multi-MB tool results.
  - The checkpoint metadata walk reuses the previous checkpoint's redacted blob as a prefix and redacts only appended lines (`checkpoint/redact_cache.go`), turning a per-Stop cost of O(whole transcript) into O(appended). The stored prefix must always end immediately after a `\n`, which is what makes plain byte concatenation reproduce the full result; content with a partial trailing line is therefore never cached. Eligibility is keyed on `paths.TranscriptFileName` (`full.jsonl`), **not** a `.jsonl` suffix — `transcript.jsonl` is regenerated in full each checkpoint and `full.jsonl.001` chunks are not appended, so neither should qualify. Reuse requires the prefix bytes to still hash the same and `redactionFingerprint()` (CLI version + commit + `redact.ConfigFingerprint()`) to match, so a rewritten transcript, changed custom rules, or a CLI upgrade all fall back to a full redaction. Bump `configFingerprintVersion` in `redact/fingerprint.go` whenever the regex layers change behaviour, or stale output can be reused. The cache lives in the git common dir, resolved without caching from the explicit worktree root through `gitrepo.ResolveWorktreeMetadata`, never under `.entire/`, because anything in the metadata directory would be walked into the checkpoint tree and committed. All three whole-transcript paths are covered: the shadow write walks files through `createRedactedBlobFromFile`, while condensation and the Stop finalize rewrite hold the transcript in memory and go through `checkpoint.RedactTranscriptCached`. Those paths do **not** redact the same bytes — the shadow write stores a sanitized transcript, condensation and finalize a sanitized *and* image-externalized one — and they stay separate simply because their keys are different strings: the walk uses its real tree path, the in-memory callers a synthetic key carrying the session ID (so concurrent sessions never share an entry). There is deliberately no scope enum; sharing a key would be safe (the prefix hash rejects a mismatch) but would miss on every checkpoint. The in-memory prefix is stored as a **file** in the cache dir, not a git blob: go-git deflates the whole payload before discovering the object exists (dotgit dedups the rename, not the compression), and above `agent.MaxChunkSize` the whole-transcript blob matches no chunk the store writes, so it would linger unreachable until `git gc` pruned it and silently reverted the cache to full redaction. `redactIncrementally` owns the whole-content fallback and takes the redactor as a parameter, so prefix and suffix cannot come from different pipelines; condensation and finalize share that pipeline by both routing through `redactSessionTranscript`. A per-subagent task transcript opts out with a nil repo: it is written once per task rather than appended across checkpoints.
- Each committed session stores the (sanitized, redacted) transcript (`full.jsonl`, read by CLI resume/explain) plus a best-effort compact transcript (`transcript.jsonl`, generated via `transcript/compact`). Like `full.jsonl`, `transcript.jsonl` stores the **full compacted session** on every checkpoint (via `compact.FullWithBoundary`), so each checkpoint is self-contained and the session survives a mid-history checkpoint being lost/reverted/rebased. This checkpoint's slice begins at the session metadata's `compact_transcript_start` (a line offset in compact-output coordinates, distinct from `checkpoint_transcript_start` which indexes raw `full.jsonl` lines); a nil/absent marker means a legacy delta-only `transcript.jsonl` (read from line 0). The marker rounds toward inclusion when a streaming message straddles the boundary, so the slice never drops this checkpoint's content but may repeat ≤1 merged line at its head. Compact generation is best-effort and is skipped when the compacted output exceeds the 50MB blob cap (unlike `full.jsonl`, `transcript.jsonl` is not chunked — `full.jsonl` stays authoritative and the compact is regenerable); in the OPF finalize rewrite a failed/skipped regeneration drops the prior `transcript.jsonl` and clears the marker rather than shipping a stale, less-redacted compact. Both files are pushed with the v1 branch. The root `metadata.json` `sessions[].transcript` pointer keeps targeting `full.jsonl`; when the compact transcript was generated the session entry also carries a `compact_transcript` path pointing at `transcript.jsonl` (omitted otherwise) so external readers can locate it next to `full.jsonl`.
- **Subagent task records** - subagent work (Claude Code's Task tool) is captured as durable `session.TaskRecord` entries on session state, a pointer mid-turn (declared transcript path + labels; background launches record at launch, completions attach files/tokens/path exactly-once via `strategy.CompleteTaskRecord`, Factory Droid Workers upsert). Condensation materializes each record — declared path first, agent-layout fallback, same sanitize → externalize → redact pipeline — into `tasks/<toolUseID>/{agent-<agentID>.jsonl, task.json}` inside the parent session's checkpoint (unavailable transcript → `task.json` with a stable path-free reason; the writer redacts `task.json`'s free-text `task_description` itself, since the record carries it verbatim); live records store transcript-so-far each condensation, completed records are removed after a successful write. `State.HasTaskContent()` is the trigger currency: records-only sessions condense, records never live on the shadow branch, and shadow-branch existence does not imply task content (shadow pinning keys on `StepCount` only). `SaveTaskStep` is incremental-only (post-todo).
- Uses the `post-rewrite` Git hook to keep local session linkage aligned after amend/rebase rewrites
- Builds git trees in-memory using go-git plumbing APIs
- **Location-independent transcript resolution** - transcript paths are always computed dynamically from the current repo location (via `agent.GetSessionDir` + `agent.ResolveSessionFile`), never stored in checkpoint metadata. This ensures log restore (`RestoreLogsOnly`) works after repo relocation or across machines.
- **Token usage scoping** - `SessionState.TokenUsage` is the session-wide total used by `entire status`; `SessionState.CheckpointTokenUsage` is the pending checkpoint delta since the last condensation. Checkpoint metadata must stay scoped to `CheckpointTranscriptStart` or the pending checkpoint delta. Cursor tokens come only from stop-hook payloads, while Copilot CLI can also backfill full-session totals from `session.shutdown`. Condensation's transcript recompute runs with `subagentsDir=""` and so drops `SubagentTokens`; `withSubagentTokensFrom` refills it from the already-rescoped `state.CheckpointTokenUsage`, and the store sums it across a checkpoint's sessions via `types.AddTokenUsage` (the single token-summing primitive — do not hand-roll another; a field-by-field copy is how the nested total came to be dropped in the first place).
- Tracks session state in `.git/entire-sessions/` (shared across worktrees)
- **Commit-to-session linking is identity-first** (`strategy/session_identity.go`): identity comes from `SessionState.Owner`, the `proclive.Identity` that `captureSessionOwner` already records on every turn start (first non-transient ancestor — proclive skips shells, `entire` itself, and the Go toolchain, so a human commit typed in the same terminal never matches). Commit hooks snapshot their own ancestry once (`proclive.CurrentAncestry`) and match every candidate against it in memory (`Ancestry.Depth`) — one hostname/boot-id/proc walk per commit, not one per session state — linking the commit to the session whose agent process is an ancestor — in any worktree (nearest ancestor wins, so a nested agent beats the outer agent that spawned it, and only a tie at equal depth falls to the latest interaction; host/boot/start-time guards defeat PID reuse and cross-machine matches; Windows cannot introspect and falls back to worktree matching). The identity match is UNIONED with the worktree-matched set, never a replacement: a commit condenses every session with pending content in its worktree. Any session matched outside its home worktree is guest-linked — whether identity-matched or selected by the pre-existing single-worktree fallback — and is condensed and linked without mutating worktree-coupled state (`BaseCommit`, shadow-branch realignment) from the foreign worktree (`isSessionHomeWorktree`). Worktree matching is always computed (it is the sole mechanism for commits with no agent ancestry): imported sessions never link, and multi-worktree ambiguity is filtered to recently-interacting sessions (15 min) before declining. This deliberately turns some former ambiguity declines into a best-candidate link; `recentSessionWindow` is a correctness tradeoff because a session in a long-running build or tool call can age out and leave the other recent worktree to win. The stderr hint naming `entire session adopt` fires only from the commit-linking path, and only when identity matching could not rescue the commit either. Under `go test`, `session.NewStateStore` and `NewStateStoreForWorktree` refuse to open outside the temp root so non-isolated tests fail loudly instead of leaking fixture sessions into a real repo.
- **Reclaiming sessions whose agent vanished** - not every agent fires a session-end hook, and any agent can be killed before its hook runs, so a session can be left un-finalized forever. `SessionState.Owner` — the same fingerprint commit linking matches above — is captured at every turn start by `captureSessionOwner`, and `State.OwnerExited()` reports it gone via `proclive.Check`. `finalizeExitedSessions` sweeps those inside `entire status` (text and `--json`) and `entire doctor`, ending them exactly as a clean stop would. **`OwnerExited` deliberately covers IDLE as well as ACTIVE** — an agent that finishes its last turn and then quits leaves IDLE, so gating on ACTIVE alone missed the common case; only already-finalized sessions are excluded, per the shared `State.IsEnded()` predicate. Liveness is Unknown on Windows and for cross-host state, where behaviour degrades to the `StuckActiveThreshold` timeout. Because the sweep runs inside interactive commands, its eager condensing is capped by `sweepCondenseBudget` across the whole sweep: every candidate is always marked ENDED (a single atomic rename — that is what un-sticks it from `entire status`), while condensing runs only while the budget lasts, so a multi-day backlog drains over successive invocations instead of stalling one. Skipping a condense is the existing fail-open path — PostCommit retries, and `doctor` reports the session as "ended with uncondensed checkpoint data".
- **Session-end hooks that run under a host deadline** - an agent whose session-end hook fires inside its own shutdown may be killed part-way through. Such agents declare `agent.SessionEndBudgeter` (Codex: 3s cap, process tree killed on expiry). The budget bounds **only** the eager condense in `endSessionNow`, so the cheap step that un-sticks the session is never the one given up on; it is set below the host cap with enough headroom for the shell wrapper and binary load, which happen before Entire's own clock starts. Unbounded is not the same as guaranteed, though: mark-ended runs under `MutateSessionState`, whose flock acquire blocks, so a concurrent condense holding the lock can push it past the cap and get the tree killed — the exited-owner sweep above is the backstop. The bound is best-effort (condensation doesn't poll ctx between stages), and being cut off costs duplication rather than data: an incomplete condense leaves `FullyCondensed` false and PostCommit retries, except in the window between the v1 checkpoint write and the state save, where the checkpoint is already committed and PostCommit mints a second one over the same transcript range.
- **Shadow branch migration** - if user does stash/pull/rebase (HEAD changes without commit), shadow branch is automatically moved to new base commit
- **Orphaned branch cleanup** - if a shadow branch exists without a corresponding session state file, it is automatically reset when a new session starts
- PrePush hook can push `entire/checkpoints/v1` branch alongside user pushes
- **Single checkpoint sync remote** - checkpoint data syncs only to the elected checkpoint sync remote (`strategy_options.checkpoint_push_remote` setting → captured election → `origin` → sole remote → first remote in `.git/config` order; fail-closed only when the setting names a missing remote — the captured tier is fail-soft), on both the git-branch and git-refs backends. **Capture**: during pre-push, a push whose target agrees with the branch's declared push destination (`branch.<name>.pushRemote` → `remote.pushDefault` → `branch.<name>.remote`) and differs from the current election persists that remote as the captured election (git common dir, `entire-checkpoint-sync-remotes.json`), announces it on stderr, and the same push carries the checkpoints — see `strategy/checkpoint_sync_capture.go`. Declaration alone is deliberately **not** a tier: electing from tracking config at rest silently drops checkpoint sync on every push to a different remote (`git push <other> HEAD`, a `git clone -o base` pushing checkpoints to a separately added `origin`, any repo with `remote.pushDefault` — the `74e239a9` regression), and a bare push to an undeclared remote never elects (the pre-single-remote transcript leak). Phase-1 capture rules: at most one captured remote, first still-configured capture sticks (no per-push ping-pong in mixed-habit repos; a captured remote that was renamed or removed no longer vetoes the next capture), explicit `checkpoint_push_remote` outranks and disables capture. Pushes to any other remote or to a raw URL never carry checkpoint data; the dedicated `checkpoint_remote` URL mode is exempt. `entire status` shows the sync destination, its source (`observed` renders as "follows your branch's push destination"), and an unpushed-checkpoint count.
- **Checkpoint reads follow the election** - reads consult an ordered candidate chain: the elected sync remote first, then `origin` as a **read-only legacy tier** (pre-single-remote checkpoints live there; `strategy.CheckpointReadRemotes` / `CheckpointReadRemotesWithElection` resolve the chain, failing OPEN to `[origin]` on an election error). Metadata-branch fetches refresh every candidate tracking ref because branch existence does not prove a requested checkpoint exists there; other fetch targets, tracking-ref readers, blob hydration, and checkpoint-policy reads try candidates in order. Git-refs remote discovery ls-remotes every candidate **without** needing a dedicated `checkpoint_remote` and merges the listings. Local-ref writers stay **elected-remote-only**: `EnsurePrimaryRef`, the metadata-fetch advance, `promoteRemoteTrackingPrimary`, and the local checkpoint-policy ref update never act on the legacy tier (a stale origin feeding `SafelyAdvanceLocalRef` is the #1374-class hazard), keyed on the explicit election result, never on the chain's first entry. A remoteless repo keeps its "checkpoint absent" classification, which requires positive evidence (successful empty `git remote` listing, live ctx, readable settings without a `checkpoint_remote` key).
- **Checkpoint egress trust gate** - `settings.CheckpointEgressAllowed(ctx)` is the single predicate for whether checkpoint data may leave the machine: while the user-global tier is off or unconfigured, an active repo syncs as before; once `global.enabled` is true every active repo — repo-enabled or globally tracked — needs `global.trust_all`, a trusted key for the **elected checkpoint sync remote** (`trusted_origins` holds that remote's normalized `host/owner/repo` URLs — origin only by default; a `checkpoint_push_remote`, captured election, or pending capture on the current push re-keys it, so a re-election re-asks), or a trusted path when that remote has no normalizable URL. `repopolicy.ResolveSyncRemote` is the seam: strategy installs the election into it at init (`strategy/trust_sync_remote.go`), the same way settings installs `LocalSettingsTrusted`. `entire enable` records that trust itself, the three-option pre-push prompt ("Yes — trust this repo / Not now — re-ask next push / Always — trust every repo (trust_all)") records it interactively, and `entire trust [--revoke] [--remote <name>]` manages it (`--remote` records consent for a remote a held push is about to capture — the election persists only on delivery, so plain `trust` would key the still-elected remote); errors fail closed. Both egress entry points are gated — `ManualCommitStrategy.prePush` (early return beside the single-remote gate, above both backend branches, so held repos never run OPF) and `PushQueuedCheckpointRefs`. A hold never blocks the user's own push: the branch pushes, checkpoint data stays local (refs-backend refs stay queued) with one stderr explanation, and the first trusted push drains the backlog. Activation, runtime layout, user-level hooks, and lazy setup are documented in `docs/architecture/global-tracking.md`.
- **Scanner engine selection is a settings-only, fail-closed choice**: `redaction.betterleaks.enabled` (default `true`) and `redaction.goredact.enabled` (default `false`) pick which pattern-matching engine(s) feed layer 2 of `detectAllLayers` (`redact.ConfigureScanners`, `redact/scanners.go`). Both keys are honored from committed `.entire/settings.json` only — a `settings.local.json` copy is ignored with a logged warning, because the choice affects everyone who reads the repo's checkpoints, not just the developer who set it. `validateScannerSettings` fails settings load with `settings.ErrScannerConfig` when both engines are disabled. If goredact is the sole enabled engine and its scan fails at runtime, `redact.ErrScannerDegraded` propagates out of `JSONLBytes`/`JSONLBytesWithPrivacyFilter`; every checkpoint-write call site (`checkpoint/ephemeral.go`, `checkpoint/persistent.go`, `strategy/manual_commit_hooks.go`) must `errors.Is` for it and fail the write rather than fall back to under-scanned content. See `docs/security-and-privacy.md` for user-facing details.
- **OPF (OpenAI Privacy Filter) runs at pre-push, not post-commit**: when `redaction.openai_privacy_filter.enabled` is true, the PrePush hook re-redacts unpushed commits with the OPF 9th layer, builds new commits carrying an `Entire-OPF-Applied: true` trailer, and updates the local refs before pushing. Per-commit condensation stays on the fast 8-layer pipeline. **Both backends are covered, by two rewrites sharing one policy**: `manual_commit_opf_rewrite.go` walks unpushed `entire/checkpoints/v1` commits bounded by the remote tip and CAS-updates the branch; `manual_commit_opf_refs.go` walks the push queue and, per queued ref, rewrites every unpushed commit — stopping at the first commit already carrying the trailer, which makes the trailer its own watermark (steady state stops at the tip's parent) and is bounded by the shared `BootstrapTooLargeError` cap for repos that enable OPF late. Blob policy, byte caps, the single batched shell-out, and the error taxonomy are shared; only discovery and ref update differ. **Fail-closed differs by backend on purpose**: git-branch aborts the user's push, while git-refs withholds the (separately-pushed) checkpoint refs and leaves them queued, so nothing under-redacted ships without blocking the user's own push. See `docs/security-and-privacy.md` for the full flow, including divergence detection, bootstrap caps, and CAS-on-conflict semantics.
- **OPF's `command` is a trust boundary, not a setting**: `redaction.openai_privacy_filter.command` becomes `argv[0]` of an exec during pre-push, and `.entire/settings.json` is version-controlled — so reading it from the project file would let a pull request execute code on every developer who pushes (the prompt is no defense: it never names the command, `prompt_default: "always"` skips it, and non-TTY pushes auto-run). `settings.enforceOPFCommandTrust` (`settings/opf_command_trust.go`) honors `command` only from `.entire/settings.local.json`, and only when that file is untracked in **both** the index and `HEAD` — the filename is not the check, because `.gitignore` does not apply to an already-tracked path. The probe goes through go-git (`gitrepo.OpenPath`), not the git CLI, and is memoized per process: shelling out cost ~15 subprocesses per hook (`settings.Load` is uncached and runs several times per hook) and would fail verification wherever `git` is off `$PATH` — the GUI-git-client population that most needs an explicit `command`. **Two depths**: the layer check reads the index only, because a PR-delivered file is always in the index of a clone that checks it out, and checkout cannot produce a file absent from the index; the `command` check also reads HEAD. That split matters because HEAD is the expensive half — measured 8.3ms → 2.5ms per hook process in this repo, and 39ms → 11ms on reftable, where `gitrepo` routes reference reads back through the git CLI. The cost falls on everyone with a `settings.local.json`, not just OPF users, so keep it off the HEAD path. Rejection is a downgrade to the documented `$PATH` default plus a warning, never a hard error; verification failure (no repo, git missing) counts as untrusted. **Versioned means more than the file's own path**: an index or HEAD entry *named* `.entire` (a gitlink/submodule, or a blob standing where the directory should be) and an `.entire/.git` on disk all count as tracked — a submodule mounted at `.entire` is a real directory that defeats the symlink probe while `git clone --recurse-submodules` delivers its contents. **Location is a second gate**: a `command` that resolves inside the worktree (relative with a separator, or absolute under the root) is rejected regardless of ownership, because `exec.ErrDot` only guards `$PATH` lookups and a repo-delivered binary is exactly what the gate exists to block. Do not add other exec-bearing fields to the project settings file without the same gate.
- **`external_agents` is the second exec-bearing setting, gated the same way**:
  it enables the `$PATH` scan that globs for `entire-agent-*` binaries and runs
  each one's `info` subcommand, so a committed `{"external_agents": true}` would
  let a pull request turn on execution of whatever binary it could get onto a
  developer's `$PATH` — the `enforceOPFCommandTrust` rationale verbatim, minus
  even a prompt. `settings.enforceExternalAgentsTrust`
  (`settings/external_agents_trust.go`) honors it only from a
  `classifyLocalSettingsDeep`-verified `.entire/settings.local.json`, reusing
  the OPF gate's helpers; only a `true` value is gated, since `false` grants
  nothing. Rejection is a downgrade recorded on
  `EntireSettings.ExternalAgentsRejection()`, surfaced by `entire status` and by
  `external.DiscoverAndRegister` — without it, a refused grant and a setting the
  user never wrote look identical (no agent appears). **Consequence for every
  write site**: the auto-enable that fires when a user picks an external agent
  must go through `enableExternalAgentsLocally` (`setup_external_agents.go`),
  which raw-writes the single key to the local file whatever `--local`/`--project`
  said about the rest — writing it into the project file produces a setting the
  user can read back and that never takes effect.
- **A tracked `.entire/settings.local.json` is ignored wholesale**: the local layer's premise is that it is per-clone and per-developer (it is gitignored, `entire enable --local` writes it, and `CheckpointRemoteIsLocalOnly` treats presence there as proof the developer chose it). `.gitignore` does not apply to an already-tracked path, so a committed one arrives by cloning and would override project settings for everyone. `loadMergedSettings` drops the layer when the file is **proven** tracked, records `EntireSettings.LocalLayerRejection()`, and the redaction consumer prints it with the `git rm --cached` fix. It never errors — one committed file must not brick `status`/`doctor`. Two deliberately opposite failure directions, expressed as the three-state `localTrust` (`localUnverifiable` is the zero value so a forgotten assignment fails safe): an *unverifiable* repo keeps the layer (losing all local settings is worse than the risk) but still drops the exec-bearing settings, OPF `command` and `external_agents` (being wrong there means running someone else's binary); *no* repository counts as proof of locality. `CheckpointRemoteIsLocalOnly` reads the raw file outside the loader, so it repeats the check itself.
- Safe to use on main/master since it never modifies commit history

#### Key Files

- `strategy.go` - Shared types only, no behaviour: the sentinel errors (`ErrNoMetadata`, `ErrNoSession`, `ErrNotTaskCheckpoint`, `ErrEmptyRepository`), the argument and result structs (`SessionInfo`, `PendingCheckpoint`, `StepContext`, `TaskStepContext`, `TaskCheckpoint`, `SubagentCheckpoint`, `RestoredSession`), and `TaskMetadataDir()`. The strategy type itself and its constructor live in `manual_commit.go`.
- `common.go` - Helpers for metadata extraction, tree building, `ListCheckpoints()`
- `manual_commit*.go` - Manual-commit strategy: main impl, types, session state, condensation, pending-checkpoint listing + log restore (`manual_commit_pending.go`), git ops, logs, hook handlers (prepare-commit-msg, post-commit, post-rewrite, pre-push), reset
- `manual_commit_opf_rewrite.go` - Pre-push OPF re-redaction: walks unpushed v1 commits, runs OPF over their blobs, rebuilds commits with `Entire-OPF-Applied: true` trailer, CAS-updates the local ref. Sentinel error types (use `errors.As`): `V1DivergedError`, `BootstrapTooLargeError`, `V1RefMovedError`, `OPFRuntimeFailedError`, `OPFBatchTooLargeError`, `OPFRawBytesTooLargeError`, `OPFNoCategoriesError` (OPF enabled with zero effective categories — the pre-push decision and the rewrite both fail closed instead of stamping the trailer without a scan).
- `manual_commit_opf_refs.go` - The git-refs half of pre-push OPF: `RewriteQueuedCheckpointRefsWithOPF` walks the push queue and rewrites every unpushed commit on each queued ref, reusing the v1 rewrite's blob walk, caps, and error types. See the OPF bullet above for why the two backends fail closed differently.
- `settings/opf_command_trust.go` - Ownership gate for the executed OPF `command` (local-only + untracked); see the OPF trust-boundary bullet above
- `cleanup.go` - Cleanup discovery/deletion for shadow branches, session states, and checkpoint metadata
- `session_state.go` - Package-level session state functions
- `hooks.go` - Git hook installation

Note: `checkpoint/configloader.go` overrides go-git's default config loader with a symlink-following `billy.Basic` (`osSymlinkFS`) — go-git's default reads config via `os.Root`, which rejects absolute symlinks in any path component (e.g. a `~/.config` managed by a dotfile tool), silently dropping global config so author identity fell back to "Unknown" and signing was skipped.

#### Deep-Dive Reference

The phase state machine, metadata directory layout, sharded checkpoint format, multi-session metadata, checkpoint ID linking, commit trailers, and concurrent-session / shadow-branch-migration behavior are documented in:

- [Sessions and Checkpoints](docs/architecture/sessions-and-checkpoints.md) - domain model, storage layout, checkpoint ID linking, commit trailers, package structure
- [Checkpoint Scenarios](docs/architecture/checkpoint-scenarios.md) - phase state machine and worked condensation scenarios
- [Ref-Based Checkpoint Backend](docs/architecture/ref-checkpoint-backend.md) - git-refs backend: primary/mirror taxonomy, ref layout + sharding, push-discovery queue, read routing, config + rollout

#### When Modifying the Strategy

- Adding a method to `*ManualCommitStrategy` is enough — there is no interface to satisfy, and no second implementation to keep in step
- Test with `mise run test` - strategy tests are in `*_test.go` files
- Keep this file and `docs/architecture/sessions-and-checkpoints.md` current when changing strategy behavior (`AGENTS.md` is a symlink to this file)

### `entire review` Command

`entire review` runs a configured review profile. Keep documentation brief and user-facing.

See [Review Command](docs/architecture/review-command.md) for usage, minimal profile config, and key files.

### Agent-Safe CLI Fallbacks

When building CLI features, do not make useful output available only through a
TUI, picker, wizard, terminal selection menu, confirmation dialog, or stdin
question. Agents must be able to complete the same read-only workflow from a
non-interactive terminal.

Plain text output is acceptable when it contains the full information needed for
the workflow. JSON is preferred for structured data, following existing patterns
such as `--json` on `status`, `agent-help`, `sessions`, `search`, and trail
finding commands. Long human-readable output may use a pager in TTY mode, but
must provide a bypass like the existing `--no-pager` pattern on `explain`.

For interactive browsing flows, provide one of these non-interactive shapes:

- a list command that prints stable identifiers, plus a show/detail command that
  accepts an identifier
- a flag or positional argument that selects the item directly
- a complete text or JSON fallback when stdout is not a terminal, like existing
  static/text fallbacks for TUI-backed commands

When reviewing CLI changes, inspect terminal-gated paths such as
`IsTerminalWriter`, `CanPromptInteractively`, Bubble Tea, `huh`, direct stdin
reads, terminal selection menus, confirmation dialogs, and wizard flows. Flag
the change if a non-interactive agent can only see a menu, preview, truncated
summary, or cannot select the item whose details matter.

Tests for interactive CLI features should cover the non-interactive path. See
the "Spawning subprocesses in tests (TTY detection)" section above for the
`execx.NonInteractive` pattern when testing a real `entire` command.

Existing good patterns:

- `entire investigate --findings` prints a complete plain-text list and includes
  `view: entire investigate show <run-id>` hints.
- `entire investigate show <run-id>` prints the saved investigation summary and
  findings without needing a TUI.
- `entire repo clone /gh/...` prompts only when several clusters are possible;
  without a TTY it asks for `--cluster`.
- `entire experts --tui` is safe because the TUI is opt-in and non-TTY output
  falls back to deterministic plain text.
- `entire explain --no-pager` is the local pattern for avoiding pager-only long
  text output.
- `entire status --json`, `entire agent-help --json`, `entire sessions list --json`,
  and trail finding commands show the local `--json` convention.

Do not require JSON everywhere. Human-readable text is fine if it contains the
complete information an agent needs. The failure mode is requiring an
interactive terminal to select something or reveal details.

# Important Notes

- **Before committing:** Follow the "Before Every Commit (REQUIRED)" checklist above - CI will fail without it
- Integration tests: run `mise run test:integration` when changing integration test code
- When adding new features, ensure they are well-tested and documented.
- Always check for code duplication and refactor as needed.

## Go Code Style

- Write lint-compliant Go code on the first attempt. Before outputting Go code, mentally verify it passes `golangci-lint` (or your specific linter).
- Follow standard Go idioms: proper error handling, no unused variables/imports, correct formatting (gofmt), meaningful names.
- Handle all errors explicitly—don't leave them unchecked.
- Reference `.golangci.yml` for enabled linters before writing Go code.

## Accessibility

The CLI supports an accessibility mode for users who rely on screen readers. This mode uses simpler text prompts instead of interactive TUI elements.

### Environment Variable

- `ACCESSIBLE=1` (or any non-empty value) enables accessibility mode
- Users can set this in their shell profile (`.bashrc`, `.zshrc`) for persistent use

### Implementation Guidelines

When adding new interactive forms or prompts using `huh`:

**In the `cli` package:**
Use `NewAccessibleForm()` instead of `huh.NewForm()`:

```go
// Good - respects ACCESSIBLE env var
form := NewAccessibleForm(
    huh.NewGroup(
        huh.NewSelect[string]().
            Title("Choose an option").
            Options(...).
            Value(&choice),
    ),
)

// Bad - ignores accessibility setting
form := huh.NewForm(...)
```

**In the `strategy` package:**
Use the `isAccessibleMode()` helper. Note that `WithAccessible()` is only available on forms, not individual fields, so wrap confirmations in a form:

```go
form := huh.NewForm(
    huh.NewGroup(
        huh.NewConfirm().
            Title("Confirm action?").
            Value(&confirmed),
    ),
)
if isAccessibleMode() {
    form = form.WithAccessible(true)
}
if err := form.Run(); err != nil { ... }
```

### Key Points

- Always use the accessibility helpers for any `huh` forms/prompts
- Test new interactive features with `ACCESSIBLE=1` to ensure they work
- The accessible mode is documented in `--help` output
