"""Checkpoint Lens CLI.

Resolved by the Entire CLI's kubectl-style external-command lookup: a binary
named ``entire-lens`` on PATH is invoked as ``entire lens <subcommand>``.

Subcommands map one-to-one onto the Track 1 use cases:

    handoff   hand work to another developer or agent, and resume after a break
    drift     review an implementation against its stated intent, and list
              unfinished requirements
    assess    assess one change against the intent of its nearest checkpoint
    sync      push parsed checkpoint records to Databricks and read the
              cross-session aggregates back
"""

from __future__ import annotations

import argparse
import json
import sys
from concurrent.futures import ThreadPoolExecutor
from dataclasses import asdict
from typing import Any

from . import __version__
from .graph import GraphClient
from .databricks import DatabricksSync
from .entities import parse_entity_changes, risky, touched_paths
from .models import CheckpointRecord
from .reader import CheckpointReader
from . import html as htmlreport
from . import report
from . import requirements


def _configure_stdio() -> None:
    """Checkpoint text is arbitrary Unicode written by an agent, but a Windows
    console defaults to cp1252. Without this, a single arrow or em-dash in a
    recovered decision crashes the whole report. Degrade the character rather
    than the command.
    """
    for stream in (sys.stdout, sys.stderr):
        reconfigure = getattr(stream, "reconfigure", None)
        if reconfigure is not None:
            try:
                reconfigure(encoding="utf-8", errors="replace")
            except (ValueError, OSError):
                pass


def _emit(lines: list[str]) -> None:
    sys.stdout.write("\n".join(lines) + "\n")


def _load(args: argparse.Namespace) -> tuple[CheckpointReader, list[CheckpointRecord]]:
    reader = CheckpointReader(args.repo)
    records = reader.read_all()
    if not records:
        sys.stderr.write(
            "No Entire Checkpoints found in %s.\n"
            "Checkpoint Lens reads real checkpoint context; it has nothing to\n"
            "work from until at least one checkpointed commit exists.\n"
            "Run `entire status` to confirm checkpoints are enabled.\n" % args.repo
        )
        raise SystemExit(2)
    return reader, records


def _select(records: list[CheckpointRecord], wanted: str | None) -> CheckpointRecord:
    if not wanted:
        return records[-1]
    for rec in records:
        if rec.checkpoint_id == wanted or rec.checkpoint_id.startswith(wanted):
            return rec
    sys.stderr.write("No checkpoint matching %r. Known ids:\n" % wanted)
    for rec in records:
        sys.stderr.write("  " + rec.checkpoint_id + "\n")
    raise SystemExit(2)


# --------------------------------------------------------------------------
# handoff
# --------------------------------------------------------------------------

def cmd_handoff(args: argparse.Namespace) -> int:
    reader, records = _load(args)
    rec = _select(records, args.checkpoint)

    if args.json:
        payload: dict[str, Any] = {
            "checkpoint": rec.to_dict(),
            "warnings": reader.warnings,
            "history_length": len(records),
        }
        print(json.dumps(payload, indent=2, default=str))
        return 0

    lines: list[str] = []
    lines.extend(report.render_header("CHECKPOINT LENS - HANDOFF BRIEF", args.repo))
    lines.append(
        "  This brief reconstructs a stopped session from checkpoint context:"
    )
    lines.append(
        "  what was intended, what was decided, and what is still open."
    )
    lines.extend(report.render_checkpoint_summary(rec))
    lines.extend(report.render_intent(rec))
    lines.extend(report.render_sessions(rec))

    lines.append(report.section("DECISIONS, RISKS AND OPEN QUESTIONS"))
    lines.append("  Recovered verbatim - never paraphrased or generated.")
    lines.append("")
    lines.extend(report.render_decisions(rec.all_decisions(), limit=args.limit))

    lines.extend(report.render_files(rec))

    # Graph blast radius over the files this session actually touched.
    graph_lines: list[str] = []
    if not args.no_graph:
        graph_lines = _graph_blast_radius(args.repo, rec)
        lines.extend(graph_lines)

    if len(records) > 1:
        lines.append(report.section("SESSION HISTORY"))
        for r in records:
            lines.append(
                "  %s  %s  %s"
                % (
                    r.checkpoint_id,
                    (r.created_at or "")[:19] or "unknown-time",
                    (r.linked_subject or "(unlinked)")[:40],
                )
            )

    lines.extend(_databricks_section(args, headline_only=True))
    lines.extend(report.render_warnings(reader.warnings))
    lines.extend(
        report.footer(
            [
                "Intent provenance is stated above; verify any generated text "
                "against the linked commit before acting on it.",
                "Next: `entire lens drift` to see which stated requirements are "
                "still unfinished.",
            ]
        )
    )
    _emit(lines)

    if args.html:
        agg = _aggregates_for_html(args)
        _write_html(
            args.html,
            htmlreport.render_handoff(
                rec, records, reader.warnings, graph_lines, agg
            ),
        )
    return 0


def _aggregates_for_html(args: argparse.Namespace) -> dict[str, Any]:
    """Trend rows for the HTML chart, or the reason there are none."""
    if getattr(args, "no_databricks", False):
        return {"unavailable": "skipped with --no-databricks"}
    sync = DatabricksSync(args.repo)
    reason = sync.unavailable_reason()
    if reason:
        return {"unavailable": reason}
    trend = sync.open_items_trend()
    if not trend.ok:
        return {"unavailable": trend.error[:200]}
    return {"trend": trend.rows}


def _write_html(path: str, markup: str) -> None:
    with open(path, "w", encoding="utf-8") as fh:
        fh.write(markup)
    sys.stdout.write("\nHTML report written to %s\n" % path)


def _graph_blast_radius(repo: str, rec: CheckpointRecord) -> list[str]:
    """Ask the graph what else depends on what this session touched."""
    graph = GraphClient(repo)
    if not graph.available():
        return report.render_graph_evidence(
            "GRAPH IMPACT (BLAST RADIUS)",
            "entire graph impact --repo %s --symbol <symbol>" % repo,
            [],
            ok=False,
            error="entire graph plugin not available on this machine",
        )

    target = rec.linked_commit or ""
    if not target:
        return report.render_graph_evidence(
            "GRAPH IMPACT (BLAST RADIUS)",
            "entire graph checkpoint --repo %s --id %s" % (repo, rec.checkpoint_id),
            [],
            ok=False,
            error="checkpoint has no linked commit to analyze",
        )

    res = graph.commit_entities(target)
    body: list[str] = []
    if res.ok:
        changes = parse_entity_changes(res.data)
        if changes:
            paths = touched_paths(changes)
            body.append(
                "  %d entity change(s) across %d file(s):"
                % (len(changes), len(paths))
            )
            for c in changes[:15]:
                suffix = ""
                if c.dependents_count:
                    suffix = "  <- %d dependent(s)" % c.dependents_count
                body.append("    - %-46s %s%s" % (c.label[:46], c.path, suffix))
            if len(changes) > 15:
                body.append("    ... %d more" % (len(changes) - 15))
            danger = risky(changes)
            if danger:
                body.append("")
                body.append("  HIGH-RISK (signature/removal with dependents):")
                for c in danger[:5]:
                    body.append(
                        "    ! %s in %s (%d dependents)"
                        % (c.label, c.path, c.dependents_count)
                    )
        elif res.text.strip():
            for line in res.text.strip().splitlines()[:12]:
                body.append("  " + line)
    return report.render_graph_evidence(
        "GRAPH IMPACT (BLAST RADIUS)",
        res.command_str,
        body,
        ok=res.ok,
        error=res.error,
    )


# --------------------------------------------------------------------------
# drift
# --------------------------------------------------------------------------

def cmd_drift(args: argparse.Namespace) -> int:
    """Review the implementation against the plan captured in the FIRST
    checkpoint, and list what is still unfinished."""
    reader, records = _load(args)
    baseline = records[0]
    head = records[-1]

    reqs = requirements.extract_requirements(
        [baseline.intent] + [p for s in baseline.sessions for p in s.prompts],
        limit=args.limit,
    )

    graph = GraphClient(args.repo)
    graph_ok = (not args.no_graph) and graph.available()

    def check(req: requirements.Requirement) -> requirements.DriftFinding:
        finding = requirements.DriftFinding(requirement=req)
        if not graph_ok:
            finding.verdict = requirements.UNVERIFIED
            finding.command = "entire graph search --repo %s --query ..." % args.repo
            return finding
        query = " ".join(requirements.keywords(req.text, limit=6)) or req.text[:80]
        res = graph.search(query, profile=args.profile, limit=8)
        finding.command = res.command_str
        if not res.ok:
            finding.verdict = requirements.UNVERIFIED
        else:
            hits = _search_hits(res.data, res.text)
            finding.evidence = hits[:5]
            finding.verdict, finding.score = requirements.classify(hits, req)
        return finding

    # One graph search per requirement, run concurrently. Each search is an
    # independent subprocess, so the wall-clock cost is the slowest query
    # rather than their sum - 18 sequential `full`-profile searches took over
    # four minutes, which is not a tool anyone would run twice.
    if graph_ok and len(reqs) > 1:
        with ThreadPoolExecutor(max_workers=args.jobs) as pool:
            findings = list(pool.map(check, reqs))
    else:
        findings = [check(r) for r in reqs]

    # Entity-level diff between the plan's commit and now. Entity-level is the
    # point: a function that merely moved is not drift.
    diff_res = None
    entity_changes: list[Any] = []
    if graph_ok and baseline.linked_commit and head.linked_commit:
        diff_res = graph.diff(baseline.linked_commit, head.linked_commit)
        if diff_res.ok:
            entity_changes = parse_entity_changes(diff_res.data)

    if args.json:
        print(
            json.dumps(
                {
                    "baseline_checkpoint": baseline.checkpoint_id,
                    "head_checkpoint": head.checkpoint_id,
                    "findings": [
                        {
                            "requirement": f.requirement.text,
                            "origin": f.requirement.origin,
                            "verdict": f.verdict,
                            "score": round(f.score, 2),
                            "evidence": f.evidence,
                            "command": f.command,
                        }
                        for f in findings
                    ],
                    "entity_changes": [vars(c) for c in entity_changes],
                    "warnings": reader.warnings,
                },
                indent=2,
                default=str,
            )
        )
        return 0

    lines: list[str] = []
    lines.extend(report.render_header("CHECKPOINT LENS - DRIFT REPORT", args.repo))
    lines.append("  Plan (baseline) : %s" % baseline.checkpoint_id)
    lines.append("                    %s" % (baseline.linked_subject or "(unlinked)"))
    lines.append("  Current (head)  : %s" % head.checkpoint_id)
    lines.append("                    %s" % (head.linked_subject or "(unlinked)"))

    lines.extend(report.render_drift_findings(findings))

    lines.append(report.section("ENTITY-LEVEL CHANGE SINCE THE PLAN"))
    if not graph_ok:
        lines.append("  graph unavailable - entity-level comparison skipped.")
    elif diff_res is None:
        lines.append("  baseline or head checkpoint has no linked commit.")
    elif not diff_res.ok:
        lines.append("  graph diff failed: " + diff_res.error)
    else:
        lines.append("  evidence command (rerun to verify):")
        lines.append("    $ " + diff_res.command_str)
        lines.append("")
        if entity_changes:
            lines.append(
                "  %d entity change(s) across %d file(s). Entity-level, so a"
                % (len(entity_changes), len(touched_paths(entity_changes)))
            )
            lines.append("  function that only moved is not counted as drift.")
            for c in entity_changes[:12]:
                lines.append("    - %-44s %s" % (c.label[:44], c.path))
            if len(entity_changes) > 12:
                lines.append("    ... %d more" % (len(entity_changes) - 12))
        else:
            lines.append("  (graph reported no entity changes)")

    lines.extend(_databricks_section(args, headline_only=True))
    lines.extend(report.render_warnings(reader.warnings))
    lines.extend(
        report.footer(
            [
                "MISSING means no evidence was found, not that the work is "
                "absent. Verify each open item against source before acting.",
            ]
        )
    )
    _emit(lines)

    open_items = [f for f in findings if f.is_open]
    if args.fail_on_open and open_items:
        sys.stderr.write(
            "\ndrift gate FAILED: %d of %d stated requirement(s) have no "
            "complete implementation evidence.\n" % (len(open_items), len(findings))
        )

    if args.html:
        _write_html(
            args.html,
            htmlreport.render_drift(
                baseline,
                head,
                findings,
                entity_changes,
                diff_res.command_str if diff_res else "",
                reader.warnings,
            ),
        )
    # Non-zero lets drift run as a release gate in CI: "did we build what the
    # plan said we would?" fails the pipeline the same way a red test does.
    if args.fail_on_open and open_items:
        return 1
    return 0


def _search_hits(data: Any, text: str) -> list[str]:
    """Flatten graph search output into strings a requirement can be scored
    against.

    ``entire graph search --format json`` returns ``results[]`` carrying
    ``file_path`` plus line spans, and usually a ``snippet`` of the matched
    source. Both matter: the path says *where* the implementation lives, the
    snippet says *what* it is, and a requirement's keywords can legitimately
    match either.
    """
    out: list[str] = []
    if isinstance(data, dict):
        results = data.get("results")
        if isinstance(results, list):
            for item in results:
                if isinstance(item, str):
                    out.append(item)
                    continue
                if not isinstance(item, dict):
                    continue
                parts: list[str] = []
                for key in ("symbol", "symbol_name", "name", "container", "file_path", "path"):
                    val = item.get(key)
                    if isinstance(val, str) and val:
                        parts.append(val)
                snippet = item.get("snippet") or item.get("text") or ""
                if isinstance(snippet, str) and snippet:
                    parts.append(" ".join(snippet.split())[:300])
                if parts:
                    out.append(" ".join(parts))
            if out:
                return out
    if not out and text.strip():
        for line in text.strip().splitlines():
            line = line.strip()
            if line and not line.startswith("#"):
                out.append(line)
    return out


# --------------------------------------------------------------------------
# assess
# --------------------------------------------------------------------------

def cmd_assess(args: argparse.Namespace) -> int:
    """Assess one change against the stated intent of its nearest checkpoint."""
    reader, records = _load(args)
    target = args.commitish

    by_commit = {r.linked_commit: r for r in records if r.linked_commit}
    resolved = _resolve_rev(args.repo, target)
    rec = by_commit.get(resolved)
    nearest_note = "exact checkpoint for this commit"
    if rec is None:
        rec = _nearest_checkpoint(args.repo, records, resolved)
        nearest_note = "nearest preceding checkpoint (this commit has none of its own)"

    graph = GraphClient(args.repo)
    graph_ok = (not args.no_graph) and graph.available()
    changes: list[Any] = []
    res = None
    if graph_ok:
        res = graph.commit_entities(resolved or target)
        if res.ok:
            changes = parse_entity_changes(res.data)

    changed_paths = set(touched_paths(changes)) or set(
        reader.commit_files(resolved or target)
    )
    planned = set(rec.files_touched) if rec else set()
    unplanned = sorted(changed_paths - planned) if planned else []

    if args.json:
        print(
            json.dumps(
                {
                    "commit": resolved or target,
                    "checkpoint": rec.checkpoint_id if rec else None,
                    "checkpoint_relation": nearest_note,
                    "intent": rec.intent if rec else "",
                    "entity_changes": [vars(c) for c in changes],
                    "files_not_in_checkpoint": unplanned,
                    "warnings": reader.warnings,
                },
                indent=2,
                default=str,
            )
        )
        return 0

    lines: list[str] = []
    lines.extend(report.render_header("CHECKPOINT LENS - CHANGE ASSESSMENT", args.repo))
    lines.append("  commit: " + (resolved or target))
    if rec is None:
        lines.append("")
        lines.append("  No checkpoint context available for this commit.")
        lines.append("  A change-risk view without intent is just a diff -")
        lines.append("  run this against a checkpointed commit instead.")
        _emit(lines)
        return 1

    lines.append("  checkpoint: %s (%s)" % (rec.checkpoint_id, nearest_note))
    lines.extend(report.render_intent(rec))

    lines.append(report.section("WHAT THIS CHANGE ACTUALLY DID (ENTITY-LEVEL)"))
    if not graph_ok:
        lines.append("  graph unavailable - entity analysis skipped.")
    elif res is not None and not res.ok:
        lines.append("  graph failed: " + res.error)
    elif changes:
        lines.append("  evidence command (rerun to verify):")
        lines.append("    $ " + (res.command_str if res else ""))
        lines.append("")
        for c in changes[:15]:
            suffix = "  <- %d dependent(s)" % c.dependents_count if c.dependents_count else ""
            lines.append("    - %-44s %s%s" % (c.label[:44], c.path, suffix))
        danger = risky(changes)
        if danger:
            lines.append("")
            lines.append("  HIGH-RISK (signature change or removal with dependents):")
            for c in danger[:5]:
                lines.append(
                    "    ! %s in %s (%d dependents)" % (c.label, c.path, c.dependents_count)
                )
    else:
        lines.append("  (no entity changes reported)")

    lines.append(report.section("INTENT CROSS-REFERENCE"))
    if not planned:
        lines.append("  The checkpoint records no file list to compare against.")
    elif unplanned:
        lines.append("  Files changed that the checkpoint's intent did NOT mention:")
        for p in unplanned[:15]:
            lines.append("    ? " + p)
        lines.append("")
        lines.append("  These are not necessarily wrong - they are the places")
        lines.append("  where the change outgrew its stated plan, and the first")
        lines.append("  thing a reviewer should ask about.")
    else:
        lines.append("  Every changed file appears in the checkpoint's file list.")

    if args.verify:
        lines.extend(_verify_section(args, graph))

    lines.extend(report.render_warnings(reader.warnings))
    _emit(lines)
    return 0


def _verify_section(args: argparse.Namespace, graph: GraphClient) -> list[str]:
    """Run the project's tests and report an adjudicated verdict.

    An assessment that says what changed but not whether it still works is
    half an answer. This is the half that survives a reviewer asking "and did
    you run it?".
    """
    lines = [report.section("VERIFICATION")]
    if args.no_graph or not graph.available():
        lines.append("  graph unavailable - cannot adjudicate a test run.")
        return lines
    res, recorded = graph.verify(args.verify, args.verify_baseline)
    lines.append("  evidence command (rerun to verify):")
    lines.append("    $ " + res.command_str)
    lines.append("")
    if not res.ok:
        lines.append("  verification FAILED to run: " + (res.error or "unknown")[:300])
        lines.append("  NOTE: a verifier that did not run is not a passing test.")
        return lines
    if recorded:
        lines.append("  BASELINE RECORDED (%s)." % args.verify_baseline)
        lines.append("  This run is a STATE, not a delta: it says which tests pass")
        lines.append("  now, not which ones this change broke. Re-run after an edit")
        lines.append("  to get a before/after verdict.")
        lines.append("")
    body = (res.text or "").strip()
    if body:
        for line in body.splitlines()[:20]:
            lines.append("  " + line)
    else:
        lines.append("  (verifier returned no output)")
    return lines


def _resolve_rev(repo: str, rev: str) -> str:
    reader = CheckpointReader(repo)
    try:
        return reader._git("rev-parse", rev).strip()
    except Exception:  # noqa: BLE001
        return rev


def _nearest_checkpoint(
    repo: str, records: list[CheckpointRecord], commit: str
) -> CheckpointRecord | None:
    """The most recent checkpoint that is an ancestor of this commit."""
    reader = CheckpointReader(repo)
    for rec in reversed(records):
        if not rec.linked_commit:
            continue
        try:
            reader._git("merge-base", "--is-ancestor", rec.linked_commit, commit)
            return rec
        except Exception:  # noqa: BLE001
            continue
    return records[-1] if records else None


# --------------------------------------------------------------------------
# sync
# --------------------------------------------------------------------------

def _databricks_section(args: argparse.Namespace, headline_only: bool = False) -> list[str]:
    """The cross-session aggregate. Unavailable is rendered as unavailable."""
    if getattr(args, "no_databricks", False):
        return []
    sync = DatabricksSync(args.repo)
    reason = sync.unavailable_reason()
    lines = [report.section("CROSS-SESSION ANALYTICS (DATABRICKS)")]
    if reason:
        lines.append("  unavailable: " + reason)
        lines.append("  Single-checkpoint views above are unaffected; only the")
        lines.append("  cross-session trend and ranking are lost.")
        return lines
    cov = sync.coverage()
    if not cov.ok:
        lines.append("  query failed: " + cov.error[:200])
        return lines
    row = cov.rows[0] if cov.rows else {}
    lines.append(
        "  %s checkpoints | %s decisions captured | intent coverage %s%%"
        % (
            row.get("checkpoints", "?"),
            row.get("decisions_captured", "?"),
            row.get("intent_coverage_pct", "?"),
        )
    )
    lines.append(
        "  %s lines changed | avg %s%% agent-written"
        % (row.get("lines_changed", "?"), row.get("avg_agent_pct", "?"))
    )
    if headline_only:
        return lines

    trend = sync.open_items_trend()
    if trend.ok and trend.rows:
        lines.append("")
        lines.append("  Unresolved-context trend (blockers + open questions + risks):")
        for r in trend.rows:
            lines.append(
                "    %s  %s  unresolved=%s (blockers=%s open=%s risks=%s)"
                % (
                    str(r.get("checkpoint_id", ""))[:26],
                    str(r.get("created_at", ""))[:19],
                    r.get("unresolved_total", "?"),
                    r.get("blockers", "?"),
                    r.get("open_questions", "?"),
                    r.get("risks", "?"),
                )
            )
    churn = sync.file_churn()
    if churn.ok and churn.rows:
        lines.append("")
        lines.append("  File churn hotspots (touched across most checkpoints):")
        for r in churn.rows[:8]:
            lines.append(
                "    %-52s %s checkpoints"
                % (str(r.get("file_path", ""))[:52], r.get("checkpoints_touching", "?"))
            )
    return lines


def cmd_sync(args: argparse.Namespace) -> int:
    reader, records = _load(args)
    sync = DatabricksSync(args.repo)
    reason = sync.unavailable_reason()
    if reason:
        sys.stderr.write("Databricks unavailable: " + reason + "\n")
        return 2

    if not args.query_only:
        res = sync.sync(records)
        if not res.ok:
            sys.stderr.write("sync failed: " + res.error + "\n")
            return 1
        sys.stdout.write(
            "Synced %d session row(s), %d file row(s), %d decision row(s) "
            "from %d checkpoint(s).\n"
            % (
                res.sessions_written,
                res.files_written,
                res.decisions_written,
                len(records),
            )
        )

    lines = _databricks_section(args, headline_only=False)
    _emit(lines)
    return 0


# --------------------------------------------------------------------------
# entry point
# --------------------------------------------------------------------------

def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        prog="entire lens",
        description=(
            "Checkpoint Lens - turn real Entire Checkpoint context into "
            "handoff, drift and change-assessment workflows."
        ),
    )
    p.add_argument("--version", action="version", version="checkpoint-lens " + __version__)
    sub = p.add_subparsers(dest="command", required=True)

    def common(sp: argparse.ArgumentParser) -> None:
        sp.add_argument(
            "--html", metavar="PATH",
            help="also write a self-contained HTML report to PATH",
        )
        sp.add_argument("--repo", default=".", help="repository to read (default: .)")
        sp.add_argument("--json", action="store_true", help="emit structured JSON")
        sp.add_argument(
            "--no-graph", action="store_true", help="skip entire graph calls"
        )

    h = sub.add_parser("handoff", help="reconstruct a session for handoff or resume")
    common(h)
    h.add_argument("--checkpoint", help="checkpoint id (default: most recent)")
    h.add_argument("--limit", type=int, default=12, help="max decisions to show")
    h.add_argument("--no-databricks", action="store_true", help="skip Databricks")
    h.set_defaults(func=cmd_handoff)

    d = sub.add_parser("drift", help="review implementation against the original plan")
    common(d)
    d.add_argument("--limit", type=int, default=18, help="max requirements to check")
    d.add_argument(
        "--profile", default="fast", choices=["syntax-only", "fast", "full"],
        help="graph parsing depth; 'full' is slower but resolves call graphs",
    )
    d.add_argument("--jobs", type=int, default=6, help="concurrent graph searches")
    d.add_argument(
        "--fail-on-open", action="store_true",
        help="exit non-zero if any stated requirement is unimplemented (CI gate)",
    )
    d.add_argument("--no-databricks", action="store_true", help="skip Databricks")
    d.set_defaults(func=cmd_drift)

    a = sub.add_parser("assess", help="assess one change against its checkpoint's intent")
    common(a)
    a.add_argument("commitish", help="commit to assess (sha, tag, HEAD~1, ...)")
    a.add_argument("--no-databricks", action="store_true", help="skip Databricks")
    a.add_argument(
        "--verify", metavar="TEST_CMD",
        help="run this test command and report an adjudicated pass/fail verdict",
    )
    a.add_argument(
        "--verify-baseline", metavar="PATH", default=".entire/lens-verify-baseline.json",
        help="baseline file for adjudicated verification (recorded if absent)",
    )
    a.set_defaults(func=cmd_assess)

    y = sub.add_parser("sync", help="push checkpoint records to Databricks and read aggregates back")
    common(y)
    y.add_argument("--query-only", action="store_true", help="do not write; only read aggregates")
    y.add_argument("--no-databricks", action="store_true", help=argparse.SUPPRESS)
    y.set_defaults(func=cmd_sync)

    return p


def main(argv: list[str] | None = None) -> int:
    _configure_stdio()
    parser = build_parser()
    args = parser.parse_args(argv)
    return int(args.func(args))


if __name__ == "__main__":
    raise SystemExit(main())
