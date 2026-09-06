# IntentLens

## One-sentence summary

IntentLens is a checkpoint-native developer intent auditing tool that compares a developer's intended requirements from an Entire Checkpoint against implementation, Git, Graph, and test evidence while enforcing a privacy boundary around raw Checkpoint data.

## Problem, intended user and why it matters

Modern AI-assisted development can produce large amounts of code quickly, but it is still difficult to answer a fundamental question:

> Did the implementation actually fulfill what the developer asked for?

IntentLens is designed for developers and engineering teams using AI-assisted coding workflows. It turns the development context captured by an Entire Checkpoint into a requirement-level audit of the resulting implementation.

Instead of acting as another generic code summarizer, IntentLens connects:

- Developer intent
- Checkpoint/session context
- Git changes
- Changed files
- Entire Graph relationships and impact
- Test evidence
- Requirement-level evaluation

This matters because a code change can compile and appear reasonable while still missing an important part of the original requirement. IntentLens makes that gap visible and provides evidence for why a requirement is considered implemented, incomplete, uncertain, or unverifiable.

---

## Selected Entire track and why Entire is essential

We selected **E1: Build a Checkpoint-Native Developer Experience**.

Entire is essential because the central problem depends on the relationship between development intent and the resulting code.

An ordinary Git diff tells us what changed, but it does not reliably capture what the developer intended to build. Entire Checkpoints provide the development-session context needed to reconstruct that intent and connect it to implementation history.

IntentLens uses Entire for:

- Checkpoint-based development context
- Checkpoint/session metadata
- Checkpoint-linked commits
- Entire Graph structural analysis
- Checkpoint-native audit workflows

The product is therefore built around an Entire primitive rather than simply wrapping an existing generic code-review workflow.

---

## Architecture and main workflow

The primary interface is:

    entire checkpoint audit <checkpoint-id|commit-sha>

The high-level workflow is:

    Entire Checkpoint
            |
            v
    Local checkpoint/session processing
            |
            v
    Privacy boundary
            |
            v
    Sanitized evidence package
            |
            +------> Entire Graph
            |
            v
    Evaluator
            |
            v
    Requirement-level audit findings

The implementation is divided into three major responsibilities.

### 1. Checkpoint audit command

The native `entire checkpoint audit` command:

- Resolves a Checkpoint ID or checkpoint-linked commit.
- Reads the relevant Checkpoint information.
- Collects implementation evidence.
- Connects the evidence pipeline to Graph/evaluator components.
- Produces human-readable and JSON audit output.

### 2. Evidence and privacy layer

Checkpoint data is processed locally.

The raw Checkpoint prompt/transcript is treated as local-only information. The privacy layer converts local information into a typed, sanitized evidence representation.

The sanitized representation can contain:

- Checkpoint identity
- Derived atomic requirements
- Context completeness
- Approved changed-file information
- Structural/Graph evidence
- Test evidence and provenance

It must not contain:

- Raw transcript
- Raw session logs
- Raw Checkpoint prompt
- Arbitrary JSON
- Unbounded sensitive content

### 3. Evaluator

The evaluator receives the sanitized evidence representation rather than unrestricted Checkpoint/session data.

The evaluator can therefore reason about implementation compliance without receiving the original private transcript.

When context is unavailable or redacted, the system explicitly represents that state rather than inventing missing intent.

---

## Entire Graph findings and verification

Entire Graph is used as structural evidence rather than as an authoritative source of truth.

Before implementing the privacy-boundary change, Graph analysis was used to trace the affected code paths from:

    Checkpoint/session data
        -> intent/evidence construction
        -> Graph
        -> evaluator
        -> external transport

This identified the important privacy-sensitive paths, including the previous use of raw prompt text as a Graph query and the unrestricted evidence contract.

The privacy change then moves the Graph input toward sanitized requirements and structural terms rather than raw Checkpoint content.

After implementation, Graph is used again to verify the affected code paths and confirm that the privacy boundary is preserved.

Graph evidence is always treated as supporting evidence and is verified against source code and tests rather than being treated as an oracle.

---

## Noon Curveball: what changed and how we adapted

The Noon Curveball introduced a **Privacy Boundary**:

- Raw Checkpoint prompts/transcripts must not be sent to a new external service.
- The product must remain useful when sensitive Checkpoint fields are redacted or unavailable.
- The interface must distinguish complete and incomplete context.
- Incomplete context must never be presented as authoritative.
- At least one redacted/missing Checkpoint case must be tested.
- Entire Graph must be used to identify affected code paths.

This changed our architecture.

The original direction could have allowed raw intent/evidence to flow into an external evaluator. We instead introduced a privacy boundary between local Checkpoint processing and external evaluation.

The revised flow is:

    Raw Checkpoint/session data
              |
              | LOCAL ONLY
              v
    Local requirement derivation
              |
              v
    Sanitization + completeness detection
              |
              v
    Typed sanitized evidence
              |
              +------> Entire Graph
              |
              v
    External evaluator
              |
              v
    Audit result

The system explicitly distinguishes:

    COMPLETE

from:

    INCOMPLETE

If intent is redacted, missing, or insufficiently available, the system does not invent what the developer intended. Instead, affected findings are reported conservatively as uncertain or unverifiable.

This allows the product to continue providing useful implementation, Graph, and test evidence even when the original Checkpoint context is incomplete.

---

## Checkpoint links and what each checkpoint proves

### Pre-curveball checkpoint

Checkpoint:

    01MITQAJT39H8...

Description:

    Add checkpoint audit evidence collection

This checkpoint establishes the stable pre-curveball implementation.

It demonstrates that the original checkpoint-audit functionality existed before the Noon Curveball and provides the baseline from which the privacy-boundary changes were developed.

### Final checkpoint

The final Checkpoint should correspond to the privacy-boundary implementation and explain:

- What changed because of the curveball.
- Why raw Checkpoint content remains local.
- How sanitized evidence is constructed.
- How COMPLETE/INCOMPLETE context is represented.
- How redacted/missing intent is handled.
- How Graph was used before and after the change.

Replace the placeholder below with the final Checkpoint ID before submission:

    <FINAL_CHECKPOINT_ID>

---

## Setup, run and test instructions

### Prerequisites

- Go installed and available on PATH.
- Git installed.
- Entire CLI installed and authenticated.
- Entire enabled for the repository.
- Entire Graph available if Graph-based analysis is being demonstrated.
- Gemini API credentials only if running the external evaluator path.

### Verify Entire

    entire status

Expected state:

    Entire: Enabled

### List Checkpoints

    entire checkpoint list

### Build

    go build ./cmd/entire

### Run the checkpoint audit

    entire checkpoint audit <checkpoint-id>

### JSON output

    entire checkpoint audit <checkpoint-id> --json

### Run help

    entire checkpoint audit --help

### Run tests

    go test ./...

For focused development/testing, run the relevant checkpoint, evidence, and evaluator packages individually.

### Privacy test

Use a test fixture containing an unmistakable fake secret such as:

    PRIVACY_TEST_SECRET_123456789

The test should verify that the value exists only in the local Checkpoint/session input and does not appear in the serialized external evaluator request.

The test should use a fake/injectable transport rather than making a real external API request.

### Redacted Checkpoint test

Run the audit against the supplied redacted/missing Checkpoint fixture.

Expected behavior:

    Context: INCOMPLETE

The audit should continue using available safe implementation and structural evidence, but it must not invent the missing developer intent or present an unsupported conclusion as authoritative.

---

## Databricks use, data sources and limitations (if applicable)

Databricks is **not required for the core IntentLens workflow**.

The primary data sources are:

- Entire Checkpoints
- Entire session metadata/context
- Git history and diffs
- Entire Graph
- Test evidence
- Sanitized evaluator input

No external dataset is required for the core product.

---

## Known limitations and next steps

### Current limitations

1. An unavailable or heavily redacted Checkpoint cannot provide information that is no longer present. IntentLens therefore reports uncertainty rather than attempting to reconstruct missing intent.

2. Historical test results are not automatically assumed from the presence of changed test files. Test evidence must have an explicit provenance.

3. Entire Graph provides structural evidence but does not replace source inspection, tests, or runtime verification.

4. The current implementation prioritizes the native CLI and privacy-safe audit workflow over a polished graphical interface.

5. Diff contents can themselves contain sensitive information, so raw patches should not automatically be treated as safe external evidence.

### Next steps

Potential future improvements include:

- A richer interactive audit interface.
- More sophisticated local requirement extraction.
- Stronger deterministic redaction and sensitive-data detection.
- Better source-level requirement-to-symbol tracing.
- More detailed test provenance.
- Support for corrective actions such as generating a constrained fix prompt for an incomplete requirement.
- A closed loop:

      Checkpoint
          -> Audit
          -> Missing requirement
          -> Constrained fix
          -> New Checkpoint
          -> Re-audit