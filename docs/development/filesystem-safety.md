# Filesystem safety

Containment boundaries, symlink policy, and the rationale behind the shared filesystem anchors. Read before changing filesystem access or its exceptions.

Repository paths in code spans are relative to the repository root unless stated otherwise.

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
`LoadEntireSettings`, while **more than twenty files call `settings.Load`
directly** — `strategy/hooks.go`, `manual_commit_hooks.go`,
`checkpoint/remote/*`, `review/*` — and reach settings without ever passing the
pre-run.

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

### The Root Anchors

Entire does filesystem I/O in eight trees, and each has one package that owns a
shared `*os.Root` over it. **Never assemble a path into one of these and hand it
to `os.ReadFile`/`os.WriteFile`/`os.MkdirAll`/`os.ReadDir`/`filepath.Walk`.**

| Tree | Owner | Anchored on |
| --- | --- | --- |
| `.entire` | `entiredir` | worktree root (`paths.WorktreeRoot`), cwd only when there is provably no repo |
| git common dir | `gitdir` | `git rev-parse --git-common-dir`, absolutized |
| the working tree | `worktreedir` | worktree root |
| an agent's hook config | `agent.HookConfigFile` | worktree root (`.claude/`, `.cursor/`, `.github/hooks/`, `.factory/`, `.codex/`, `.opencode/plugins/`, `.pi/extensions/entire/`; also `.gemini/` for the retired Gemini CLI hook cleanup) |
| an agent's session store | `agent.SessionStore` | the agent's own `GetSessionDir` |
| the active git hooks dir | `strategy.hooksRootForInstall` / `ForRemoval` | `git rev-parse --git-path hooks`, absolutized |
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
`.entire`, or at a per-feature subdirectory of the git common dir is resolved
before the root exists.
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
managed data subtree") and the since-extracted `investigate.RunDir` ("an
unvalidated id would be a path-traversal sink") — which is the argument for the
primitive, not against it.

**What each anchor is actually protecting.** These are not uniform, and the
comments at each site say which case applies:

- `.entire` and the git common dir hold names built from agent-supplied session
  IDs and tool-use IDs. Several call sites used to carry
  hand-written comments explaining that an unvalidated ID would be a traversal
  sink feeding `os.RemoveAll`. The root makes that structural.
- The working tree holds names from `git status` and from checkpoint **tree
  entries**, which may have been fetched from a remote. The removed rewind
  implementation demonstrated this boundary: its restore writes were rooted
  before its reads were. There is no current worktree restore path.
- An agent's session store is where a hook payload's session ID becomes a path, via the agent's own `ResolveSessionFile`. `SessionStore.SessionFile` is two checks, not one: it **validates the ID** (`validation.ValidateSessionID`) before the resolver ever sees it, then converts the result back to a name inside the store and **rejects** an ID that left it. The order is the point — several agents use the ID as a *directory* component (Copilot: `<dir>/<id>/events.jsonl`) or return it verbatim when absolute (Codex, Pi), so a containment check on the resolver's output is not a substitute for refusing the input. `agent.Agent.ResolveSessionFile` documents that it must not be called with unvalidated input, and `TestResolveSessionFileCallersAreSanctioned` enforces it: the sanctioned callers are `SessionFile` itself, `external`'s pure delegation, and the e2e harness helper that resolves an ID it generated. Two agents violated the contract for as long as it existed — a new integration copies the nearest existing one rather than re-deriving whether the ID was checked — which is why the rule is a guard test rather than a comment. A malformed ID reports `ErrUnsafeSessionName`; one that resolved out of the store reports `ErrOutsideSessionStore`. Distinct sentinels because "path is outside the agent's session directory" names the wrong problem for an ID that never resolved anywhere.
- The git hooks directory holds no untrusted *name* — the five hook filenames are
  compile-time constants — so it is anchored for the opposite reason: what is at
  those names arrived from somewhere else. It was the last tree Entire wrote to
  with bare `os.ReadFile`/`os.WriteFile` on a joined path, so a symlink at
  `.git/hooks/pre-push` was read through and then *written* through, replacing
  whatever the link named with a shell script. `hooksRootForInstall` refuses a
  link at the directory (git's own `--git-path hooks` answer, which `core.hooksPath` can put
  anywhere, which is why no other anchor reaches it); the four reads go through
  `osroot.ReadFileNoFollow`; and the write is `jsonutil.WriteFileAtomicIn`,
  whose rename **replaces** a leaf link rather than following it.
  `osroot.OpenFileNoFollow` is not the tool for that write — it rejects
  `O_TRUNC` by design, precisely to push truncating writes onto the rename.
  A symlinked hook is then classified as *foreign* rather than as absent, so it
  is backed up to `<hook>.pre-entire` and chained to exactly as a foreign script
  would be: refusing to read through someone's link must not mean silently
  discarding it.

  **The directory refusal is install-only, and that asymmetry is load-bearing.**
  `hooksRootForRemoval` resolves the link and anchors on its target; only
  `hooksRootForInstall` refuses. Removal deletes files carrying Entire's marker
  and renames back the backups Entire itself made, so it acts only on files
  Entire created and the redirect costs nothing. Sharing one function cost a
  great deal: `entire disable` exited non-zero forever with no other uninstall
  path, and `gitHookStateInHooksDir` reported `GitHooksAbsent`, which sent
  `EnsureSetup` to `InstallGitHook` and failed **every agent turn** on the same
  refusal. A refusal a user can neither act on nor uninstall past is worse than
  the redirect it declines to follow.

  **A hook that cannot be READ is never replaced.** `classifyExistingHook`
  identifies absent / ours / foreign positively and returns an error for
  anything else, because the write is `jsonutil.WriteFileAtomicIn` and
  `rename(2)` needs no permission on the target at all: a mode-0000 hook was
  classified "not foreign", got no backup, and was silently destroyed. The
  in-place truncating write this replaced failed loudly with `EACCES`, so
  switching to the rename (correct, for symlinks) turned a loud failure into
  data loss. Removal treats the same case as present-and-not-ours, so a backup
  is not renamed over it either, and warns rather than erroring so uninstall
  still finishes. `doctor`'s `checkGitHookSymlinks` reports both conditions, and
  it is a separate function from `checkAgentDirSymlinks` because that one scans
  worktree-relative paths through the worktree root and `core.hooksPath` can
  name a directory outside the worktree entirely.

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
  `agent.HookConfigFile`: a symlinked `.claude` / `.cursor` / `.codex` /
  `.pi` is refused at the create, because a working tree arrives by
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

  **One escape hatch, and only for these directories.**
  `allow_symlinked_agent_dirs` in `.entire/settings.local.json` names
  worktree-relative agent config directories whose symlinks Entire follows
  instead of refusing, anchoring its root on the resolved target
  (`agent.AnchorWorktreePath` / `OpenAnchoredRoot`). It exists because a
  dotfile-managed `.claude` (chezmoi, stow, yadm) is an ordinary setup among
  exactly the people who run coding agents, and the previous answer was "stop
  managing it that way".

  Three properties make it a hatch rather than a hole, and all three are load
  bearing:

  - **A list, not a boolean.** A flag would disable the class; naming a path is
    the user saying which arrangement is theirs.
  - **Two independent boundaries.** `enforceSymlinkedAgentDirsTrust` answers
    "may this FILE grant anything" with the same untracked-and-verified gate as
    the OPF command and `external_agents` (a repository that could both ship the
    link and vouch for it is the whole attack). `agent.SetVouchedSymlinkedDirs`
    then answers "is this PATH one an agent config lives under", against a
    pinned list plus a structural `neverVouchable` rule, so `.entire` and
    `.git/hooks` are unspellable *even from a verified local file*.
  - **Pinned, not derived.** `agent.vouchableDirs` was first derived from
    `AllHookConfigRelPaths()`, which is wrong twice: that registry is mutable at
    runtime, so an external plugin could widen what a user may vouch for, and it
    is empty in a binary that has not imported the agent packages, so the set
    silently collapsed depending on the caller's import graph.
    `TestVouchableDirsMatchTheBuiltInAgents` (in the `cli` package, where every
    built-in agent is registered) fails on drift in both directions.

  **The Entire-owned directory is not vouchable, and `RemoveDir` reads the
  worktree-relative name.** `.pi/extensions/entire` is the one directory Entire
  both creates *and* deletes, so it cannot also be a link the user manages;
  `neverVouchable` refuses any path whose base is `entire`, and the pinned list
  omits it. `HookConfigFile` therefore carries **two coordinates** — `name`
  (root-relative, for I/O) and `relName` (worktree-relative, for decisions) —
  the same split `entiredir.Name` draws. `RemoveDir` decides on `relName` and
  removes on `name`: deciding on the root-relative name let a vouch for
  `.pi/extensions/entire` anchor the root *on* that directory, collapse the name
  to `index.ts`, and refuse with "refusing to remove the worktree root" about a
  path that was neither — leaving an extension pi still discovers. Reading
  either coordinate for both jobs gets one of them wrong.

  **The policy is scoped to the worktree it was loaded for.** It has to be a
  package global — `settings` may import `agent`, not the reverse, so the
  package that owns the value pushes it in — and an unscoped global would mean
  last-load-wins deciding for a process that loads settings for one tree and
  writes an agent config for another. `SetVouchedSymlinkedDirs` records the
  root, `AnchorWorktreePath` and the doctor/status reporting compare against it,
  and a mismatch refuses (degrading to the strict behaviour, never to following
  another tree's link). The key is **canonicalized and typed**
  (`agent.worktreeKey`, built only by `keyFor`), so a raw path cannot be
  compared against a stored key — that does not compile. The first version
  compared plain strings and silently disabled the feature on Windows: readers
  pass git's `--show-toplevel` (`C:/repo`) while settings derives its root
  through `entiredir.PathTo`, whose `filepath.Join` rewrites it (`C:\repo`).
  One directory, two spellings, never equal, and every symptom looked exactly
  like an absent grant. This is a different question from why `vouchableDirs` is
  pinned: that is the set a user MAY name, which must not be widenable at
  runtime; this is the set a user DID name, which is per-configuration and has
  to come from somewhere mutable.

  Scope notes: vouching for `.claude` does **not** vouch for links inside it,
  since below the anchor everything is a name in a root again; the scaffolds go
  through the same anchor (`openScaffoldTarget`), because a vouched directory
  that hook installation follows and scaffolding refuses leaves `entire enable`
  half-applied across two directories; and a vouched but *unresolvable* link is
  an error, not a quiet fall back to refusing. `entire status` prints what is
  being followed and any rejected entries, and `doctor` reports the link under
  FOLLOWING SYMLINKS rather than as a fault. `.entire`, the settings files, and
  the git hooks directory are deliberately excluded — the settings case is
  circular (a symlinked `settings.local.json` vouching for symlinked settings
  files authorizes itself) and the hooks case already has git's own
  `core.hooksPath`.

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
  `.claude/skills/`, `.claude/agents/`, `.codex/agents/`. It
  was not one of the seven call sites `HookConfigFile` replaced, and until it was
  anchored it did `os.MkdirAll` two levels and `os.WriteFile` through a symlinked
  `.claude`, landing files outside the repository and reporting Created. It now
  takes a worktree root rather than an assembled absolute path, and its callers
  no longer fall back to `os.Getwd()` when `WorktreeRoot` fails.
- **An agent's session DIRECTORY is the exception, and is followed.** The store refuses a symlink for every name *inside* it — nested directories go through `MkdirAllNoSymlink`, the leaf through `LstatNoSymlinks` — but `openRoot` is a plain `os.OpenRoot(s.dir)` and `openRootForWrite` a plain `os.MkdirAll`, both of which follow a link at the store directory itself or above it.

  That is deliberate and was reverted back into place once. The store's location comes from the agent's own `GetSessionDir`, not from checkpoint metadata or a hook payload, so it is not the untrusted input the rest of this section is about; and `~/.claude` or `~/.codex` managed by chezmoi/stow/yadm is an ordinary setup among exactly the people who run coding agents. Refusing it broke reads as well as writes, whenever the link was the deepest component that existed yet, with no opt-out — `allow_symlinked_agent_dirs` covers worktree-relative agent *config* directories, never the home session store. Anyone who can plant a symlink in that directory can write the transcripts directly and does not need Entire to follow it.

  The store and every directory created beneath it are `0700`, not `0750`: they hold session transcripts, and with the usual umask the difference is group-readable. Copilot, Cursor and Codex all write into nested directories, so the root alone was not enough.
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
  `opencode export` takes a path, a transcript path becomes a checkpoint's
  `SessionRef`.
  Those keep an absolute spelling; the reads and writes around them still go
  through a root.
- **Anything outside the CLI packages takes an `fs.FS`, not a path.**
  `redact.LoadPacks(fsys, dir, logger)` is handed `root.FS()`, which is how pack
  discovery stays confined without `redact` depending on the CLI.
- **A directory cannot be created, statted, or removed through its own root.**
  Those operations (`setupEntireDirectory`, `removeEntireDirectory`, the one
  `MkdirAll` of an agent's session dir in `resume.go`) legitimately use plain
  `os` calls.
- **`Root.Link` takes two root-relative names, and `Root.Symlink` with an
  absolute target is unusable on Windows.** `Root.Link(absPath, name)` is a
  path escape everywhere. `Root.Symlink(absPath, name)` on Windows (Go 1.27)
  writes the reparse target without the `\??\` prefix, so the link is created
  but every follow fails with `ERROR_INVALID_NAME` — the 0-byte
  `bin\entire-graph.exe` bug. See `plugin_store_windows.go` and
  `materializeManagedEntry`.

**Deliberately not rooted**, with the reason:

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
  directory — while the `os.ReadFile(sessionRef)` reads are not (the count is
  pinned per file by `agent.TestTranscriptReadsOnlyShrink`), and the
  read is what pulls transcript content into checkpoints. Closing it is a
  protocol change rather than a refactor, which is why it is scoped separately;
  the shape it wants is `HookInput.RepoPath` plus the same
  `sessionStoreForWrite` resolution the write half already uses. Do not "fix"
  it by anchoring a root on `filepath.Dir(sessionRef)` — that is the derived
  base the rule above refuses, and it would contain nothing while looking like
  it did.

  `TestTranscriptReadsOnlyShrink` (`agent/transcript_read_guard_test.go`) is a
  **ratchet** over that set: it pins the per-file count of
  `os.ReadFile(sessionRef)`-shaped reads and fails the build when one grows or a
  new file appears, and equally when a listed one goes away without its entry
  following. Growth is the regression it exists to stop — a new agent
  integration copies the nearest existing one, so the shape spreads by
  imitation — and a stale entry is the slower failure, since the count is the
  only record of how much of the gap is left.
- **A directory the caller is about to create, replace, or delete** — creating,
  statting, or removing a directory is an operation on it from the outside, which
  a root over it cannot perform. `setupEntireDirectory`, `removeEntireDirectory`,
  the `MkdirAll` behind each anchor, and the plugin index clone are all this case.
- **Paths the user named** (`doctor bundle --out`, `api --input`) and fixed
  platform files such as `/dev/tty`, `CONIN$`, `CONOUT$`, and `/proc/<pid>/*`.
  No boundary exists to enforce.
