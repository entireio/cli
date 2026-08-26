# External Agent Discovery Refactor — Implementation Plan

Status: agreed. All open decisions resolved — see [Decisions](#decisions).
Scope: `cmd/entire/cli/agent/external/discovery.go`, `cmd/entire/cli/agent/registry.go`,
`cmd/entire/cli/agent_group.go`, and their tests.

Read this whole document before writing code. Tests first, implementation last.

## Why

`discoverAndRegister` — the shared body behind `DiscoverAndRegister` and
`DiscoverAndRegisterAlways` — has two problems.

**It is sequential, with a budget that does not hold.** Binaries are exec'd one
at a time inside a single 10s budget checked only *between* binaries. A hung
binary is bounded instead by `external.defaultRunTimeout` (30s), so a scan can
overrun its own cap by ~30s, and every later binary is dropped as "timed out".
Both hook call sites (`hooks_cmd.go:48`, `hooks_git_cmd.go:177`) wrap the scan
in a 5s context that one slow plugin blows straight through.

**Failures vanish.** `discoverAndRegister` returns nothing. Bad JSON, protocol
mismatch, non-zero exit, missing exec bit, timeout — every one is
`logging.Debug`'d and then lost. A plugin that is present but unloadable is
indistinguishable from a plugin that was never installed.

Both matter because the `Always` variant runs on interactive, user-waiting
paths, up to four times per process (see [Memoization](#memoization)).

## Target design

Three phases, in order. Only the middle one touches a subprocess.

```
collect (filesystem only)  →  probe (parallel, one budget each)  →  register (deterministic)
```

### Registry entries carry either a factory or an error

State lives in the **agent registry**, not in a second registry inside
`external`. In `cmd/entire/cli/agent/registry.go`, replacing
`map[types.AgentName]Factory`:

```go
type entry struct {
    factory Factory // nil when the external binary could not be loaded
    binary  string  // external binaries only; "" for built-ins
    err     error   // non-nil when unusable
}

var registry = make(map[types.AgentName]entry)
```

Sharing the name-keyed map is only safe because [a built-in always wins a
collision](#decisions): a failure entry can therefore never displace a working
built-in.

- `Register(name, factory)` — unchanged signature, stores `entry{factory}`.
  Built-in `init()` registrations are untouched.
- `RegisterExternal(name, binary, factory)` and
  `RegisterExternalFailure(name, binary, err)`.
- `Get(name)` — absent entry gives today's `unknown agent` error; an entry with
  `err != nil` returns that error, wrapped as
  `external agent %q (%s) is not usable: %w`. Never calls a nil factory.
- `List`, `StringList`, `DetectAll`, `GetByAgentType`, `LauncherFor`, `Default`
  — skip `factory == nil`, so they keep meaning "agents you can actually use".
- `ExternalFailures()` — sorted snapshot for listing. Return a copy; never hand
  out the live map.
- `ProbedExternalBinaries()` — the memo source, built from entries with a
  non-empty `binary`.
- `ResetExternalsForTesting()` — drops every external entry.

### Error taxonomy

`cmd.Run` reports only `signal: killed` when the context kills the child, so a
timeout is not otherwise distinguishable from a crash. Export two sentinels so
callers classify without string-matching:

- `ErrInfoTimeout` — the `info` call exceeded its budget. Join it with the
  context error so `errors.Is(err, context.DeadlineExceeded)` also holds.
- `ErrNotExecutable` — a matching regular file without the exec bit (Unix).

Anything else stays the raw wrapped exec/protocol error (bad JSON, protocol
version mismatch, non-zero exit, stat failure). This is what makes a too-tight
budget diagnosable instead of looking like a broken plugin.

### Collect phase (cheap, no exec)

1. `os.Getenv("PATH")`; empty → return.
2. For each dir in `filepath.SplitList` order:
   `filepath.Glob(dir + "/entire-agent-*")`. Glob error → skip that dir.
3. Per match: `StripExeExt`, derive the agent name, and **dedupe on the derived
   agent name, not the binary basename**. Today's basename dedupe lets
   `entire-agent-foo.exe` and `entire-agent-foo.com` in one directory both
   derive agent `foo`, so the second silently overwrites the first while
   `PATHEXT` picks whichever it likes. First `$PATH` dir wins, matching `$PATH`
   semantics.
4. Name collision with an already-registered agent → skip **without executing
   the binary**, log at `Warn`, and record nothing. See
   [decision 2](#2-a-built-in-always-wins-a-name-collision).
5. Skip binaries already in `ProbedExternalBinaries()`. See
   [Memoization](#memoization).
6. `os.Stat`: directory → record a failure entry; regular file without the exec
   bit on non-Windows → record `ErrNotExecutable`; other stat error → record it.
   None of these spawns a goroutine.

The output is an ordered, deduped `[]struct{name, binPath}`. Keep it a distinct,
testable step — it is the "string array" half of the spec.

### Probe phase (parallel)

If the caller context is already done, skip exec entirely and record **every**
scanned candidate as broken with the context error. A dead caller is not
evidence a plugin is broken, but the found-on-`$PATH` set should stay visible
rather than be dropped.

Otherwise, one goroutine per candidate. Each derives its own
`context.WithTimeout(callerCtx, infoTimeout)` — derived from the caller so a
tighter caller deadline still wins, and per-binary so total wall time is ~one
budget rather than N×budget. Inside: `New(ctx, binPath)`, then `Wrap(ea)`. Each
goroutine sends `{name, binPath, agent, err}` on a buffered channel and returns;
**it does not register.**

No worker pool. The realistic candidate count is a handful, and a cap would
serialize exactly the case the parallelism exists for.

### Register phase

The calling goroutine drains all results, then applies them in collect order, so
registration is deterministic and cannot leave half-registered state if the
context dies mid-drain. Per result: `RegisterExternal` or
`RegisterExternalFailure`. Log a warning when a binary's `info.name` differs
from its derived name (see [decision 4](#4-an-infoname-mismatch-keeps-working)).

### What gets deleted

`discoveryTimeout` (10s), the `discoveryCanceled` / `discoveryCtxErr` helper
pair, and the `statExternalAgent` seam. A single `ctx.Err()` guard at entry
replaces the two helpers, whose whole purpose was guarding a
derived-context-shadowing bug that the new shape removes by construction: each
probe owns a context derived from the caller, so a cancelled caller cancels
every probe and each error already wraps the context error.

Keep the `lookPathExternalAgent` seam —
`TestDiscoverAndRegisterNamedAlways_RejectsPathSeparators` uses it to assert we
never look up a name containing a path separator.

### The named path

`discoverAndRegisterNamed` keeps its `exec.LookPath` resolution (it honours
`PATHEXT`) and its path-separator rejection, switches to `infoTimeout`, records
failures the same way, and still returns the error to its caller.

| | resolution | on failure |
| --- | --- | --- |
| `DiscoverAndRegister{,Always}` | glob every `$PATH` dir, dedupe, one goroutine per binary | record the failure, keep going |
| `DiscoverAndRegisterNamedAlways` | `exec.LookPath` one name, no goroutine | record the failure **and** return the error |

### `entire agent list` surfaces broken externals

`runAgentList` (`cmd/entire/cli/agent_group.go`) never discovers externals
today, so external agents do not appear there at all. Add
`external.DiscoverAndRegisterAlways(ctx)` at the top — the `Always` variant,
because a listing command should show what is on `$PATH` regardless of the
`external_agents` setting, the same reasoning `runSetupFlow` already uses. Then
print a section after the existing list whenever `agent.ExternalFailures()` is
non-empty:

```
Agents:
  ✓ claude-code
    codex
    roger-roger

Broken external agents:
  ✗ foo  (/usr/local/bin/entire-agent-foo): info: invalid JSON: ...
```

## Memoization

`entire enable` / bare `entire` with no `--agent` calls
`DiscoverAndRegisterAlways` up to four times: `setup.go:891` (root/enable
`RunE`, so `--agent` resolves), `setup.go:402` (`runSetupFlow`), `setup.go:473`
and `:496` (`detectOrSelectAgent`, non-interactive and interactive branches).
They are defensive, not sequential-by-design — each is independently reachable.

Do **not** prune those call sites in this change; auditing every entry point is
separate work. Instead memoize per binary path: the collect phase skips any
binary already recorded ready or broken. Calls 2–4 then cost a `$PATH` glob and
nothing else, and the broken list stops flapping between calls.

Accepted cost: a `chmod +x` performed mid-process is not picked up. The user
re-runs the command.

## Two invariants to preserve

- **A missing binary is not a broken agent.**
  `DiscoverAndRegisterNamedAlways` returns `nil` on `exec.ErrNotFound` — that
  means "no such plugin, fall through to other resolution", and its one
  production caller (`explain_summary_provider.go`, the `--summarize-provider`
  override) depends on it. It must not land in the registry as a failure either.
- **The `external_agents` gate stays where it is.** `DiscoverAndRegister` checks
  `settings.IsExternalAgentsEnabled` and returns before scanning;
  `DiscoverAndRegisterAlways` does not. Only the shared body changes.

## Decisions

### 1. `infoTimeout = 300 * time.Millisecond`, provisional

One named constant for both entry points, replacing the 10s aggregate budget.
The requester owns this number and intends to tune it — see
[the budget warning](#-the-300ms-budget-is-measured-to-be-too-small) before
writing tests.

### 2. A built-in always wins a name collision

Override was considered and **rejected as a hijack vector**. Keep this rationale
with the code — it is the reason the collision branch must never call `New`.

`agent.Register` overwrites process-wide, and the gated `DiscoverAndRegister`
runs inside `hooks_cmd.go` and `hooks_git_cmd.go`. So dropping
`entire-agent-claude-code` anywhere on `$PATH` — `./node_modules/.bin`, a
`mise`/`asdf` shim dir, any dir ahead of the real one — would silently replace
the built-in agent that reads transcripts and writes checkpoints, on every hook.
Today built-ins are immune *and* the colliding binary is never executed;
override would mean executing it and handing it the session. The same mechanism
shadows `vogon`, breaking the e2e canary in a way that looks like a CLI bug.

So: the built-in wins, the binary is **not** executed, and the shadowing attempt
is logged at `Warn` with binary path and agent name — which fixes the original
invisibility complaint without the takeover. A collision produces **no** failure
entry; it is not a broken agent.

### 3. Everything else unusable is a failure entry

Exec failure, timeout, invalid JSON, protocol mismatch, `Wrap` failure, missing
exec bit, and a glob match that turns out to be a directory. The point of the
refactor is that "why isn't my plugin showing up?" has an answer, and a
directory or an un-`chmod`'d file is a common cause. The two exceptions are
above: a missing binary, and a name collision.

### 4. An `info.name` mismatch keeps working

A binary at `entire-agent-foo` reporting `"name": "bar"` stays registered under
the binary-derived name `foo`, plus a warning log. It works today and is
cosmetic: every resolution path in the CLI keys off the registry name, never
`Agent.Name()`. The mismatch reaches only log attributes and
`AgentSession.AgentName` (`resume.go:1048`); hook installation is delegated to
the binary itself, so the hook command it writes is the plugin's own
responsibility. Making it an error would break any out-of-tree plugin that
already has one.

### 5. `DiscoverAndRegisterNamedAlways` drops from 10s to 300ms

Confirmed. It shares the budget with the scan path. Its only production caller
is the `--summarize-provider` / dispatch override, and that is still just an
`info` call.

## ⚠ The 300ms budget is measured to be too small

The requester owns this number and intends to tune it, so it is **not** a
blocker. But it was measured, and at 300ms the design does not work on a cold
binary. Do not rediscover this from failing tests.

Measured on macOS (Apple silicon), forking a freshly written shell script:

| Case | Time |
| --- | --- |
| First exec in a fresh process, freshly written file | 320–394ms |
| Each further *newly written* file, same process | 140–170ms |
| Re-exec of a file already run once | 7–12ms |
| `paths.WorktreeRoot` shelling out to git, *inside* the budget | ~13ms |
| Five freshly written files exec'd **concurrently** | >1000ms each |

That last row was measured while implementing this plan, and it is worse than
the sequential numbers suggest: cold starts contend with each other, so the very
parallelism that fixes the serialization problem also makes the first-run cost
spike. A healthy mock breached a **1s** budget while four neighbours were
starting cold. Two tests therefore execute their mock once before the measured
call, to keep cold-start cost out of the budget under test.

The cost is dominated by **first exec of a given binary**, not process warm-up:
on macOS a newly installed binary pays code-signing / Gatekeeper validation and
cold page-in on its first run. Consequences:

- A user who has just installed a plugin gets a failure entry with
  `ErrInfoTimeout` on the very first `entire` command, and a working plugin from
  then on. That is the worst possible first impression, and it is intermittent.
- A real plugin is a compiled binary or, worse, a Node/Python wrapper — both
  strictly slower to start than the `sh` script measured above.
- **Every test that writes a mock agent into a fresh `t.TempDir()` hits the cold
  path.** At 300ms essentially every discovery test that execs a mock fails.
  This is not a test bug; do not "fix" it by loosening assertions. `infoTimeout`
  is a package `var` so those tests can raise it, keeping the shipped default at
  300ms for the requester to tune. Where the budget itself is what a test
  measures, warm the mock with one throwaway exec instead of raising it.
- `Agent.run` additionally calls `paths.WorktreeRoot(ctx)`, which can shell out
  to git (~13ms) *inside* the budget, before the plugin is even executed.
- **Four pre-existing tests broke on the budget alone.** The
  `TestRunExplain*ExternalNativeTranscript` cases each write a mock agent into a
  fresh `t.TempDir()` and rely on discovery to find it. They passed in isolation
  and failed in a full package run, purely because the cold exec exceeded 300ms
  under load. Nothing about those tests is wrong; the budget is. They pass now
  only because every test that writes a mock goes through
  `agenttestutil.WriteExternalAgentBinary`, which warms the binary first — a
  luxury no real user gets on their first command after installing a plugin.

Whatever value is chosen, `ErrInfoTimeout` must stay distinguishable from a
genuine load failure.

## Tests

Tests in `discovery_test.go` modify process-global state (`os.Setenv`,
`t.Chdir`, the agent registry) so they cannot use `t.Parallel()`. Keep that
constraint and its existing comment. Reuse the existing `setupDiscoveryDir`,
`makeInfoJSON`, `enableExternalAgents` and `mockInfoScript` helpers.

Two constraints that will otherwise waste a debugging cycle:

- **Every mock agent written into a fresh `t.TempDir()` is a cold exec** —
  140–400ms on macOS. Raise `infoTimeout` in any test that execs one.
- **Reset external registry entries between tests** via
  `ResetExternalsForTesting` in `t.Cleanup`. Without it, discovery results leak
  across tests and the memoization skip makes failures depend on test order.
  Built-in registry entries are never reset — keep using unique agent names per
  test, as the existing tests already do.

### Delete

Heavily-mocked failure plumbing, and a hand-rolled context that exists only to
prove a context-shadowing bug the refactor removes by construction:

- `TestDiscoverAndRegisterNamedAlways_StatError`
- `TestDiscoverAndRegisterNamedAlways_LookPathError`
- `TestDiscoverAndRegisterNamedAlways_HelperDisappearsAfterLookup`
- `TestDiscoverAndRegisterNamedAlways_DeadlineWhileLookingUpMissingHelper`
- `TestDiscoverAndRegisterNamedAlways_ReportsCallerDeadlineExpiredDuringLookup`
- the `stalledPropagationCtx` type

### Keep

`_FindsAgent`, `_Deduplication`, `_SkipsWhenDisabled`, `_EmptyPATH`,
`_UnreadableDir`, `_ContinuesAfterRegistrationError`, `_SkipsInfoFailure`,
`TestIsExternal_*`, the two Windows `.bat` tests, and
`Named_{InvalidInfo,MissingHelper,RejectsPathSeparators,TimesOutStalledInfo,CanceledContext}`.

Three change what they assert:

- `_SkipsNameConflict` — built-in survives (per
  [decision 2](#2-a-built-in-always-wins-a-name-collision)), and now also
  asserts the binary was **never executed** (observably, via a marker file it
  would have created).
- `_SkipsNonExecutable` — now asserts a failure entry, not silence.
- `_SkipsDirectory` — now asserts a failure entry, not silence.

### Add

Realistic behaviour only — no mock-seam tests.

1. **Parallelism.** Four binaries that `sleep` past the budget on `$PATH`
   alongside one that answers. Assert the fast one registers, the slow ones are
   recorded broken, and total elapsed time is far below `4×infoTimeout`. Use
   four rather than one: with a single slow binary, serial and concurrent
   execution differ by only one budget, which is not a gap you can assert on
   reliably. This is the regression the refactor exists to prevent.
2. **Timeout is classifiable.** The stalled binary's entry satisfies
   `errors.Is(err, ErrInfoTimeout)` and
   `errors.Is(err, context.DeadlineExceeded)`.
3. **Non-executable is classifiable.** `errors.Is(err, ErrNotExecutable)`.
4. **`Get` behaviour.** Ready name → agent, no error. Broken name → the stored
   error. Unknown name → the existing not-found error. And `List` /
   `StringList` exclude the broken name while `GetByAgentType` and `DetectAll`
   do not panic on a nil factory.
5. **Already-cancelled caller.** Every binary found on `$PATH` is recorded
   broken with the context error, and none is executed — asserted observably
   via a marker file.
6. **Memoization.** Two consecutive `DiscoverAndRegisterAlways` calls: neither a
   good nor a failing binary is re-executed the second time (marker-file count
   stays 1), and the snapshots are unchanged.
7. **Dedupe on derived name.** The same derived name in dir A and dir Z resolves
   to dir A's binary, distinguished by its `info.type`.
8. **`info.name` mismatch.** `entire-agent-foo` reporting `"name": "bar"`
   registers under `foo` and is callable.
9. **`entire agent list`** (`agent_group_test.go`) shows a ready external in the
   normal list and a broken one under `Broken external agents:` with its reason.

### Verification

```bash
mise run fmt && mise run lint
mise run test
mise run test:integration
mise run test:e2e:canary   # external discovery runs in e2e/testutil; hooks use a 5s ctx
```

Rerun `mise run lint` after any `mise run fmt` that changed files. Use the
mise-installed `golangci-lint` binary directly — a stale v1 shadows it on
`$PATH`.

Then manually, in a scratch repo: one good binary, one printing garbage, and one
`sleep 60` on `$PATH`. `entire agent list` should show the good one in the list
and the other two under *Broken external agents* with distinct reasons,
returning fast; `time entire agent list` confirms the budget holds. Add a good
`entire-agent-claude-code` and confirm the built-in still wins with a `Warn`
naming the shadowing binary.

## Call sites (for blast-radius review)

- `DiscoverAndRegisterAlways` — `setup.go:203, 402, 473, 496, 891`,
  `explain_summary_provider.go:30`, `e2e/testutil/session_paths.go:26`
- `DiscoverAndRegister` (gated) — `hooks_cmd.go:48`, `hooks_git_cmd.go:177`,
  `resume.go:66`, `checkpoint_resume.go:62`, `attach.go:116`,
  `explain.go:1118`, `review/cmd.go:193`, `trail_resume_cmd.go:165`,
  `strategy/manual_commit_condensation.go:40`
- `DiscoverAndRegisterNamedAlways` — `explain_summary_provider.go:31`

The gated path running in hooks is why
[decision 2](#2-a-built-in-always-wins-a-name-collision) went the way it did.
Signatures do not change, so no call site needs editing —
`runAgentList` gains a call it did not have.

## Documentation

- `docs/architecture/external-agent-protocol.md` — §Discovery is already stale
  ("runs once during CLI initialization"); rewrite it for lazy, per-command,
  parallel discovery under a per-binary budget. §Error Handling changes from
  "the binary is skipped" to "recorded as a broken agent and shown by
  `entire agent list`". Note the `Warn` on a collision, and add the
  undocumented `ENTIRE_CLI_VERSION` env var to §Environment while in there.
- `docs/architecture/external-commands.md` — update the
  `DiscoverAndRegisterAlways` row (~line 267).

## Out of scope

- Retuning `infoTimeout`. The requester owns it; the measured table above is the
  input.
- Turning an `info.name` / binary-suffix mismatch into a hard error.
- Surfacing broken externals in `entire status` and its `--json` output.
- Pruning the redundant `DiscoverAndRegisterAlways` call sites in `setup.go`.
  Memoization makes the extra calls nearly free.
