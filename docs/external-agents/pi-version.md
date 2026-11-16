# External Agent Discovery Refactor — Implementation Plan

Status: fully agreed. Both previously-open decisions are resolved — see
[Resolved decisions](#resolved-decisions).
Scope: `cmd/entire/cli/agent/external/discovery.go` and its tests.

Read this whole document before writing code.

## Why

`discoverAndRegister` — the shared body behind `DiscoverAndRegister` and
`DiscoverAndRegisterAlways` — executes every `entire-agent-*` binary on `$PATH`
**serially** under one 10s budget, checking for cancellation only *between*
binaries. One slow plugin therefore eats the whole budget and every later
binary is dropped as "timed out". Every failure is `logging.Debug`'d and then
lost, so a plugin that is present but unloadable is indistinguishable from a
plugin that was never installed.

Both matter because the `Always` variant runs on interactive, user-waiting
paths, up to three times per process (see [Idempotency](#idempotency)).

## Target design

### Two registries in `external`

Ready agents keep going into the `agent` registry (so `agent.Get`,
`agent.List`, `hookAgentOptions`, `DetectAll` are unchanged). `external` gains
its own record of both outcomes, for a future "external agent state" listing:

```go
// BrokenAgent is an external agent binary that exists on $PATH but could not
// be loaded.
type BrokenAgent struct {
    Name       types.AgentName
    BinaryPath string
    Err        error
}
```

Package-level state, guarded by one mutex:

- `ready map[types.AgentName]agent.Agent`
- `broken map[types.AgentName]BrokenAgent`

Accessors:

- `Get(name) (agent.Agent, error)` — ready → the agent; broken → its `Err`;
  neither → a not-found error.
- A listing accessor returning sorted snapshots of both maps. Return copies;
  never hand out the live maps.

The `agent` registry must keep meaning "agents you can actually use". Do not put
broken entries in it — every `agent.List()` consumer would have to learn to
skip them.

### Error taxonomy

`cmd.Run` reports only `signal: killed` when the context kills the child, so a
timeout is not otherwise distinguishable from a crash. Export two sentinels so
callers classify without string-matching:

- `ErrInfoTimeout` — the `info` call exceeded its budget. Join it with the
  context error so `errors.Is(err, context.DeadlineExceeded)` also holds.
- `ErrNotExecutable` — a matching regular file without the exec bit (Unix).

Anything else stays the raw wrapped exec/protocol error (bad JSON, protocol
version mismatch, non-zero exit, stat failure).

### One shared single-binary load

Both entry points converge on one function. Only *resolution* and *error
surfacing* differ.

```
resolve path → stat / exec-bit check → New(ctx, infoTimeout) runs `info` → Wrap → record
```

| | resolution | on failure |
| --- | --- | --- |
| `DiscoverAndRegister{,Always}` | glob every `$PATH` dir, dedupe, one goroutine per binary | record in `broken`, keep going |
| `DiscoverAndRegisterNamedAlways` | `exec.LookPath` one name, no goroutine | record in `broken` **and** return the error |

`discoveryTimeout` (10s) is deleted. `infoTimeout = 300 * time.Millisecond`
replaces it for both paths. Keep it a single named constant — the requester
intends to tune it.

### Scan phase (cheap, no exec)

1. `os.Getenv("PATH")`; empty → return.
2. For each dir in `filepath.SplitList` order: `filepath.Glob(dir + "/entire-agent-*")`.
   Glob error → skip that dir.
3. Per match: dedupe by binary basename; `StripExeExt`; derive the agent name.
4. `os.Stat`: directory → **ignore entirely** (not an agent, not recorded).
   Regular file without exec bit on non-Windows → record `ErrNotExecutable`,
   no goroutine. Other stat error → record it, no goroutine.
5. Skip names already in `ready` or `broken` (see [Idempotency](#idempotency)).
6. Name collision with an already-registered agent → skip without executing the
   binary, and log at `Warn` (see [decision 1](#resolved-decisions)).

The output of this phase is an ordered `[]struct{name, binPath}`, deduped.
Keep it a distinct, testable step — it is the "string array" half of the spec.

### Exec phase (parallel)

If the caller context is already done, skip exec entirely and record **every**
scanned candidate as broken with the context error. A dead caller is not
evidence a plugin is broken, but the requester wants the found-on-`$PATH` set
visible, so record it rather than dropping it.

Otherwise, one goroutine per candidate. Each derives its own
`context.WithTimeout(callerCtx, infoTimeout)` — derived from the caller so a
tighter caller deadline still wins, and per-binary so total wall time is ~300ms
rather than N×300ms. Each sends `{name, binPath, agent, err}` on a channel and
returns; **it does not register**.

The collector drains all results, then applies them in scan order. Registration
order must be deterministic (first `$PATH` dir wins among externals) and must
not leave half-registered state if the context dies mid-drain.

No worker pool. The realistic candidate count is a handful.

### Idempotency

`entire enable` / bare `entire` with no `--agent` calls `DiscoverAndRegisterAlways`
three times: `setup.go:891` (root/enable `RunE`, so `--agent` resolves),
`setup.go:402` (`runSetupFlow`), `setup.go:496` (`runManageAgents`). They are
defensive, not sequential-by-design — each is independently reachable (bare root,
`entire configure`, `entire enable`, the non-interactive skill path).

Do **not** prune those call sites in this change; auditing every entry point is
separate work. Instead make discovery idempotent per agent name: the scan phase
skips any name already present in `ready` or `broken`. Calls 2 and 3 then cost a
`$PATH` glob and nothing else, and the broken list stops flapping between calls.

Accepted cost: a `chmod +x` performed mid-process is not picked up. The user
re-runs the command.

### Two invariants to preserve

- **A missing binary is not a broken agent.** `DiscoverAndRegisterNamedAlways`
  returns `nil` on `exec.ErrNotFound` — that means "no such plugin, fall
  through to other resolution", and its one production caller
  (`explain_summary_provider.go:57`, the `--summarize-provider` override)
  depends on it. It must not land in `broken` either.
- **The `external_agents` gate stays where it is.** `DiscoverAndRegister`
  checks `settings.IsExternalAgentsEnabled` and returns before scanning;
  `DiscoverAndRegisterAlways` does not. Only the shared body changes.

## Resolved decisions

### 1. A built-in always wins a name collision. Log it at `Warn`.

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

So: built-in wins, the binary is **not** executed, and the shadowing attempt is
logged at `Warn` with binary path and agent name — which fixes the original
invisibility complaint without the takeover. A collision produces **no** `broken`
entry; it is not a broken agent.

### 2. `DiscoverAndRegisterNamedAlways` drops from 10s to 300ms.

Confirmed. It shares the budget with the scan path. Its only production caller
is the `--summarize-provider` / dispatch override, and that is still just an
`info` call.

## ⚠ The 300ms budget is measured to be too small — read before coding

The requester owns this number and intends to tune it, so it is **not** a
blocker for the refactor. But it was measured, and at 300ms the design does not
work on a cold binary. Do not discover this again from failing tests.

Measured on macOS (Apple silicon), forking a freshly written shell script:

| Case | Time |
| --- | --- |
| First exec in a fresh process, freshly written file | 320–394ms |
| Each further *newly written* file, same process | 140–170ms |
| Re-exec of a file already run once | 7–12ms |
| `git rev-parse --show-toplevel` (inside the budget, see below) | ~13ms |

So the cost is dominated by **first exec of a given binary**, not by process
warm-up: on macOS a newly installed binary pays code-signing / Gatekeeper
validation and cold page-in on its first run. Consequences:

- A user who has just installed a plugin gets a `broken` entry with
  `ErrInfoTimeout` on the very first `entire` command, and a working plugin from
  then on. That is the worst possible first impression, and it is intermittent.
- A real plugin is a compiled binary or, worse, a Node/Python wrapper — both
  strictly slower to start than the `sh` script measured above.
- **Every test that writes a mock agent into a fresh `t.TempDir()` hits the cold
  path**, so at 300ms essentially all discovery tests fail. This is not a test
  bug; do not "fix" it by loosening the assertions.
- `Agent.run` additionally calls `paths.WorktreeRoot(ctx)`, which can shell out
  to git (~13ms) *inside* the budget, before the plugin is even executed.

Options, for the requester to pick from:

1. **Raise the constant** to ~2s. Still bounded, still parallel, so a scan costs
   ~2s worst case instead of N×2s; a healthy plugin answers in ~10ms and the
   budget is never approached. Simplest, and the one this document assumes if no
   other decision is made.
2. Keep 300ms as the steady-state budget but grant a longer budget on a binary's
   first exec (e.g. keyed by path+mtime in the cache dir). Correct but adds
   persistent state to a discovery path — over-engineered for now.
3. Keep 300ms and accept first-run failure. Not recommended; see above.

Whichever value is chosen, `ErrInfoTimeout` must stay distinguishable from a
genuine load failure — that is what makes a too-tight budget diagnosable instead
of looking like a broken plugin.

## Tests

Tests in `discovery_test.go` modify process-global state (`os.Setenv`,
`t.Chdir`, the agent registry, and the new ready/broken registries) so they
cannot use `t.Parallel()`. Keep that constraint and its existing comment.

Two constraints that will otherwise waste a debugging cycle:

- **Every mock agent written into a fresh `t.TempDir()` is a cold exec** —
  140–400ms on macOS. See the budget section above. Tests that assert a healthy
  binary loads will fail at `infoTimeout = 300ms` for that reason alone.
- **Reset the ready/broken registries between tests** (a package-private helper
  called from a shared setup, plus `t.Cleanup`). Without it, discovery results
  leak across tests and the new idempotency skip makes failures depend on test
  order. The `agent` registry itself is never reset — keep using unique agent
  names per test, as the existing tests already do.

### Delete

Heavily-mocked failure plumbing, and a hand-rolled context that exists only to
prove a context-shadowing bug the refactor removes by construction:

- `TestDiscoverAndRegisterNamedAlways_StatError`
- `TestDiscoverAndRegisterNamedAlways_LookPathError`
- `TestDiscoverAndRegisterNamedAlways_HelperDisappearsAfterLookup`
- `TestDiscoverAndRegisterNamedAlways_DeadlineWhileLookingUpMissingHelper`
- `TestDiscoverAndRegisterNamedAlways_ReportsCallerDeadlineExpiredDuringLookup`
- the `stalledPropagationCtx` type

Also delete the `statExternalAgent` seam (call `os.Stat` directly) and the
`discoveryCanceled` / `discoveryCtxErr` helpers, whose whole purpose was
guarding the shadowing bug. Keep the `lookPathExternalAgent` seam —
`TestDiscoverAndRegisterNamedAlways_RejectsPathSeparators` uses it to assert we
never look up a name containing a path separator.

### Keep

`_FindsAgent`, `_Deduplication`, `_SkipsNameConflict` (built-in survives, per
decision 1), `_SkipsNonExecutable` (now asserts a `broken` entry, not silence),
`_SkipsDirectory`, `_SkipsWhenDisabled`, `_EmptyPATH`, `_UnreadableDir`,
`_ContinuesAfterRegistrationError`, `_SkipsInfoFailure`,
`Named_{InvalidInfo,MissingHelper,RejectsPathSeparators,TimesOutStalledInfo,CanceledContext}`,
the two Windows `.bat` tests, `TestIsExternal_*`.

### Add

Realistic behaviour only — no mock-seam tests.

1. **Parallelism.** Put four binaries that `sleep` past the budget on `$PATH`
   alongside one that answers. Assert the fast one registers, the slow ones land
   in `broken`, and total elapsed time is far below `4×infoTimeout`. Use four
   rather than one: with a single slow binary, serial and concurrent execution
   differ by only one budget, which is not a gap you can assert on reliably. This
   is the regression the refactor exists to prevent.
2. **Timeout is classifiable.** The stalled binary's `broken` entry satisfies
   `errors.Is(err, ErrInfoTimeout)` and `errors.Is(err, context.DeadlineExceeded)`.
3. **Non-executable is classifiable.** `errors.Is(err, ErrNotExecutable)`.
4. **`Get` behaviour.** Ready name → agent, no error. Broken name → the stored
   error. Unknown name → not-found error.
5. **Already-cancelled caller.** Every binary found on `$PATH` is recorded
   broken with the context error, and none is executed. Assert non-execution
   observably — e.g. a binary that would create a marker file.
6. **Idempotency.** Two consecutive `DiscoverAndRegisterAlways` calls: a broken
   binary is not re-executed the second time (marker-file count stays 1) and the
   ready/broken snapshots are unchanged.

Tests must reset the `external` package registries between cases (add a
test-only reset helper, or `t.Cleanup` in a shared setup) or state will leak
between them and make failures order-dependent.

### Verification

```bash
mise run fmt && mise run lint
mise run test
mise run test:e2e:canary   # external discovery runs in e2e/testutil
```

Rerun `mise run lint` after any `mise run fmt` that changed files.

## Call sites (for blast-radius review)

- `DiscoverAndRegisterAlways` — `setup.go:203, 402, 473, 496, 891`,
  `explain_summary_provider.go:30`, `e2e/testutil/session_paths.go:26`
- `DiscoverAndRegister` (gated) — `hooks_cmd.go:48`, `hooks_git_cmd.go:177`,
  `resume.go:66`, `checkpoint_resume.go:62`, `attach.go:116`, `explain.go:1118`,
  `review/cmd.go:193`, `trail_resume_cmd.go:165`,
  `strategy/manual_commit_condensation.go:40`
- `DiscoverAndRegisterNamedAlways` — `explain_summary_provider.go:31`

The gated path running in hooks is why [decision 1](#resolved-decisions) went
the way it did. Signature changes are not planned, so no call site should need
editing.

## Documentation

`docs/architecture/external-agent-protocol.md` describes the discovery contract.
Update it if the per-binary budget becomes user-visible; the collision rule is
unchanged from today's behaviour apart from the new `Warn`.

