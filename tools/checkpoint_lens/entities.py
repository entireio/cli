"""Entity-level change parsing for ``entire graph commit`` / ``graph diff``.

The graph returns changes grouped by file::

    {"base": ..., "head": ...,
     "files": [{"path": ..., "status": "A", "language": "Python",
                "changes": [{"type": "added", "kind": "function",
                             "name": "cmd_handoff", "dependents_count": 0}]}]}

Working at this level is the whole point of using the graph instead of a text
diff: a function that merely moved reports no entity change, so it does not
show up as drift.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any


@dataclass
class EntityChange:
    path: str
    name: str
    kind: str
    change_type: str
    language: str = ""
    dependents_count: int = 0
    start_line: int = 0

    @property
    def label(self) -> str:
        return "%s %s %s" % (self.change_type, self.kind, self.name)

    @property
    def is_risky(self) -> bool:
        """A signature change with dependents is the shape that breaks callers
        silently, so it is called out separately from an ordinary edit."""
        return self.dependents_count > 0 and self.change_type in {
            "signature_changed",
            "removed",
            "renamed",
        }


def parse_entity_changes(data: Any) -> list[EntityChange]:
    """Flatten a graph commit/diff payload into entity changes.

    Returns an empty list for any unrecognised shape rather than guessing - an
    empty result is rendered by callers as "graph returned no entries", never
    as "nothing changed".
    """
    if not isinstance(data, dict):
        return []
    files = data.get("files")
    if not isinstance(files, list):
        return []

    out: list[EntityChange] = []
    for f in files:
        if not isinstance(f, dict):
            continue
        path = str(f.get("path", ""))
        language = str(f.get("language", "") or "")
        changes = f.get("changes")
        if not isinstance(changes, list):
            continue
        for c in changes:
            if not isinstance(c, dict):
                continue
            out.append(
                EntityChange(
                    path=path,
                    name=str(c.get("name", "") or ""),
                    kind=str(c.get("kind", "") or ""),
                    change_type=str(c.get("type", "") or ""),
                    language=language,
                    dependents_count=int(c.get("dependents_count", 0) or 0),
                    start_line=int(c.get("after_start_line", 0) or 0),
                )
            )
    return out


def touched_paths(changes: list[EntityChange]) -> list[str]:
    seen: list[str] = []
    for c in changes:
        if c.path and c.path not in seen:
            seen.append(c.path)
    return seen


def risky(changes: list[EntityChange]) -> list[EntityChange]:
    return sorted(
        [c for c in changes if c.is_risky],
        key=lambda c: c.dependents_count,
        reverse=True,
    )
