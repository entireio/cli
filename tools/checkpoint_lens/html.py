"""Self-contained HTML reports.

One file, no server, no CDN, no external fonts - everything inline. That is a
deliberate constraint: the report has to open from a USB stick or an email
attachment on a judge's or a colleague's machine, and it has to keep working
when the network, the Databricks warehouse, or the graph plugin does not.

Anything that could not be computed is rendered as an explicit "unavailable"
panel rather than being omitted, because a silently missing section reads as a
clean bill of health.

Above all of that sits ONE completeness banner, the same verdict the terminal
prints, placed before the stat grid. The per-panel notes say which piece is
missing; the banner is the single thing a reader cannot miss, and it is what
stops a partial reconstruction from being read as a full one.
"""

from __future__ import annotations

import html as _html
from typing import Any

from .models import CheckpointRecord
from .report import KIND_LABEL, SOURCE_LABEL, intent_provenance

CSS = """
:root{
  --bg:#f7f7f5; --panel:#ffffff; --ink:#16161a; --muted:#5c5f66;
  --line:#e3e3df; --accent:#2f5fd0; --shadow:0 1px 2px rgba(0,0,0,.05);
  --blocker:#b3261e; --risk:#a8560c; --open:#8a6d00; --rejected:#6b4ea8;
  --decision:#1d6f42; --rationale:#3a6b8a; --assumption:#5c5f66;
}
@media (prefers-color-scheme:dark){
  :root{
    --bg:#141416; --panel:#1c1c1f; --ink:#ececef; --muted:#a2a5ad;
    --line:#2c2c31; --accent:#8fb0ff; --shadow:none;
    --blocker:#ff8a80; --risk:#ffb870; --open:#ffd95e; --rejected:#c3a6ff;
    --decision:#7ee0a5; --rationale:#8fc7e8; --assumption:#a2a5ad;
  }
}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--ink);
  font:15px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif}
.wrap{max-width:960px;margin:0 auto;padding:32px 20px 64px}
header{border-bottom:2px solid var(--ink);padding-bottom:16px;margin-bottom:28px}
h1{font-size:24px;margin:0 0 6px;letter-spacing:-.01em}
.sub{color:var(--muted);font-size:14px}
h2{font-size:13px;text-transform:uppercase;letter-spacing:.09em;
  color:var(--muted);margin:32px 0 12px;font-weight:600}
.panel{background:var(--panel);border:1px solid var(--line);border-radius:8px;
  padding:16px 18px;box-shadow:var(--shadow)}
.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:12px}
.stat{background:var(--panel);border:1px solid var(--line);border-radius:8px;padding:12px 14px}
.stat .n{font-size:22px;font-weight:650;letter-spacing:-.02em}
.stat .l{font-size:12px;color:var(--muted);margin-top:2px}
.prov{font-size:12px;color:var(--muted);font-style:italic;margin-top:6px}
.item{border-left:3px solid var(--line);padding:8px 0 8px 14px;margin:12px 0}
.item .tag{display:inline-block;font-size:11px;font-weight:700;letter-spacing:.06em;
  padding:1px 7px;border-radius:3px;border:1px solid currentColor;margin-bottom:6px}
.item .src{font-size:11.5px;color:var(--muted);margin-top:5px}
.k-blocker{border-left-color:var(--blocker)} .k-blocker .tag{color:var(--blocker)}
.k-risk{border-left-color:var(--risk)} .k-risk .tag{color:var(--risk)}
.k-open_question{border-left-color:var(--open)} .k-open_question .tag{color:var(--open)}
.k-rejected{border-left-color:var(--rejected)} .k-rejected .tag{color:var(--rejected)}
.k-decision{border-left-color:var(--decision)} .k-decision .tag{color:var(--decision)}
.k-rationale{border-left-color:var(--rationale)} .k-rationale .tag{color:var(--rationale)}
.k-assumption{border-left-color:var(--assumption)} .k-assumption .tag{color:var(--assumption)}
code,pre{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12.5px}
pre{background:var(--panel);border:1px solid var(--line);border-radius:6px;
  padding:10px 12px;overflow-x:auto;margin:8px 0}
ul.files{list-style:none;padding:0;margin:0;columns:2;column-gap:24px}
ul.files li{font-family:ui-monospace,monospace;font-size:12.5px;padding:2px 0;
  break-inside:avoid;color:var(--muted)}
table{border-collapse:collapse;width:100%;font-size:13px}
th,td{text-align:left;padding:7px 10px;border-bottom:1px solid var(--line)}
th{font-size:11px;text-transform:uppercase;letter-spacing:.06em;color:var(--muted)}
td.num{text-align:right;font-variant-numeric:tabular-nums}
.verdict{font-weight:650;font-size:12px}
.v-IMPLEMENTED{color:var(--decision)} .v-PARTIAL{color:var(--risk)}
.v-MISSING{color:var(--blocker)} .v-UNVERIFIED{color:var(--muted)}
.warn{border-left:3px solid var(--risk);background:var(--panel);padding:12px 14px;
  border-radius:0 6px 6px 0;margin:10px 0;font-size:13.5px}
.na{color:var(--muted);font-style:italic}
.banner{border:2px solid var(--decision);border-radius:8px;padding:14px 16px;
  margin:0 0 24px;background:var(--panel)}
.banner.partial{border-color:var(--risk)}
.banner .verdict-line{font-size:16px;font-weight:700;letter-spacing:-.01em;margin-bottom:4px}
.banner.partial .verdict-line{color:var(--risk)}
.banner .lede{font-size:13px;color:var(--muted);margin-bottom:10px}
.banner ul{list-style:none;padding:0;margin:0}
.banner li{font-size:13px;padding:3px 0;display:flex;gap:8px;align-items:baseline}
.banner .st{flex:0 0 92px;font-size:10.5px;font-weight:700;letter-spacing:.06em;
  text-transform:uppercase;font-family:ui-monospace,monospace}
.banner .st-available{color:var(--decision)}
.banner .st-missing{color:var(--blocker)}
.banner .st-redacted{color:var(--risk)}
.banner .why{color:var(--muted)}
footer{margin-top:44px;padding-top:16px;border-top:1px solid var(--line);
  font-size:12px;color:var(--muted)}
.scroll{overflow-x:auto}
"""


def esc(text: Any) -> str:
    return _html.escape(str(text if text is not None else ""))


def _stat(n: Any, label: str) -> str:
    return '<div class="stat"><div class="n">%s</div><div class="l">%s</div></div>' % (
        esc(n),
        esc(label),
    )


def _decisions_block(decisions: list[Any], limit: int = 40) -> str:
    if not decisions:
        return '<div class="panel na">No decision context recovered from this checkpoint.</div>'
    order = ["blocker", "open_question", "risk", "rejected", "decision", "assumption", "rationale"]
    by_kind: dict[str, list[Any]] = {}
    for d in decisions:
        by_kind.setdefault(d.kind, []).append(d)
    out: list[str] = []
    shown = 0
    for kind in order:
        for d in by_kind.get(kind, []):
            if shown >= limit:
                break
            out.append(
                '<div class="item k-%s"><span class="tag">%s</span>'
                '<div>%s</div><div class="src">source: %s &middot; confidence: %s</div></div>'
                % (
                    esc(kind),
                    esc(KIND_LABEL.get(kind, kind.upper())),
                    esc(d.text),
                    esc(SOURCE_LABEL.get(d.source, d.source)),
                    esc(d.confidence),
                )
            )
            shown += 1
    if shown < len(decisions):
        out.append('<p class="na">%d more in the JSON output.</p>' % (len(decisions) - shown))
    return "\n".join(out)


def _trend_svg(rows: list[dict[str, Any]]) -> str:
    """A bar chart of unresolved items per checkpoint, drawn as inline SVG.

    Hand-rolled rather than pulled from a charting CDN: the file has to render
    with no network. Two checkpoints or fewer is not a trend, and the caller is
    told so rather than being shown a line through two points.
    """
    if not rows:
        return '<div class="panel na">No trend data.</div>'
    vals = [int(r.get("unresolved_total") or 0) for r in rows]
    peak = max(vals) or 1
    w, h, pad, gap = 900, 170, 28, 10
    n = len(vals)
    bar = max(14, (w - 2 * pad - gap * (n - 1)) // n)
    bars: list[str] = []
    for i, v in enumerate(vals):
        bh = int((h - 2 * pad) * (v / peak))
        x = pad + i * (bar + gap)
        y = h - pad - bh
        label = esc(str(rows[i].get("checkpoint_id", ""))[:6])
        bars.append(
            '<rect x="%d" y="%d" width="%d" height="%d" rx="3" fill="currentColor" opacity="%.2f"/>'
            '<text x="%d" y="%d" font-size="11" text-anchor="middle" fill="currentColor" opacity=".95">%d</text>'
            '<text x="%d" y="%d" font-size="9" text-anchor="middle" fill="currentColor" opacity=".5">%s</text>'
            % (
                x, y, bar, max(bh, 2), 0.35 + 0.65 * (v / peak),
                x + bar // 2, max(y - 5, 12), v,
                x + bar // 2, h - pad + 14, label,
            )
        )
    return (
        '<div class="panel scroll" style="color:var(--accent)">'
        '<svg viewBox="0 0 %d %d" width="100%%" height="%d" role="img" '
        'aria-label="Unresolved items per checkpoint">%s</svg></div>'
        % (w, h, h, "".join(bars))
    )


def _completeness_banner(comp: Any) -> str:
    """The same single verdict the terminal prints, first in the document.

    It sits above the stat grid on purpose: those big numbers are the most
    confident-looking thing on the page, and a reader must know what they are
    computed from before reading them.
    """
    if comp is None:
        return ""
    cls = "banner" if comp.is_complete else "banner partial"
    parts = ['<div class="%s">' % cls]
    if comp.is_complete:
        parts.append(
            '<div class="verdict-line">Context: COMPLETE</div>'
            '<div class="lede">All %d inputs behind this report were readable.</div>'
            % len(comp.inputs)
        )
    else:
        parts.append(
            '<div class="verdict-line">Context: PARTIAL</div>'
            '<div class="lede">%d of %d inputs are missing or redacted. Everything '
            "below is reconstructed from the rest, and the absence of a finding "
            "is not evidence that there is nothing to find.</div>"
            % (len(comp.degraded), len(comp.inputs))
        )
    parts.append("<ul>")
    for i in comp.inputs:
        label = {"available": "ok", "missing": "unavailable", "redacted": "redacted"}.get(
            i.status, i.status
        )
        why = (" &mdash; " + esc(i.detail)) if i.detail else ""
        parts.append(
            '<li><span class="st st-%s">%s</span><span>%s<span class="why">%s</span></span></li>'
            % (esc(i.status), esc(label), esc(i.name), why)
        )
    parts.append("</ul></div>")
    return "".join(parts)


def render_handoff(
    rec: CheckpointRecord,
    history: list[CheckpointRecord],
    warnings: list[str],
    graph_lines: list[str] | None = None,
    aggregates: dict[str, Any] | None = None,
    comp: Any = None,
) -> str:
    agg = aggregates or {}
    sess = rec.sessions[0] if rec.sessions else None

    parts: list[str] = []
    parts.append("<header><h1>Checkpoint Lens &mdash; Handoff Brief</h1>")
    parts.append(
        '<div class="sub">%s &middot; %s &middot; branch <code>%s</code></div></header>'
        % (esc(rec.checkpoint_id), esc(rec.created_at or "unknown time"), esc(rec.branch or "?"))
    )

    parts.append(_completeness_banner(comp))

    parts.append('<div class="grid">')
    parts.append(_stat(len(rec.files_touched), "files touched"))
    parts.append(_stat(len(rec.all_decisions()), "decisions recovered"))
    if sess:
        parts.append(_stat("%.0f%%" % sess.attribution.agent_percentage, "agent-written"))
        parts.append(_stat(format(sess.attribution.total_lines_changed, ","), "lines changed"))
    parts.append(_stat(format(rec.token_usage.total, ","), "tokens"))
    parts.append("</div>")

    parts.append("<h2>Stated intent</h2><div class='panel'>")
    parts.append("<div>%s</div>" % esc(rec.intent[:2000] or "No intent recoverable."))
    parts.append('<div class="prov">Provenance: %s</div></div>' % esc(intent_provenance(rec.intent_source)))

    if rec.linked_commit:
        parts.append("<h2>Linked commit</h2><div class='panel'><code>%s</code> &mdash; %s</div>"
                     % (esc(rec.linked_commit[:12]), esc(rec.linked_subject)))

    parts.append("<h2>Decisions, risks and open questions</h2>")
    parts.append('<p class="na">Recovered verbatim from checkpoint context &mdash; never paraphrased or generated.</p>')
    parts.append(_decisions_block(rec.all_decisions()))

    if graph_lines:
        parts.append("<h2>Graph impact (blast radius)</h2>")
        parts.append("<pre>%s</pre>" % esc("\n".join(graph_lines)))

    trend = agg.get("trend")
    if trend is not None:
        parts.append("<h2>Unresolved context over time (Databricks)</h2>")
        if trend:
            parts.append(_trend_svg(trend))
            if len(trend) < 3:
                parts.append('<p class="na">Only %d checkpoints &mdash; too few to read as a trend.</p>' % len(trend))
        else:
            parts.append('<div class="panel na">Databricks returned no rows.</div>')
    elif agg.get("unavailable"):
        parts.append("<h2>Unresolved context over time (Databricks)</h2>")
        parts.append('<div class="warn">Cross-session analytics unavailable: %s<br>'
                     "Single-checkpoint sections above are unaffected.</div>" % esc(agg["unavailable"]))

    parts.append("<h2>Files touched</h2><div class='panel'><ul class='files'>")
    parts.extend("<li>%s</li>" % esc(f) for f in rec.files_touched)
    parts.append("</ul></div>")

    if len(history) > 1:
        parts.append("<h2>Checkpoint history</h2><div class='panel scroll'><table>")
        parts.append("<tr><th>Checkpoint</th><th>When</th><th>Commit</th></tr>")
        for r in history:
            parts.append(
                "<tr><td><code>%s</code></td><td>%s</td><td>%s</td></tr>"
                % (esc(r.checkpoint_id), esc((r.created_at or "")[:19]), esc(r.linked_subject or "(unlinked)"))
            )
        parts.append("</table></div>")

    if warnings:
        parts.append("<h2>Warnings &mdash; this view may be incomplete</h2>")
        parts.extend('<div class="warn">%s</div>' % esc(w) for w in warnings)

    parts.append(
        "<footer>Generated by <code>entire lens</code> from real Entire Checkpoints. "
        "Every figure is derived from committed checkpoint data; nothing is synthesised. "
        "Graph findings are evidence, not proof &mdash; rerun the printed commands to verify."
        "</footer>"
    )
    return _document("Checkpoint Lens - Handoff", "".join(parts))


def render_drift(
    baseline: CheckpointRecord,
    head: CheckpointRecord,
    findings: list[Any],
    entity_changes: list[Any],
    diff_command: str,
    warnings: list[str],
    comp: Any = None,
) -> str:
    from .requirements import IMPLEMENTED, MISSING, PARTIAL, UNVERIFIED, VERDICT_NOTE

    counts: dict[str, int] = {}
    for f in findings:
        counts[f.verdict] = counts.get(f.verdict, 0) + 1
    total = len(findings) or 1
    done = counts.get(IMPLEMENTED, 0)

    parts: list[str] = []
    parts.append("<header><h1>Checkpoint Lens &mdash; Drift Report</h1>")
    parts.append('<div class="sub">Plan <code>%s</code> &rarr; current <code>%s</code></div></header>'
                 % (esc(baseline.checkpoint_id), esc(head.checkpoint_id)))

    parts.append(_completeness_banner(comp))

    parts.append('<div class="grid">')
    parts.append(_stat("%.0f%%" % (100.0 * done / total), "requirement coverage"))
    parts.append(_stat(len(findings), "requirements extracted"))
    parts.append(_stat(counts.get(MISSING, 0) + counts.get(PARTIAL, 0), "still open"))
    parts.append(_stat(len(entity_changes), "entity changes"))
    parts.append("</div>")

    parts.append("<h2>Plan vs implementation</h2>")
    parts.append('<div class="panel scroll"><table>')
    parts.append("<tr><th>Requirement</th><th>Verdict</th><th>Evidence</th></tr>")
    for f in findings:
        parts.append(
            '<tr><td>%s</td><td class="verdict v-%s">%s</td><td><code>%s</code></td></tr>'
            % (
                esc(f.requirement.text),
                esc(f.verdict),
                esc(f.verdict),
                esc((f.evidence[0] if f.evidence else "")[:90]),
            )
        )
    parts.append("</table></div>")
    parts.append('<p class="na">%s</p>' % esc(VERDICT_NOTE[MISSING]))

    parts.append("<h2>Entity-level change since the plan</h2>")
    if diff_command:
        parts.append("<pre>$ %s</pre>" % esc(diff_command))
    if entity_changes:
        parts.append(
            '<p class="na">Entity-level, so a function that merely moved is not counted as drift.</p>'
        )
        parts.append('<div class="panel scroll"><table>')
        parts.append("<tr><th>Change</th><th>File</th><th class='num'>Dependents</th></tr>")
        for c in entity_changes[:60]:
            parts.append(
                "<tr><td>%s</td><td><code>%s</code></td><td class='num'>%s</td></tr>"
                % (esc(c.label), esc(c.path), esc(c.dependents_count or ""))
            )
        parts.append("</table></div>")
    else:
        parts.append('<div class="panel na">No entity changes reported.</div>')

    if warnings:
        parts.append("<h2>Warnings</h2>")
        parts.extend('<div class="warn">%s</div>' % esc(w) for w in warnings)

    parts.append(
        "<footer>MISSING means no evidence was found, not that the work is absent. "
        "Verify each open item against source before acting.</footer>"
    )
    return _document("Checkpoint Lens - Drift", "".join(parts))


def _document(title: str, body: str) -> str:
    return (
        "<!doctype html><html lang='en'><head><meta charset='utf-8'>"
        "<meta name='viewport' content='width=device-width,initial-scale=1'>"
        "<title>%s</title><style>%s</style></head><body><div class='wrap'>%s</div></body></html>"
        % (esc(title), CSS, body)
    )
