# entire recall — architecture

Design notes for `entire recall`, written before implementation and kept as
the rationale for the shape of `recall/` and the Go shim. The "expected to be
hard" and "might be wrong" sections were written up front and are left as
written; the README's privacy-boundary section records what changed since.

## What we are building

`entire recall` turns Entire checkpoint history into associative,
provenance-weighted memory. A fresh agent session asks "what did we decide and
why?" and gets ranked, trust-scored answers with the backing commit, instead of
a transcript dump.

Two verbs:

- `entire recall ingest` — walks the commits on the current branch that carry an
  `Entire-Checkpoint` trailer, reads each checkpoint's stored transcript, and
  writes engrams into a FluctlightDB brain under `.entire/recall/brain/`
  (gitignored: it is derived state, rebuildable from the checkpoint refs).
- `entire recall <question>` — activates the brain with the question, reranks
  the hits by provenance and by agreement with the ledger, and prints them.
  `--html` writes one static file with the same content.

## Architecture

```
 entire/checkpoints/v1 + Entire-Checkpoint trailers
                 │
        Go shim (cmd/entire/cli/recall.go, < 200 lines, no ranking logic)
        ├─ commits with trailers  → sha, subject, files, unified-0 diff
        ├─ checkpoint store       → transcript → user / assistant turns
        └─ JSON on stdin ────────────────────────────────┐
                                                         ▼
                                    Rust binary  recall/  (fluctlightdb)
                                    ├─ ingest:  checkpoints → engrams
                                    ├─ graph:   entire graph impact → file-edge map
                                    └─ activate: question → ranked hits (JSON out)
                                                         │
        Go shim renders text / --html  ◄─────────────────┘
```

The Go side is deliberately a shim: it knows how to read Entire's own storage
(the checkpoint store, the trailer parser, the transcript condenser) and how to
render. Every ranking decision lives in Rust so the benchmarked algorithm is
ported verbatim, not re-derived.

### Ingest mapping

| source            | engram                                            | salience |
|-------------------|---------------------------------------------------|----------|
| user prompt       | `Episode`, `ProvenanceKind::UserExplicit`         | 0.85     |
| assistant turn    | `Episode`, `ProvenanceKind::ChatAssertion`        | 0.45     |
| commit msg + SHA  | `Episode`, `ProvenanceKind::LedgerVerified`       | 0.90     |

Context string: `checkpoint:<id> commit:<sha> role:<role>`. Files changed become
edges to file-node engrams, and the graph impact surface adds file→file edges.

### Ranking

Never sort on raw activation. Provenance maps to `confidence::SourceKind`;
`agreement()` returns Corroborated / Neutral / Contradicted against the commit
the claim sits on; corroboration adds `Evidence(Verified)`, contradiction adds
`Evidence(Unknown, 0.15)` and multiplies confidence by 0.45; the score is
`activation * activation_multiplier(confidence)`; then sort and dedupe.

`agreement()` checks, in order: scope (isolation claim vs graph reach), file
(named path never touched), polarity (word-boundary tokens only), M1 identifier
anchor (code-shaped token absent from diff vocabulary), lexical (≥2 shared
terms), 4-gram cosine (> 0.10). No IDF anchor: it was measured to wreck
specificity (0.72 → 0.51).

Benchmark to preserve: P 0.800, R 0.624, Spec 0.720, ~339 µs/pair on the 840
labelled pairs from real entireio/cli commits.

## Why FluctlightDB

- **Spreading activation, not vector search.** The question is answered by
  association through a graph of engrams (prompts, assertions, commits, files),
  so a paraphrased question still reaches the commit it is about via the
  turns that surrounded it. No embedding model, no network, deterministic.
- **Provenance is a first-class field.** `Episode.provenance` distinguishes a
  user's stated intent from an assistant's assertion from the ledger record;
  `confidence::recall_confidence` fuses independent evidence via noisy-OR. That
  is exactly the trust model the product needs, and it already exists.
- **Durable on disk, reopenable read-only.** `open` / `checkpoint` /
  `open_readonly` give an on-disk brain under `.entire/recall/` that survives
  process restarts, so ingest and query are separate invocations.
- **Cheap.** The validated reference runs at 339 µs per claim/commit pair.

## What we expect to be hard

1. **Real transcripts are not the fixture.** The fixture has two clean turns per
   checkpoint. Real Claude Code transcripts have long tool-heavy assistant
   turns, system reminders, and multi-session checkpoints. The condenser
   (`summarize.BuildCondensedTranscriptFromBytes`) gives user/assistant entries;
   we will truncate assistant turns and skip tool entries, and expect noise.
2. **The graph is slow cold.** `entire graph impact` re-indexes at ~30–90 s
   per process without a warm cache; with `entire graph index` warmed and
   `--head`, a query is ~1.5 s. Ingest must query `--head` and tolerate the
   graph being unavailable (fall back to no reach edges, never fail ingest).
3. **Line numbers drift.** Impact is queried by `<file>:<line>` against HEAD,
   but a checkpoint's diff refers to the tree at its own commit. For older
   commits the enclosing symbol may have moved; we accept approximate reach.
4. **The trailer is only stamped when a session has content.** Commits made
   outside an active agent session carry no `Entire-Checkpoint` trailer and are
   invisible to recall by design. Verified on this repo: the scaffold commit
   has no trailer and `entire checkpoint list` is empty.

## What might be wrong

- The 4-gram cosine threshold (0.10) and the lexical threshold (≥2 terms) were
  tuned on commit *subjects*; real transcripts may produce many Neutral
  verdicts, which would make the reranking look inert on real transcripts.
- The M1 identifier anchor assumes the diff vocabulary covers what the claim
  names. A claim naming a symbol from a file the commit *reads* but does not
  change would be marked Contradicted. This is the documented precision/recall
  trade and we keep it, but it is the most likely source of a surprising
  verdict in practice.
- Using the callers of the symbol at the first changed line as the "reach" of
  a file is a proxy for the whole file's blast radius. It is the cheap,
  explainable version, not the exhaustive one.
- FluctlightDB is pinned to a git dependency; a build break there is a build
  break here. `cargo build` currently passes in 26 s, and we vendor nothing.
