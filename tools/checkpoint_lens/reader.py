"""CheckpointReader - parses real Entire Checkpoints into structured records.

This repo uses the git-refs checkpoint backend, so checkpoints live under
``refs/entire/checkpoints/<last2-of-ULID>/<ULID>`` rather than on the legacy
``entire/checkpoints/v1`` branch. Each ref points at a commit whose tree is::

    metadata.json                 root manifest (sessions[], files_touched, ...)
    <n>/metadata.json             per-session metadata (attribution, tokens)
    <n>/prompt.txt                every user prompt, separator-joined
    <n>/transcript.jsonl          compact transcript
    <n>/full.jsonl                full transcript (large; not read by default)

Reads go through git plumbing (``git cat-file``) against the checkpoint tree, so
nothing is checked out and the working tree is never touched.
"""

from __future__ import annotations

import json
import re
import subprocess
from typing import Any, Iterable

from .models import (
    INTENT_FROM_FIRST_PROMPT,
    INTENT_FROM_SUMMARY,
    INTENT_FROM_TRANSCRIPT,
    INTENT_UNAVAILABLE,
    Attribution,
    CheckpointRecord,
    Decision,
    SOURCE_COMMIT,
    SOURCE_TRANSCRIPT,
    SessionRecord,
    TokenUsage,
)

CHECKPOINT_REF_PREFIX = "refs/entire/checkpoints/"
CHECKPOINT_TRAILER = "Entire-Checkpoint:"
PROMPT_SEPARATOR = re.compile(r"\n-{3,}\n")

# Markers that classify a line of transcript prose as decision context. This is
# the difference between "what changed" (git) and "why it changed" (checkpoint).
# Order is significant: the first matching kind wins, so the most specific
# and most consequential classifications are tried first.
#
# "instead of" and "rather than" are deliberately NOT rejection markers. They
# are ordinary English connectives that appear in most technical prose, and
# including them classified plain statements of fact as rejected options. A
# rejection has to be stated as one.
# Each kind is a regex requiring the marker to be *predicated of this work*,
# not merely mentioned.
#
# Substring matching on bare nouns was tried first and does not work: "risk"
# matched "file-churn/risk score" (a column name), and "abandoned" matched a
# sentence *defining* what checkpoints preserve ("what was attempted and
# abandoned"). Both were reported as findings about the project. A marker has
# to appear as a claim - "we abandoned", "the risk is" - to count.
DECISION_MARKERS: list[tuple[str, "re.Pattern[str]"]] = [
    (
        "blocker",
        re.compile(
            r"\b(hit a blocker|blocked on|is a blocker|blocker:|cannot proceed|"
            r"could not proceed|is not possible|does not exist in this repo)\b",
            re.IGNORECASE,
        ),
    ),
    (
        "rejected",
        re.compile(
            r"\b(reject(s|ed|ing)?|ruled out|decided against|abandoned|discarded|"
            r"(we|i) are not going to)\b",
            re.IGNORECASE,
        ),
    ),
    (
        "decision",
        re.compile(
            r"\b((we|i) decided|decided to|(we|i) chose|chose to|opted (for|to)|"
            r"going with|(we|i) will use|settled on|the decision (is|was)|"
            # Engineering prose states decisions passively as often as it states
            # them agentively: "deliberately a transparent classifier", "chosen
            # over an LLM call", "in favour of Python". Requiring "we decided"
            # alone missed most real decisions in commit messages.
            r"deliberately|by design|on purpose|chosen over|in favou?r of|"
            r"we prefer|is preferred over)\b",
            re.IGNORECASE,
        ),
    ),
    (
        "risk",
        re.compile(
            r"\b(the risk (is|here|being)|risks? that|at risk of|is risky|"
            r"could break|would break|regression|is unsafe|is fragile|"
            r"is dangerous|biggest .{0,20}risk|quality risk)\b",
            re.IGNORECASE,
        ),
    ),
    (
        "open_question",
        re.compile(
            r"\b(still unresolved|remains unresolved|remains open|still needs? to|"
            r"not yet decided|not yet confirmed|to be decided|still not sure|"
            r"undecided|open question:|not yet implemented|still missing)\b",
            re.IGNORECASE,
        ),
    ),
    (
        "assumption",
        re.compile(
            r"\b((we|i) assume|assuming that|the assumption (is|was)|presumably|"
            r"on the assumption)\b",
            re.IGNORECASE,
        ),
    ),
    # Last, so any more specific kind wins. This is the catch-all for stated
    # *reasoning* - the causal prose that says why something is the way it is.
    # It is the single most abundant form the "why" actually takes in commit
    # messages and agent explanations ("chosen so that ...", "idempotent, so
    # re-running never double-counts"), and omitting it lost most of the
    # product's own subject matter.
    (
        "rationale",
        re.compile(
            r"\b(because|so that|the reason|which is why|on the grounds that|"
            r"rather than|instead of|in order to|otherwise)\b",
            re.IGNORECASE,
        ),
    ),
]

# A line that is mostly capitals is a section heading, not a sentence. Commit
# messages in this project use them freely, and they classified as findings.
def _is_heading(sentence: str) -> bool:
    letters = [c for c in sentence if c.isalpha()]
    if len(letters) < 8:
        return False
    upper = sum(1 for c in letters if c.isupper())
    return upper / len(letters) > 0.7

# Markdown emphasis and inline-code fences are noise in a terminal report.
MARKUP = re.compile(r"\*\*|__|`")

MIN_DECISION_LEN = 30
MAX_DECISION_LEN = 400

# git log record/field separators - ASCII unit/record separator, chosen because
# they cannot occur in a commit subject or body.
_FIELD_SEP = "\x1f"
_RECORD_SEP = "\x1e"


class GitError(RuntimeError):
    pass


class CheckpointReader:
    """Reads checkpoints out of a repository. Generic: takes any repo path."""

    def __init__(self, repo: str = ".") -> None:
        self.repo = repo
        self._warnings: list[str] = []

    # ---------------- git plumbing ----------------

    def _git(self, *args: str) -> str:
        proc = subprocess.run(
            ["git", "-C", self.repo, *args],
            capture_output=True,
            check=False,
        )
        if proc.returncode != 0:
            msg = proc.stderr.decode("utf-8", "replace").strip()
            raise GitError("git " + " ".join(args) + ": " + msg)
        return proc.stdout.decode("utf-8", "replace")

    def _blob(self, tree: str, path: str) -> str | None:
        """Read one path out of a checkpoint tree. A missing path is not fatal -
        a checkpoint may legitimately omit a transcript or prompt file."""
        path = path.lstrip("/")
        try:
            return self._git("cat-file", "-p", tree + ":" + path)
        except GitError:
            return None

    @property
    def warnings(self) -> list[str]:
        return list(self._warnings)

    # ---------------- discovery ----------------

    def list_refs(self) -> list[tuple[str, str, str]]:
        """Return (checkpoint_id, ref, commit_sha), oldest first.

        Checkpoint IDs are ULIDs, which sort lexicographically by creation time,
        so a plain sort is a true chronological ordering.
        """
        out = self._git(
            "for-each-ref", "--format=%(objectname)\t%(refname)", CHECKPOINT_REF_PREFIX
        )
        rows: list[tuple[str, str, str]] = []
        for line in out.splitlines():
            if not line.strip():
                continue
            sha, _, ref = line.partition("\t")
            cid = ref.rsplit("/", 1)[-1]
            rows.append((cid, ref, sha))
        rows.sort(key=lambda r: r[0])
        return rows

    def commit_links(self) -> dict[str, tuple[str, str]]:
        """Map checkpoint_id -> (repo commit sha, subject) via the
        ``Entire-Checkpoint:`` commit trailer the CLI writes on every
        checkpointed commit."""
        links: dict[str, tuple[str, str]] = {}
        fmt = "--format=%H" + _FIELD_SEP + "%s" + _FIELD_SEP + "%b" + _RECORD_SEP
        try:
            log = self._git("log", fmt, "--all")
        except GitError as exc:
            self._warnings.append("could not scan commit trailers: " + str(exc))
            return links
        for entry in log.split(_RECORD_SEP):
            entry = entry.strip("\n")
            if not entry:
                continue
            parts = entry.split(_FIELD_SEP)
            if len(parts) < 3:
                continue
            sha, subject, body = parts[0], parts[1], parts[2]
            for line in body.splitlines():
                line = line.strip()
                if line.startswith(CHECKPOINT_TRAILER):
                    cid = line[len(CHECKPOINT_TRAILER):].strip()
                    # Oldest wins: a checkpoint belongs to the commit that
                    # introduced it, not to a later cherry-pick of it.
                    links.setdefault(cid, (sha, subject))
        return links

    # ---------------- parsing ----------------

    def read_all(self, limit: int | None = None) -> list[CheckpointRecord]:
        refs = self.list_refs()
        if limit:
            refs = refs[-limit:]
        links = self.commit_links()
        records: list[CheckpointRecord] = []
        for cid, ref, sha in refs:
            try:
                records.append(self.read_one(cid, ref, sha, links))
            except (GitError, json.JSONDecodeError) as exc:
                self._warnings.append("checkpoint " + cid + " unreadable: " + str(exc))
        return records

    def read_one(
        self,
        cid: str,
        ref: str,
        sha: str,
        links: dict[str, tuple[str, str]] | None = None,
    ) -> CheckpointRecord:
        links = links if links is not None else self.commit_links()
        tree = sha + "^{tree}"
        raw = self._blob(tree, "metadata.json")
        if raw is None:
            raise GitError("checkpoint " + cid + " has no root metadata.json")
        root: dict[str, Any] = json.loads(raw)

        linked_sha, linked_subject = links.get(cid, ("", ""))
        rec = CheckpointRecord(
            checkpoint_id=root.get("checkpoint_id", cid),
            ref=ref,
            checkpoint_commit=sha,
            linked_commit=linked_sha,
            linked_subject=linked_subject,
            branch=root.get("branch", ""),
            files_touched=list(root.get("files_touched") or []),
            token_usage=TokenUsage.from_dict(root.get("token_usage")),
        )

        commit_message = self.commit_message(linked_sha) if linked_sha else ""
        for idx, entry in enumerate(root.get("sessions") or []):
            rec.sessions.append(
                self._read_session(tree, cid, idx, entry, commit_message)
            )
        if rec.sessions:
            rec.created_at = rec.sessions[0].created_at

        # The checkpoint's own files_touched is known to under-report (observed:
        # 2 files recorded against a commit that changed 5). Union it with the
        # real commit stat so Graph blast-radius sees every file that moved.
        if linked_sha:
            union = set(rec.files_touched) | set(self.commit_files(linked_sha))
            rec.files_touched = sorted(union)
        return rec

    def commit_message(self, sha: str) -> str:
        """Full commit message. A deliberate, edited record of what was decided
        - the highest-authority decision source available."""
        try:
            return self._git("log", "-1", "--format=%B", sha)
        except GitError:
            return ""

    def commit_files(self, sha: str) -> list[str]:
        try:
            out = self._git("show", "--pretty=format:", "--name-only", sha)
        except GitError:
            return []
        return [line.strip() for line in out.splitlines() if line.strip()]

    def _read_session(
        self,
        tree: str,
        cid: str,
        idx: int,
        entry: dict[str, Any],
        commit_message: str = "",
    ) -> SessionRecord:
        meta_raw = self._blob(tree, entry.get("metadata") or ("/%d/metadata.json" % idx))
        meta: dict[str, Any] = {}
        if meta_raw:
            try:
                meta = json.loads(meta_raw)
            except json.JSONDecodeError as exc:
                self._warnings.append(
                    "session %d of %s has unreadable metadata: %s" % (idx, cid, exc)
                )

        metrics = meta.get("session_metrics") or {}
        sess = SessionRecord(
            checkpoint_id=cid,
            session_index=idx,
            session_id=meta.get("session_id", ""),
            created_at=meta.get("created_at", ""),
            branch=meta.get("branch", ""),
            agent=meta.get("agent", ""),
            model=meta.get("model", ""),
            save_step_count=int(meta.get("save_step_count", 0) or 0),
            turn_count=int(metrics.get("turn_count", 0) or 0),
            files_touched=list(meta.get("files_touched") or []),
            token_usage=TokenUsage.from_dict(meta.get("token_usage")),
            attribution=Attribution.from_dict(meta.get("initial_attribution")),
        )

        prompt_raw = self._blob(tree, entry.get("prompt") or ("/%d/prompt.txt" % idx))
        if prompt_raw:
            sess.prompts = [
                p.strip() for p in PROMPT_SEPARATOR.split(prompt_raw) if p.strip()
            ]


        # Intent: prefer a generated checkpoint summary when one exists, else
        # fall back to the first substantive user prompt. Which source was used
        # is recorded and always rendered, so a reader can tell recovered
        # context from derived context.
        transcript = self._blob(
            tree, entry.get("compact_transcript") or ("/%d/transcript.jsonl" % idx)
        )
        if transcript:
            sess.transcript_available = True

        # Extraction needs the prompts (to suppress echoes) and the commit
        # message (the highest-authority decision source), so it runs only once
        # both are known.
        if transcript or commit_message:
            sess.decisions = extract_decisions(
                transcript or "", prompts=sess.prompts, commit_message=commit_message
            )

        summary = meta.get("summary")
        summary = summary if isinstance(summary, dict) else {}
        if summary.get("intent"):
            sess.intent = str(summary["intent"]).strip()
            sess.intent_source = INTENT_FROM_SUMMARY
        elif sess.prompts:
            sess.intent = self._pick_intent(sess.prompts)
            sess.intent_source = INTENT_FROM_FIRST_PROMPT
        elif transcript:
            # Not every checkpoint carries a prompt.txt - observed on real data,
            # where the root manifest records "prompt": "" and the file is
            # absent from the tree. The transcript still holds the user's own
            # words, so recover intent from the first user message rather than
            # reporting no intent at all.
            recovered = first_user_message(transcript)
            if recovered:
                sess.intent = recovered
                sess.intent_source = INTENT_FROM_TRANSCRIPT
            else:
                sess.intent_source = INTENT_UNAVAILABLE
        else:
            sess.intent_source = INTENT_UNAVAILABLE
        return sess

    @staticmethod
    def _pick_intent(prompts: list[str]) -> str:
        """The first prompt long enough to state an intent. Short operational
        prompts ("cat CLAUDE.md") are commands, not intent, so they are
        skipped."""
        for p in prompts:
            if len(p) >= 120:
                return p
        return prompts[0] if prompts else ""


def _iter_text(transcript: str) -> Iterable[tuple[str, str]]:
    """Yield (speaker, text) for every prose block in a compact transcript."""
    for line in transcript.splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            obj = json.loads(line)
        except json.JSONDecodeError:
            continue
        speaker = obj.get("role") or obj.get("type") or "unknown"
        content = obj.get("content")
        if isinstance(content, str):
            yield speaker, content
        elif isinstance(content, list):
            for item in content:
                if isinstance(item, dict) and isinstance(item.get("text"), str):
                    yield speaker, item["text"]


def first_user_message(transcript: str, min_len: int = 40) -> str:
    """The first substantive user message in a compact transcript.

    Third-choice intent source, used when a checkpoint has no prompt.txt at
    all. Short operational messages are skipped for the same reason
    ``_pick_intent`` skips them: they are commands, not intent.
    """
    fallback = ""
    for speaker, text in _iter_text(transcript):
        if speaker != "user":
            continue
        cleaned = text.strip()
        if not cleaned:
            continue
        if not fallback:
            fallback = cleaned
        if len(cleaned) >= min_len:
            return cleaned
    return fallback


def attribute_to_first_appearance(
    records: list[CheckpointRecord],
) -> dict[str, list[Decision]]:
    """Attribute each decision to the checkpoint where it FIRST appeared.

    Entire stores the *whole* compacted session in every checkpoint, not just
    that checkpoint's slice, so a decision recorded at checkpoint 2 is still
    present in the transcript of checkpoints 3..n. Counting raw occurrences
    therefore makes every item look like it was raised again on every commit,
    and any "unresolved items over time" trend becomes monotonically
    increasing noise.

    Attributing to first appearance answers the question the trend is actually
    asking - *when was this raised* - and makes a falling line mean what a
    reader assumes it means.

    Returns checkpoint_id -> decisions first seen at that checkpoint. Records
    must be in chronological order (ULID sort, which
    :meth:`CheckpointReader.list_refs` guarantees).
    """
    seen: set[str] = set()
    out: dict[str, list[Decision]] = {}
    for rec in records:
        fresh: list[Decision] = []
        for d in rec.all_decisions():
            key = _normalize(d.text)[:120]
            if key and key not in seen:
                seen.add(key)
                fresh.append(d)
        out[rec.checkpoint_id] = fresh
    return out


def _normalize(text: str) -> str:
    """Lowercase, drop punctuation, collapse whitespace.

    Collapsing is load-bearing, not cosmetic: this string is the deduplication
    key, and without it "the backend!" and "the backend" differ only by a
    trailing space and are counted as two distinct decisions.
    """
    return " ".join(re.sub(r"[^a-z0-9 ]+", " ", text.lower()).split())


def _tokens(text: str) -> set[str]:
    return {t for t in _normalize(text).split() if len(t) > 2}


class EchoFilter:
    """Rejects sentences that merely repeat what the user asked for.

    This is the difference between a decision and a restatement. The agent
    quoting the task back ("commit everything with a substantive message",
    "open questions we still need to decide") trips every keyword a real
    decision would, so without this filter the highest-value section of the
    report fills up with the prompt it was given.

    A sentence is an echo when it appears in the prompt text verbatim, or when
    most of its distinctive words do. The threshold is deliberately high: it is
    worse to drop a real decision than to keep a borderline one, so only strong
    overlap is suppressed.
    """

    OVERLAP_THRESHOLD = 0.6

    def __init__(self, prompts: list[str] | None) -> None:
        joined = "\n".join(prompts or [])
        self._normalized = _normalize(joined)
        self._sentences = [
            _tokens(s)
            for s in re.split(r"(?<=[.!?])\s+|\n", joined)
            if len(s.strip()) >= MIN_DECISION_LEN
        ]

    def is_echo(self, sentence: str) -> bool:
        if not self._normalized:
            return False
        norm = _normalize(sentence).strip()
        if not norm:
            return True
        if norm in self._normalized:
            return True
        toks = _tokens(sentence)
        if not toks:
            return True
        for prompt_toks in self._sentences:
            if not prompt_toks:
                continue
            overlap = len(toks & prompt_toks) / len(toks)
            if overlap >= self.OVERLAP_THRESHOLD:
                return True
        return False


def _classify_sentence(sentence: str) -> str | None:
    for kind, pattern in DECISION_MARKERS:
        if pattern.search(sentence):
            return kind
    return None


def _sentences_of(text: str) -> Iterable[str]:
    """Yield whole sentences from prose that may be hard-wrapped.

    Agent and commit-message prose is wrapped at ~80 columns, so splitting on
    every newline severs sentences mid-clause and the report then shows
    fragments that begin in the middle ("dangerous half of the bug, since
    ..."). Paragraphs are unwrapped first - blank lines and list bullets are
    real boundaries, a bare newline is not - and only then split on sentence
    punctuation.
    """
    for block in re.split(r"\n\s*\n|\n(?=\s*[-*+]\s)|\n(?=\s*\d+[.)]\s)", text):
        unwrapped = " ".join(block.split())
        if not unwrapped:
            continue
        for raw in re.split(r"(?<=[.!?])\s+", unwrapped):
            cleaned = MARKUP.sub("", raw).strip().lstrip("-*#>| ").strip()
            if cleaned:
                yield cleaned


def extract_decisions(
    transcript: str,
    prompts: list[str] | None = None,
    commit_message: str = "",
) -> list[Decision]:
    """Recover decisions, rejected options, assumptions, risks and open
    questions from a checkpoint.

    Deliberately a transparent classifier rather than an LLM call: the output is
    verbatim source text a reviewer can audit, it needs no network, and it
    cannot invent a decision that was never made.

    Three things keep the signal-to-noise usable, and all three were added
    after reading real output that was mostly noise:

    * Only the *assistant* speaks decisions. A user turn is a request.
    * Sentences echoing the prompt are dropped (see :class:`EchoFilter`).
    * Questions are dropped - asking something is not deciding it.

    The commit message is mined first and ranked highest: it is a deliberate,
    edited record of what was decided, whereas transcript prose is a by-product
    of doing the work.
    """
    found: list[Decision] = []
    seen: set[str] = set()

    def add(kind: str, text: str, speaker: str, source: str) -> None:
        key = _normalize(text)[:120]
        if key and key not in seen:
            seen.add(key)
            found.append(
                Decision(kind=kind, text=text, speaker=speaker, source=source)
            )

    # 1. Commit message body - highest authority.
    if commit_message:
        body = commit_message.split("\n", 1)[1] if "\n" in commit_message else ""
        for s in _sentences_of(body):
            if not (MIN_DECISION_LEN <= len(s) <= MAX_DECISION_LEN):
                continue
            if s.startswith("Co-Authored-By:") or s.startswith("Entire-Checkpoint:"):
                continue
            if _is_heading(s):
                continue
            kind = _classify_sentence(s)
            if kind:
                add(kind, s, "commit", SOURCE_COMMIT)

    # 2. Assistant transcript prose - the fallback, echo-filtered.
    echo = EchoFilter(prompts)
    for speaker, text in _iter_text(transcript):
        if speaker != "assistant":
            continue
        for s in _sentences_of(text):
            if not (MIN_DECISION_LEN <= len(s) <= MAX_DECISION_LEN):
                continue
            if s.endswith("?") or _is_heading(s):
                continue
            kind = _classify_sentence(s)
            if not kind:
                continue
            if echo.is_echo(s):
                continue
            add(kind, s, speaker, SOURCE_TRANSCRIPT)

    return found
