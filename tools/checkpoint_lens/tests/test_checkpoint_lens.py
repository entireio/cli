"""Tests for Checkpoint Lens.

Run with:  python -m unittest discover -s tools/checkpoint_lens/tests -v

Stdlib unittest deliberately: the tool must be runnable on a judge's machine
from a clean checkout with no pip install beyond the Databricks connector,
which is itself optional.

The behaviours pinned here are the ones where being wrong is dangerous rather
than merely untidy: a failing graph must never look like "no impact", an
unrecognised payload must never look like "nothing changed", and credentials
must never be reported as configured when they are absent.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", "..")))

from tools.checkpoint_lens.databricks import DatabricksConfig, DatabricksSync
from tools.checkpoint_lens.entities import EntityChange, parse_entity_changes, risky, touched_paths
from tools.checkpoint_lens.graph import GraphClient, _names
from tools.checkpoint_lens.models import (
    INTENT_FROM_FIRST_PROMPT,
    Attribution,
    CheckpointRecord,
    Decision,
    SessionRecord,
    TokenUsage,
)
from tools.checkpoint_lens.reader import CheckpointReader, extract_decisions
from tools.checkpoint_lens import requirements as reqs


# ---------------------------------------------------------------- decisions

class TestDecisionExtraction(unittest.TestCase):
    def _line(self, role: str, text: str) -> str:
        return json.dumps({"role": role, "content": [{"text": text}]})

    def test_classifies_blocker_risk_and_decision(self):
        transcript = "\n".join(
            [
                self._line("assistant", "I hit a blocker: the handoff file does not exist here."),
                self._line("assistant", "We decided to use the git-refs backend for all reads."),
                self._line("assistant", "This could break every caller of the old reader."),
            ]
        )
        found = extract_decisions(transcript)
        kinds = {d.kind for d in found}
        self.assertIn("blocker", kinds)
        self.assertIn("decision", kinds)
        self.assertIn("risk", kinds)

    def test_text_is_verbatim_not_paraphrased(self):
        """The whole point of a keyword classifier over an LLM is that a
        reviewer can audit the exact source sentence."""
        sentence = "We decided to union files_touched with the real commit stat."
        found = extract_decisions(self._line("assistant", sentence))
        self.assertEqual(1, len(found))
        self.assertEqual(sentence, found[0].text)

    def test_ignores_short_and_unmarked_prose(self):
        transcript = "\n".join(
            [
                self._line("assistant", "ok"),
                self._line("assistant", "Here is a perfectly ordinary sentence with no marker."),
            ]
        )
        self.assertEqual([], extract_decisions(transcript))

    def test_survives_malformed_jsonl(self):
        """A truncated or corrupt transcript line must not take the command
        down - partial context is still useful context."""
        transcript = "\n".join(
            ["{not json at all", self._line("assistant", "We decided to keep going regardless.")]
        )
        found = extract_decisions(transcript)
        self.assertEqual(1, len(found))

    def test_deduplicates_repeated_statements(self):
        line = self._line("assistant", "We decided to use the git-refs backend for all reads.")
        found = extract_decisions("\n".join([line, line, line]))
        self.assertEqual(1, len(found))


# ------------------------------------------------------------- entity diff

class TestEntityParsing(unittest.TestCase):
    PAYLOAD = {
        "base": "aaa",
        "head": "bbb",
        "files": [
            {
                "path": "tools/x.py",
                "status": "M",
                "language": "Python",
                "changes": [
                    {"type": "signature_changed", "kind": "function", "name": "f", "dependents_count": 7},
                    {"type": "added", "kind": "function", "name": "g", "dependents_count": 0},
                ],
            }
        ],
    }

    def test_parses_nested_file_changes(self):
        changes = parse_entity_changes(self.PAYLOAD)
        self.assertEqual(2, len(changes))
        self.assertEqual("tools/x.py", changes[0].path)
        self.assertEqual(7, changes[0].dependents_count)

    def test_risky_is_signature_or_removal_with_dependents(self):
        danger = risky(parse_entity_changes(self.PAYLOAD))
        self.assertEqual(1, len(danger))
        self.assertEqual("f", danger[0].name)

    def test_added_symbol_with_no_dependents_is_not_risky(self):
        c = EntityChange(path="a.py", name="g", kind="function", change_type="added")
        self.assertFalse(c.is_risky)

    def test_unrecognised_payload_returns_empty_not_garbage(self):
        """An empty list is rendered by callers as 'graph returned no entries'.
        It must never be produced by guessing at an unknown shape."""
        for bad in (None, {}, {"files": "nope"}, [], "text", {"files": [1, 2]}):
            self.assertEqual([], parse_entity_changes(bad))

    def test_touched_paths_deduplicates_preserving_order(self):
        changes = [
            EntityChange(path="b.py", name="1", kind="f", change_type="added"),
            EntityChange(path="a.py", name="2", kind="f", change_type="added"),
            EntityChange(path="b.py", name="3", kind="f", change_type="added"),
        ]
        self.assertEqual(["b.py", "a.py"], touched_paths(changes))


# ------------------------------------------------------------- requirements

class TestRequirementExtraction(unittest.TestCase):
    def test_extracts_numbered_and_bulleted_behaviour(self):
        plan = "\n".join(
            [
                "1. Build a CheckpointReader that parses checkpoints.",
                "- Implement a GraphClient wrapping entire graph.",
            ]
        )
        found = reqs.extract_requirements([plan])
        self.assertEqual(2, len(found))

    def test_rejects_event_logistics(self):
        """A pasted planning document is mostly logistics. Without this filter
        the extracted 'plan' is dominated by venue and schedule lines."""
        plan = "\n".join(
            [
                "- **Venue:** Scaler School of Technology, Bengaluru",
                "- **Submission deadline:** 3:00 PM IST hard cutoff",
                "- Build a CheckpointReader that parses checkpoint metadata.",
            ]
        )
        found = reqs.extract_requirements([plan])
        self.assertEqual(1, len(found))
        self.assertIn("CheckpointReader", found[0].text)

    def test_rejects_prose_without_behaviour(self):
        plan = "- The project is interesting and the team is motivated today"
        self.assertEqual([], reqs.extract_requirements([plan]))

    def test_deduplicates_across_prompts(self):
        line = "1. Build a CheckpointReader that parses checkpoints."
        found = reqs.extract_requirements([line, line])
        self.assertEqual(1, len(found))

    def test_keywords_drop_stopwords(self):
        kw = [k.lower() for k in reqs.keywords("We must build the CheckpointReader for all files")]
        self.assertIn("checkpointreader", kw)
        self.assertNotIn("must", kw)
        self.assertNotIn("the", kw)


class TestDriftClassification(unittest.TestCase):
    REQ = reqs.Requirement(text="Build a CheckpointReader that parses checkpoint metadata")

    def test_no_hits_is_missing(self):
        verdict, score = reqs.classify([], self.REQ)
        self.assertEqual(reqs.MISSING, verdict)
        self.assertEqual(0.0, score)

    def test_strong_match_is_implemented(self):
        hits = ["CheckpointReader reader.py parses checkpoint metadata records"]
        verdict, _ = reqs.classify(hits, self.REQ)
        self.assertEqual(reqs.IMPLEMENTED, verdict)

    def test_weak_match_is_partial_not_implemented(self):
        verdict, score = reqs.classify(["unrelated.py checkpoint"], self.REQ)
        self.assertIn(verdict, (reqs.PARTIAL, reqs.MISSING))
        self.assertLess(score, 0.5)

    def test_every_verdict_has_a_reader_facing_note(self):
        for v in (reqs.IMPLEMENTED, reqs.PARTIAL, reqs.MISSING, reqs.UNVERIFIED):
            self.assertTrue(reqs.VERDICT_NOTE[v].strip())

    def test_missing_note_warns_against_treating_absence_as_proof(self):
        self.assertIn("not proof", reqs.VERDICT_NOTE[reqs.MISSING].lower())


# ------------------------------------------------------------------- graph

class TestGraphClientSafety(unittest.TestCase):
    def test_failure_is_not_silently_empty(self):
        """A graph that cannot answer must report ok=False. Rendering an error
        as an empty impact section is how 'unavailable' becomes 'safe to
        change', which is the exact bug this guards."""
        client = GraphClient(".")
        res = client._run(["graph", "definitely-not-a-real-subcommand"])
        self.assertFalse(res.ok)
        self.assertTrue(res.error)

    def test_impact_failure_keeps_command_for_verification(self):
        client = GraphClient(".")
        client._available = False
        summary = client.impact("NoSuchSymbol")
        self.assertTrue(summary.command)

    def test_names_tolerates_shapes_and_missing_keys(self):
        self.assertEqual(["a"], _names({"callers": ["a"]}, "callers"))
        self.assertEqual(["f (x.py)"], _names({"callers": [{"name": "f", "file": "x.py"}]}, "callers"))
        self.assertEqual([], _names({}, "callers"))


# -------------------------------------------------------------- databricks

class TestDatabricksConfig(unittest.TestCase):
    def test_unconfigured_is_reported_not_assumed(self):
        cfg = DatabricksConfig()
        self.assertFalse(cfg.configured)

    def test_missing_credentials_produce_a_reason(self):
        sync = DatabricksSync(".", config=DatabricksConfig())
        reason = sync.unavailable_reason()
        self.assertTrue(reason)

    def test_partial_credentials_are_not_configured(self):
        cfg = DatabricksConfig(server_hostname="h", http_path="", access_token="t")
        self.assertFalse(cfg.configured)

    def test_table_names_are_fully_qualified(self):
        cfg = DatabricksConfig(catalog="workspace", schema="checkpoint_lens")
        self.assertEqual("workspace.checkpoint_lens.checkpoint_sessions", cfg.table("checkpoint_sessions"))

    def test_env_overrides_file(self):
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, ".databricks.local.json"), "w", encoding="utf-8") as fh:
                json.dump({"server_hostname": "from-file", "http_path": "p", "access_token": "t"}, fh)
            os.environ["DATABRICKS_SERVER_HOSTNAME"] = "from-env"
            try:
                cfg = DatabricksConfig.resolve(d)
                self.assertEqual("from-env", cfg.server_hostname)
            finally:
                del os.environ["DATABRICKS_SERVER_HOSTNAME"]


# ------------------------------------------------------------------ models

class TestModels(unittest.TestCase):
    def test_token_total_sums_every_component(self):
        t = TokenUsage(input_tokens=1, cache_creation_tokens=2, cache_read_tokens=3, output_tokens=4)
        self.assertEqual(10, t.total)

    def test_session_row_is_flat_for_delta(self):
        s = SessionRecord(checkpoint_id="C", session_index=0, intent="do a thing")
        row = s.to_row()
        for value in row.values():
            self.assertNotIsInstance(value, (TokenUsage, Attribution))

    def test_checkpoint_intent_comes_from_first_session_that_has_one(self):
        rec = CheckpointRecord(checkpoint_id="C")
        rec.sessions = [
            SessionRecord(checkpoint_id="C", session_index=0, intent=""),
            SessionRecord(
                checkpoint_id="C", session_index=1, intent="real intent",
                intent_source=INTENT_FROM_FIRST_PROMPT,
            ),
        ]
        self.assertEqual("real intent", rec.intent)
        self.assertEqual(INTENT_FROM_FIRST_PROMPT, rec.intent_source)

    def test_all_decisions_spans_sessions(self):
        rec = CheckpointRecord(checkpoint_id="C")
        rec.sessions = [
            SessionRecord(checkpoint_id="C", session_index=0, decisions=[Decision("risk", "a" * 40)]),
            SessionRecord(checkpoint_id="C", session_index=1, decisions=[Decision("blocker", "b" * 40)]),
        ]
        self.assertEqual(2, len(rec.all_decisions()))


# ------------------------------------------------------------------ reader

class TestReaderAgainstEmptyRepo(unittest.TestCase):
    """An ordinary git repo with no Entire checkpoints must produce an empty
    result and no crash - the tool is meant to be pointed at any repo."""

    def test_repo_without_checkpoints_reads_empty(self):
        with tempfile.TemporaryDirectory() as d:
            subprocess.run(["git", "init", "-q", d], check=True, capture_output=True)
            reader = CheckpointReader(d)
            self.assertEqual([], reader.list_refs())
            self.assertEqual([], reader.read_all())

    def test_prompt_separator_splits_on_rule(self):
        self.assertEqual(
            ["first prompt", "second prompt"],
            [p.strip() for p in
             __import__("tools.checkpoint_lens.reader", fromlist=["PROMPT_SEPARATOR"])
             .PROMPT_SEPARATOR.split("first prompt\n---\nsecond prompt")],
        )

    def test_intent_prefers_a_substantive_prompt_over_a_command(self):
        prompts = ["cat CLAUDE.md", "x" * 200]
        self.assertEqual("x" * 200, CheckpointReader._pick_intent(prompts))

    def test_intent_falls_back_to_first_prompt_when_all_are_short(self):
        self.assertEqual("ls", CheckpointReader._pick_intent(["ls", "pwd"]))


class TestReaderAgainstThisRepo(unittest.TestCase):
    """Integration: this repository has real checkpoints, so the reader is
    exercised against genuine data rather than a fixture."""

    @classmethod
    def setUpClass(cls):
        repo = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
        cls.reader = CheckpointReader(repo)
        cls.records = cls.reader.read_all()

    def test_finds_real_checkpoints(self):
        if not self.records:
            self.skipTest("no checkpoints in this clone")
        self.assertGreater(len(self.records), 0)

    def test_checkpoints_are_chronological(self):
        if len(self.records) < 2:
            self.skipTest("need two checkpoints to test ordering")
        ids = [r.checkpoint_id for r in self.records]
        self.assertEqual(sorted(ids), ids, "ULIDs must sort chronologically")

    def test_checkpoints_link_to_commits_via_trailer(self):
        if not self.records:
            self.skipTest("no checkpoints in this clone")
        self.assertTrue(any(r.linked_commit for r in self.records))

    def test_files_touched_unions_checkpoint_with_commit(self):
        """The regression this guards: the checkpoint's own files_touched
        under-reports, so a checkpoint-only read hides changed files from
        blast-radius analysis."""
        linked = [r for r in self.records if r.linked_commit]
        if not linked:
            self.skipTest("no linked checkpoints")
        rec = linked[0]
        commit_files = set(self.reader.commit_files(rec.linked_commit))
        if not commit_files:
            self.skipTest("commit reported no files")
        self.assertTrue(commit_files.issubset(set(rec.files_touched)))


if __name__ == "__main__":
    unittest.main(verbosity=2)
