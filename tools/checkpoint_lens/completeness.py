"""One authoritative answer to "how much of this is actually here?".

WHY THIS MODULE EXISTS
----------------------
Checkpoint Lens already reported degraded inputs - a warnings block, a "graph
unavailable" line, a "Databricks unavailable" line, an intent-provenance label.
Five separate signals, each true, each local to its own section, none of them
at the top, and none of them covering a missing transcript, an absent
metadata.json or content that Entire had already redacted.

A reader who skims a report and stops before the warnings block therefore read
a partial reconstruction as a complete one. That is the failure this module
removes: ONE banner, computed once, rendered first, in every output mode
(terminal, HTML and --json). The per-section notes stay as the detail.

THE RULE THIS ENCODES
---------------------
Absence of evidence is not evidence of absence. A section that says "no
blockers found" means one thing when the transcript was read and another when
there was no transcript to read, and the product must never let those two look
alike. Every input is therefore reported positively - readable or not - rather
than only mentioned when it fails.

REDACTION IS A THIRD STATE
--------------------------
An input can be present, readable, and still incomplete, because Entire's
redaction pipeline removed content before the checkpoint was ever committed.
That is neither "available" nor "missing": the checkpoint is intact and some of
what it described is gone. It is reported as REDACTED and counts as partial.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from .models import CheckpointRecord, INTENT_UNAVAILABLE

# Status values. REDACTED is deliberately distinct from MISSING: conflating
# them would either overstate what we hold or understate what was removed.
AVAILABLE = "available"
MISSING = "missing"
REDACTED = "redacted"


@dataclass
class Input:
    """One input the report is built from, and whether we actually had it."""

    name: str
    status: str
    detail: str = ""

    @property
    def ok(self) -> bool:
        return self.status == AVAILABLE


@dataclass
class ContextCompleteness:
    inputs: list[Input] = field(default_factory=list)

    @property
    def degraded(self) -> list[Input]:
        return [i for i in self.inputs if not i.ok]

    @property
    def is_complete(self) -> bool:
        return not self.degraded

    @property
    def verdict(self) -> str:
        return "COMPLETE" if self.is_complete else "PARTIAL"

    def to_dict(self) -> dict[str, Any]:
        """The same verdict --json consumers get. An agent reading this must be
        able to branch on `is_complete` without parsing prose."""
        return {
            "verdict": self.verdict,
            "is_complete": self.is_complete,
            "inputs_total": len(self.inputs),
            "inputs_available": len(self.inputs) - len(self.degraded),
            "inputs": [
                {"name": i.name, "status": i.status, "detail": i.detail}
                for i in self.inputs
            ],
        }


def assess(
    rec: CheckpointRecord,
    warnings: list[str] | None = None,
    graph_ok: bool | None = None,
    graph_detail: str = "",
    databricks_reason: str | None = None,
) -> ContextCompleteness:
    """Assess one checkpoint's context.

    `graph_ok` and `databricks_reason` are ``None`` when the caller did not
    consult that subsystem at all (``--no-graph`` / ``--no-databricks``). That
    is still not complete context - the user chose to run without it, and the
    banner says so rather than quietly counting it as fine.
    """
    inputs: list[Input] = []
    sessions = rec.sessions or []

    # --- session metadata --------------------------------------------------
    if not sessions:
        inputs.append(Input("session metadata", MISSING, "this checkpoint records no sessions"))
    elif all(s.metadata_available for s in sessions):
        inputs.append(Input("session metadata", AVAILABLE))
    else:
        n = sum(1 for s in sessions if not s.metadata_available)
        inputs.append(
            Input(
                "session metadata",
                MISSING,
                "%d of %d session(s) have no readable metadata.json; agent, "
                "turn and token figures for those are unknown, not zero"
                % (n, len(sessions)),
            )
        )

    # --- stated intent -----------------------------------------------------
    if not rec.intent or rec.intent_source == INTENT_UNAVAILABLE:
        inputs.append(
            Input("stated intent", MISSING, "no summary, prompt.txt or transcript to recover it from")
        )
    else:
        inputs.append(Input("stated intent", AVAILABLE, "source: " + rec.intent_source))

    # --- transcript --------------------------------------------------------
    if any(s.transcript_available for s in sessions):
        inputs.append(Input("session transcript", AVAILABLE))
    else:
        inputs.append(
            Input(
                "session transcript",
                MISSING,
                "decisions, risks and open questions could not be recovered at all",
            )
        )

    # --- redaction ---------------------------------------------------------
    if any(s.redacted_content for s in sessions):
        inputs.append(
            Input(
                "recovered text",
                REDACTED,
                "Entire's redaction pipeline removed content before this "
                "checkpoint was committed; what it removed is not recoverable here",
            )
        )
    else:
        inputs.append(Input("recovered text", AVAILABLE, "no redaction markers found"))

    # --- files touched -----------------------------------------------------
    if rec.files_touched:
        inputs.append(Input("files touched", AVAILABLE))
    else:
        inputs.append(Input("files touched", MISSING, "no file list on the checkpoint or its commit"))

    # --- graph -------------------------------------------------------------
    if graph_ok is None:
        inputs.append(Input("graph impact", MISSING, "not consulted (--no-graph)"))
    elif graph_ok:
        inputs.append(Input("graph impact", AVAILABLE))
    else:
        inputs.append(
            Input(
                "graph impact",
                MISSING,
                (graph_detail or "entire graph did not answer")
                + "; unavailable is not the same as 'no impact'",
            )
        )

    # --- cross-session analytics -------------------------------------------
    if databricks_reason is None:
        inputs.append(
            Input("cross-session analytics", MISSING, "not consulted (--no-databricks)")
        )
    elif databricks_reason:
        inputs.append(Input("cross-session analytics", MISSING, databricks_reason))
    else:
        inputs.append(Input("cross-session analytics", AVAILABLE))

    # --- reader warnings ---------------------------------------------------
    warnings = warnings or []
    if warnings:
        inputs.append(
            Input(
                "checkpoint read",
                MISSING,
                "%d warning(s) during the read - see the WARNINGS section" % len(warnings),
            )
        )
    else:
        inputs.append(Input("checkpoint read", AVAILABLE))

    return ContextCompleteness(inputs=inputs)
