# recall

Rust core for `entire recall`: checkpoint history as associative,
provenance-weighted memory. The Go side (`cmd/entire/cli/recall_*.go`) reads
Entire's checkpoint store and renders; every ranking decision lives here.
Design and rationale: `docs/recall/architecture.md`.

## Build

```sh
cd recall && cargo build --release      # binary at target/release/recall
cargo test                              # reference tests + additions
cargo run --release --example bench     # agreement() over the 840 labelled pairs
```

`entire recall` finds the binary via `$ENTIRE_RECALL_BIN`, then `recall` on
`$PATH`, then `recall/target/{release,debug}/recall` inside the repo.

## Graph cache: warm it before ingest

Ingest asks the code graph for each changed file's blast radius. The graph
caches its index **per HEAD commit**: the first query after HEAD moves rebuilds
the index (about 90 s in this repo); every later query is ~1.5 s.

1. Reach the commit you want to query from.
2. Warm the cache once:
   ```sh
   entire graph index --repo .
   ```
3. Build the memory:
   ```sh
   entire recall ingest            # ~1.5 s per impact query, capped at 40
   ```
4. Ask questions:
   ```sh
   entire recall why did we drop the retry wrapper
   entire recall --json what is unfinished in dispatch
   ```

**Moving HEAD** (commits, amends, rebases, checkouts) means the next ingest
pays the full re-index again; run step 2 again before step 3. `entire recall ingest --no-graph`
skips the graph entirely (fast, but the isolation check can never fire).

`.entire/recall/` is derived state, gitignored, and rebuilt from scratch on
every ingest.

## Privacy boundary: sensitive repositories

**Nothing leaves the machine, and you can check.** FluctlightDB is an embedded
database; the brain lives under `.entire/recall/brain`. The crate's dependency
tree does pull in `axum`, `hyper` and `tokio` through FluctlightDB, and it
ships a raft transport, so "embedded" is not the argument. The argument is
measured:

```sh
cargo build --release
recall/scripts/verify-offline.sh
# verify-offline: OK — 0 network syscalls across N traced processes (ingest + activate)
```

The script runs ingest and activate under `strace -f -e trace=network` and
fails on a single socket call. FluctlightDB's raft transport is behind its
`distributed` feature, which this crate does not enable, and its HTTP server
(`serve.rs`) is only reachable by calling it, which recall never does. The one
network activity on the ingest path is `git` fetching the repository's own
checkpoint refs from its checkpoint remote; that is Entire's existing store,
not a new service.

**What ingest keeps locally.** `.entire/recall/brain/checkpoints.json` holds a
plaintext copy of the (already redacted) user and assistant turns it indexed.
It is gitignored and the directory is created mode 0700, but it is a second
local copy of transcript text. A repository that does not want one runs:

```sh
entire recall ingest --no-transcripts
```

which never opens a transcript: every checkpoint is ingested as its commit
record (sha, subject, touched files, diff) with the transcript declared
unavailable, and every answer is marked PARTIAL.

**Redacted or unavailable fields.** The shim declares what it could not
provide in each checkpoint's `unavailable` list (`session`, `diff`, `files`,
`commit_message`). A checkpoint whose store entry cannot be read is **not
dropped**: its ledger record is built from git and ingested partial. On the
ranking side, a check that needs a missing field returns the fourth verdict:

| verdict | meaning |
|---|---|
| corroborated | an independent check agreed with the commit |
| neutral | the checks ran and found nothing either way |
| CONTRADICTED | a check found the claim at odds with the commit |
| UNVERIFIABLE | the check that would have decided could not run: its input was unavailable |

Unavailability is declared by the shim, never inferred from an empty field, so
a complete checkpoint takes exactly the path it always did — the benchmark
profile (P 0.869 / R 0.270 / Spec 0.927) is unchanged. An inline `REDACTED`
marker in a claim never changes routing either (17 of the 840 bench pairs
carry one); it adds the partial marker and the cap.

Every hit judged from partial context carries `partial: [...]` naming the
missing fields, and its confidence is capped at 0.50 — below any corroborated
hit, above the bare chat/intent baselines — so a partial hit can never outrank
a fully verified one. Ledger hits keep their confidence (the commit record is
itself complete) but carry the marker.

**The interface never presents partial as complete.** Every answer opens with
a context line, in text and in `--json` (`{"coverage": ..., "hits": [...]}`):

```
context: complete · 3 checkpoints · graph ok
context: PARTIAL · 3 of 43 checkpoints complete · 40 without transcript · graph ok
```

`complete` is printed only when every checkpoint seen was indexed whole, the
commit walk was not cut short by its budget, and the graph was consulted.

## Graph reach: how edges are found

The first `+` line of a new file is a package or import line and resolves to no
symbol, so impact-by-diff-line yields nothing. Ingest instead streams
`entire graph symbols` once, picks the first two code symbols each touched
file defines, runs `entire graph impact --head` on each, and unions the files
of their callers, type consumers and data flows. Files with no code symbols
(docs, JSON) get no edges. Nothing is fabricated: no graph, no edges.
