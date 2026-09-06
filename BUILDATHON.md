# Checkpoint Lens

## One-sentence summary

`entire lens` turns the context inside real Entire Checkpoints — stated intent,
decisions, rejected options, open questions, attribution — into four working
developer workflows: hand off, review against intent, list unfinished
requirements, and assess a single change — and it can **fail a build** when the
implementation has drifted from the plan.

## Problem, intended user and why it matters

**The user:** a developer or coding agent picking up work they did not just
finish — after a break, after a handoff, or when reviewing someone else's
agent-assisted branch.

**The problem:** git records *what* changed. It does not record why the change
was made, what was tried and abandoned, what the author assumed, or what was
still unresolved when they stopped. With AI agents producing large diffs
quickly, that gap is now the expensive part: the diff is cheap to generate and
expensive to understand.

Entire Checkpoints already capture that missing context — prompts, transcripts,
decisions, attribution — but as raw material. Reading a 1.1 MB `full.jsonl` is
not a workflow. Checkpoint Lens is the layer that turns it into one.

**Why it matters:** the concrete failure this prevents is resuming work by
reading a diff, re-deriving the plan from the code, and silently dropping the
requirement the previous session had already identified but not yet built.

## Selected Entire track and why Entire is essential

**Track 1 — Build a Checkpoint-Native Developer Experience.**

Checkpoint context is not decoration here; it is the input. Remove it and every
command loses its primary source:

| Command | Without checkpoints |
| --- | --- |
| `handoff` | no intent, no decisions, no open questions — only a file list |
| `drift` | no plan to compare against; nothing to measure drift *from* |
| `assess` | degrades to a plain diff with no intent to cross-reference |
| `sync` | nothing to aggregate |

A concrete, measured demonstration that checkpoint data is load-bearing *and*
that it must be combined with git rather than trusted alone: for checkpoint
`01M1TH4V7EFETXQ0QGNR2E871W`, the checkpoint's own `files_touched` records **2**
files while the linked commit changed **5**. The reader unions the two, so the
Graph blast-radius analysis sees `.entire/settings.json` and
`BUILDATHON_HANDOFF.md` — files a checkpoint-only read would have missed, and
whose *purpose* a git-only read could not have explained.

Entire is also the delivery mechanism, not just the data source: `entire-lens`
is resolved through the CLI's own kubectl-style external-command lookup, so
`entire lens ...` is a first-class Entire subcommand rather than a side script.

## Architecture and main workflow

```
refs/entire/checkpoints/<2>/<ULID>   real checkpoint data (git-refs backend)
              |
              v
   CheckpointReader (reader.py)      git plumbing only; never checks anything out
              |                      + Entire-Checkpoint trailer -> commit link
              |                      + union with git commit stat
              v
       CheckpointRecord / SessionRecord (models.py)   <-- ONE schema
         |            |             |            |
         v            v             v            v
     report.py    graph.py      requirements  databricks.py
     (terminal)   entities.py   .py (drift)   (Delta + aggregates)
         |            |             |            |
         +------------+------+------+------------+
                             v
                    cli.py  (entire lens ...)
```

One schema (`models.py`) is shared by the terminal report and the Delta tables,
so a number shown locally and the same number aggregated in Databricks cannot
drift apart.

**Main workflow (the one to watch):**

1. `entire lens handoff` — reconstruct a stopped session: intent (with its
   provenance), per-session attribution, decisions/risks/open questions
   recovered verbatim, files touched, and the Graph blast radius of the commit.
2. `entire lens drift` — extract requirements from the *first* checkpoint's
   stated plan, search the current codebase for each, and report coverage plus
   what is still open. Adds an entity-level `graph diff` from the plan's commit
   to now.
3. `entire lens assess <commit>` — entity-level view of one change, plus an
   intent cross-reference listing files the change touched that its checkpoint's
   plan never mentioned.
4. `entire lens sync` — push records to Databricks, read cross-session
   aggregates back.

**The capability that makes this more than a report generator:**
`entire lens drift --fail-on-open` exits non-zero when a requirement stated in
the plan has no implementation evidence. "Did we build what we said we would
build?" becomes a pipeline gate, failing the same way a red test does.
`entire lens assess <commit> --verify "<test cmd>"` closes the other half:
it runs the tests through `entire graph verify`, which reports *which tests
changed state* rather than dumping runner output — so a test that was already
failing is not counted as evidence against the change under review.

Both `handoff` and `drift` accept `--html PATH` and emit a single
self-contained file — no server, no CDN, no external fonts — which is the
fallback when live infrastructure fails during a demo. Samples are committed
under `docs/demo/`.

## Entire Graph findings and verification

Graph output is treated as **evidence, not an oracle**. Every Graph-backed
section prints the exact command that produced it so a reader can rerun it, and
a graph that cannot answer renders as *unavailable* — never as "no impact".

**Required action 1 — search / definition lookup.** Used to locate checkpoint
storage and parsing in this codebase (`api/checkpoint/metadata.go`,
`cmd/entire/cli/checkpoint/open.go`), which is what established that this repo
uses the **git-refs** backend rather than the legacy `entire/checkpoints/v1`
branch. **Verified against source** by reading `.entire/settings.json`
(`"checkpoints": {"primary": {"type": "git-refs"}}`) and listing the live refs.
The original plan in checkpoint 1 named the v1 branch; the graph finding
corrected it, and `reader.py` reads refs as a result.

**Required action 2 — relationship / impact analysis before a high-risk
change.** `entire graph commit <sha> --repo . --json` before changing the
blast-radius code path. This produced the run's most valuable finding: the
change list is `files[].changes[]` carrying `dependents_count`, which is what
lets the tool flag a *signature change or removal that has callers* — the shape
that breaks things silently. Running it against our own HEAD reported
`.gitignore body_changed` with 53 dependents.

**Required action 3 — final semantic diff of the submitted implementation.**
`entire graph diff --base <plan commit> --head <final commit> --json`, reported
in `entire lens drift`: **75 entity changes across 9 files** between the plan
and the implementation. Entity-level is the point — a function that only moved
is not counted as drift, which a raw text diff would have false-flagged.

**Where the Graph was wrong, and how we caught it.** Three interface defects
were found by verifying against real payloads rather than trusting our first
call. All three had the same dangerous shape — a *quiet* failure:

- `entire graph commit` takes a **positional** rev and `--json`. Called with
  `--commit/--format` it **exits 0** and prints `commit accepts at most one
  revision` to stdout. A trusting caller renders an empty blast radius, which
  reads as "this change has no dependents."
- `entire graph search` takes `--top-k`, not `--limit`. Every requirement was
  reporting UNVERIFIED, indistinguishable at a glance from a query the graph
  genuinely could not answer.
- Search results are keyed on `file_path` + `snippet`, not `symbol`/`name`, so
  the hit extractor was scoring against nothing.

This is exactly why the operating guide says to verify. All three are fixed and
the fix is pinned by tests that assert a failing graph reports `ok=False` rather
than an empty section.

## Noon Curveball: what changed and how we adapted

> **Not yet received at the time of writing.** The curveball is revealed at
> 12:00 IST. This section will record: the constraint, the assumption it broke,
> the Graph impact analysis run before touching the affected area, the change
> made, and the test that proves the new behaviour.

The pre-noon stable state is checkpoint `01M1TM6JKPX4MV0DV829FQ77GA`
(commit `9ee636fe1`).

## Checkpoint links and what each checkpoint proves

Checkpoints are on the `RV/Entire_Trunk` branch of the fork, pushed to the
Entire mirror (`entire://aws-ap-south-1.entire.io/gh/rohithvishnukumar/entire_cli_kernel_001`).
Read any of them with `entire checkpoint explain <id>`.

| Checkpoint ID | Commit | What it proves |
| --- | --- | --- |
| `01M1TH4V7EFETXQ0QGNR2E871W` | `34c1528d8` | **Milestone 1 — initial understanding and intended architecture.** Problem framing, why Entire is essential, planned CheckpointReader + GraphClient + Databricks architecture, and the open questions. No product code yet, deliberately. |
| `01M1TM6JKPX4MV0DV829FQ77GA` | `9ee636fe1` | **Milestone 2 — last stable state before the Noon Curveball.** Working `handoff` on real data. Records the deliberate architecture deviation from milestone 1 (Go → Python-as-Entire-subcommand) *with the reason*, plus unresolved work and open risks. |
| `01M1TM6ZCD0CP72K2HW6S945KY` | `6ca259776` | Hygiene: committed bytecode removed. Shows up later as noise in the file-churn aggregate — an honest artefact of our own history. |
| `01M1TM9WCDTJJBYQNFEDCBJ04N` | `dda47c39a` | **Graph evidence + a real bug.** The silent `graph commit` interface defect, found by verification rather than assumption. |
| `01M1TNJQEJTDEK7Y6MSGR5KBBQ` | `c1e0521a0` | `drift`, `assess`, Databricks sync, 40 tests. Two further silent graph-interface bugs found and fixed. |

The milestone-2 checkpoint is the one worth opening: it records a *rejected*
option (installing Go) with the reasoning, which is precisely the context git
alone cannot preserve.

## Setup, run and test instructions

Requires Python 3.9+, `git`, the `entire` CLI, and the `graph` plugin.

```bash
git clone <fork url> && cd entire_cli_kernel_001
git checkout RV/Entire_Trunk

# Run directly (no install needed):
python -m tools.checkpoint_lens.cli handoff --repo .

# Or as a first-class Entire subcommand (put the repo root on PATH):
#   PowerShell:  $env:PATH = "$PWD;$env:PATH"
#   bash:        export PATH="$PWD:$PATH"
entire lens handoff --repo .
entire lens drift   --repo .
entire lens assess  HEAD --repo .
entire lens sync    --repo .          # requires Databricks credentials

# Every command supports --json, --no-graph and --no-databricks.
```

**Tests** (stdlib `unittest`, no pip install required):

```bash
python -m unittest discover -s tools/checkpoint_lens/tests -t . -v
# 40 tests
```

**Databricks credentials** are never committed. Provide either environment
variables (`DATABRICKS_SERVER_HOSTNAME`, `DATABRICKS_HTTP_PATH`,
`DATABRICKS_TOKEN`) or a gitignored `.databricks.local.json` in the repo root:

```json
{ "server_hostname": "...", "http_path": "/sql/1.0/warehouses/...",
  "access_token": "...", "catalog": "workspace", "schema": "checkpoint_lens" }
```

Install the optional connector with `pip install databricks-sql-connector`.
Without it, every command still runs; only the cross-session section is absent,
and it says so.

## Databricks use, data sources and limitations

**Capabilities used:** Delta tables on Unity Catalog, queried through a
serverless SQL warehouse via `databricks-sql-connector`.

**Workspace:** `dbc-fbf9202b-29fc.cloud.databricks.com`
**Schema:** `workspace.checkpoint_lens`
**Warehouse:** `/sql/1.0/warehouses/08d05243c52e3597` (one 2X-Small serverless,
the Free Edition allowance)

**Tables** (`tools/checkpoint_lens/databricks.py`):

| Table | Grain |
| --- | --- |
| `checkpoint_sessions` | one row per session per checkpoint |
| `checkpoint_files` | one row per file per checkpoint |
| `checkpoint_decisions` | one row per recovered decision/risk/open question |

**Why it is essential rather than storage.** The three aggregates are chosen so
that none can be computed from a single checkpoint:

- `open_items_trend` — is unresolved context *accumulating or being discharged*
  across the whole project history? A blocker raised in checkpoint 1 and still
  unanswered at checkpoint 5 is invisible to any single-checkpoint view. Live
  result: unresolved fell from 13 at milestone 1 to 1 thereafter.
- `file_churn` — hotspots are a property of the *sequence* of checkpoints, not
  of any one commit.
- `coverage` — intent-coverage %, decisions captured, and average agent-written
  percentage across all sessions.

Delete the Databricks layer and the tool still runs; every trend and ranking
degrades to a single point in time, and the CLI prints that explicitly rather
than presenting one checkpoint's numbers as a trend.

**Data provenance.** Every row is derived from *this repository's own* Entire
Checkpoints, created by the author and their coding agent during the event. No
third-party, customer, personal or confidential data is uploaded. Free-text
columns (`intent`, decision `text`) are the author's and the agent's own prose,
truncated to 800 characters before upload — these tables exist for aggregation,
not to rehost transcripts. Credentials are gitignored and never committed.

**Two data-quality decisions that materially change the numbers**, both worth
inspecting:

- *Decisions are attributed to the checkpoint where they FIRST appeared.*
  Entire stores the whole compacted session in every checkpoint, so counting
  raw occurrences made a single blocker look like it was raised again on every
  later commit and turned the unresolved-items trend into a monotonically
  increasing line. First-appearance attribution answers the question the trend
  is actually asking — *when was this raised* — and is what makes a falling
  line mean what a reader assumes it means.
- *Build artefacts, vendored code and lockfiles are excluded from churn.* This
  repository's own committed-then-deleted `__pycache__` sat at the top of a
  ranking that is supposed to point a reviewer at risky source.

**Credential handling, verified rather than asserted.** Databricks credentials
were pasted into a working session during the build. They reached no
checkpoint: Entire's own redaction pipeline caught the token (11 `REDACTED`
markers in the transcript), and a scan of every checkpoint ref, every tracked
file, the entire git history and the generated HTML found zero occurrences.
The credentials file is untracked and gitignored.

**Sync semantics.** Idempotent: a `DELETE` scoped to the repo key followed by
batched inserts, so re-running after new checkpoints never double-counts, and
one workspace can host several repos.

**Limitations.**
- Current volume is small (5 checkpoints, one repo, one day). Trend lines are
  real but short; nothing here should be read as a statistically meaningful
  trend yet.
- `file_churn`'s top entries currently include `__pycache__` `.pyc` files that
  were committed and then removed in our own history. That is honest data, not
  a bug, but it is noise in a risk ranking — path filtering is the obvious next
  step.
- Free Edition offers no production SLA; if the warehouse is unavailable during
  judging the CLI degrades gracefully and says the cross-session view is
  unavailable.

## Known limitations and next steps

**Honest limitations:**

1. **Decision extraction is a transparent classifier, not a language model.**
   Chosen deliberately so output is verbatim, auditable, offline and incapable
   of inventing a decision that was never made. It went through three rounds of
   precision work against real output, each pinned by tests: prompt echoes are
   suppressed (a decision is what the agent *concluded*, not what the user
   *asked for*); markers must be predicated of the work rather than merely
   mentioned (the word "risk" inside the column name "file-churn/risk score" is
   not a risk); and causal prose is captured as `rationale`, which turns out to
   be the most abundant form the "why" actually takes. It still cannot
   recognise a decision stated in a form no marker covers.
2. **`drift` verdicts are heuristic.** The score is the fraction of a
   requirement's keywords appearing in Graph search hits. `MISSING` means *no
   evidence was found* — a prompt to verify, not proof of absence — and the
   report says so on every run.
3. **Requirement extraction depends on the plan being written down.** When the
   baseline checkpoint's prompts contain a pasted planning document, extraction
   picks up non-requirements; metadata and logistics are filtered, but the
   filter is heuristic.
4. **Single-repo assumption in the Databricks key.** The repo key is the
   directory basename, so two clones of the same repo in differently-named
   directories would be treated as different projects.
5. **`drift` costs one Graph search per requirement.** Now concurrent and on
   the `fast` profile (58s for 18 requirements, down from 4m01s), but it is
   still the slowest command.
6. **Not tested on a repo other than this one.** The tool takes `--repo` and
   makes no assumptions about its own layout, and it is covered against an
   empty git repo, but it has not been exercised against a large third-party
   checkpoint history.

**Next steps toward production readiness:**

- Replace the keyword classifier with a checkpoint-summary-backed intent
  source: `entire checkpoint explain --generate` populates `Summary.Intent` and
  `Summary.OpenItems`, which the reader already prefers when present. That
  would raise precision substantially while keeping the current path as an
  offline fallback.
- Persist drift verdicts per checkpoint so `open_items_trend` can distinguish a
  requirement that was *completed* from one that was silently *dropped* — the
  single most useful thing this data could support and the natural next
  Databricks query.
- Path filtering and a weighted risk score (`churn × dependents`) for the
  hotspot ranking.
- A `--fail-on-open` exit code so `drift` can run in CI as a release gate.
