# The bug: OPF's batch cap is smaller than a single real checkpoint

`OPF-INVESTIGATION.md` in this worktree asked whether OPF had ever run on a real
transcript. The answer came back **no** — zero commits carrying
`Entire-OPF-Applied` anywhere. That is the *corroboration*. This file is the
finding it corroborates.

## The defect

`batchDefaultLimit = 2 * 1024 * 1024` prose-leaf bytes
(`cmd/entire/cli/strategy/manual_commit_opf_rewrite.go:185`) is **below the size
of one real checkpoint's transcript**. Exceed it and
`RewriteQueuedCheckpointRefsWithOPF` / `RewriteUnpushedV1WithOPF` return
`OPFBatchTooLargeError` and rewrite **nothing**.

Measured against 504 real checkpoint refs in a working checkout
(`.worktrees/linear-2`):

| | |
| --- | --- |
| local checkpoint refs | 504 |
| median checkpoint tip-commit tree, raw | ~5 MB |
| tip trees exceeding 2 MiB **raw** (sample of 20) | **18** |
| a median tree's `0/full.jsonl` alone | 3.9 MB |
| prose-leaf ratio of a real `full.jsonl` | ~0.84 |
| whole un-trailered ancestry per ref, raw (n=80) | median ~9.5 MB |

`isRedactableBlobName` (`manual_commit_opf_rewrite.go:613-615`) excludes exactly
one file, `content_hash.txt` — so `full.jsonl`, the session transcript, is in
the batch. The cap counts almost exactly the thing checkpoints are made of.

So: **with OPF enabled, an ordinary push carrying one real session already trips
the cap.** No unusual configuration, no backlog, no destination change required.

## Both backends, different failure modes

Both share the cap — `resolveBatchLimit()` and `redact.SumProseLeafBytes` are
called identically at `manual_commit_opf_rewrite.go:372-373` (git-branch/v1) and
`manual_commit_opf_refs.go:132-135` (git-refs).

- **git-branch** — the rewrite error propagates out of `prePush`, so the user's
  `git push` **aborts**. Loud and immediate.
- **git-refs** — `opfGateForCheckpointRefs` warns and withholds the flush
  (`manual_commit_push.go:441-444`), and the user's push succeeds. The queue is
  untouched, so the next push builds the identical batch and fails identically.
  **Quiet, and permanent.** The only documented way out is
  `ENTIRE_OPF_BATCH_LIMIT`.

## Why nobody has reported it

`redaction.openai_privacy_filter.enabled` defaults to **off**, and it isn't
enabled in this repo. Combined with zero OPF-stamped commits anywhere, the
likeliest explanation is that the feature has never been exercised against
real-sized content — only against small synthetic test fixtures with a faked
runtime.

"OPF aborts every push I make" (git-branch) would have been reported within a
day. It hasn't been.

## What needs deciding

1. **Is 2 MiB the right number?** Its comment sizes it by inference time —
   "2 MB at ~5.4s/100KB ≈ ~110s… generous enough that any realistic push fits."
   The first half may be right and the second half is measurably false. What is
   the real cost of a 4 MB transcript at that rate, and is a multi-minute pause
   on every push acceptable? If not, the feature needs a different shape, not a
   bigger number.

2. **Does the measure double-count?** `collectTreeBlobs` walks the whole tree of
   **every** un-applied commit, and checkpoint ref chains carry near-identical
   trees — on at least one ref, two consecutive commits have byte-identical
   trees. `SumProseLeafBytes` explicitly does not dedup
   (`redact/batch.go:196-201`) while the batch redactor dedups by leaf text. So
   the cap may be enforcing a cost that isn't actually paid. Deduping the
   collect pass by blob hash looks like a few lines.

3. **Should the cap be cumulative at all?** Raw and prose-leaf caps are summed
   across the whole flush; the bootstrap cap is per-ref
   (`manual_commit_opf_refs.go:100-102`). Refs are otherwise independent — each
   is pushed separately, each has its own ancestry.

4. **`full.jsonl` vs `transcript.jsonl`.** Both are in the tree and both are the
   same session (3.9 MB and 369 KB in the sampled tree). Does OPF need to scan
   both?

## Rejected approaches, with reasons

Do not re-propose these.

- **Chunking the rewrite** (process as many refs as fit, leave the rest queued).
  Chunks must be ref-atomic — a commit chain can't be rewritten in parts, since
  each rebuilt commit is the next one's parent. With most refs individually over
  the cap, the "oversized ref processed alone" escape becomes the normal path,
  which silently disables the cap rather than preserving it. Full write-up, with
  an accurate diagnosis worth reading, in
  `.worktrees/linear-2/docs/superpowers/plans/2026-09-16-opf-must-make-progress.md`.
- **Running the v1 rewrite on a push where checkpoint delivery is gated.** The
  target carries no checkpoints, so `resolveRemoteV1Tip` returns zero, the whole
  local history is treated as a bootstrap, and `rebuildCheckpointCommit` builds
  the deepest commit with **no parents** (`:568-571`). Local
  `entire/checkpoints/v1` gets CAS-updated to an unrelated root chain and every
  later push diverges permanently. Repos with >100 unpushed v1 commits fail
  harmlessly on the bootstrap cap; smaller ones are damaged — the risk is
  inverted.

## What this blocks

`.worktrees/linear-2` holds a committed, green change
(`peyton/checkpoint-destination-redelivery`) that re-delivers checkpoints when
their destination changes. It doesn't break anything that works today, but it
adds another route into this hole, so it's on hold pending a direction here.
See `docs/superpowers/plans/2026-09-17-checkpoint-remote-handoff.md` there.
