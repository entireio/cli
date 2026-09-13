"""Tests for extraction quality - the credibility surface of the product.

Every case here corresponds to real noise observed in output against this
repository's own checkpoints. The product's central claim is that it preserves
*why* a change happened; if this section fills with restatements of the prompt
or with mentions of a keyword, the claim does not survive first contact with a
reader.
"""

from __future__ import annotations

import json
import os
import sys
import unittest

sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", "..")))

from tools.checkpoint_lens import html as htmlreport
from tools.checkpoint_lens.databricks import is_generated_path
from tools.checkpoint_lens.models import (
    INTENT_FROM_FIRST_PROMPT,
    CheckpointRecord,
    Decision,
    SessionRecord,
)
from tools.checkpoint_lens.reader import (
    EchoFilter,
    attribute_to_first_appearance,
    extract_decisions,
    first_user_message,
)


def line(role: str, text: str) -> str:
    return json.dumps({"role": role, "content": [{"text": text}]})


def kinds(found: list[Decision]) -> set[str]:
    return {d.kind for d in found}


class TestEchoSuppression(unittest.TestCase):
    """A decision is something the agent CONCLUDED, not a restatement of what
    the user asked for. Without this filter the highest-value section of the
    report filled with the prompt it was given."""

    def test_verbatim_echo_of_the_prompt_is_dropped(self):
        prompt = "Commit everything with a substantive message covering the architecture."
        self.assertEqual([], extract_decisions(line("assistant", prompt), prompts=[prompt]))

    def test_near_echo_is_dropped(self):
        prompt = "Build a CheckpointReader that parses checkpoints, because we need intent."
        near = "Build a CheckpointReader parsing checkpoints because we need the intent."
        self.assertEqual([], extract_decisions(line("assistant", near), prompts=[prompt]))

    def test_genuine_decision_survives_the_filter(self):
        prompt = "Build a CheckpointReader that parses checkpoints."
        real = "We chose the git-refs backend because this repo does not use the v1 branch."
        self.assertEqual(1, len(extract_decisions(line("assistant", real), prompts=[prompt])))

    def test_user_turns_are_never_decisions(self):
        """A user turn is a request, not a conclusion."""
        text = "We decided to use the git-refs backend for every read path here."
        self.assertEqual([], extract_decisions(line("user", text)))

    def test_questions_are_not_decisions(self):
        q = "Should we have decided to use the git-refs backend for reads instead?"
        self.assertEqual([], extract_decisions(line("assistant", q)))

    def test_absent_prompts_suppress_nothing(self):
        real = "We chose the git-refs backend because the v1 branch is legacy here."
        self.assertEqual(1, len(extract_decisions(line("assistant", real), prompts=[])))

    def test_echo_filter_tolerates_empty_and_none(self):
        self.assertFalse(EchoFilter([]).is_echo("anything at all goes here"))
        self.assertFalse(EchoFilter(None).is_echo("anything at all goes here"))


class TestClassifierPrecision(unittest.TestCase):
    """Substring matching on bare nouns matched *mentions* rather than claims.
    Each case below was a real false positive in shipped output."""

    def d(self, text: str) -> list[Decision]:
        return extract_decisions(line("assistant", text))

    def test_mention_of_the_word_risk_is_not_a_risk(self):
        self.assertNotIn(
            "risk", kinds(self.d("The table stores a file-churn/risk score computed per path."))
        )

    def test_defining_abandonment_is_not_a_rejection(self):
        self.assertNotIn(
            "rejected",
            kinds(self.d("Checkpoints preserve what the agent attempted and what it dropped.")),
        )

    def test_stated_rejection_is_classified(self):
        self.assertIn(
            "rejected", kinds(self.d("Installing Go was rejected in favour of Python for speed."))
        )

    def test_causal_prose_is_captured_as_rationale(self):
        self.assertIn(
            "rationale",
            kinds(self.d("Aggregates run in SQL so that no single checkpoint can answer them.")),
        )

    def test_all_caps_heading_is_not_a_finding(self):
        self.assertEqual([], self.d("DATABRICKS IS LOAD-BEARING, NOT SUPERFICIAL STORAGE"))

    def test_hard_wrapped_sentence_is_not_split_mid_clause(self):
        """Wrapped prose was severed at newlines, producing report entries that
        began in the middle of a clause."""
        wrapped = "We chose the git-refs backend because the legacy branch\nis not used here at all."
        found = self.d(wrapped)
        self.assertEqual(1, len(found))
        self.assertTrue(found[0].text.startswith("We chose"))
        self.assertIn("at all", found[0].text)

    def test_markdown_emphasis_is_stripped(self):
        found = self.d("We **chose** the `git-refs` backend because it is the current one.")
        self.assertTrue(found)
        self.assertNotIn("**", found[0].text)
        self.assertNotIn("`", found[0].text)


class TestCommitMessageSource(unittest.TestCase):
    def test_commit_body_is_mined_and_ranked_high(self):
        msg = (
            "feat: do a thing\n\n"
            "The reader unions both lists because the checkpoint under-reports files.\n"
        )
        found = extract_decisions("", commit_message=msg)
        self.assertEqual(1, len(found))
        self.assertEqual("commit_message", found[0].source)
        self.assertEqual("high", found[0].confidence)

    def test_subject_line_alone_is_not_mined(self):
        """Only the body carries reasoning; the subject is a label."""
        self.assertEqual([], extract_decisions("", commit_message="fix: chose a better name"))

    def test_trailers_are_not_decisions(self):
        msg = "feat: x\n\nCo-Authored-By: Someone <a@b.c>\nEntire-Checkpoint: 01ABC\n"
        self.assertEqual([], extract_decisions("", commit_message=msg))

    def test_transcript_is_medium_confidence(self):
        found = extract_decisions(
            line("assistant", "We chose the git-refs backend because it is current here.")
        )
        self.assertEqual("transcript", found[0].source)
        self.assertEqual("medium", found[0].confidence)


class TestFirstAppearanceAttribution(unittest.TestCase):
    """Entire stores the whole compacted session in EVERY checkpoint, so raw
    occurrence counts make one blocker look like it was raised again on every
    commit, and any trend line becomes monotonically increasing noise."""

    def rec(self, cid: str, texts: list[str]) -> CheckpointRecord:
        r = CheckpointRecord(checkpoint_id=cid)
        r.sessions = [
            SessionRecord(
                checkpoint_id=cid,
                session_index=0,
                decisions=[Decision("risk", t) for t in texts],
            )
        ]
        return r

    def test_repeated_decision_counts_only_at_first_appearance(self):
        shared = "No tests yet, which is the biggest quality risk on the board."
        records = [
            self.rec("A", [shared]),
            self.rec("B", [shared]),
            self.rec("C", [shared, "A second distinct risk appears at C here."]),
        ]
        first = attribute_to_first_appearance(records)
        self.assertEqual(1, len(first["A"]))
        self.assertEqual(0, len(first["B"]), "a repeat must not be re-counted")
        self.assertEqual(1, len(first["C"]))

    def test_every_checkpoint_gets_a_key_even_when_empty(self):
        first = attribute_to_first_appearance([self.rec("A", [])])
        self.assertEqual([], first["A"])

    def test_attribution_is_insensitive_to_punctuation_and_case(self):
        records = [self.rec("A", ["We chose the Git-Refs backend!"]),
                   self.rec("B", ["we chose the git refs backend"])]
        first = attribute_to_first_appearance(records)
        self.assertEqual(0, len(first["B"]))


class TestGeneratedPathFilter(unittest.TestCase):
    """Build artefacts dominated the churn ranking that is meant to point a
    reviewer at risky source."""

    def test_build_artefacts_are_generated(self):
        for p in (
            "tools/checkpoint_lens/__pycache__/cli.cpython-311.pyc",
            "node_modules/left-pad/index.js",
            "dist/app.min.js",
            "go.sum",
            "target/debug/thing.o",
        ):
            self.assertTrue(is_generated_path(p), p)

    def test_source_files_are_not_generated(self):
        for p in ("tools/checkpoint_lens/reader.py", "BUILDATHON.md", "cmd/entire/main.go"):
            self.assertFalse(is_generated_path(p), p)

    def test_windows_separators_are_handled(self):
        self.assertTrue(is_generated_path("tools\\pkg\\__pycache__\\x.pyc"))


class TestIntentFromTranscript(unittest.TestCase):
    def test_first_substantive_user_message_wins(self):
        transcript = "\n".join(
            [line("user", "ls"), line("assistant", "ok"), line("user", "x" * 60)]
        )
        self.assertEqual("x" * 60, first_user_message(transcript))

    def test_falls_back_to_a_short_message_when_that_is_all_there_is(self):
        self.assertEqual("ls", first_user_message(line("user", "ls")))

    def test_no_user_message_returns_empty(self):
        self.assertEqual("", first_user_message(line("assistant", "only me talking here")))


class TestHtmlReport(unittest.TestCase):
    """The HTML report is the fallback when live infrastructure fails during a
    demo, so it must open with no network at all."""

    def rec(self) -> CheckpointRecord:
        r = CheckpointRecord(checkpoint_id="01TEST", branch="main")
        r.files_touched = ["a.py"]
        r.sessions = [
            SessionRecord(
                checkpoint_id="01TEST",
                session_index=0,
                intent="Do the thing",
                intent_source=INTENT_FROM_FIRST_PROMPT,
                decisions=[Decision("risk", "Something could break in the reader path.")],
            )
        ]
        return r

    def test_report_loads_no_external_resources(self):
        markup = htmlreport.render_handoff(self.rec(), [], [], None, None)
        for tag in ("<script", "<link", "<img", "<iframe"):
            self.assertNotIn(tag, markup)

    def test_content_is_escaped(self):
        r = self.rec()
        r.sessions[0].intent = "<script>alert(1)</script>"
        markup = htmlreport.render_handoff(r, [], [], None, None)
        self.assertNotIn("<script>alert(1)</script>", markup)
        self.assertIn("&lt;script&gt;", markup)

    def test_unavailable_databricks_is_stated_not_omitted(self):
        """A silently missing section reads as a clean bill of health."""
        markup = htmlreport.render_handoff(
            self.rec(), [], [], None, {"unavailable": "no credentials"}
        )
        self.assertIn("no credentials", markup)

    def test_warnings_are_rendered(self):
        markup = htmlreport.render_handoff(self.rec(), [], ["partial read"], None, None)
        self.assertIn("partial read", markup)

    def test_trend_chart_is_inline_svg(self):
        rows = [{"checkpoint_id": "A", "unresolved_total": 3},
                {"checkpoint_id": "B", "unresolved_total": 1}]
        markup = htmlreport.render_handoff(self.rec(), [], [], None, {"trend": rows})
        self.assertIn("<svg", markup)
        self.assertNotIn("<script", markup)

    def test_two_points_are_not_presented_as_a_trend(self):
        rows = [{"checkpoint_id": "A", "unresolved_total": 3},
                {"checkpoint_id": "B", "unresolved_total": 1}]
        markup = htmlreport.render_handoff(self.rec(), [], [], None, {"trend": rows})
        self.assertIn("too few to read as a trend", markup)


if __name__ == "__main__":
    unittest.main(verbosity=2)
