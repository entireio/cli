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
    SessionRecord,
    TokenUsage,
)

CHECKPOINT_REF_PREFIX = "refs/entire/checkpoints/"
CHECKPOINT_TRAILER = "Entire-Checkpoint:"
PROMPT_SEPARATOR = re.compile(r"\n-{3,}\n")

# Markers that classify a line of transcript prose as decision context. This is
# the difference between "what changed" (git) and "why it changed" (checkpoint).
DECISION_MARKERS: list[tuple[str, tuple[str, ...]]] = [
    ("blocker", ("blocker", "blocked", "cannot proceed", "does not exist", "hit a blocker")),
    ("risk", ("risk", "danger", "unsafe", "could break", "regression", "fragile")),
    ("rejected", ("instead of", "rather than", "rejected", "ruled out", "decided against", "not going to")),
    ("assumption", ("assume", "assuming", "assumption", "presumably")),
    ("decision", ("decided", "decision", "chose", "choosing", "we will use", "going with", "opted")),
    ("open_question", ("open question", "unresolved", "still need", "tbd", "to be decided", "not yet confirmed")),
]

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

        for idx, entry in enumerate(root.get("sessions") or []):
            rec.sessions.append(self._read_session(tree, cid, idx, entry))
        if rec.sessions:
            rec.created_at = rec.sessions[0].created_at

        # The checkpoint's own files_touched is known to under-report (observed:
        # 2 files recorded against a commit that changed 5). Union it with the
        # real commit stat so Graph blast-radius sees every file that moved.
        if linked_sha:
            union = set(rec.files_touched) | set(self.commit_files(linked_sha))
            rec.files_touched = sorted(union)
        return rec

    def commit_files(self, sha: str) -> list[str]:
        try:
            out = self._git("show", "--pretty=format:", "--name-only", sha)
        except GitError:
            return []
        return [line.strip() for line in out.splitlines() if line.strip()]

    def _read_session(
        self, tree: str, cid: str, idx: int, entry: dict[str, Any]
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
            sess.decisions = extract_decisions(transcript)

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


def extract_decisions(transcript: str) -> list[Decision]:
    """Recover decisions, rejected options, assumptions, risks and open
    questions from transcript prose.

    Deliberately a transparent keyword classifier rather than an LLM call: the
    output is verbatim source text a reviewer can audit, it needs no network,
    and it cannot invent a decision that was never made.
    """
    found: list[Decision] = []
    seen: set[str] = set()
    for speaker, text in _iter_text(transcript):
        for raw in re.split(r"(?<=[.!?])\s+|\n", text):
            s = raw.strip().lstrip("-*# ").strip()
            if not (MIN_DECISION_LEN <= len(s) <= MAX_DECISION_LEN):
                continue
            low = s.lower()
            for kind, markers in DECISION_MARKERS:
                if any(m in low for m in markers):
                    key = s[:120].lower()
                    if key not in seen:
                        seen.add(key)
                        found.append(Decision(kind=kind, text=s, speaker=speaker))
                    break
    return found
