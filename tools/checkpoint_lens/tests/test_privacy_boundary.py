"""Privacy-boundary tests: redacted / missing checkpoint data, and egress.

WHY THIS FILE EXISTS
--------------------
Added in response to the Buildathon 2026 Track 1 noon curveball (PRIVACY
BOUNDARY). It pins two properties that the rest of the suite did not cover:

  1. Raw prompt and transcript text cannot reach the external service. Not
     "is truncated before it does" - cannot.
  2. A checkpoint whose fields are redacted or absent still produces useful
     output, and that output says plainly that it is incomplete.

ON THE FIXTURE - READ THIS
--------------------------
`SyntheticCheckpointRepo` below builds a **SYNTHETIC** checkpoint. It is not a
recording of a real session and it is not derived from one. It is a real git
repository with real `refs/entire/checkpoints/**` refs, assembled by hand so
that the degraded shapes we need can be tested deliberately:

    * a session whose `metadata.json` is absent from the tree
    * a session with no `prompt.txt`
    * a session with no transcript at all
    * a transcript whose content Entire's redaction pipeline replaced with
      REDACTED markers

The real checkpoints in this repository happen to contain the redaction case
(see `TestRedactionOnRealCheckpoints`), but not the missing-field cases, and a
test that can only run where the defect happens to exist is not a test. The
synthetic repo is labelled as synthetic here, in the class name, and in the
commit message that introduced it.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import tempfile
import unittest

from tools.checkpoint_lens import completeness, report
from tools.checkpoint_lens.databricks import (
    EGRESS_COLUMNS,
    EgressViolation,
    DatabricksSync,
    assert_egress_safe,
    derive_signals,
)
from tools.checkpoint_lens.models import Decision, has_redaction_marker
from tools.checkpoint_lens.reader import CheckpointReader

REDACTED = "REDACTED"


class SyntheticCheckpointRepo:
    """A throwaway git repo carrying SYNTHETIC Entire checkpoint refs.

    Writes blobs and trees through plumbing rather than a working tree, which
    is how the real checkpoint store builds them, so `CheckpointReader` reads
    this exactly as it reads a genuine ref.
    """

    def __init__(self) -> None:
        self.path = tempfile.mkdtemp(prefix="lens-synthetic-")

    def close(self) -> None:
        shutil.rmtree(self.path, ignore_errors=True)

    def git(self, *args: str, stdin: str | None = None) -> str:
        proc = subprocess.run(
            ["git", "-C", self.path] + list(args),
            input=stdin.encode("utf-8") if stdin is not None else None,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=True,
        )
        return proc.stdout.decode("utf-8", "replace").strip()

    def init(self) -> None:
        subprocess.run(["git", "init", "-q", self.path], check=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self.git("config", "user.name", "Synthetic Fixture")
        self.git("config", "user.email", "synthetic@example.invalid")
        self.git("config", "commit.gpgsign", "false")
        with open(os.path.join(self.path, "src.py"), "w", encoding="utf-8") as fh:
            fh.write("def widget():\n    return 1\n")
        self.git("add", "src.py")

    def commit(self, message: str) -> str:
        self.git("commit", "-q", "--no-gpg-sign", "-m", message)
        return self.git("rev-parse", "HEAD")

    def _blob(self, content: str) -> str:
        return self.git("hash-object", "-w", "--stdin", stdin=content)

    def _tree(self, entries: list[tuple[str, str, str]]) -> str:
        """entries: (mode-and-type, sha, name)."""
        spec = "\n".join(
            "%s %s\t%s" % (mode, sha, name) for mode, sha, name in entries
        )
        return self.git("mktree", stdin=spec + "\n")

    def add_checkpoint(
        self,
        checkpoint_id: str,
        *,
        metadata: dict | None,
        prompt: str | None,
        transcript: str | None,
    ) -> None:
        """Write one checkpoint ref. A `None` argument means the blob is
        ABSENT from the tree, which is the case the reader used to paper
        over."""
        session_entries: list[tuple[str, str, str]] = []
        if metadata is not None:
            session_entries.append(
                ("100644 blob", self._blob(json.dumps(metadata)), "metadata.json")
            )
        if prompt is not None:
            session_entries.append(("100644 blob", self._blob(prompt), "prompt.txt"))
        if transcript is not None:
            session_entries.append(
                ("100644 blob", self._blob(transcript), "transcript.jsonl")
            )
        session_tree = self._tree(session_entries)

        root = {
            "cli_version": "0.10.5-synthetic",
            "checkpoint_id": checkpoint_id,
            "strategy": "manual-commit",
            "branch": "synthetic",
            "checkpoints_count": 1,
            "files_touched": ["src.py"],
            "sessions": [
                {
                    "metadata": "/0/metadata.json",
                    "transcript": "/0/full.jsonl",
                    "compact_transcript": "/0/transcript.jsonl",
                    "prompt": "/0/prompt.txt",
                }
            ],
            "token_usage": {"input_tokens": 1, "api_call_count": 1},
        }
        root_tree = self._tree(
            [
                ("040000 tree", session_tree, "0"),
                ("100644 blob", self._blob(json.dumps(root)), "metadata.json"),
            ]
        )
        cp_commit = self.git(
            "commit-tree", root_tree, "-m",
            "SYNTHETIC checkpoint fixture " + checkpoint_id,
        )
        self.git(
            "update-ref",
            "refs/entire/checkpoints/%s/%s" % (checkpoint_id[-2:], checkpoint_id),
            cp_commit,
        )


def _transcript(lines: list[dict]) -> str:
    return "\n".join(json.dumps(x) for x in lines) + "\n"


class SyntheticFixtureMixin(unittest.TestCase):
    """Shared SYNTHETIC repo with one healthy and three degraded checkpoints."""

    @classmethod
    def setUpClass(cls) -> None:
        cls.repo = SyntheticCheckpointRepo()
        cls.repo.init()
        cls.repo.commit("synthetic: baseline")

        healthy_meta = {
            "checkpoint_id": "01SYNTHHEALTHY0000000000AA",
            "session_id": "sess-healthy",
            "created_at": "2026-09-06T09:00:00Z",
            "branch": "synthetic",
            "agent": "Claude Code",
            "model": "claude-opus-5",
            "save_step_count": 1,
            "files_touched": ["src.py"],
            "session_metrics": {"turn_count": 2},
            "token_usage": {"input_tokens": 10, "output_tokens": 20, "api_call_count": 2},
        }
        cls.repo.add_checkpoint(
            "01SYNTHHEALTHY0000000000AA",
            metadata=healthy_meta,
            prompt=(
                "Build a widget renderer that validates its input before "
                "drawing, so that a malformed payload cannot crash the page."
            ),
            transcript=_transcript(
                [
                    {"role": "user", "content": "Build a widget renderer."},
                    {
                        "role": "assistant",
                        "content": (
                            "We chose a validating renderer over a permissive one, "
                            "because a malformed payload must fail loudly rather "
                            "than draw a broken widget."
                        ),
                    },
                ]
            ),
        )

        redacted_meta = dict(healthy_meta)
        redacted_meta.update(
            {"checkpoint_id": "01SYNTHREDACTED000000000BB", "session_id": "sess-redacted"}
        )
        cls.repo.add_checkpoint(
            "01SYNTHREDACTED000000000BB",
            metadata=redacted_meta,
            prompt="Deploy token: " + REDACTED + "\nHost: " + REDACTED,
            transcript=_transcript(
                [
                    {"role": "user", "content": "Deploy with " + REDACTED},
                    {
                        "role": "assistant",
                        "content": (
                            "We decided to rotate the credential rather than "
                            "reuse " + REDACTED + " for the new environment."
                        ),
                    },
                ]
            ),
        )

        # No prompt.txt and no transcript: intent is unrecoverable.
        starved_meta = dict(healthy_meta)
        starved_meta.update(
            {"checkpoint_id": "01SYNTHSTARVED0000000000CC", "session_id": "sess-starved"}
        )
        cls.repo.add_checkpoint(
            "01SYNTHSTARVED0000000000CC",
            metadata=starved_meta,
            prompt=None,
            transcript=None,
        )

        # No metadata.json at all: agent/turns/tokens are unknown, not zero.
        cls.repo.add_checkpoint(
            "01SYNTHNOMETA00000000000DD",
            metadata=None,
            prompt="Do the thing that the plan describes in some detail here.",
            transcript=None,
        )

        cls.reader = CheckpointReader(cls.repo.path)
        cls.records = cls.reader.read_all()
        cls.by_id = {r.checkpoint_id: r for r in cls.records}

    @classmethod
    def tearDownClass(cls) -> None:
        cls.repo.close()


class TestSyntheticFixtureIsUsable(SyntheticFixtureMixin):
    """The fixture must actually reproduce the degraded shapes."""

    def test_all_four_synthetic_checkpoints_are_read(self):
        self.assertEqual(len(self.records), 4, [r.checkpoint_id for r in self.records])

    def test_reader_survives_every_degraded_shape(self):
        # The point is that none of these raised. A reader that throws on a
        # redacted or truncated checkpoint fails the "keep working" rule
        # before any rendering question arises.
        for rec in self.records:
            self.assertTrue(rec.checkpoint_id)
            self.assertEqual(rec.files_touched, ["src.py"])


class TestDegradedCheckpointsStillProduceOutput(SyntheticFixtureMixin):
    """Requirement: useful output when sensitive fields are redacted or absent."""

    def test_redacted_checkpoint_still_recovers_a_decision(self):
        rec = self.by_id["01SYNTHREDACTED000000000BB"]
        decisions = rec.all_decisions()
        self.assertTrue(decisions, "redaction must not empty the decision section")
        self.assertTrue(
            any("rotate the credential" in d.text for d in decisions),
            [d.text for d in decisions],
        )

    def test_checkpoint_without_prompt_or_transcript_still_renders(self):
        rec = self.by_id["01SYNTHSTARVED0000000000CC"]
        lines = report.render_checkpoint_summary(rec) + report.render_intent(rec)
        body = "\n".join(lines)
        self.assertIn("01SYNTHSTARVED0000000000CC", body)
        self.assertIn("no intent recoverable", body)

    def test_missing_metadata_does_not_render_as_zero(self):
        rec = self.by_id["01SYNTHNOMETA00000000000DD"]
        comp = completeness.assess(rec, warnings=self.reader.warnings)
        meta = [i for i in comp.inputs if i.name == "session metadata"][0]
        self.assertEqual(meta.status, completeness.MISSING)
        self.assertIn("not zero", meta.detail)


class TestCompletenessBannerIsExplicit(SyntheticFixtureMixin):
    """Requirement: one clear signal, and never 'incomplete presented as
    complete'."""

    def test_healthy_checkpoint_with_everything_present_reads_complete(self):
        rec = self.by_id["01SYNTHHEALTHY0000000000AA"]
        comp = completeness.assess(
            rec, warnings=[], graph_ok=True, databricks_reason=""
        )
        self.assertTrue(comp.is_complete, [i.name for i in comp.degraded])
        self.assertEqual(comp.verdict, "COMPLETE")
        self.assertIn("CONTEXT: COMPLETE", "\n".join(report.render_completeness(comp)))

    def test_redacted_checkpoint_can_never_read_complete(self):
        rec = self.by_id["01SYNTHREDACTED000000000BB"]
        comp = completeness.assess(
            rec, warnings=[], graph_ok=True, databricks_reason=""
        )
        self.assertFalse(comp.is_complete)
        self.assertEqual(comp.verdict, "PARTIAL")
        redacted = [i for i in comp.inputs if i.status == completeness.REDACTED]
        self.assertEqual([i.name for i in redacted], ["recovered text"])

    def test_banner_states_the_verdict_and_names_each_degraded_input(self):
        rec = self.by_id["01SYNTHSTARVED0000000000CC"]
        comp = completeness.assess(rec, warnings=self.reader.warnings)
        body = "\n".join(report.render_completeness(comp))
        self.assertIn("CONTEXT: PARTIAL", body)
        self.assertNotIn("CONTEXT: COMPLETE", body)
        for degraded in comp.degraded:
            self.assertIn(degraded.name, body)
        self.assertIn("floor, not a ceiling", body)

    def test_every_input_is_reported_positively_not_only_the_failures(self):
        # A list of only failures is ambiguous: a reader cannot tell an input
        # that passed from one nobody looked at.
        rec = self.by_id["01SYNTHHEALTHY0000000000AA"]
        comp = completeness.assess(rec, warnings=[], graph_ok=True, databricks_reason="")
        body = "\n".join(report.render_completeness(comp))
        for i in comp.inputs:
            self.assertIn(i.name, body)

    def test_json_carries_a_machine_readable_verdict(self):
        rec = self.by_id["01SYNTHREDACTED000000000BB"]
        payload = completeness.assess(rec, warnings=[]).to_dict()
        self.assertEqual(payload["verdict"], "PARTIAL")
        self.assertFalse(payload["is_complete"])
        self.assertEqual(len(payload["inputs"]), payload["inputs_total"])
        self.assertTrue(all(set(i) == {"name", "status", "detail"} for i in payload["inputs"]))

    def test_not_consulted_is_not_the_same_as_available(self):
        rec = self.by_id["01SYNTHHEALTHY0000000000AA"]
        comp = completeness.assess(rec, warnings=[], graph_ok=None, databricks_reason=None)
        self.assertFalse(comp.is_complete)
        details = " ".join(i.detail for i in comp.degraded)
        self.assertIn("not consulted", details)

    def test_empty_decisions_says_why_it_is_empty(self):
        # "Nothing found" and "nothing to search" must not render alike.
        searched = "\n".join(report.render_decisions([], transcript_available=True))
        unsearched = "\n".join(report.render_decisions([], transcript_available=False))
        self.assertNotEqual(searched, unsearched)
        self.assertIn("no decisions matched", searched)
        self.assertIn("NO TRANSCRIPT", unsearched)


class TestNoRawTextCrossesTheEgressBoundary(SyntheticFixtureMixin):
    """Requirement: raw prompts and transcripts must not be sent externally.

    These run entirely offline - `DatabricksSync` builds rows without a
    connection, and the rows are inspected here rather than sent.
    """

    def test_schema_carries_no_free_text_column(self):
        for table, spec in EGRESS_COLUMNS.items():
            names = [c for c, _ in spec]
            self.assertNotIn("intent", names, table)
            self.assertNotIn("text", names, table)

    def test_derive_signals_returns_no_text(self):
        secret = "Deploy token abcdef and a whole sentence of prompt text."
        sig = derive_signals(secret, "repo")
        self.assertEqual(set(sig), {"len", "word_count", "digest", "redacted"})
        for value in sig.values():
            self.assertNotIn(str(value), (secret,))
        self.assertNotIn("Deploy", json.dumps(sig))
        self.assertEqual(sig["len"], len(secret))

    def test_digest_is_salted_per_repo(self):
        a = derive_signals("same text", "repo-a")["digest"]
        b = derive_signals("same text", "repo-b")["digest"]
        self.assertNotEqual(a, b)
        self.assertEqual(a, derive_signals("same text", "repo-a")["digest"])

    def test_derive_signals_flags_redaction(self):
        self.assertTrue(derive_signals("token " + REDACTED, "r")["redacted"])
        self.assertFalse(derive_signals("token abc", "r")["redacted"])

    def test_built_rows_contain_no_prompt_or_transcript_text(self):
        rows = _build_rows(self.repo.path, self.records)
        haystack = json.dumps(rows, default=str)
        for phrase in (
            "widget renderer",
            "malformed payload",
            "rotate the credential",
            "Deploy token",
            "validating renderer",
        ):
            self.assertNotIn(phrase, haystack, "raw text reached an outgoing row: " + phrase)

    def test_the_guard_rejects_free_text_if_it_ever_returns(self):
        good = ["01ABC", "2026-09-06T00:00:00Z", "src/widget.py", "repo"]
        assert_egress_safe("checkpoint_files", [good])
        prose = list(good)
        prose[2] = "We chose a validating renderer over a permissive one,\nbecause it fails loudly."
        with self.assertRaises(EgressViolation):
            assert_egress_safe("checkpoint_files", [prose])

    def test_the_guard_rejects_a_row_that_does_not_match_the_spec(self):
        with self.assertRaises(EgressViolation):
            assert_egress_safe("checkpoint_files", [["too", "few"]])
        with self.assertRaises(EgressViolation):
            assert_egress_safe("no_such_table", [[]])

    def test_ddl_and_egress_spec_cannot_drift_apart(self):
        import re

        from tools.checkpoint_lens import databricks as db

        for table, ddl in (
            ("checkpoint_sessions", db.DDL_SESSIONS),
            ("checkpoint_files", db.DDL_FILES),
            ("checkpoint_decisions", db.DDL_DECISIONS),
        ):
            body = ddl[ddl.index("(") + 1 : ddl.rindex(")")]
            declared = [
                re.split(r"\s+", line.strip())[0]
                for line in body.strip().splitlines()
                if line.strip()
            ]
            self.assertEqual(declared, [c for c, _ in EGRESS_COLUMNS[table]], table)


class TestRedactionOnRealCheckpoints(unittest.TestCase):
    """The redaction case is not only synthetic - this repo's own checkpoints
    carry it, which is what makes the synthetic version representative."""

    def test_marker_detection_matches_what_entire_writes(self):
        self.assertTrue(has_redaction_marker("PAT: " + REDACTED))
        self.assertTrue(has_redaction_marker("<tool-use-id>[" + REDACTED + "]</tool-use-id>"))
        self.assertFalse(has_redaction_marker("nothing sensitive here"))
        self.assertFalse(has_redaction_marker("unredactedly"))
        self.assertFalse(has_redaction_marker(""))


def _build_rows(repo_path: str, records) -> dict:
    """The rows a real sync would send, obtained from the real sync path.

    `DatabricksSync.build_rows` is the function both `sync` and `--dry-run`
    call, so this inspects the actual outgoing payload rather than a
    re-implementation of it. A copy here would only ever prove the copy safe.
    """
    return DatabricksSync(repo_path).build_rows(records)


if __name__ == "__main__":
    unittest.main()
