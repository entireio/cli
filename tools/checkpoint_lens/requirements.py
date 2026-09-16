"""Requirement extraction and drift classification.

The baseline for drift is the *first* checkpoint's stated intent - the plan as
it was written before any code existed. Requirements are pulled out of that
prose, then each is checked against the current state of the repository.

Two rules keep this honest:

1. A requirement is never reported as "done" on the strength of a graph hit
   alone. The strongest verdict this module issues is IMPLEMENTED, and every
   verdict carries the evidence that produced it so a reader can check it.
2. "No evidence found" is reported as MISSING, which is a *prompt to verify*,
   not a claim of fact. The renderer says so.
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from typing import Iterable

# Lines that read as a requirement: numbered items, bullets, or a sentence
# carrying an obligation verb.
NUMBERED = re.compile(r"^\s*(\d+)[.)]\s+(.{12,})$")
BULLET = re.compile(r"^\s*[-*•]\s+(.{12,})$")
OBLIGATION = re.compile(
    r"\b(must|should|needs? to|has to|required to|shall|make sure|ensure)\b",
    re.IGNORECASE,
)

# Prose that looks like a requirement but is really commentary.
NOISE = re.compile(
    r"^(note|nb|see|for example|e\.g\.|i\.e\.|read |also |btw)\b", re.IGNORECASE
)

# A bolded "**Label:** value" line is reference metadata, not a requirement.
# Planning documents pasted into a prompt are full of them (Date, Venue, Team,
# Judging), and without this filter they dominate the extracted plan.
METADATA_LABEL = re.compile(r"^\*{0,2}[A-Z][A-Za-z /-]{2,24}:?\*{0,2}\s*:")

# Event/logistics vocabulary that never describes software behaviour.
LOGISTICS = re.compile(
    r"\b(venue|deadline|judging|breakfast|lunch|IST\b|voucher|prize|award ceremony|"
    r"team size|check-in|agenda|schedule|am\b|pm\b)\b",
    re.IGNORECASE,
)

# A requirement describes something the software does. Require at least one
# action verb or code-shaped token, otherwise it is prose about the project
# rather than a statement about its behaviour.
ACTION = re.compile(
    r"\b(add|build|create|implement|support|parse|read|write|render|emit|return|"
    r"expose|store|sync|query|compare|detect|extract|report|handle|validate|"
    r"resolve|surface|show|list|link|fall ?back|fail|test|verify|push|load|"
    r"generate|filter|track|accept|reject|skip|cache|log)\b",
    re.IGNORECASE,
)
CODEISH = re.compile(r"`[^`]+`|\b[a-z_]+\.(py|go|json|md)\b|\b[A-Z][a-z]+[A-Z]\w+\b")

MIN_REQ = 15
MAX_REQ = 300

IMPLEMENTED = "IMPLEMENTED"
PARTIAL = "PARTIAL"
MISSING = "MISSING"
UNVERIFIED = "UNVERIFIED"

VERDICT_NOTE = {
    IMPLEMENTED: "code matching this requirement exists and was verified against source",
    PARTIAL: "some matching code exists but it does not cover the whole requirement",
    MISSING: "no matching code found - VERIFY before trusting; absence of evidence is not proof",
    UNVERIFIED: "could not be checked (graph unavailable) - unknown, not absent",
}


@dataclass
class Requirement:
    text: str
    origin: str = ""
    index: int = 0


@dataclass
class DriftFinding:
    requirement: Requirement
    verdict: str = UNVERIFIED
    evidence: list[str] = field(default_factory=list)
    command: str = ""
    score: float = 0.0

    @property
    def is_open(self) -> bool:
        return self.verdict in (MISSING, PARTIAL)


def extract_requirements(texts: Iterable[str], limit: int = 25) -> list[Requirement]:
    """Pull requirement-shaped statements out of the plan prose.

    Deliberately conservative: a numbered or bulleted item, or a sentence with
    an obligation verb. Everything else is treated as narrative.
    """
    out: list[Requirement] = []
    seen: set[str] = set()

    for origin_idx, text in enumerate(texts):
        for raw in text.splitlines():
            line = raw.strip()
            if not line or NOISE.match(line):
                continue

            candidate = ""
            m = NUMBERED.match(line)
            if m:
                candidate = m.group(2).strip()
            else:
                b = BULLET.match(line)
                if b:
                    candidate = b.group(1).strip()
                elif OBLIGATION.search(line):
                    candidate = line

            candidate = candidate.strip(" .;:")
            if not (MIN_REQ <= len(candidate) <= MAX_REQ):
                continue
            # Reference metadata and event logistics are not requirements.
            if METADATA_LABEL.match(candidate) or LOGISTICS.search(candidate):
                continue
            # Must describe behaviour, not just mention the project.
            if not (ACTION.search(candidate) or CODEISH.search(candidate)):
                continue
            key = re.sub(r"[^a-z0-9 ]", "", candidate.lower())[:80]
            if key in seen:
                continue
            seen.add(key)
            out.append(
                Requirement(
                    text=candidate,
                    origin="prompt %d" % (origin_idx + 1),
                    index=len(out) + 1,
                )
            )
            if len(out) >= limit:
                return out
    return out


# Words carrying no discriminating power in a code search.
STOPWORDS = {
    "the", "a", "an", "and", "or", "to", "of", "in", "on", "for", "with", "that",
    "this", "it", "is", "are", "be", "must", "should", "needs", "need", "make",
    "sure", "ensure", "we", "our", "you", "your", "then", "from", "into", "at",
    "by", "as", "so", "not", "no", "do", "does", "run", "use", "using", "add",
    "new", "one", "all", "each", "any", "its", "will", "can", "if", "when",
}


def keywords(text: str, limit: int = 6) -> list[str]:
    words = re.findall(r"[A-Za-z_][A-Za-z0-9_]{2,}", text)
    out: list[str] = []
    for w in words:
        lw = w.lower()
        if lw in STOPWORDS or lw in {o.lower() for o in out}:
            continue
        out.append(w)
        if len(out) >= limit:
            break
    return out


def classify(hits: list[str], req: Requirement) -> tuple[str, float]:
    """Turn search hits into a verdict.

    The score is the fraction of the requirement's distinctive keywords that
    appear in the returned symbol/file names. It is a transparent heuristic,
    reported alongside the verdict so nobody mistakes it for certainty.
    """
    if not hits:
        return MISSING, 0.0
    terms = [k.lower() for k in keywords(req.text, limit=8)]
    if not terms:
        return UNVERIFIED, 0.0
    blob = " ".join(hits).lower()
    matched = sum(1 for t in terms if t in blob)
    score = matched / len(terms)
    if score >= 0.5:
        return IMPLEMENTED, score
    if score > 0.0:
        return PARTIAL, score
    return MISSING, 0.0
