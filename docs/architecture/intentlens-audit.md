# IntentLens audit contract

IntentLens evaluates a bounded evidence package. The intended flow is:

`Entire checkpoint -> developer intent -> atomic requirements -> Git diff + graph + source + tests -> evidence package -> Gemini -> audit result -> recommended fix`

Gemini must not receive an undifferentiated checkpoint and guess whether a change looks correct. Requirement extraction and evidence evaluation use separate prompts in `cmd/entire/cli/review/intentlens/prompts.go`. The evaluator receives only the evidence package and the strict schema.

## Classification

- `IMPLEMENTED`: supplied evidence shows implementation/connection and includes a passing relevant test. Mere existence of code or a graph node is insufficient.
- `INCOMPLETE`: supplied evidence identifies a concrete missing, disconnected, failing, contradictory, or partial behavior.
- `UNCERTAIN`: evidence or verification is insufficient or conflicting.

Confidence never substitutes for evidence. Conclusions may reference only listed evidence. Requirements must retain their original meaning. `INCOMPLETE` and `UNCERTAIN` require actionable recommendations; `IMPLEMENTED` requires an empty recommendation.

The JSON Schema is `cmd/entire/cli/review/intentlens/audit.schema.json`. `ParseAuditJSON` rejects unknown fields and malformed or schema-invalid data, then `ValidateSemantics` enforces unique IDs, recommendations, and evidence sufficiency that JSON Schema does not express reliably.

## Integration boundary

`entire review audit --file <path>` (or `--file -`) accepts the same audit JSON a future backend/Gemini structured-output integration will provide. `--demo` uses deterministic synthetic files in `cmd/entire/cli/review/intentlens/testdata`; it does not call Gemini or a backend and visibly labels that fact.
