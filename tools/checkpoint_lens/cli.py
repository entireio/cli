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
from dataclasses import asdict
from typing import Any

from . import __version__
from .graph import GraphClient
from .entities import parse_entity_changes, risky, touched_paths
from .models import CheckpointRecord
from .reader import CheckpointReader
from . import report


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
    lines.append("  Recovered verbatim from the session transcript.")
    lines.append("")
    lines.extend(report.render_decisions(rec.all_decisions(), limit=args.limit))

    lines.extend(report.render_files(rec))

    # Graph blast radius over the files this session actually touched.
    if not args.no_graph:
        lines.extend(_graph_blast_radius(args.repo, rec))

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
    return 0


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
        sp.add_argument("--repo", default=".", help="repository to read (default: .)")
        sp.add_argument("--json", action="store_true", help="emit structured JSON")
        sp.add_argument(
            "--no-graph", action="store_true", help="skip entire graph calls"
        )

    h = sub.add_parser("handoff", help="reconstruct a session for handoff or resume")
    common(h)
    h.add_argument("--checkpoint", help="checkpoint id (default: most recent)")
    h.add_argument("--limit", type=int, default=12, help="max decisions to show")
    h.set_defaults(func=cmd_handoff)

    return p


def main(argv: list[str] | None = None) -> int:
    _configure_stdio()
    parser = build_parser()
    args = parser.parse_args(argv)
    return int(args.func(args))


if __name__ == "__main__":
    raise SystemExit(main())
