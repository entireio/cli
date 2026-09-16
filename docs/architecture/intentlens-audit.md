# IntentLens checkpoint audit

`entire checkpoint audit <checkpoint-id>` resolves a real checkpoint and collects
local checkpoint intent, linked Git changes and changed test-file paths. The
terminal dashboard shows requirement IDs, statuses, confidence and recommendations.
`--requirement R2` expands evidence and full requirement text; `--json` emits
validated audit JSON.

## Privacy boundary

Checkpoint prose and evidence descriptions stay local. The local
`EvidencePackage` and its string wrappers are display metadata, not a guarantee
that arbitrary text is secret-free. The `Evaluator` interface instead accepts
`EvaluatorInput`, a snapshot constructed from bounded `RequirementSignals`
booleans. It has private fields, no strings or raw JSON, and generated ordinal
requirement IDs. Provider prompts are built only from those closed signals and
fixed code-owned descriptions. A unique arbitrary sentinel test and recursive
type inspection cover this boundary. Provider errors are reported without their
payload. Credentials are read at evaluation time, never from fixtures.

## Conservative classification

`IMPLEMENTED` requires locally verified implementation, connection, and passing
behavior evidence. `INCOMPLETE` requires verified missing behavior.
Incomplete/redacted context, conflicting evidence and absent verification yield
`UNCERTAIN`. Provider results must preserve the requirement IDs/count and satisfy
the evidence-derived status and confidence constraints in addition to the audit
schema and semantic validation.

The current collector does not establish per-requirement behavior or execute
tests. Changed test files are not passing tests. The raw Graph query path is
disabled pending a safe local typed adapter. Real audits therefore currently
return conservative results locally without requiring provider credentials.
Integrating a local verifier that can substantiate `RequirementSignals` is the
remaining capability; the Gemini adapter is exercised only with fake transports
in this contribution. Local intent splitting is bounded and heuristic, not a
general natural-language requirements parser.

## Development preview

`entire review audit --demo` uses bundled, explicitly synthetic data. It displays
all three statuses and does not collect a checkpoint or call Git, Graph or Gemini
for evidence. It is never a fallback for production audits.
`entire review audit --demo --requirement R2` shows evidence details.
The existing `--file` preview accepts already-produced audit JSON without
submitting it to an evaluator.

## Verification

- `go test -count=1 ./cmd/entire/cli -run 'Test.*CheckpointAudit|Test.*Audit'`
- `go test -count=1 ./cmd/entire/cli/review/intentlens/...`
- `go test -count=1 ./cmd/entire/cli/review -run TestIntentLensAudit`
