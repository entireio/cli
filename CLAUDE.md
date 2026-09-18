# Entire CLI — repository instructions

Go CLI built with Cobra and Charmbracelet Huh. `AGENTS.md` is a symlink to this
file. Keep this entry point short: repo-wide rules and pointers, not command
catalogs, implementation histories, or subsystem specifications.

## Project map

- `cmd/entire/`: entry point and external-command dispatch.
- `cmd/entire/cli/`: Cobra commands, group roots, and CLI helpers. Group/verb files
  use `<noun>_group.go` and `<noun>_<verb>.go`.
- `cmd/entire/cli/agent/`: built-in agent integrations and external-agent protocol.
- `cmd/entire/cli/strategy/`: manual-commit strategy and lifecycle/git hooks.
- `cmd/entire/cli/checkpoint/`: ephemeral and persistent checkpoint storage.
- `cmd/entire/cli/session/`: session state shared across worktrees.
- `cmd/entire/cli/integration_test/`: simulated-hook integration tests.
- `e2e/`: real-agent tests and deterministic Vogon canary.
- `internal/entireclient/`: API clients, authentication, and user directories.

Use the Go version pinned by `go.mod` and tools configured in `mise.toml`.
For installed CLI usage, run `entire agent-help`, then
`entire agent-help <command>` for current flags; do not guess from a static list.

## Read when relevant

Read the applicable reference **before changing that area**, not every reference
at session start. Follow its related links when the task crosses those boundaries.

| Area being changed | Reference |
| --- | --- |
| Commands, help, experimental visibility, repo refs, prompts | [CLI conventions](docs/development/cli-conventions.md) |
| Test harnesses, isolation, TTY behavior, source guards | [Testing](docs/development/testing.md) |
| Filesystem I/O, `.entire`, symlinks, hook configs, root anchors | [Filesystem safety](docs/development/filesystem-safety.md) |
| Git operations, executable lookup, subprocesses, Windows launching | [Git and subprocess safety](docs/development/git-safety.md) |
| Caller identification or session current/tokens/adopt | [Caller-session resolution](docs/development/caller-session-resolution.md) |
| Control-plane auth or data-plane routing | [API routing](docs/development/api-routing.md) |
| Checkpoint writes, lifecycle, sync, settings trust, redaction | [Implementation contracts](docs/development/checkpoint-implementation.md), [domain model](docs/architecture/sessions-and-checkpoints.md), [scenarios](docs/architecture/checkpoint-scenarios.md) |
| Ref-based checkpoint backend | [Ref backend](docs/architecture/ref-checkpoint-backend.md) |
| Agent integrations | [Agent guide](docs/architecture/agent-guide.md), [integration checklist](docs/architecture/agent-integration-checklist.md) |
| External commands or agents | [External commands](docs/architecture/external-commands.md), [agent protocol](docs/architecture/external-agent-protocol.md) |
| Security, privacy, scanners, OPF | [Security and privacy](docs/security-and-privacy.md), [implementation contracts](docs/development/checkpoint-implementation.md) |
| Review command | [Review architecture](docs/architecture/review-command.md) |
| E2E tests | [E2E guide](e2e/README.md) |

## Verification

| Task | Command / requirement |
| --- | --- |
| Unit tests | `mise run test` |
| Integration tests | `mise run test:integration`; required when changing integration test code |
| Deterministic E2E canary | `mise run test:e2e:canary`; safe without API calls |
| All CI tests | `mise run test:ci` (unit + integration + canary) |
| Format and lint | `mise run fmt && mise run lint` |
| Before every commit | **`mise run check`** (format, lint, all CI tests) |
| Before any push or remote code update | **`mise run lint` on the current tree**, after the latest formatting pass |
| Duplication checks | `mise run dup` or `mise run dup:staged`; normal lint also checks duplication |
| Windows installer | `mise run test:ps1`; see testing reference for prerequisites |

**Do not run real-agent E2E tests unless the user explicitly requests them.** They
make paid API calls. Vogon is the exception: its canary is deterministic and free.
When changing E2E prompt wording, run the canary and update Vogon's regex parsing
if needed. Formatting can rewrite files; an earlier lint pass is not sufficient.

Before implementing Go code, search nearby packages and helpers for reusable
patterns. Use `/go:discover-related` when available, otherwise `rg` and related
implementations/tests. Check `.golangci.yaml`; handle errors and avoid duplication.

## Test safety

- Every top-level test and subtest calls `t.Parallel()` unless it changes
  process-global state (`t.Chdir`, `t.Setenv`, or their `os` equivalents).
- Git tests use isolated temporary repositories, preferably
  `testutil.InitRepo(t, dir)` plus its file/add/commit helpers. Redirect CWD before
  calling CWD-based handlers. Never mutate the real checkout's git/session state.
- Tests must not access the developer's real config, cache, or keychain.
  In-process fallbacks are shared per process; use per-test overrides and
  `tokenstore.UseFileBackendForTesting` when isolation between tests is needed.
  Tests that can reach the OS keyring need `keyring.MockInit()` in `TestMain`.
- Spawned binaries do not inherit Go's test detection. Harnesses must isolate
  `ENTIRE_CONFIG_DIR`, `XDG_CACHE_HOME`, `ENTIRE_TOKEN_STORE=file`,
  `ENTIRE_TOKEN_STORE_PATH`, and `ENTIRE_TEST_AUTH_STORE_FILE`.
- Prefer `execx.NonInteractive` for real CLI/git test subprocesses so a developer's
  terminal cannot make them prompt or hang.
- Caller-resolution tests clear every variable from `agent.CallerSessionEnvVars()`;
  never hand-maintain a partial list or inherit the developer's agent identity.
- Source-scanning guards use `testutil.GitGrepGuard`, restrict pathspecs to Go
  files, and fail on zero matches. Do not weaken guard ledgers to silence failures.

## Filesystem and settings safety

**Use the existing anchor for each tree; never assemble a path and perform bare
I/O beneath it.** Read the filesystem reference before adding or changing I/O.

| Tree | Canonical owner |
| --- | --- |
| `.entire` | `entiredir` |
| Git common directory | `gitdir` |
| Working tree | `worktreedir` |
| Agent hook configuration | `agent.HookConfigFile` |
| Agent session store | `agent.SessionStore` |
| Active git hooks directory | `strategy.hooksRootForInstall` / `ForRemoval` |
| Per-user config/cache | `userdirs.ConfigRoot` / `CacheRoot` |
| Managed plugins | `pluginRoot` |

- Root bases come from trusted resolvers, **not `filepath.Dir(target)`**. Confining
  a file to its own derived parent proves nothing. Prefer an existing anchor.
- Roots share the `osroot.Shared` registry. Open child roots with
  `osroot.SharedChild` / `OpenChild`, not bare `parent.OpenRoot`.
- Use `osroot` operations and `jsonutil.WriteFileAtomicIn` / `CreateTempIn`.
  Directories use `MkdirAllNoSymlink`; rooted read-only operations must also
  enforce symlink policy. Atomic writes replace leaf links rather than follow them.
- `.entire` must be a real directory; validate through
  `paths.ValidateEntireDirAt` / `RequireEntireDir`. Do not treat an unresolved
  repository as absent. Settings reads must also refuse symlinks themselves.
- Do not broaden symlink exceptions. Developer-vouched agent config directories
  are scoped, locally trusted exceptions; `.entire` and git hooks are not vouchable.
- `entiredir.OpenForRead` must not create directories. Reset cached roots before
  deleting/recreating their directories. Use `entiredir.Name` / `MustName` to
  bridge repo-relative constants and root-relative I/O names.
- All settings access goes through `settings`; never read or parse settings JSON
  ad hoc. Per-user config/cache resolution belongs only to `userdirs`.
- Repository-controlled settings must not authorize executable discovery, custom
  executables, or instructions for permission-bypassed agents. Preserve the
  existing local-provenance gates and their rejection reporting.
- Existing exceptions (explicit user paths, global git config, transcript-read
  protocol gaps) are documented in the references; do not generalize them.

## Git and subprocess safety

- Open repositories only through `gitrepo.OpenCurrent(ctx)` or
  `gitrepo.OpenPath(root)`, never direct go-git opens. This preserves alternates
  and reftable support. Do not fall back to CWD after failed repo resolution.
- Use `gitrepo.Status`, never `worktree.Status()`. Agent-hook capture paths use
  `StatusWithBudget` and preserve degraded-capture reporting on budget exhaustion.
- Every `git status` subprocess passes **`--no-optional-locks`**. This does not
  protect worktree-comparing `git diff` from refreshing the index; use the
  hook-safe alternatives in the git reference.
- Use the git CLI, not go-git v5, for checkout and hard reset; go-git can delete
  ignored/untracked directories. This is an implementation rule, not permission
  to run destructive git commands.
- Git-relative paths resolve against `paths.WorktreeRoot`, never process CWD.
- A git subprocess targeting a repository independently of the current hook must
  use `gitrepo.EnvWithoutRepoOverrides()`; `Dir` or `-C` alone does not override
  inherited selectors. Queries inspecting the commit Git is preparing must retain
  the hook environment, including its temporary `GIT_INDEX_FILE`. User-invoked
  CWD commands also honor intentionally exported repo selectors.
- PATH scanners use `execx.PathScanDirs()`; external-agent execution requires
  absolute binary paths. User-directory overrides use the checked `userdirs`
  resolvers before any I/O. Preserve the documented developer-owned OPF exception.
- When Entire owns execution, pass arguments separately; never interpolate
  dynamic values into `cmd.exe`. Agent-owned shell execution uses the shared
  escaping helper. See the reference for the literal-only Unix updater exception.

## CLI behavior and output

- Load settings through the `settings` package, not new helpers in CLI consumers.
- Operational/debug messages use `logging.Debug/Info/Warn/Error`; user output uses
  `cmd.OutOrStdout()` / `cmd.ErrOrStderr()`. Never log prompts, file contents, or
  commit messages; log operational metadata only.
- Cobra suppresses errors globally; `main.go` prints returned errors. Return
  `NewSilentError(err)` only after printing a custom error yourself.
- Every read-only workflow must work without a TUI: provide complete text/JSON,
  stable IDs and detail commands, or direct selection flags. Test that path.
- Use `uiform.New` (or `NewAccessibleForm` in `cli`) for Huh forms; these wire
  accessibility and theming centrally. Do not hand-roll `WithAccessible`.
- Separately opened prompt terminals use `interactive.OpenPromptTTY()` and
  `PromptTTY.Close()`, not a bare file close (Windows pending reads can hang).
- Register experimental commands with `experimental.Register`. Classify new
  commands in `agentHelpClassification`; default to unlisted/user-owned and
  flag the product choice for review. Agent guidance belongs in `agentHelpGuidance`,
  not Cobra `Short`/`Long` or first-turn injection.

## Session, checkpoint, and API contracts

- `*strategy.ManualCommitStrategy` is the only strategy; there is no interface or
  worktree restore path. Checkpoints use shadow/metadata refs, not working-branch
  commits. Log resume is distinct from restoring worktree files.
- Caller identity comes from `strategy.ResolveCallerSession`, not newest state.
  Preserve resolution provenance; do not narrate worktree fallback or ambiguous
  matches as identified callers. `IsCaller()` excludes those guesses; consult
  the caller-resolution reference for current enforcement gaps.
- Preserve sanitize → image externalization → redact ordering where all apply,
  fail-closed scanner behavior, and checkpoint-scoped token accounting. Read the
  implementation contracts before modifying any transcript/storage pipeline.
- Control-plane precedence belongs to `coreapi`; display `client.CoreOrigin()`
  rather than independently resolving a possibly different target.
- Repo-scoped data-plane requests resolve the repo's cell and fail on resolution
  failure. `/me` uses the home cell; repo-set queries use shared fanout helpers.
  Multi-cell operations share one `auth.CellClientFactory` per operation.

## Maintaining these instructions

Update the relevant reference when subsystem behavior changes. Update this file
only for repository-wide development rules or routing links. Preserve safety
contracts in references and regression tests rather than accumulating incident
narratives here. Prefer existing docs over a second account of the same behavior.

The root-file budget is **20 KiB**, enforced alongside local documentation links
by `go test ./docs/development`. Extract specialized material instead of expanding
that budget. Do not require every linked document
to be loaded on every task. Keep `AGENTS.md` as the symlink to this file.
