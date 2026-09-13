# Checkpoint evidence collector

## Purpose

Provide a typed, reusable Go collector for `entire checkpoint audit <id>` without
implementing the Cobra command, a frontend, or any model integration.

## Boundary

The new `cmd/entire/cli/evidence` package accepts a checkpoint ID and returns a
normalized `Evidence` value. It reads checkpoint data through the public
checkpoint reader interfaces and Git data through `gitops`; it never shells out
to the Entire CLI.

## Evidence model

`Evidence` contains checkpoint metadata, developer intent and prompt provenance,
linked commits, per-commit patches, changed files, source references, and test
evidence. A checkpoint can link to more than one commit, so commit evidence
preserves all associations and identifies a primary association instead of
discarding data.

Patches and session context are caller-configurable and report truncation rather
than silently losing evidence. Source references describe the origin of each
piece of evidence; they do not read arbitrary repository files.

## Collection flow

1. Read checkpoint and latest session metadata/content through the checkpoint
   reader interfaces.
2. Derive intent from the checkpoint summary and stored developer prompts,
   recording the source of each value.
3. Resolve imported checkpoint commit anchors or normal `Entire-Checkpoint`
   commit-trailer associations with a narrowly reusable `gitops` resolver.
4. For every associated commit, obtain its changed-file list and patch through
   `gitops`, then normalize the results into the evidence model.
5. Emit an explicit historical test record with `status=unavailable`: checkpoint
   storage has no authoritative historical test-result artifact. The collector
   does not run current-checkout tests. The type nevertheless reserves a
   `current_checkout` scope so a future explicit runner cannot be confused with
   historical evidence.

## Errors and omissions

A missing checkpoint is an error. Missing commit associations, unavailable Git
objects, omitted transcripts, truncated patches, and unavailable historical test
results are represented in the returned evidence with an explicit status and
reason where applicable, rather than fabricated as successful evidence.

## Tests

Unit tests use isolated temporary Git repositories and fake checkpoint readers.
They cover checkpoint/intent normalization, trailer and imported commit linkage,
committed changed files and patches, source provenance, and the explicit
historical-test-unavailable record.
