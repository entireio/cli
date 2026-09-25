# Git CLI → go-git audit

Static audit of this checkout against `~/Work/entire/go-git` main, treating that
source as available (no release-availability gating). No production code changed.
API availability is not a claim of behavioral equivalence; candidates below need
regression tests before migration. Line numbers identify the audited checkout.

Scope: production Go subprocesses, their variadic wrappers/callers, test and
benchmark helpers, and shell/build tooling. Test invocations are grouped rather
than listing hundreds of individual fixture commands. Embedded transcripts,
help text, and commands merely suggested to an agent are not executable call sites.

## Recommendation

1. Replace small local reads and literal branch-name validation first.
2. Consolidate object-tree diffs and history traversal behind shared helpers.
3. Prototype `Reindex()` to retire partial-clone *stale-index* subprocess fallbacks.
4. Revisit v5-era branch deletion and checkout exceptions separately, with parity
   tests and an explicit update to the repository safety rules.
5. Keep native Git for repository discovery, transactional refs, clean filters,
   general user-config/transport compatibility, hook-running operations, and
   diagnostics intended to show native Git's view.

A go-git call through Entire's reftable adapter can still spawn Git. Replacing a
caller with go-git does not eliminate subprocesses in reftable repositories.
Always open via `gitrepo.OpenCurrent` / `OpenPath`, not a new direct `PlainOpen`.

## 1. Strong replacement candidates

Paths below are relative to `cmd/entire/cli/` unless stated otherwise.

| Call sites | Current Git operation | Replacement and qualifications |
| --- | --- | --- |
| `git_operations.go:337` | `check-ref-format --branch` | `plumbing.ValidateBranchName`. Main has the actual branch shorthand checks, including leading `-` and `HEAD`; do not substitute only `ReferenceName.Validate`. It deliberately does not expand `@{-1}`. Appropriate for literal branch creation names. |
| `head_checkpoint_flags.go:39` | `log -1 --format=%B` | `repo.Head()` → `repo.CommitObject()` → `Commit.Message`. Preserve target repo and missing/unborn HEAD behavior. |
| `gitexec/gitexec.go:37`, consumed by `review/cmd.go:1752` | `rev-parse HEAD` | `repo.Head().Hash()`, preserving repository selectors at the open boundary. |
| `strategy/common.go:1605,1624` | `show-ref --verify --quiet` | `repo.Reference(fullName, false)`. Distinguish not-found from corruption/permission failures. The deletion following the first check is a separate decision. |
| `git_operations.go:506`; `strategy/unpushed_checkpoints.go:77` | `rev-parse --verify ...^{commit}` | Resolve ref/revision and validate/peel to a commit. A ref's existence alone is insufficient. |
| `dispatch/mode_local.go:440,450,460` | symbolic default branch, ref existence, `merge-base --is-ancestor` | `repo.Reference(name, false)`, `ResolveRevision`, `Commit.IsAncestor`. Keep detached/unborn behavior and shallow-boundary handling. |
| `strategy/checkpoint_sync_capture.go:217`; `status.go:930`; `trail_checkout_worktree.go:468` | symbolic/current branch name | Read `HEAD` **without resolving it**, then inspect its type/target. `repo.Head()` alone loses the distinction needed for an unborn branch. Preserve each caller's detached behavior. |
| `trail_review_cmd.go:1449` | `rev-parse ref` | `ResolveRevision` for the supported ref grammar; do not promise all native revision expressions. |
| `strategy/manual_commit_push.go:387`; `doctor_bundle.go:170` | `for-each-ref` | `repo.References()` / `Storer.IterReferences()`, filter prefix, stop early for existence. Sort where output stability matters. |
| `checkpoint/remote/checkpoint_ref.go:384`; `repo_remote.go:90` | `remote` names | `repo.Remotes()` or config remote keys. Straightforward for repo-local remotes; test included config if supporting the complete native remote namespace. |
| `checkpoint/remote/git.go:618` | `rev-parse --is-shallow-repository` | Candidate, but not simply `len(repo.Storer.Shallow()) > 0`: characterization confirms native Git reports an existing **empty** shallow file as shallow. The current bool API returns false on read failure/cancellation; changing that contract belongs in a separate change. |
| `strategy/push_common.go:355` | `show HEAD:.entire/settings.json` | Commit tree → `Tree.File` → contents. This is a trust comparison, not a new settings parser: preserve the existing settings/provenance gate. Missing promisor objects still need a fetch path. |
| `plugin_gitremote.go:371` | `show <tag>:<metadata>` after clone | Resolve/peel tag → commit tree → file reader. Retain the metadata size limit. |
| `gitops/diff.go:40,42` | two-tree / root `diff-tree -r -z` | `object.DiffTreeContext` / `DiffTreeWithOptions`, collect paths from changes; enumerate the root tree for an initial commit. Preserve both sides of renames, deletions, modes, submodules, and configured rename behavior. No index/worktree walk is needed. |

These are low-complexity *operation* replacements, not permission to bypass
native repository resolution. If a helper already receives `*git.Repository`,
reuse it rather than repeatedly opening the repository.

## 2. Feasible, but require an algorithm or semantic adapter

| Call sites | Operation | Assessment |
| --- | --- | --- |
| `strategy/unpushed_checkpoints.go:63`; `review/scope.go:203` | `rev-list --count A..B` | Walk commits reachable from B minus **all** commits reachable from A. Do not stop at A or subtract two raw counts. |
| `strategy/common.go:291` | `rev-list` looking for shallow boundaries | A visited-set walk can stop as soon as a shallow hash is found. Never follow parents through a shallow boundary. |
| `strategy/push_common.go:653`; `strategy/metadata_reconcile.go:305` | `merge-base` | `Commit.MergeBase` exists. Preserve no-common-ancestor errors, shallow handling, timeout expectations, and multiple-base behavior; the API returns a slice. |
| `strategy/push_common.go:680` | `rev-list --reverse --topo-order --no-merges A..B` | Reachability subtraction plus topological ordering and merge filtering. Date order or a first-parent walk is not equivalent. Keep traversal caps and cancellation checks. |
| `dispatch/mode_local.go:368,527`; `review_context.go:391,413` | ranged/filtered/formatted logs | `Repository.Log` and object fields remove subprocess output parsing. Reproduce revision exclusions, time bounds, limit/order, trailer matching, and all-ref behavior where requested. |
| `strategy/telemetry_signals.go:369` | bounded `log --name-only` | Log iterator + commit message + parent-tree diffs. Preserve skip-one, lookback ordering, and the deliberate exclusion of merge-commit file lists. |
| `review/scope.go:222` | `diff --name-only base...HEAD` | Find merge base, then diff its tree against HEAD. A two-dot tree comparison is not equivalent. |
| `experts_cmd.go:594`; `strategy/manual_commit_hooks.go:2891` | staged `diff --cached --name-only` | Compare HEAD tree against index entries using go-git index/merkle-tree plumbing. Do not replace with a full worktree status walk. Cover unborn HEAD, conflict stages, intent-to-add, modes, submodules, renames, and `ACMRD` filtering. |
| `attribution.go:652` | `blame --line-porcelain -- file` | `git.Blame(commit, path)` exists, but is **not** a drop-in: native invocation includes uncommitted working-file changes. Need dirty-line attribution and parity for renames/merges/config, or retain CLI. |
| `repo_remote.go:177,185` | `remote add/set-url` | `CreateRemote` / config mutation. Preserve fetch refspec creation, all existing URL/pushURL entries, unrelated config, and safe concurrent updates. Do not replace a whole remote config just to change its fetch URL. |
| `checkpoint/remote/git.go:327,349`; `strategy/checkpoint_sync_remote.go:231`; `setup_checkpoint_remote.go:265` | local config writes/enumeration/raw URL reads | Config `Raw` sections can represent these. Verify include semantics and avoid read-modify-write loss of concurrent/unrelated settings; raw ownership checks must not use rewritten URLs. |

History APIs do not all accept a context. Retaining a `CommandContext` deadline
requires more than passing a context to the caller: add bounded/cancellable
traversal, or keep native Git where a hard killable budget is part of the contract.
Partial/shallow clones and replace refs also need explicit compatibility tests.

## 3. Current main makes older workarounds worth revisiting

### Stale object indexes: particularly promising

- `checkpoint/fetching_tree.go:262`: `cat-file --batch-check` with lazy fetch off.
- `checkpoint/fetching_tree.go:287`: `cat-file -p` with lazy fetch off.
- `checkpoint/parse_tree.go:313`: `ls-tree` fallback.

These are already *fallbacks after go-git reads fail*, not ordinary object reads
waiting for an obvious conversion. Main has `storage/filesystem.ObjectStorage.Reindex()`
(`storage/filesystem/object.go:292`), which reloads external pack indexes and
prewarms them; its tests specifically cover externally added packs and concurrent
reindexing. I found no `Reindex` call in Entire's current code.

Prototype: after an external fetch, invalidate/reindex the actual shared object
storer, then retry `HasEncodedObject`, `BlobObject`, or `TreeObject`. Verify through
Entire's alternate-object and reftable wrappers. Batch the refresh, rather than
reindexing for each blob. Retain fallback until partial-clone regressions pass.

This only addresses **objects already on disk**. Main still does not transparently
fetch missing promisor objects on demand. It does not remove `FetchBlobs` or prove
that every existing fallback is redundant.

### Branch deletion

- `checkpoint/ephemeral.go:714`: shadow branch deletion.
- `strategy/common.go:1614`: shared `DeleteBranchCLI`.

Both explain the subprocess using v5 packed-ref/worktree deletion bugs. Main's
`Storer.RemoveReference` removes loose and packed references, so that specific
reason deserves fresh tests. However, `git branch -D` also refuses branches checked
out in another worktree and handles branch config/reflog cleanup. A raw ref deletion
is not a general branch-delete replacement. Internal shadow refs are the narrower
candidate; test packed refs, linked worktrees, and concurrent native Git writes.

### Checkout and hard reset

- `git_operations.go:324`: checkout.
- `plugin_index.go:276`: hard-reset the managed plugin index.
- `benchutil/benchutil.go:503`: benchmark checkout.

Main's `worktree.go` now has tree-to-tree reset logic explicitly preserving
untracked files, with tests for ignored directories and tracked files inside them.
Do not claim the historical v5 deletion bug still applies unchanged to main.

Nevertheless, repository instructions and `.golangci.yaml` currently forbid the
replacement. Treat migration as a separate safety-reviewed change: ignored
`.entire`/`.worktrees`, dirty/staged files, overwrite collisions, linked worktrees,
submodules, sparse state, clean/smudge filters, and post-checkout hooks need parity.
Managed-cache reset is a narrower experiment than user-branch checkout.

### Worktree APIs exist, but differ materially

- `trail_checkout_worktree.go:414`: `worktree add <path> <existing-branch>`.
- `trail_checkout_worktree.go:504`; `resume_picker.go:466`: `worktree list --porcelain`.
- `review_target.go:107`: `worktree remove --force`.

Main's `x/plumbing/worktree` has `New`, `Add`, `List`, `Open`, `Remove`, and `Init`.
But `Add` creates a branch from the worktree name rather than directly expressing
this existing-branch checkout; `List` returns linked-worktree **names**, not native
porcelain records or the main worktree; `Remove` deletes **metadata only**, not the
working directory. They also require a compatible `WorktreeStorer`. Keep current
CLI calls unless building and testing a complete adapter, including rooted I/O.

## 4. Keep native Git for now

### Transactional refs and reftable

| Call sites | Reason |
| --- | --- |
| `gitrepo/ref_cas.go:86,264` (`update-ref --stdin`, symbolic-ref probe) | Entire prepares a native ref transaction, holds locks while rejecting symbolic refs, then commits/aborts. Main's filesystem `CheckAndSetReference` uses Billy/file locking and in-place writes (`dotgit_setref.go`), not native Git's `.lock` transaction protocol. It is not equivalent for concurrent Git writers, no-deref rejection, or create-if-absent. Keep the coupled probe/transaction. |
| `strategy/cleanup.go:316` (`update-ref -d ref expected`) | Expected-old-value deletion protects against deleting another writer's ref. `RemoveReference` has no expected-value argument. |
| `gitrepo/reftable.go:137` and its callers | Native `symbolic-ref`, `update-ref`, `rev-parse`, `for-each-ref` implement the reference backend itself. Main has no native reftable storage replacement. Routing back through this storer just recurses or still shells out. |

### Repository discovery and native path/config semantics

Keep these as the shared native discovery boundary, not scattered replacements
with `PlainOpen`: ownership (`safe.directory`), `GIT_DIR`/`GIT_WORK_TREE`, linked
worktrees, `core.hooksPath`, and common-directory resolution are observable behavior.

- `paths/paths.go:128` — toplevel.
- `gitdir/gitdir.go:82,169` — common dir.
- `settings/settings.go:678` — common dir.
- `strategy/common.go:1385`, `metadata_reconcile.go:363`,
  `manual_commit_session.go:484` — common dir.
- `strategy/hooks.go:114,137` — git dir / effective hooks path.
- `trail_checkout_worktree.go:64,446,455` — common dir and path validation.
- `session_adopt.go:286` — toplevel/common-dir identity checks.
- `dispatch/mode_local.go:168`, `dispatch_wizard.go:538` — validate repo paths.

Some repeated calls could instead reuse **already resolved** trusted roots. That
is a consolidation opportunity, not evidence go-git matches native discovery.

Likewise retain effective-config fallbacks in `git_operations.go:96`,
`setup_identity.go:180`, `strategy/checkpoint_sync_capture.go:254`, and
`checkpoint/remote/git.go:137` (`core.sshCommand`). Go-git has config scopes and
worktree-config support, but that does not establish full include/includeIf,
environment override, and native precedence parity.

`gitremote/gitremote.go:168,209`, `strategy/manual_commit_push.go:360`, and
`remote_topology.go:131` depend on effective remote URL semantics. Main implements
`insteadOf`, but its remote config unmarshalling appends `pushurl` entries to
`URLs`; it is not an equivalent model for distinct fetch URLs, multiple push URLs,
and `pushInsteadOf`. Keep native `remote get-url` / `remote -v` here.

### Ignore/status/worktree content

- `checkpoint/ephemeral.go:1223` — `check-ignore --no-index -z --stdin`.
- `trail_checkout_worktree.go:97,226` — ignore directory probe / ignored-file listing.
- `strategy/common.go:1643` — nonignored untracked files.
- `checkpoint/ephemeral.go:1383`, `git_operations.go:216`, `review/scope.go:238`,
  `setup_bootstrap.go:381` — porcelain status.
- `gitrepo/worktree_hash.go:79` — path-specific clean-filtered `hash-object`.

Go-git has ignore matchers, per-directory scopes, and global/system pattern loaders.
Main's status `ignoreScope()` itself loads repository patterns plus `w.Excludes`,
not a complete native effective-global-config setup. Ignore precedence, directory
negation, linked-worktree excludes, untracked expansion, and submodules require
more work than swapping in `Status()`.

The first-checkpoint status subprocess has a **killable wall-clock budget** and
is intentionally independent of the go-git budget-breach latch. Preserve that.
Any migrated status consumer must use `gitrepo.Status` / `StatusWithBudget`.

Keep `HashWorktreeFiles`: hashing raw bytes with go-git does not reproduce native
path-specific clean filters, including user programs. It also intentionally avoids
worktree `git diff`'s index refresh. See [Git safety](git-safety.md).

### Network and remote-helper subprocesses

| Call sites | Operations / reason to retain |
| --- | --- |
| `checkpoint/remote/git.go:241,388,430,555,643` | Shared fetch, fetch-pack-by-hash, push, ls-remote, command construction. Go-git has fetch/push/list APIs, but this boundary also owns token routing, native credentials, SSH command selection/noninteractive behavior, filtered/shallow fetches, URL remote config side effects, and error classification. This is a transport migration, not a small substitution. |
| `git_operations.go:259`; `trail_cmd.go:2469,2564,2588,2606` | ls-remote, fetch, push/upstream setup, branch deletion. Preserve auth, push hooks (where enabled), tracking config, multi-URL behavior, and transport support. |
| `repo_clone.go:953` | Interactive native clone, including `entire://` helper routing. `PlainCloneContext` alone does not provide native external-remote-helper behavior. |
| `plugin_gitremote.go:119,296,353`; `plugin_index.go:254,273` | Bounded ls-remote and shallow clone/fetch. Technically feasible with `Remote.ListContext`, clone/fetch options. Need auth compatibility, URL allowlists, no-prompt behavior, deadlines, response/metadata bounds. Better pilot than user clone if transport scope is explicitly restricted. |
| `internal/remotehelper/githelper/push.go:79` (repo-root-relative) | `send-pack --stateless-rpc --helper-status --stdin`. This is a protocol engine inside a native remote helper, not a regular `repo.Push` call. Keep. |

### Hook-running mutations and human diagnostics

- `attach.go:971`: `commit --amend --only`. Preserve hooks, signing, identity,
  parent/message behavior, and unchanged staged content. Not a simple `Commit` swap.
- `setup_bootstrap.go:366,375,391`: init/add/commit; `setup_identity.go:166,171`:
  identity writes. Library APIs exist, but native templates/config, filters,
  ignored files, hooks, and the explicit signing override need a product decision.
- `trail_review_cmd.go:1403`: `apply --check` and actual apply. Main has no
  equivalent general `git apply` API; object diff/patch generation is not application.
- `doctor_bundle.go:132,135,138`: native status/log/remotes are useful diagnostic
  evidence. Keep even though a custom approximation could be built.
- `telemetry/detached.go:508`: `git --version` measures installed Git, not go-git.

## 5. Tests, benchmarks, tooling, and non-Go scripts

Do not blanket-replace test shell-outs. Integration/E2E Git is both the actor that
runs Entire's hooks and an independent oracle for go-git-backed production code.
Keeping both sides independent catches packed-ref, reftable, index, signing,
filter, and subprocess-environment incompatibilities.

- `cmd/entire/cli/integration_test/{testenv,multi_pushurl,backend}.go`, individual
  `*_test.go`, and `e2e/testutil/{repo,backend,assertions,artifacts}.go`: keep native
  commit/push/checkout/config/worktree operations and compatibility assertions.
  Pure fixture blob/tree/ref construction can use existing go-git test helpers,
  but not if doing so removes the independent assertion or behavior under test.
- `e2e/vogon/main.go:615`: keep native Git actions; these simulate an agent workflow.
- `cmd/entire/cli/testutil/gitenv/gitenv.go:155`: native repo initialization/config
  is intentional test infrastructure.
- `cmd/entire/cli/testutil/gitgrep.go:54,72`: keep the required `GitGrepGuard`.
- `cmd/entire/cli/benchutil/benchutil.go:560,575,584`: fixture `hash-object -w` can
  use encoded-object writes; `pack-refs` can use `Storer.PackRefs` in an exclusively
  owned fixture. Keep native `gc` if benchmarking native packed layouts; go-git
  repack is not full native GC. Checkout discussed above.
- `tools/complexity/main.go:621`: `log --numstat` could use log + tree patches,
  but matching binary stats, date filtering, and merge behavior costs more code;
  low priority outside the shipped CLI. `tools/complexity/render.py` also resolves
  HEAD using Git; go-git would require a Go bridge/rewrite.
- `scripts/{create-nightly-tag,check-pr-binaries,migrate-sessions,test-attribution-e2e,test-copilot-token-metadata}.sh`,
  `.claude/skills/test-repo/test-harness.sh`, `mise-tasks/{bench/compare,dup/staged,lint/gomod,lint/secretpatterns,release}`,
  and release/nightly/publish workflows: keep native Git in shell/CI. Replacing
  these with go-git means porting tooling to Go, not replacing a Go subprocess.
- `.cursor/install.sh`: version/reftable feature probes must test installed Git.
  Installer/container package installation is likewise not a go-git candidate.
- Agent extension/plugin strings and runner JSON include Git-related instructions
  and hook arguments; these are not additional direct Go Git subprocesses.

## Migration acceptance criteria

For each selected group, add focused native-Git parity tests in isolated repos:
ordinary + linked worktrees, SHA-1/SHA-256, loose/packed/reftable refs, unborn and
detached HEAD, shallow/partial clones, unusual path bytes, and errors vs absence.
Add concurrency tests for any mutation and verify no unwanted index writes.

Retain the existing filesystem anchors, settings trust gates, and repo-selector
semantics. Benchmark actual hook/command latency: eliminating one subprocess is
not a win if the replacement walks the entire worktree or graph. Update stale
v5 comments and safety documentation only alongside a validated migration.

## Characterization tests added after the static audit

The first migration group now has native-baseline tests in:

- `cmd/entire/cli/git_branch_validation_test.go`: literal names and previous-checkout expansion.
- `cmd/entire/cli/gitexec/gitexec_test.go`: SHA-1/SHA-256 × files/reftable, packed refs,
  detached/linked HEAD, target directory versus CWD/environment, errors and cancellation.
- `cmd/entire/cli/gitops/diff_{parity,layout}_test.go`: modes, symlinks, gitlinks,
  binary content, unusual paths, rename endpoints, index preservation, repository
  formats, linked worktrees, shared-clone alternates, root/non-root behavior and failures.
- `cmd/entire/cli/checkpoint/remote/shallow_parity_test.go`: explicit/implicit root,
  bare/linked repositories, unshallow, empty/malformed shallow files and failed reads.
- `cmd/entire/cli/head_checkpoint_read_test.go`: detached/linked/subdirectory reads
  and checkpoint lookup failures.
- `cmd/entire/cli/local_ref_read_test.go`: commit-typed metadata tracking refs and
  attached/unborn/detached branch reporting.
- `cmd/entire/cli/strategy/{ref_read_parity,tracking_ref_read}_test.go`: ref peeling,
  nested annotated tags, non-commit objects, missing/corrupt refs, packed/linked
  reads, exact remote-prefix matching and native deletion visibility.
- `cmd/entire/cli/repo_remote_read_test.go`: included/worktree config, deduplicated
  remote names and read failures.

Important compatibility findings pinned by these tests:

1. `ValidateBranchName` expands `@{-1}` today. A literal-only validator must not
   silently replace this behavior without a deliberate contract change.
2. `HeadSHA` honors exported repository selectors even when an explicit directory
   is supplied. Do not substitute `OpenPath` without preserving caller selection.
3. An existing empty shallow file yields true from native Git.
4. `diff-tree --root <non-root-commit>` still compares against its parent; an empty
   base argument must not unconditionally enumerate every file in the target tree.
5. Remote enumeration includes config includes and worktree-local remote entries.
6. The native branch-display fallback reports unborn HEAD as detached, not as the
   symbolic branch name; changing that behavior is separate from replacing the exec.

These tests characterize the current implementations, not the correctness of a
future go-git replacement. Production operations remain unchanged. No benchmarks
or real-agent E2E were run for this work.
