# entire recall — evaluation

How `agreement()` was measured, what the numbers mean, what was tried and
rejected, and where the approach stops generalizing. `architecture.md` records
the design as written before implementation; the figures here supersede the
"benchmark to preserve" line in its Ranking section, which was taken from the
pre-port reference.

## Benchmark

The published benchmark for message–code inconsistency is CodeFuse-CommitEval
(arXiv 2511.19875). Its dataset is unavailable — the LFS objects 404 — so the
fixture here is reconstructed from the paper's published taxonomy: 300 real
commits from this repository's history, mutated under four of its seven rules
(operation-type, file-path, function-name, component mismatch), producing 840
labelled claim/commit pairs in `recall/bench/bench_840.json`.

`agreement()` as shipped, over those pairs:

| metric      | value        |
| ----------- | ------------ |
| Precision   | 0.869        |
| Recall      | 0.270        |
| Specificity | 0.927        |
| Latency     | ~145 µs/pair |

```sh
cd recall && cargo run --release --example bench
```

The bench runs with an empty graph, so the scope check never fires there; it
measures the five text-only checks.

## Reading the recall figure

0.270 is low on purpose, and the tool should be described as a high-precision
screen rather than a comprehensive detector.

Against the paper's average across six LLMs (P 0.803, R 0.860, Spec 0.638) this
matches on precision, beats every model tested on specificity, and runs about
four orders of magnitude cheaper — while catching roughly a quarter of the
genuinely inconsistent claims.

Every failure is a false negative on corroboration: saying "unverified" where
"corroborated" was available. Nothing is invented in the other direction. For a
trust signal a developer leans on while resuming unfamiliar work, being right
87% of the time when it speaks and silent otherwise is more useful than flagging
more and being wrong more often.

## Rejected: IDF anchoring

An IDF-weighted variant of the identifier anchor raised recall to 0.81 and
collapsed specificity to 0.51. It was measured and rejected: a reviewer tool
that false-alarms on half of all healthy commits gets uninstalled. The
`grader_ref.rs` example retains the reference implementation it was compared
against.

## Generalization across repositories

The same generator was run against five further repositories in four languages,
4,961 labelled pairs in total. Only the 840-pair set ships in this repository.

| repo         | language   | pairs | P     | R     | Spec  |
| ------------ | ---------- | ----- | ----- | ----- | ----- |
| entireio/cli | Go         | 840   | 0.800 | 0.624 | 0.720 |
| fastapi      | Python     | 889   | 0.827 | 0.868 | 0.643 |
| fzf          | Go/shell   | 800   | 0.720 | 0.776 | 0.497 |
| express      | JavaScript | 805   | 0.672 | 0.850 | 0.303 |
| bat          | Rust       | 812   | 0.670 | 0.846 | 0.290 |
| requests     | Python     | 815   | 0.664 | 0.885 | 0.230 |

**The thresholded checks do not transfer.** Specificity ranges from 0.23 to 0.72
depending on commit style, so the n-gram and diff-miss thresholds need
per-repository calibration before this is safe to run outside a tuned repo. That
calibration is the first item of production work, not a footnote.

**The structural check does transfer.** The identifier anchor is structural
rather than tuned, and measured alone it holds specificity between 0.847 and
0.950 on every repository above, with precision never below 0.67 — across four
languages it was never designed against. It is the generalizable core.

## Fixture provenance and secret scanning

`recall/bench/bench_840.json` is generated from this repository's own public
commit history; no private, customer, or personal data is involved.

**GitHub secret scanning flags an AWS Access Key ID inside it.** That string is
Entire's own fixture for exercising secret redaction. It is already present
across many commits in this repository, it is allowlisted as test data, and it
is not a live credential. Push protection may still warn on the file.

## Known limitations

- **The semantic lane is dark.** Engrams are ingested without embedding vectors,
  so activation runs on the lexical index alone and a paraphrase — a claim that
  agrees with a commit in different words — reads as Neutral rather than
  Corroborated. Populating `semantic_vector` with a local embedder is the
  single highest-value next step.
- **Real transcripts are noisy.** A pasted docs link no longer reads as an
  untouched repository path (pinned by `url_in_claim_is_not_a_file_path`), but
  prose containing a slash can still be misread. Long single-turn prompts also
  degrade ranking: a 2.5 KB brief matches almost any question by sheer term
  count, and one false path token then contradicts the whole turn. The fix is
  per-sentence claims rather than per-turn, which is ingest work rather than
  ranking work.
- **Graph cache invalidation.** Moving HEAD invalidates the graph cache and
  re-indexing costs about 90 s, so the first ingest after a commit is slow. See
  `recall/README.md` for the warming procedure.

**Next steps, in order:** populate semantic vectors to close the paraphrase gap;
per-repository threshold calibration; incremental ingest so recall stays fast as
history grows; and surface `recall` output inside an agent session rather than
as a separate command.
