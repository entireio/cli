"""Structured records parsed out of real Entire Checkpoints.

These dataclasses are the single schema shared by every consumer: the terminal
and HTML reports, and the Databricks Delta table. Keeping one schema is what
lets the same record be rendered locally and aggregated across sessions.
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field, asdict
from typing import Any


# Entire's own redaction pipeline replaces a detected secret with this marker
# before the transcript is ever committed, so it is what a redacted checkpoint
# actually looks like on disk. Both the egress layer and the completeness
# banner need to recognise it: text carrying the marker is content we do NOT
# have, and rendering it as though it were recovered content is the exact
# "incomplete presented as complete" failure this tool exists to prevent.
REDACTION_MARKER = re.compile(r"(?:\[)?\bREDACTED\b(?:\])?")


def has_redaction_marker(text: str) -> bool:
    """True when Entire's redaction pipeline removed content from this text."""
    return bool(text) and bool(REDACTION_MARKER.search(text))


# How a session's "stated intent" was obtained. Surfaced in every report so a
# reader can tell recovered context from synthesised context (responsible-use
# rule: label synthetic or derived content).
INTENT_FROM_SUMMARY = "checkpoint_summary"
INTENT_FROM_FIRST_PROMPT = "first_user_prompt"
INTENT_FROM_TRANSCRIPT = "first_user_message_in_transcript"
INTENT_UNAVAILABLE = "unavailable"


@dataclass
class TokenUsage:
    input_tokens: int = 0
    cache_creation_tokens: int = 0
    cache_read_tokens: int = 0
    output_tokens: int = 0
    api_call_count: int = 0

    @classmethod
    def from_dict(cls, d: dict[str, Any] | None) -> "TokenUsage":
        d = d or {}
        return cls(
            input_tokens=int(d.get("input_tokens", 0) or 0),
            cache_creation_tokens=int(d.get("cache_creation_tokens", 0) or 0),
            cache_read_tokens=int(d.get("cache_read_tokens", 0) or 0),
            output_tokens=int(d.get("output_tokens", 0) or 0),
            api_call_count=int(d.get("api_call_count", 0) or 0),
        )

    @property
    def total(self) -> int:
        return (
            self.input_tokens
            + self.cache_creation_tokens
            + self.cache_read_tokens
            + self.output_tokens
        )


@dataclass
class Attribution:
    """Who wrote the code in this session - agent vs human."""

    agent_lines: int = 0
    human_added: int = 0
    human_modified: int = 0
    human_removed: int = 0
    total_lines_changed: int = 0
    agent_percentage: float = 0.0

    @classmethod
    def from_dict(cls, d: dict[str, Any] | None) -> "Attribution":
        d = d or {}
        return cls(
            agent_lines=int(d.get("agent_lines", 0) or 0),
            human_added=int(d.get("human_added", 0) or 0),
            human_modified=int(d.get("human_modified", 0) or 0),
            human_removed=int(d.get("human_removed", 0) or 0),
            total_lines_changed=int(d.get("total_lines_changed", 0) or 0),
            agent_percentage=float(d.get("agent_percentage", 0.0) or 0.0),
        )


# Where a recovered decision came from, in descending order of authority. A
# commit message is a deliberate, edited record; transcript prose is a
# by-product of working. Both are verbatim, but they are not equally reliable,
# and the report says which is which.
SOURCE_SUMMARY = "checkpoint_summary"
SOURCE_COMMIT = "commit_message"
SOURCE_TRANSCRIPT = "transcript"

SOURCE_CONFIDENCE = {
    SOURCE_SUMMARY: "high",
    SOURCE_COMMIT: "high",
    SOURCE_TRANSCRIPT: "medium",
}


@dataclass
class Decision:
    """A decision, rejected option, assumption, risk or blocker recovered from
    a checkpoint. `kind` is the classifier; `text` is verbatim prose, never
    paraphrased, so a reader can audit it against the source."""

    kind: str
    text: str
    speaker: str = "assistant"
    source: str = SOURCE_TRANSCRIPT

    @property
    def confidence(self) -> str:
        return SOURCE_CONFIDENCE.get(self.source, "medium")


@dataclass
class SessionRecord:
    checkpoint_id: str
    session_index: int
    session_id: str = ""
    created_at: str = ""
    branch: str = ""
    agent: str = ""
    model: str = ""
    turn_count: int = 0
    save_step_count: int = 0
    prompts: list[str] = field(default_factory=list)
    intent: str = ""
    intent_source: str = INTENT_UNAVAILABLE
    files_touched: list[str] = field(default_factory=list)
    decisions: list[Decision] = field(default_factory=list)
    token_usage: TokenUsage = field(default_factory=TokenUsage)
    attribution: Attribution = field(default_factory=Attribution)
    # Which of this session's inputs were actually readable. These exist so
    # that "we read it and there was nothing" and "we could not read it" are
    # different states everywhere downstream. Before they existed, an absent
    # metadata blob became an empty dict and rendered as `0 turns / unknown
    # agent`, which is a confident-looking answer to a question we had not
    # asked anything about.
    metadata_available: bool = False
    prompt_available: bool = False
    transcript_available: bool = False
    redacted_content: bool = False

    def to_row(self) -> dict[str, Any]:
        """Flatten to one Delta-table row. Nested structs are flattened because
        the aggregate SQL groups and sums over these columns directly."""
        return {
            "checkpoint_id": self.checkpoint_id,
            "session_index": self.session_index,
            "session_id": self.session_id,
            "created_at": self.created_at,
            "branch": self.branch,
            "agent": self.agent,
            "model": self.model,
            "turn_count": self.turn_count,
            "save_step_count": self.save_step_count,
            "intent": self.intent,
            "intent_source": self.intent_source,
            "prompt_count": len(self.prompts),
            "files_touched": list(self.files_touched),
            "files_touched_count": len(self.files_touched),
            "decision_count": len(self.decisions),
            "input_tokens": self.token_usage.input_tokens,
            "output_tokens": self.token_usage.output_tokens,
            "cache_read_tokens": self.token_usage.cache_read_tokens,
            "cache_creation_tokens": self.token_usage.cache_creation_tokens,
            "api_call_count": self.token_usage.api_call_count,
            "total_tokens": self.token_usage.total,
            "agent_percentage": self.attribution.agent_percentage,
            "total_lines_changed": self.attribution.total_lines_changed,
        }


@dataclass
class CheckpointRecord:
    checkpoint_id: str
    ref: str = ""
    checkpoint_commit: str = ""
    linked_commit: str = ""
    linked_subject: str = ""
    created_at: str = ""
    branch: str = ""
    files_touched: list[str] = field(default_factory=list)
    token_usage: TokenUsage = field(default_factory=TokenUsage)
    sessions: list[SessionRecord] = field(default_factory=list)
    warnings: list[str] = field(default_factory=list)

    @property
    def intent(self) -> str:
        for s in self.sessions:
            if s.intent:
                return s.intent
        return ""

    @property
    def intent_source(self) -> str:
        for s in self.sessions:
            if s.intent:
                return s.intent_source
        return INTENT_UNAVAILABLE

    def all_decisions(self) -> list[Decision]:
        out: list[Decision] = []
        for s in self.sessions:
            out.extend(s.decisions)
        return out

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)
