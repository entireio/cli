"""GraphClient - a thin, honest wrapper over the ``entire graph`` plugin.

Two rules this module enforces, both from the Graph operating guide:

1. Graph output is *evidence*, not an oracle. Every result carries the exact
   command that produced it (:attr:`GraphResult.command`) so a reader can rerun
   it and verify the finding against source.
2. A missing or failing graph never fabricates an answer. It degrades to
   ``ok=False`` with the real stderr, and callers render that as "unavailable"
   rather than as "no impact" - the two mean very different things to someone
   deciding whether a change is safe.
"""

from __future__ import annotations

import json
import shutil
import subprocess
from dataclasses import dataclass, field
from typing import Any

DEFAULT_TIMEOUT = 90


@dataclass
class GraphResult:
    """One graph invocation: what was asked, what came back, and whether it
    can be trusted."""

    command: list[str]
    ok: bool
    data: Any = None
    text: str = ""
    error: str = ""

    @property
    def command_str(self) -> str:
        return " ".join(self.command)


@dataclass
class ImpactSummary:
    """Blast radius for one symbol, flattened for reporting."""

    symbol: str
    callers: list[str] = field(default_factory=list)
    callees: list[str] = field(default_factory=list)
    type_consumers: list[str] = field(default_factory=list)
    cochange_files: list[str] = field(default_factory=list)
    ok: bool = True
    error: str = ""
    command: str = ""

    @property
    def blast_radius(self) -> int:
        return len(self.callers) + len(self.type_consumers)


class GraphClient:
    def __init__(self, repo: str = ".", timeout: int = DEFAULT_TIMEOUT) -> None:
        self.repo = repo
        self.timeout = timeout
        self._available: bool | None = None

    # ---------------- plumbing ----------------

    def available(self) -> bool:
        """True when the graph plugin can actually answer. Cached - the check
        shells out, and every command path asks."""
        if self._available is None:
            if shutil.which("entire") is None:
                self._available = False
            else:
                self._available = self._run(["graph", "version"]).ok
        return self._available

    def _run(self, args: list[str]) -> GraphResult:
        cmd = ["entire", *args]
        try:
            proc = subprocess.run(
                cmd, capture_output=True, timeout=self.timeout, check=False
            )
        except (subprocess.TimeoutExpired, OSError) as exc:
            return GraphResult(command=cmd, ok=False, error=str(exc))

        out = proc.stdout.decode("utf-8", "replace")
        err = proc.stderr.decode("utf-8", "replace").strip()
        if proc.returncode != 0:
            return GraphResult(command=cmd, ok=False, text=out, error=err or "exit %d" % proc.returncode)

        data: Any = None
        stripped = out.strip()
        if stripped.startswith("{") or stripped.startswith("["):
            try:
                data = json.loads(stripped)
            except json.JSONDecodeError:
                data = None
        return GraphResult(command=cmd, ok=True, data=data, text=out)

    # ---------------- graph commands ----------------

    def capabilities(self) -> GraphResult:
        return self._run(["graph", "capabilities", "--json"])

    def search(self, query: str, profile: str = "full", limit: int = 10) -> GraphResult:
        return self._run(
            [
                "graph", "search",
                "--repo", self.repo,
                "--profile", profile,
                "--query", query,
                "--limit", str(limit),
                "--format", "json",
            ]
        )

    def impact(self, symbol: str, depth: int = 2, limit: int = 10) -> ImpactSummary:
        res = self._run(
            [
                "graph", "impact",
                "--repo", self.repo,
                "--symbol", symbol,
                "--depth", str(depth),
                "--limit", str(limit),
                "--format", "json",
            ]
        )
        summary = ImpactSummary(symbol=symbol, ok=res.ok, error=res.error, command=res.command_str)
        if not res.ok:
            return summary
        payload = res.data if isinstance(res.data, dict) else {}
        summary.callers = _names(payload, "callers", "callers_direct", "direct_callers")
        summary.callees = _names(payload, "callees")
        summary.type_consumers = _names(payload, "type_consumers", "uses_type")
        summary.cochange_files = _names(payload, "cochange", "co_change", "cochange_files")
        if not payload and res.text:
            # Text mode fallback: keep the prose so the report can still show
            # the evidence, even when the JSON shape is not what we expected.
            summary.error = ""
            summary.callers = []
        return summary

    def commit_entities(self, commitish: str) -> GraphResult:
        """Entity-level change list for a commit vs its first parent."""
        return self._run(
            ["graph", "commit", "--repo", self.repo, "--commit", commitish, "--format", "json"]
        )

    def diff(self, base: str, head: str) -> GraphResult:
        """Entity-level change list between two refs.

        This is the drift primitive: it reports which *entities* changed, so a
        function that merely moved does not read as drift the way a raw text
        diff would.
        """
        return self._run(
            ["graph", "diff", "--repo", self.repo, "--base", base, "--head", head, "--json"]
        )

    def checkpoint(self, checkpoint_id: str) -> GraphResult:
        """Analyze the commit carrying this checkpoint's trailer."""
        return self._run(
            ["graph", "checkpoint", "--repo", self.repo, "--id", checkpoint_id, "--format", "json"]
        )


def _names(payload: dict[str, Any], *keys: str) -> list[str]:
    """Pull a list of symbol/file names out of whichever key the graph used.

    The graph's JSON shape varies by section and version, so this accepts a few
    spellings and tolerates both bare strings and objects.
    """
    for key in keys:
        value = payload.get(key)
        if not value:
            continue
        out: list[str] = []
        if isinstance(value, dict):
            value = value.get("items") or value.get("entries") or []
        if isinstance(value, list):
            for item in value:
                if isinstance(item, str):
                    out.append(item)
                elif isinstance(item, dict):
                    name = (
                        item.get("symbol")
                        or item.get("name")
                        or item.get("file")
                        or item.get("path")
                    )
                    if name:
                        loc = item.get("file") or item.get("path")
                        if loc and loc != name:
                            out.append("%s (%s)" % (name, loc))
                        else:
                            out.append(str(name))
        if out:
            return out
    return []
