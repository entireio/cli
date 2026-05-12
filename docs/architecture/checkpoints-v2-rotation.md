# Checkpoints v2 Rotation

This document summarizes how checkpoints v2 stores and rotates raw transcripts,
and how the push path keeps archived full-transcript generations disjoint.

## Ref Layout

Checkpoints v2 splits permanent checkpoint data from raw transcript data:

```text
refs/entire/checkpoints/v2/main
refs/entire/checkpoints/v2/full/current
refs/entire/checkpoints/v2/full/0000000000001
refs/entire/checkpoints/v2/full/0000000000002
```

`/main` is the permanent ref. It stores metadata, prompts, compact
`transcript.jsonl` data, and compact transcript hashes. Cleanup does not delete
or rewrite `/main`.

`/full/current` is the active raw-transcript generation. It stores chunked
`raw_transcript` files, such as `raw_transcript` and `raw_transcript.001`, plus
`raw_transcript_hash.txt` entries for recently written checkpoints.

`/full/<13-digit-number>` refs are archived raw-transcript generations. They are
cleanup units: when the newest checkpoint in an archived generation is outside
the retention window, cleanup can delete the whole ref and let git GC reclaim
its raw transcript blobs.

## Normal Write Path

`WriteCommittedWithSessionIndex` writes each checkpoint to two refs:

1. `/main` receives the permanent metadata and compact transcript data.
2. `/full/current` receives the raw transcript data.

After writing `/full/current`, the store counts checkpoints in that generation.
If the count is below the configured threshold, the write is done. If the count
reaches the threshold, the store attempts to rotate `/full/current`.

The current default threshold is 100 checkpoints per full-transcript generation.

## Local Rotation

Rotation is local-only. It does not fetch, push, or contact the checkpoint
remote. Remote publication happens later in the git `pre-push` hook.

When `/full/current` reaches the threshold, rotation does this:

1. Determine the next local archived generation number, for example
   `/full/0000000000007`.
2. Build an archive commit whose tree is the current generation plus
   `generation.json`.
3. Build a fresh orphan commit with an empty tree for the next
   `/full/current`.
4. Verify that `/full/current` still points at the commit read at the start of
   rotation. If another writer advanced it, abort without rotating.
5. Record a pending full-generation publication marker.
6. Update the local archived generation ref to the archive commit.
7. Compare-and-set `/full/current` to the fresh orphan commit.

The archive and orphan commits are created before the pending marker is written,
but commit creation only writes objects. Those objects are not reachable from
the v2 refs until steps 6 and 7 update the refs. This matters because the marker
contains the object hashes that pre-push needs, while still being recorded before
the visible generation reset occurs.

If recording the pending marker fails, rotation fails before either ref is
changed. If a later ref update fails, rotation removes the marker best-effort
before returning.

## Pending Publication Marker

The pending marker is local-only metadata stored under the git common directory:

```text
<git-common-dir>/entire-v2-rotations/pending.json
<git-common-dir>/entire-v2-rotations/pending.lock
```

The file is guarded by the CLI's cross-process file lock helper. It is not a
git ref and is never pushed to the checkpoint remote.

Each pending publication records:

```json
{
  "archive_ref_name": "refs/entire/checkpoints/v2/full/0000000000007",
  "archive_commit_hash": "<archive commit>",
  "previous_full_current_hash": "<old /full/current>",
  "reset_full_current_root_hash": "<new orphan root>",
  "queued_at": "2026-05-11T12:00:00Z"
}
```

The fields have specific meanings:

- `archive_ref_name`: the archived generation ref that must be published.
- `archive_commit_hash`: the local archive commit created during rotation.
- `previous_full_current_hash`: the `/full/current` hash observed when local
  rotation started. If nothing else has pushed, this is also the remote current
  hash being replaced.
- `reset_full_current_root_hash`: the root commit of the next local generation.
- `queued_at`: local diagnostic timestamp.

Multiple local rotations before one push produce multiple pending publications.
Pre-push publishes all pending archive refs before resetting remote
`/full/current`.

## Pre-Push Publication

The pre-push hook reads pending publications before pushing active v2 refs.

For pending local rotations, pre-push:

1. Drops stale pending reset publications until the newest queued
   `reset_full_current_root_hash` is in the live local `/full/current` history.
   This covers the crash or failure window where a marker was recorded but the
   reset never happened.
2. Pushes the remaining pending archived generation refs together.
3. Reads the current remote `/full/current` hash. If it already matches local
   `/full/current` (idempotent case after a successful prior push), clears the
   pending marker and returns.
4. Refuses to continue if the remote `/full/current` hash is not an ancestor of
   any pending archive commit. This is the explicit "is the remote covered by a
   local archive?" gate.
5. Pushes local `/full/current` using `--force-with-lease` anchored to the
   remote hash read in step 3.
6. Removes the pending marker only after archive refs and the current reset are
   published successfully.

The leased current update is the key difference from a normal push. Replacing
remote `/full/current` with a fresh orphan generation is not a fast-forward, so
normal `git push` rejects it. The lease anchors the reset to the remote hash
just observed, so the push only succeeds if no other writer raced ahead between
the read and the push. The ancestor check in step 4 is what enforces "the
remote current is covered by the local archive"; `previous_full_current_hash`
in the marker is informational and is not consulted for the lease.

### Example: Local Rotation With No New Work

```text
Before rotation:
  remote /full/current = C100
  local  /full/current = C100

Local rotation:
  local /full/0000000000007 = A7, parented on C100
  local /full/current = N7, a fresh orphan root
  pending marker expects remote current C100

Pre-push:
  push /full/0000000000007
  force-with-lease /full/current from C100 to N7
  clear pending marker
```

Final remote state:

```text
/full/0000000000007 contains the old generation
/full/current is empty and contains none of the archived checkpoint IDs
```

### Example: Local Rotation, Then More Work Before Push

```text
Local rotation:
  /full/0000000000007 contains old checkpoint IDs 1..100
  /full/current starts at orphan root N7

More local checkpoints are written:
  /full/current advances from N7 to C105 and contains IDs 101..105

Pre-push:
  push /full/0000000000007
  force-with-lease remote /full/current from old current to C105
```

The remote archived generation still contains only IDs 1..100. Remote
`/full/current` contains only IDs 101..105. The current generation does not need
to be empty at push time; it only needs to descend from the recorded
`reset_full_current_root_hash`.

### Example: Multiple Local Rotations Before One Push

```text
Local state:
  /full/0000000000007 contains IDs 1..100
  /full/0000000000008 contains IDs 101..200
  /full/current contains IDs 201..205

Pending publications:
  publication for /full/0000000000007
  publication for /full/0000000000008
```

Pre-push pushes both archived refs, then resets remote `/full/current` to the
latest local current head. Cleanup can later delete generations 7 and 8
independently.

## Normal Non-Fast-Forward Recovery

For ordinary additive divergence on `/main` or `/full/current`, recovery uses a
tree merge:

1. Fetch the remote ref to a temporary local ref.
2. Flatten the local and remote trees.
3. Combine entries by path.
4. Build a merged tree and commit.
5. Retry the push.

This works because checkpoint data is sharded by checkpoint ID. Different
checkpoints normally land at different paths.

This generic merge is safe for active generations only when both sides are still
adding checkpoints to the same generation. It is not used to publish a local
rotation reset.

## Remote Rotation Conflict Recovery

A separate recovery path handles the case where another machine already rotated
and pushed the remote generation.

The shape looks like this:

```text
Starting point:
  remote /full/current and local /full/current share C95

Machine A:
  writes 7 more checkpoints
  rotates
  pushes /full/0000000000007 and fresh /full/current

Machine B:
  still has old /full/current history
  writes 3 more checkpoints on top of C95
  pushes /full/current and gets non-fast-forward
```

If Machine B used the generic `/full/current` tree merge, the old generation
would be rehydrated into fresh remote `/full/current`, causing the same
checkpoint IDs to appear in both an archived generation and the new current
generation.

Instead, recovery detects remote archived generations and treats this as a
remote rotation conflict:

1. List remote `/full/*` archive refs.
2. Treat missing local archives and same-name/different-hash archives as remote
   rotation evidence.
3. Fetch candidate archived generations with complete object graphs.
4. Find the archive that shares history with local `/full/current`.
5. Merge local `/full/current` into that archived generation.
6. Update `generation.json` timestamps in the merged archive.
7. Push the updated archive.
8. Adopt remote `/full/current` locally.

### Example: User A Rotates, User B Has Local Work

```text
Shared base:
  /full/current has IDs 1..95

User A:
  adds 5 more checkpoints (IDs 96..100)
  rotation triggers at the 100 threshold
  pushes /full/0000000000007 and the fresh /full/current

Remote:
  /full/0000000000007 has IDs 1..100
  /full/current is a fresh orphan

User B:
  on stale shared base, adds 3 more checkpoints
  pushes /full/current and hits non-fast-forward

Recovery:
  B's local old-current work is merged into /full/0000000000007
  local /full/current is replaced with remote fresh /full/current
```

Final remote state:

```text
/full/0000000000007 has IDs 1..100 plus B's 3 checkpoints
/full/current remains fresh
```

Generation 7 can exceed the nominal threshold. That is acceptable. The more
important property is that each raw transcript is reachable from one generation,
not duplicated across the archive and current generation.

## Archived Ref Collisions

Outside the remote rotation conflict recovery path described above, archived
generation refs are treated as effectively immutable cleanup units. If pre-push
tries to publish a pending archive ref and the remote already has that same
archive name at a different commit, the push surfaces an error instead of
silently merging or renaming the local archive.

Automatic recovery here is intentionally conservative:

- Merging two archives could hide corruption or stale local state.
- Merging could also make a generation much larger than the rotation threshold.
- Renaming the local archive to the next number adds complexity and may not
  preserve the intended generation boundary.
- Overwriting the remote archive is not acceptable.

The expected common case is handled by remote rotation conflict recovery on
`/full/current`. A direct archive-name collision should be investigated from
the concrete error and logs.

## Cleanup and GC

Cleanup operates only on archived `/full/<N>` refs:

1. List archived generation refs.
2. Read each archive's `generation.json`.
3. If the newest checkpoint in the archive is older than the retention window,
   delete the whole ref locally and on the checkpoint remote.
4. Git GC can reclaim the raw transcript blobs once no refs reach them.

`/main` is never cleaned up this way. It contains the permanent, compact data
needed for checkpoint listing, explain, and API reads.

The rotation and recovery rules above are designed to preserve one cleanup
property: deleting an archived generation should make that generation's raw
transcript blobs unreachable unless they are also intentionally preserved
elsewhere.

## Failure Windows

Local rotation is deliberately ordered so the pre-push handoff exists before the
visible generation reset:

- If the pending marker cannot be written, rotation aborts before moving refs.
- If a ref update fails after the marker is written, the marker is removed
  best-effort.
- If the process stops after `/full/current` is reset, the pending marker
  remains and pre-push can publish the intended rotation.
- If a stale marker remains from an interrupted attempt where `/full/current`
  was never reset, including a stop after only the local archive ref moved,
  pre-push drops that stale reset publication before pushing pending archive
  refs. The raw transcript data is still reachable from local `/full/current`,
  so this does not lose checkpoint data.

These rules avoid the production failure mode where remote old
`/full/current` was merged back into a fresh local generation after rotation.
