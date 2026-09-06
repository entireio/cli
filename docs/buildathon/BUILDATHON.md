# Entire Echo

## One-sentence summary

Entire Echo is an external `entire-echo` plugin that reconstructs one checkpoint into an evidence-linked, accessible review and continuation aid.

## Problem, intended user and why it matters

The intended user is not specified more narrowly than a user who needs to understand what a checkpoint requested, implemented, left uncertain, or may affect. Echo turns checkpoint metadata, scoped transcript content, prompts, stored summary, changed files, and an associated diff into a factual review that can be continued safely.

This matters because the review remains grounded in checkpoint and code evidence, preserves provenance, and provides an accessible linear view instead of requiring users to inspect raw or unbounded transcripts.

## Selected Entire track and why Entire is essential

Selected track: Bengaluru Tech Week Buildathon, Track E1 — Checkpoint-Native Developer Experience.

Entire is essential because Echo consumes the parent CLI’s public checkpoint output and relies on Entire’s checkpoint/session model, transcript scoping, summary fields, commit linkage, plugin mechanism, and `entire graph` command. Echo is not a second checkpoint store, agent runner, or autonomous code changer.

## Architecture and main workflow

Echo is a small external plugin named `entire-echo`; it does not modify the built-in command tree.

The initial workflow is:

1. Run `entire checkpoint explain <checkpoint-id-or-commit> --json` for machine-readable metadata.
2. Run `entire checkpoint explain <checkpoint-id-or-commit> --transcript` for the selected session’s checkpoint-scoped transcript.
3. Obtain the associated commit diff through local git when it can be resolved from the checkpoint/commit linkage.
4. Validate the metadata and retain the exact checkpoint ID, session ID, source command, and byte/line boundaries.
5. Reconstruct factual cards for the request, implementation, and continuation state.
6. Extract changed files and candidate symbols from the diff, then issue focused `entire graph search --repo . --profile full` queries.
7. Produce deterministic findings in four categories: implemented, missing/uncertain, potentially affected, and continuation.
8. Render an accessible terminal view or local web view, with optional explicit user-controlled speech.

The output also includes a versioned `--json` evidence bundle and an optional user-selected HTML/JSON export. The first demo uses the latest-session default; an explicit transcript/session selector is deferred.

## Entire Graph findings and verification

The available `entire graph` command is treated as a CLI capability, not an in-process client API. Its reported capabilities include Go call, type, and data-flow relations.

Echo should attach only returned Graph locations and relations as evidence, including the symbol, relation, file, and lines. These findings are labelled “potentially affected”; a Graph result is not treated as proof of runtime behavior. If Graph is unavailable, times out, has no result, or returns a partial result, Echo preserves warnings and either omits impact claims or downgrades them to potential.

No concrete Graph query results or verification run are provided in the initial architecture document.

## Noon Curveball: what changed and how we adapted

Not specified in the initial architecture document.

## Checkpoint links and what each checkpoint proves

No concrete checkpoint IDs or links are specified in the initial architecture document.

For a future demo, each checkpoint entry should identify the exact checkpoint, session, and commit and state what its evidence proves. The evidence model requires:

- Requested: checkpoint-scoped user prompt or stored prompt.
- Implemented: diff hunk and/or checkpoint transcript action, plus changed file.
- Existing summary: the relevant `Metadata.Summary` field, labelled “stored summary”.
- Possibly affected: a Graph symbol/relation/file/line result, labelled “potential”.
- Missing/uncertain: an explicit open item, friction item, absent expected evidence item, or reviewer question; never an unsupported fact.
- Continue safely: exact identifiers, changed files, open items, evidence links, and safe read-only commands.

## Setup, run and test instructions

The architecture specifies the intended invocation but does not provide validated build, installation, or test commands.

Intended invocation:

```text
entire echo <checkpoint-id-or-commit>
```

The plugin is expected to be discovered as `entire-echo` on `PATH`. It should invoke the installed `entire` binary and `entire graph` command through the inherited `PATH`; this is listed as an assumption that has not yet been verified.

The architecture does not specify a concrete test command. [Placeholder: add setup, run, and test commands after implementation.]

## Databricks use, data sources and limitations (if applicable)

Databricks use is not specified and is not part of the proposed architecture.

Data sources are local or provided by the parent CLI: checkpoint JSON output, checkpoint-scoped transcript output, stored prompts and summary, changed files, an associated local git diff when available, and local `entire graph` results. Echo should not depend on ambient credentials; checkpoints and diffs are not sent to a cloud reviewer by default.

Limitations include transcript-format differences across agents, unavailable or partial Graph results, unavailable diff linkage, and the possibility that imported or unusual checkpoints are metadata/transcript-only.

## Known limitations and next steps

Known limitations and unresolved decisions include:

- The project is at architecture/investigation stage; no product functionality is stated as implemented.
- The latest-session default is sufficient only for the first demo; multi-session comparison is deferred.
- Browser availability and browser speech synthesis are not verified.
- Voice interaction is only a stretch adapter and must have equivalent keyboard controls.
- The delivery form—terminal-only, local web app, or both—is undecided.
- Whether an approved local model/runtime exists is undecided; deterministic template synthesis is the defined fallback.
- The policy for optional exports and browser history is undecided.
- Whether Graph queries should be user-visible and replayable in the output bundle is undecided.
- Accessibility testing resources are not yet identified.

The proposed next steps are to verify plugin and Graph invocation on supported development machines, choose the judging delivery form, implement the deterministic evidence model and renderers, verify diff linkage for ordinary committed checkpoints, and conduct accessibility testing before the demo.
