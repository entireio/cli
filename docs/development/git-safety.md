# Git and subprocess safety

Git API contracts, index-write hazards, executable resolution, and platform-specific subprocess rules.

Repository paths in code spans are relative to the repository root unless stated otherwise.

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
repositories the user's own git refuses. Anything that writes must stop
instead — that is what "we could not find out which repository this is" means.
Key files: `gitrepo/repository.go` (open entry points) and
`gitrepo/reftable.go` (`reftableStorer`).

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

**The flag does not disable the equivalent refresh in worktree-comparing `git
diff`.** `builtin/diff.c`'s `refresh_index_quietly()` does not consult
`use_optional_locks()`: measured on Git 2.50.1, both `git diff <tree> --
<paths>` and `git --no-optional-locks diff <tree> -- <paths>` rewrote a
stat-stale index. `git diff --cached` and a two-tree diff do not read the
worktree and are unaffected. Hook code needing exact clean-filtered content
uses `git hash-object`; `git diff-index` is also non-refreshing but can report a
stat-dirty, content-identical file as changed. The source guard
`TestGitWorktreeDiffCallSitesDoNotRefreshTheIndex` prevents the unsafe form from
being introduced on the hook path.

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

**A per-user directory override must be absolute**, and there is one
implementation of that rule: `userdirs.RequireAbsoluteOverride`. A relative
value resolves against the working directory, so the same environment names a
different directory in every process — usually one inside whatever repository
the command ran from. It covers all three trees an override can redirect:
`pluginParentDir` (`ENTIRE_PLUGIN_DIR`, `XDG_DATA_HOME`, `LOCALAPPDATA` — a
tree whose `bin` subdirectory `main.go` prepends to `$PATH`), and the config and
cache directories (`ENTIRE_CONFIG_DIR`, `XDG_CACHE_HOME`), which hold the login
tokens and the discovery caches. Leaving it to `osroot` (which refuses a
relative root open) and to `main.go`'s `PATH` restore was not wrong, but each
backstop answers a question of its own, two layers from where this one is
decided.

Rejecting beats falling through to the platform default: for the config
directory that default is the developer's REAL `~/.config/entire`, so quietly
substituting it for a test harness's mistyped override is worse than an error.
`userdirs.Config()`/`Cache()` cannot report — too many callers only want the
string — so **every consumer that turns one into I/O checks, and there are
four**: `userdirs`' own roots, `contexts`, `discovery`, and the token store.
The first three used to launder a relative directory through `filepath.Abs`,
which produced a plausible-looking absolute path out of the exact mistake being
guarded against. Do not reintroduce it.

Two rules about *where* the check goes, both learned by getting them wrong:

- **Who checks is enforced, not enumerated.** `TestUserDirConsumersAreAudited`
  requires every caller of `Config()`/`Cache()` to appear in a ledger with the
  reason it is safe. Two successive doc comments tried to list the consumers
  instead and both were wrong within a commit or two: the token store slipped
  past the first (bearer tokens at `./<value>/tokens.json`) and `plugin_index`
  past the second (an index clone and its lock file in the working directory).
  Converting a caller to `ConfigDirChecked`/`CacheDirChecked` removes it from
  the ledger, since it is then safe by construction; the list is meant to
  shrink.

- **It must precede every `MkdirAll`, not merely every root open.** Checking at
  the root is checking at the READ, and `contexts.FilePath` and
  `withCacheFileLock` run several steps earlier: they created `./<value>` and
  dropped a `.lock` inside it on the way to reporting the refusal, which is the
  mistake itself. `resolveUserRoot` already had this right; the other two did
  not.
- **A root whose base is DERIVED cannot enforce this, so its owner must check
  for itself.** The token store is the fourth consumer, and it does open a root
  — the claim that it had none was wrong. `fileStore.dir` anchors on
  `filepath.Dir` of its own path, one of the two places the root-base rule
  permits a derived base (`ENTIRE_TOKEN_STORE_PATH` names a file the caller
  chose), and gets there through `filepath.Abs`. That `Abs` is precisely why the
  root can never refuse a relative config dir: it launders `./relative-config`
  into a plausible absolute path *before* the root exists, which is the same
  laundering removed from `contexts` and `discovery`. So the store calls
  `userdirs.ConfigDirChecked` and carries the error on `fileStore.pathErr`,
  reported by `dir` and `ensureDir` ahead of any filesystem access — verified
  through `Get`/`Set`/`Delete`, not just the resolver. Left out, it put bearer
  tokens at `./<value>/tokens.json`. An explicit `ENTIRE_TOKEN_STORE_PATH` is
  deliberately still exempt: the user named that file.

- **Only a USER-supplied override is refused; Entire's own fallback is
  absolutized.** `userdirs.ownFallbackDir` resolves the home-relative default
  that `configDir`/`cacheDir` produce when `os.UserHomeDir` fails, because by
  the time a consumer sees a plain string it can no longer tell a value the user
  set from one Entire made up — and refusing both was a regression: on a machine
  with no resolvable home (no `HOME`, an odd container, a service account) every
  command touching a saved login or a discovery cache began failing with advice
  about a variable the user had never set. There is nothing for them to fix
  there, and a cwd-relative directory that works beats a hard failure, so the
  distinction is drawn in the one place that still has the information.

**The OPF `command` is the deliberate exception, and stays one.**
`redaction.openai_privacy_filter.command` becomes `argv[0]` of an
`exec.CommandContext` in `redact/opf.go` with no `cmd.Dir` and no absoluteness
check, so `"command": "./tools/opf"` resolves against the pre-push process's
working directory. That is allowed because the field is not repo-supplied: the
[OPF ownership gate](checkpoint-implementation.md) honors it only from an untracked, index-and-HEAD-verified
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

**The auto-updater's `sh -c` is a considered exception, and it is enforced
rather than asserted.** `versioncheck.realRunInstaller` (unix only) runs the
update command through a shell, which the rule above would otherwise forbid. It
is allowed because there is no dynamic value in it: on unix
`UpdateCommandForCurrentBinary` returns one of five compile-time literals, and
the binary's path and version choose *between* them and never appear *in* them —
while the shell is load-bearing for the fallback, which is a pipeline
(`curl … | bash`). The tempting next change is exactly the one that breaks this:
interpolating a channel or a version into the command.
`TestUpdateCommandIsAlwaysALiteral` (in `versioncheck_unix_test.go`) drives every
unix install manager and channel with adversarial paths and versions and fails
when the result is not a known string. A command that genuinely needs a runtime
value must be built and run as argv, not added to that set.

Windows is deliberately outside that invariant rather than an exception to it:
`fallbackInstallCommand` there interpolates the running binary's directory into
`-InstallDir`, and it is safe to because Windows never *runs* the command —
`realRunInstaller` is unimplemented there, so the string is only ever printed
for the user to paste.
