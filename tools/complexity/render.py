#!/usr/bin/env python3
"""Render the complexity baseline page from cxtool + reach + dupl outputs.

usage: render.py <out_dir_all> <reach_dir> <dupl_by_feature.json> <repo_root> <html_out>

Reads report.json / reach.json / dupl-by-feature.json only; the CSVs are for
humans and CI diffs. Rank policy (which features count in rankings) comes from
the data, which carries it from features.json — nothing here re-decides it.
The "Reading the numbers" section is interpretation written against one
snapshot; its inline figures are literals on purpose and go stale with the
tree, which the section says itself.
"""
import json, math, subprocess, sys, html, datetime, statistics, collections

OUT, REACH, DUPL, REPO, HTML_OUT = sys.argv[1:6]

def esc(s): return html.escape(str(s), quote=True)
def num(n): return f"{int(n):,}"

# ---------- load ----------
report = json.load(open(f"{OUT}/report.json"))
reach = json.load(open(f"{REACH}/reach.json"))
dupl = json.load(open(DUPL))
meta = report["meta"]
features, areas, files, funcs = report["features"], report["areas"], report["files"], report["funcs"]
COGNIT_WARN, COV_WARN = meta["cognit_warn"], meta["cov_warn"]
MAXFAN = reach["meta"]["maxfanout"]
_rev = subprocess.run(["git", "-C", REPO, "rev-parse", "--short", "HEAD"], capture_output=True, text=True)
if _rev.returncode != 0:
    # The commit is how a rendered page is tied back to a tree, so losing it
    # silently would make the report undiffable while still looking finished.
    sys.exit(f"git rev-parse in {REPO} failed: {_rev.stderr.strip() or _rev.returncode}")
commit = _rev.stdout.strip()
today = datetime.date.today().isoformat()

def cov(row):  # features, areas, packages, files and funcs all carry stmts/covered
    # -1 = no statements known
    return -1 if row["stmts"] == 0 else 100 * row["covered"] / row["stmts"]

def ratio(row):
    return 0 if row["prod_loc"] == 0 else row["test_loc"] / row["prod_loc"]

def density(row):
    return 0 if row["prod_loc"] == 0 else 100 * row["sum_cognit"] / row["prod_loc"]

for f in features:
    f["cov"], f["ratio"], f["density"] = cov(f), ratio(f), density(f)
    f["commits_90d"] = f["churn"].get("90d", {}).get("commits", 0)
    f["dupl"] = dupl["prod_by_feature"].get(f["name"], 0)

ranked_feature = {f["name"]: f["ranked"] for f in features}
core = [f for f in features if f["ranked"]]
tot_prod = sum(f["prod_loc"] for f in core); tot_test = sum(f["test_loc"] for f in core)
tot_stmts = sum(f["stmts"] for f in core); tot_cov = sum(f["covered"] for f in core)
tot_over = sum(f["funcs_over_cognit"] for f in core); tot_funcs = sum(f["funcs"] for f in core)
gen_loc = sum(a["gen_loc"] for a in areas)
median_cov = statistics.median(f["cov"] for f in core if f["cov"] >= 0)

hist = {int(k): v for k, v in reach["owner_histloc"].items()}
excl_loc = hist.get(1, 0)
CORE_ROOTS = 20  # "reached by ≥ this many roots" = the shared substrate
core_loc = sum(v for k, v in hist.items() if k >= CORE_ROOTS)
bucket_vals = [
    ("no root", hist.get(0, 0)),
    ("1 root", excl_loc),
    ("2–5", sum(v for k, v in hist.items() if 2 <= k <= 5)),
    ("6–19", sum(v for k, v in hist.items() if 6 <= k < CORE_ROOTS)),
    (f"{CORE_ROOTS}+", core_loc),
]

cobra = [c for c in reach["commands"] if c["kind"] == "cobra"]
pseudo = [c for c in reach["commands"] if c["kind"] != "cobra"]
top_cmds = sorted(cobra, key=lambda c: -c["excl_loc"])[:26]

def command(suffix):
    """The interpretation section quotes these; a rename must fail loudly,
    never render as zero."""
    for c in cobra:
        if c["root"].endswith(suffix):
            return c
    raise KeyError(f"no cobra root matching {suffix!r} in reach.json")

REV, INV, PREPUSH, EXPERTS = command("review.NewCommand"), command("investigate.NewCommand"), command("newHooksGitPrePushCmd"), command("newExpertsCmd")
EXP_TOTAL = REV["excl_loc"] + INV["excl_loc"] + EXPERTS["excl_loc"]

unreached = [u for u in reach["unreached"] if u["ranked"]]
un_by_file = collections.defaultdict(lambda: {"n": 0, "loc": 0, "cog": 0, "names": []})
for u in unreached:
    b = un_by_file[u["file"]]
    b["n"] += 1; b["loc"] += u["loc"]; b["cog"] += u["cognit"]; b["names"].append(u["name"])
un_files = sorted(un_by_file.items(), key=lambda kv: -kv[1]["loc"])[:22]
un_total = sum(u["loc"] for u in unreached)

prod_funcs = [fn for fn in funcs if ranked_feature[fn["feature"]]]
top_funcs = sorted(prod_funcs, key=lambda x: -x["cognit"])[:15]
undercov_all = [fn for fn in prod_funcs if fn["cognit"] >= COGNIT_WARN and fn["stmts"] > 0 and cov(fn) < COV_WARN]
undercov = sorted(undercov_all, key=lambda x: -x["cognit"])[:22]
hot_files = sorted(
    ((fl["churn"].get("90d", {}).get("commits", 0), fl) for fl in files
     if not fl["is_test"] and not fl["generated"] and fl["ranked"] and fl["sum_cognit"] > 0),
    key=lambda t: -(t[0] * t[1]["sum_cognit"]))
hot_files = [(c, fl) for c, fl in hot_files if c > 0][:15]

# ---------- svg: scatter ----------
def scatter():
    W, H = 920, 470; L, R_, T, B = 64, 24, 20, 56
    pts = [f for f in core if f["cov"] >= 0 and f["sum_cognit"] > 0]
    xmin, xmax = 50, 4000
    def X(v): return L + (math.log10(max(v, xmin)) - math.log10(xmin)) / (math.log10(xmax) - math.log10(xmin)) * (W - L - R_)
    def Y(v): return T + (100 - v) / 100 * (H - T - B)
    def Rr(loc): return max(5, min(22, 3 + math.sqrt(loc) / 7))
    grp = lambda a: "command" if a == "command" else ("capture" if a in ("capture", "agents") else "infra")
    parts = [f'<svg class="chart" viewBox="0 0 {W} {H}" role="img" aria-labelledby="sc-title" style="width:100%;height:auto"><title id="sc-title">Cognitive complexity vs statement coverage, per feature</title>']
    for yv in (0, 25, 50, 75, 100):
        parts.append(f'<line x1="{L}" x2="{W-R_}" y1="{Y(yv):.1f}" y2="{Y(yv):.1f}" class="grid"/><text x="{L-8}" y="{Y(yv)+4:.1f}" class="tick" text-anchor="end">{yv}%</text>')
    for xv in (100, 300, 1000, 3000):
        parts.append(f'<line y1="{T}" y2="{H-B}" x1="{X(xv):.1f}" x2="{X(xv):.1f}" class="grid"/><text y="{H-B+18}" x="{X(xv):.1f}" class="tick" text-anchor="middle">{xv}</text>')
    parts.append(f'<text x="{(L+W-R_)/2:.0f}" y="{H-8}" class="axis" text-anchor="middle">Σ cognitive complexity (log scale)</text>')
    parts.append(f'<text transform="translate(14,{(T+H-B)/2:.0f}) rotate(-90)" class="axis" text-anchor="middle">statement coverage</text>')
    # One stable labelling rule: the 12 features with the most under-covered complexity.
    label = set(f["name"] for f in sorted(pts, key=lambda f: -f["uncovered_cognit"])[:12])
    for f in sorted(pts, key=lambda f: -f["prod_loc"]):
        x, y, r = X(f["sum_cognit"]), Y(f["cov"]), Rr(f["prod_loc"])
        tip = f'{f["name"]} — Σcognit {num(f["sum_cognit"])}, coverage {f["cov"]:.0f}%, {num(f["prod_loc"])} prod LOC, uncovered cognit {f["uncovered_cognit"]}'
        parts.append(f'<circle class="dot g-{grp(f["area"])}" cx="{x:.1f}" cy="{y:.1f}" r="{r:.1f}" tabindex="0" data-tip="{esc(tip)}"><title>{esc(tip)}</title></circle>')
    used = []
    for f in sorted((f for f in pts if f["name"] in label), key=lambda f: Y(f["cov"])):
        right = X(f["sum_cognit"]) < L + 0.70 * (W - L - R_)
        x = X(f["sum_cognit"]) + (Rr(f["prod_loc"]) + 5 if right else -(Rr(f["prod_loc"]) + 5))
        y = Y(f["cov"]) + 4
        anchor = "start" if right else "end"
        w = 7 * len(f["name"])
        x0, x1 = (x, x + w) if right else (x - w, x)
        while any(abs(y - uy) < 13 and x0 < ux1 and x1 > ux0 for ux0, ux1, uy in used): y += 13
        used.append((x0, x1, y))
        parts.append(f'<text x="{x:.1f}" y="{y:.1f}" class="lbl" text-anchor="{anchor}">{esc(f["name"])}</text>')
    parts.append("</svg>")
    return "".join(parts)

def bar(v, vmax, cls="bar"):
    w = 0 if vmax == 0 else max(1.5, 100 * v / vmax)
    return f'<span class="{cls}"><i style="width:{w:.1f}%"></i></span>'

# The chip bands derive from COV_WARN so a non-default -cov-warn cannot leave
# the colouring disagreeing with every other "under-covered" figure on the page.
COV_CRIT, COV_SER = COV_WARN, COV_WARN + 15

def cov_cell(c):
    if c < 0: return '<td class="n">n/a</td>'
    chip = ""
    if c < COV_CRIT: chip = ' <span class="chip crit">▲ low</span>'
    elif c < COV_SER: chip = ' <span class="chip ser">▲ low</span>'
    return f'<td class="n" data-v="{c:.1f}">{bar(c,100,"bar cov")}<span>{c:.0f}%</span>{chip}</td>'

# ---------- html ----------
# The area filter buttons are derived from the features actually listed: a
# hardcoded list goes stale when an area is added, and offers a button that
# filters to nothing when every feature in an area is norank.
_shown_areas = {f["area"] for f in features if f["ranked"]}
area_buttons = "".join(
    ['<button aria-pressed="true" data-f="all">all</button>']
    + [f'<button aria-pressed="false" data-f="{esc(a["name"])}">{esc(a["name"])}</button>'
       for a in sorted(areas, key=lambda a: -a["prod_loc"]) if a["name"] in _shown_areas])

def feature_rows():
    shown = [f for f in features if f["ranked"]]
    mx_loc = max(f["prod_loc"] for f in shown); mx_cog = max(f["sum_cognit"] for f in shown)
    rows = []
    for f in sorted(shown, key=lambda f: -f["sum_cognit"]):
        rows.append(
            f'<tr data-area="{esc(f["area"])}">'
            f'<td class="name"><span class="fname">{esc(f["name"])}</span></td>'
            f'<td><span class="area">{esc(f["area"])}</span></td>'
            f'<td class="n" data-v="{f["prod_loc"]}">{bar(f["prod_loc"],mx_loc)}<span>{num(f["prod_loc"])}</span></td>'
            f'<td class="n" data-v="{f["ratio"]}">{f["ratio"]:.2f}</td>'
            f'<td class="n" data-v="{f["sum_cognit"]}">{bar(f["sum_cognit"],mx_cog)}<span>{num(f["sum_cognit"])}</span></td>'
            f'<td class="n" data-v="{f["density"]}">{f["density"]:.1f}</td>'
            f'<td class="n" data-v="{f["funcs_over_cognit"]}">{f["funcs_over_cognit"]}</td>'
            f'{cov_cell(f["cov"])}'
            f'<td class="n" data-v="{f["uncovered_cognit"]}">{f["uncovered_cognit"]}</td>'
            f'<td class="n" data-v="{f["commits_90d"]}">{f["commits_90d"]}</td>'
            f'<td class="n" data-v="{f["dupl"]}">{f["dupl"] or "·"}</td>'
            "</tr>")
    return "\n".join(rows)

def cmd_rows():
    mx = max(c["excl_loc"] for c in top_cmds)
    out = []
    for c in top_cmds:
        top = sorted(c["excl_features_loc"].items(), key=lambda kv: -kv[1])
        main = top[0][0] if top else "—"
        out.append(f'<tr><td class="mono">{esc(c["root"])}</td><td><span class="fname">{esc(main)}</span></td>'
                   f'<td class="n">{bar(c["excl_loc"],mx)}<span>{num(c["excl_loc"])}</span></td><td class="n">{num(c["excl_cognit"])}</td><td class="n muted">{num(c["reach_loc"])}</td></tr>')
    return "\n".join(out)

def func_row(fn):
    return (f'<tr><td class="n">{fn["cognit"]}</td><td class="n">{fn["cyclo"]}</td><td class="n">{fn["loc"]}</td>'
            f'{cov_cell(cov(fn))}<td><span class="fname">{esc(fn["feature"])}</span></td>'
            f'<td class="mono small">{esc(fn["file"])}:{fn["start"]} <b>{esc(fn["name"])}</b></td></tr>')

def hot_rows():
    return "\n".join(
        f'<tr><td class="n">{c}</td><td class="n">{fl["sum_cognit"]}</td><td class="n">{num(fl["loc"])}</td>{cov_cell(cov(fl))}'
        f'<td><span class="fname">{esc(fl["feature"])}</span></td><td class="mono small">{esc(fl["rel"])}</td></tr>'
        for c, fl in hot_files)


def unreached_rows():
    out = []
    for f, b in un_files:
        names = ", ".join(b["names"][:4]) + ("…" if len(b["names"]) > 4 else "")
        out.append(f'<tr><td class="n">{b["loc"]}</td><td class="n">{b["n"]}</td><td class="n">{b["cog"]}</td><td class="mono small">{esc(f)}<br><span class="muted">{esc(names)}</span></td></tr>')
    return "\n".join(out)

def dupl_rows():
    by = dupl["prod_by_feature"]; lines = dupl["lines_by_feature"]; prod = {f["name"]: f["prod_loc"] for f in features}
    return "\n".join(
        f'<tr><td><span class="fname">{esc(name)}</span></td><td class="n">{n}</td><td class="n">~{lines.get(name,0)}</td><td class="n">{1000*lines.get(name,0)/max(1,prod.get(name,1)):.0f}</td></tr>'
        for name, n in sorted(by.items(), key=lambda kv: -kv[1])[:12])

def largest_dupl():
    seen = set(); out = []
    for d in dupl["largest"]:
        key = tuple(sorted([(d["file"], d["line"]), (d["other"], d["other_line"])]))
        if key in seen: continue
        seen.add(key)
        out.append(f'<li><b>{d["span"]} lines</b> <span class="mono small">{esc(d["file"])}:{d["line"]}</span> ≡ <span class="mono small">{esc(d["other"])}:{d["other_line"]}</span></li>')
        if len(out) >= 7: break
    return "\n".join(out)

def bucket_bars():
    mx = max(v for _, v in bucket_vals)
    return "\n".join(f'<tr><td>{esc(l)}</td><td class="n">{bar(v,mx)}<span>{num(v)}</span></td></tr>' for l, v in bucket_vals)

def covtxt(row):
    c = cov(row)
    return "n/a cov" if c < 0 else f"{c:.1f}% cov"

area_tiles = "".join(
    f'<div class="tile"><div class="tlabel">{esc(a["name"])}</div><div class="tval">{num(a["prod_loc"])}</div><div class="tsub">prod LOC · {covtxt(a)} · ratio {ratio(a):.2f}</div></div>'
    for a in sorted(areas, key=lambda a: -a["prod_loc"]) if a["ranked"])

# The dark palette is written once; the artifact host stamps data-theme on the
# root for an explicit choice, while the un-stamped default follows the OS via
# the media query — both scopes must carry the same tokens.
DARK_VARS = """color-scheme:dark;
 --bg:#0f1214;--surface:#171b20;--ink:#eef1f4;--ink2:#b3bcc7;--muted:#8792a0;--hair:#2a3139;--grid:#242b33;
 --accent:#3987e5;--accent-ink:#86b6ef;--bar:#1c3557;--barfill:#3987e5;
 --s1:#3987e5;--s2:#d95926;--s3:#199e70;--ring:#171b20;--chip-crit-bg:#3a1c1c;--chip-ser-bg:#3a2619;"""

page = f"""<title>Entire CLI Complexity Baseline</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=IBM+Plex+Sans:wght@400;500;600&family=IBM+Plex+Mono:wght@400;500&display=swap">
<style>
:root{{color-scheme:light;
 --bg:#f4f6f8;--surface:#ffffff;--ink:#15191e;--ink2:#4b5563;--muted:#7b8593;--hair:#dfe4ea;--grid:#e6eaef;
 --accent:#2a78d6;--accent-ink:#1c5cab;--bar:#9ec5f4;--barfill:#2a78d6;
 --s1:#2a78d6;--s2:#eb6834;--s3:#1baf7a;--crit:#d03b3b;--ser:#ec835a;--ring:#ffffff;
 --chip-crit-bg:#fbe4e4;--chip-ser-bg:#fdeee6;}}
@media (prefers-color-scheme:dark){{:root:not([data-theme="light"]){{{DARK_VARS}}}}}
:root[data-theme="dark"]{{{DARK_VARS}}}
*{{box-sizing:border-box}}
body{{margin:0;background:var(--bg);color:var(--ink);font:15px/1.55 "IBM Plex Sans",system-ui,-apple-system,"Segoe UI",sans-serif}}
.mono,.tick,.lbl{{font-family:"IBM Plex Mono",ui-monospace,SFMono-Regular,Menlo,monospace}}
.wrap{{max-width:1160px;margin:0 auto;padding:40px 28px 80px}}
header{{display:grid;grid-template-columns:1fr auto;gap:24px;align-items:end;margin-bottom:36px;border-bottom:1px solid var(--hair);padding-bottom:24px}}
.eyebrow{{font-size:12px;letter-spacing:.08em;text-transform:uppercase;color:var(--muted);font-weight:500}}
h1{{font-size:34px;line-height:1.1;margin:8px 0 10px;font-weight:600;letter-spacing:-.01em;text-wrap:balance}}
.lede{{max-width:68ch;color:var(--ink2);font-size:16px;margin:0}}
.meta{{text-align:right;color:var(--muted);font-size:13px;line-height:1.7}}
h2{{font-size:22px;font-weight:600;margin:56px 0 8px;letter-spacing:-.005em;text-wrap:balance}}
h3{{font-size:16px;font-weight:600;margin:28px 0 8px}}
p{{max-width:72ch}} p.note{{color:var(--ink2);font-size:14px}}
.tiles{{display:grid;grid-template-columns:repeat(auto-fit,minmax(170px,1fr));gap:12px;margin:20px 0}}
.tile{{background:var(--surface);border:1px solid var(--hair);border-radius:8px;padding:14px 16px}}
.tlabel{{font-size:12px;color:var(--muted);letter-spacing:.04em;text-transform:uppercase;font-weight:500}}
.tval{{font-size:30px;font-weight:600;letter-spacing:-.02em;margin:4px 0 2px}}
.tsub{{font-size:13px;color:var(--ink2)}}
.card{{background:var(--surface);border:1px solid var(--hair);border-radius:8px;padding:18px 20px;margin:16px 0;overflow-x:auto}}
table{{border-collapse:collapse;width:100%;font-size:13px}}
th{{text-align:left;font-weight:500;color:var(--muted);font-size:11px;letter-spacing:.03em;text-transform:uppercase;padding:8px 8px;border-bottom:1px solid var(--hair);white-space:nowrap;cursor:pointer;user-select:none}}
th.n,td.n{{text-align:right;font-variant-numeric:tabular-nums}}
th[data-sorted="desc"]::after{{content:" ▾"}} th[data-sorted="asc"]::after{{content:" ▴"}}
td{{padding:6px 8px;border-bottom:1px solid var(--grid);vertical-align:top}}
tr:last-child td{{border-bottom:0}}
td.n{{white-space:nowrap}}
.bar{{display:inline-block;width:52px;height:8px;background:var(--bar);border-radius:0 4px 4px 0;vertical-align:middle;margin-right:8px;overflow:hidden;position:relative}}
.bar i{{display:block;height:100%;background:var(--barfill);border-radius:0 4px 4px 0}}
.bar.cov{{width:44px}}
.fname{{font-family:"IBM Plex Mono",ui-monospace,monospace;font-size:12.5px}}
.small{{font-size:12.5px}} .muted{{color:var(--muted)}}
.area{{font-size:11px;letter-spacing:.04em;text-transform:uppercase;padding:2px 7px;border-radius:4px;border:1px solid var(--hair);color:var(--ink2);white-space:nowrap}}
.chip{{font-size:11px;padding:1px 6px;border-radius:4px;margin-left:6px;font-weight:500}}
.chip.crit{{background:var(--chip-crit-bg);color:var(--crit)}} .chip.ser{{background:var(--chip-ser-bg);color:var(--ser)}}
.filters{{display:flex;gap:8px;flex-wrap:wrap;margin:14px 0 6px}}
.filters button{{font:inherit;font-size:13px;padding:5px 12px;border-radius:999px;border:1px solid var(--hair);background:var(--surface);color:var(--ink2);cursor:pointer}}
.filters button[aria-pressed="true"]{{border-color:var(--accent);color:var(--accent-ink);background:color-mix(in srgb,var(--accent) 10%,var(--surface))}}
.filters button:focus-visible,th:focus-visible,.dot:focus-visible{{outline:2px solid var(--accent);outline-offset:2px}}
.chart .grid{{stroke:var(--grid);stroke-width:1}}
.chart .tick,.chart .axis{{fill:var(--muted);font-size:11px}} .chart .axis{{font-family:"IBM Plex Sans",system-ui,sans-serif;font-size:12px}}
.chart .lbl{{fill:var(--ink2);font-size:11px}}
.dot{{stroke:var(--ring);stroke-width:2;opacity:.9;cursor:pointer}}
.dot.g-command{{fill:var(--s1)}} .dot.g-capture{{fill:var(--s2)}} .dot.g-infra{{fill:var(--s3)}}
.dot:hover{{opacity:1;stroke:var(--ink)}}
.legend{{display:flex;gap:18px;font-size:13px;color:var(--ink2);margin:6px 0 0}} .legend i{{display:inline-block;width:10px;height:10px;border-radius:50%;margin-right:6px;vertical-align:-1px}}
#tip{{position:fixed;pointer-events:none;background:var(--ink);color:var(--bg);padding:6px 10px;border-radius:6px;font-size:12.5px;max-width:340px;display:none;z-index:9}}
.two{{display:grid;grid-template-columns:1fr 1fr;gap:16px}} @media(max-width:860px){{.two{{grid-template-columns:1fr}}}}
.reading li{{margin:10px 0;max-width:80ch}} .reading b{{font-weight:600}}
pre{{background:var(--surface);border:1px solid var(--hair);border-radius:8px;padding:14px 16px;overflow-x:auto;font-size:12.5px;line-height:1.5;font-family:"IBM Plex Mono",ui-monospace,monospace}}
.toc{{display:flex;gap:14px;flex-wrap:wrap;font-size:13px;margin:0 0 8px}} .toc a{{color:var(--accent-ink);text-decoration:none}} .toc a:hover{{text-decoration:underline}}
a{{color:var(--accent-ink)}}
@media (prefers-reduced-motion:no-preference){{.bar i{{transition:width .3s ease}}}}
</style>
<div class="wrap">
<header>
 <div>
  <div class="eyebrow">entireio/cli · deterministic baseline</div>
  <h1>Entire CLI Complexity Baseline</h1>
  <p class="lede">Where the codebase's complexity sits, what each command exclusively owns, and where complexity outruns tests. Every number below is computed by a script from the tree at <span class="mono">{esc(commit)}</span>; the interpretation section at the end is the only part that is judgment.</p>
 </div>
 <div class="meta">measured {today}<br>unit + integration coverage<br>churn windows 90 / 180 days</div>
</header>
<nav class="toc"><a href="#glance">At a glance</a><a href="#method">Method</a><a href="#features">Cost by feature</a><a href="#scatter">Complexity vs coverage</a><a href="#commands">What each command owns</a><a href="#hotspots">Hotspots</a><a href="#dupl">Duplication</a><a href="#dead">Unreferenced code</a><a href="#reading">Reading</a><a href="#rerun">Re-run</a></nav>

<h2 id="glance">At a glance</h2>
<p class="note">Production code only: test files and the {num(gen_loc)} lines of generated ogen client (<span class="mono">internal/coreapi/*_gen.go</span>) are excluded from every complexity figure, and features marked <span class="mono">norank</span> in the mapping (test infrastructure, test-only agents) are excluded from headline totals and rankings. "Cognitive complexity" is gocognit's metric — how hard a function is to read, not how many paths it has.</p>
<div class="tiles">
 <div class="tile"><div class="tlabel">Production LOC</div><div class="tval">{num(tot_prod)}</div><div class="tsub">{num(tot_funcs)} functions in {len(core)} features</div></div>
 <div class="tile"><div class="tlabel">Test LOC</div><div class="tval">{num(tot_test)}</div><div class="tsub">{tot_test/tot_prod:.2f} test lines per prod line</div></div>
 <div class="tile"><div class="tlabel">Statement coverage</div><div class="tval">{100*tot_cov/tot_stmts:.0f}%</div><div class="tsub">median feature {median_cov:.0f}% · unit + integration</div></div>
 <div class="tile"><div class="tlabel">Functions with cognitive &gt; {COGNIT_WARN}</div><div class="tval">{num(tot_over)}</div><div class="tsub">{100*tot_over/tot_funcs:.1f}% of functions</div></div>
 <div class="tile"><div class="tlabel">Exclusive to one root</div><div class="tval">{num(excl_loc)}</div><div class="tsub">LOC reachable from exactly one command or main</div></div>
 <div class="tile"><div class="tlabel">Reached by no root</div><div class="tval">{num(un_total)}</div><div class="tsub">LOC in ranked features, incl. main/init as roots</div></div>
</div>
<div class="tiles">{area_tiles}</div>

<h2 id="method">Method</h2>
<p>Three small, deterministic tools were run against the tree; nothing here is estimated by an agent.</p>
<ul>
 <li><b>Per-function metrics</b> — every non-test Go file is parsed with <span class="mono">go/ast</span>; cyclomatic complexity from <span class="mono">fzipp/gocyclo</span>, cognitive complexity from <span class="mono">uudashr/gocognit</span> (the libraries behind the golangci linters of the same names), LOC per declaration.</li>
 <li><b>Coverage</b> — <span class="mono">go test -coverpkg=./... -coverprofile</span> for the unit suite and again for <span class="mono">-tags=integration</span>; the profiles are merged per block (max count) and attributed to the enclosing function by line range. The e2e suite spawns binaries and is not included.</li>
 <li><b>Churn</b> — <span class="mono">git log --numstat</span> over 90 and 180 days, per file (renames not followed).</li>
 <li><b>Feature mapping</b> — a checked-in JSON of path rules (<span class="mono">features.json</span>): a package directory, or a <span class="mono">&lt;noun&gt;_*.go</span> glob in the <span class="mono">cli</span> mega-package, maps to one feature; test files follow their source file unless a rule names them directly. {meta["unmapped_files"]} files unmapped ({num(meta["unmapped_loc"])} LOC).</li>
 <li><b>Command reachability</b> — the whole program is built to SSA and a VTA call graph (<span class="mono">golang.org/x/tools/go/callgraph/vta</span>); unresolved dynamic call sites with more than {MAXFAN} candidate callees are dropped as noise, closures count only when their enclosing function is reached, and generic instantiations roll up to their origin. Every function returning <span class="mono">*cobra.Command</span> is a root ({reach["cobra_roots"]} of them), plus one pseudo-root for package initializers and one per <span class="mono">main</span>. Traversal never enters another root, so a command's set is its own work. A declaration reached from exactly one root is <em>exclusive</em> to it.</li>
 <li><b>Duplication</b> — <span class="mono">dupl</span> at threshold 50 (the same recipe as <span class="mono">mise run dup</span>), aggregated per feature by <span class="mono">cxtool/dupl</span>.</li>
</ul>
<p class="note">Caveats: VTA over-approximates dynamic dispatch, so "reached" sets are generous and "exclusive" sets conservative; reflection and template lookups are not modelled. Coverage is statement coverage — it says a line ran, not that it was asserted on.</p>

<h2 id="features">Cost by feature</h2>
<p class="note">Click a column to sort. <b>cog/100</b> is cognitive complexity per 100 production lines (density); <b>&gt;{COGNIT_WARN}</b> counts functions above gocognit's usual warning threshold; <b>uncov. cog</b> is the cognitive complexity sitting in functions with under {COV_WARN:.0f}% coverage — the single best "complex and untested" number. Coverage under {COV_SER:.0f}% is flagged.</p>
<div class="filters" role="group" aria-label="Filter by area">
 {area_buttons}
</div>
<div class="card"><table id="ft"><thead><tr>
<th data-k="s">feature</th><th data-k="s">area</th><th class="n" data-k="n">prod LOC</th><th class="n" data-k="n">test ratio</th><th class="n" data-k="n" data-sorted="desc">Σ cognit</th><th class="n" data-k="n">cog/100</th><th class="n" data-k="n">&gt;{COGNIT_WARN}</th><th class="n" data-k="n">coverage</th><th class="n" data-k="n" title="cognitive complexity in functions under {COV_WARN:.0f}% coverage">uncov. cog</th><th class="n" data-k="n" title="commits touching the feature in the last 90 days">commits</th><th class="n" data-k="n" title="dupl findings in production code">dupl</th>
</tr></thead><tbody>
{feature_rows()}
</tbody></table></div>

<h2 id="scatter">Complexity vs coverage</h2>
<p class="note">Each dot is a ranked feature; size is production LOC; labels mark the twelve features with the most under-covered complexity. The lower-right is where complexity outruns tests. Hover or tab to a dot for its numbers; the table above is the same data.</p>
<div class="card">{scatter()}
<div class="legend"><span><i style="background:var(--s1)"></i>command surface</span><span><i style="background:var(--s2)"></i>capture pipeline &amp; agents</span><span><i style="background:var(--s3)"></i>infra &amp; platform</span></div></div>

<h2 id="commands">What each command exclusively owns</h2>
<p class="note">From the call graph. <b>exclusive LOC</b> is code no other command, initializer or <span class="mono">main</span> reaches — the marginal cost of that command; deleting it would free that much. <b>reached LOC</b> is everything it can touch, shared infrastructure included, and is inflated by interface dispatch.</p>
<div class="card"><table><thead><tr><th>command constructor</th><th>mostly</th><th class="n">exclusive LOC</th><th class="n">excl. cognit</th><th class="n">reached LOC</th></tr></thead><tbody>
{cmd_rows()}
</tbody></table></div>
<div class="two">
<div class="card"><h3 style="margin-top:0">How shared is the code?</h3><p class="note" style="margin-top:0">Production declarations bucketed by how many roots reach them.</p>
<table><tbody>{bucket_bars()}</tbody></table>
<p class="note">{num(core_loc)} LOC is reached by {CORE_ROOTS} or more roots — the root command's pre/post-run path (settings, logging, telemetry, the <span class="mono">.entire</span> guard, git repo access). Every command drags this in. Dynamic call sites that VTA could not resolve (it falls back to "every function with this signature") are cut at {MAXFAN} callees, so the buckets are conservative.</p></div>
<div class="card"><h3 style="margin-top:0">Pseudo-roots</h3><table><thead><tr><th>root</th><th class="n">exclusive LOC</th><th class="n">reached LOC</th></tr></thead><tbody>
{"".join(f'<tr><td class="mono">{esc(c["root"])}</td><td class="n">{num(c["excl_loc"])}</td><td class="n muted">{num(c["reach_loc"])}</td></tr>' for c in pseudo)}
</tbody></table></div>
</div>

<h2 id="hotspots">Hotspots</h2>
<h3>Most complex functions</h3>
<div class="card"><table><thead><tr><th class="n">cognit</th><th class="n">cyclo</th><th class="n">LOC</th><th class="n">coverage</th><th>feature</th><th>function</th></tr></thead><tbody>
{"".join(func_row(fn) for fn in top_funcs)}
</tbody></table></div>
<h3>Complex and under-covered <span class="muted small">(cognitive ≥ {COGNIT_WARN}, coverage &lt; {COV_WARN:.0f}%, {len(undercov_all)} functions total)</span></h3>
<div class="card"><table><thead><tr><th class="n">cognit</th><th class="n">cyclo</th><th class="n">LOC</th><th class="n">coverage</th><th>feature</th><th>function</th></tr></thead><tbody>
{"".join(func_row(fn) for fn in undercov)}
</tbody></table></div>
<h3>Churn × complexity <span class="muted small">(commits in 90 days × Σ cognitive, per file)</span></h3>
<p class="note">The files that are both hard to read and constantly edited. This is where refactoring pays back fastest; a complex file nobody touches can wait.</p>
<div class="card"><table><thead><tr><th class="n">commits 90d</th><th class="n">Σ cognit</th><th class="n">LOC</th><th class="n">coverage</th><th>feature</th><th>file</th></tr></thead><tbody>
{hot_rows()}
</tbody></table></div>

<h2 id="dupl">Duplication</h2>
<p class="note">{dupl["total"]} <span class="mono">dupl</span> findings at threshold 50; {sum(dupl["test_by_feature"].values())} of them are in test files (table-driven test setup mostly — cheap duplication). The {sum(dupl["prod_by_feature"].values())} in production code are below; the last column normalises by feature size.</p>
<div class="two">
<div class="card"><table><thead><tr><th>feature</th><th class="n">findings</th><th class="n">~lines</th><th class="n">per kLOC</th></tr></thead><tbody>{dupl_rows()}</tbody></table></div>
<div class="card"><h3 style="margin-top:0">Largest duplicated spans</h3><ul class="small" style="padding-left:18px;margin:0">{largest_dupl()}</ul></div>
</div>

<h2 id="dead">Code no root reaches</h2>
<p class="note">{num(un_total)} LOC in {len(unreached)} declarations across ranked features that neither a cobra command, an initializer nor a <span class="mono">main</span> can reach. Some is exported API used only by tests; some is reflection targets the analysis cannot see; the rest is dead. Verify before deleting — grep for the name in <span class="mono">_test.go</span> first.</p>
<div class="card"><table><thead><tr><th class="n">LOC</th><th class="n">funcs</th><th class="n">cognit</th><th>file · functions</th></tr></thead><tbody>
{unreached_rows()}
</tbody></table></div>

<h2 id="reading">Reading the numbers</h2>
<p class="note">This section is interpretation, written after looking at the code behind the outliers of the {esc(commit)} snapshot. Its inline figures are frozen to that snapshot; everything above regenerates.</p>
<ol class="reading">
 <li><b>The capture pipeline is the load-bearing core and it is in decent shape.</b> <span class="mono">capture:strategy</span> + <span class="mono">checkpoint-store</span> + <span class="mono">hooks-lifecycle</span> is ~38k prod LOC with a 1.5–1.75 test ratio and 77–80% coverage. Its complexity is real but concentrated in a handful of known-hard places — <span class="mono">handleLifecycleTurnEnd</span> (374 lines, cognitive 83), the two OPF rewrites, <span class="mono">EnsurePrimaryRef</span>. These are candidates for splitting, not cutting.</li>
 <li><b>The pre-push OPF re-redaction is a {num(PREPUSH["excl_loc"])}-LOC feature reachable from exactly one hook.</b> <span class="mono">newHooksGitPrePushCmd</span> exclusively owns {num(PREPUSH["excl_loc"])} LOC / {num(PREPUSH["excl_cognit"])} cognitive — <span class="mono">manual_commit_opf_rewrite.go</span> and <span class="mono">manual_commit_opf_refs.go</span> (two backends, one policy). It is well tested; the question is whether both backends still need to exist.</li>
 <li><b><span class="mono">entire review</span> is the single most expensive command surface</b>: {num(REV["excl_loc"])} exclusive LOC, {num(REV["excl_cognit"])} exclusive cognitive, coverage 70% — and <span class="mono">review/picker.go</span> (1,347 lines of <span class="mono">huh</span> forms) is 32% covered. It is experimental. Together with <span class="mono">investigate</span> ({num(INV["excl_loc"])} exclusive) and <span class="mono">experts</span> ({num(EXPERTS["excl_loc"])}), the experimental surface is ~{EXP_TOTAL/1000:.0f}k exclusive LOC that the capture core never needs.</li>
 <li><b><span class="mono">cmd:trail</span> carries the most complexity with the least test backing among shipped features</b>: 8.2k prod LOC at a 0.88 test ratio, 65% coverage, 468 points of cognitive complexity in under-covered functions — the highest of any feature — and the highest density of the large features (20.6/100). Much of it is 30 near-identical cobra leaf constructors; the shape suggests a table-driven verb registry would remove a few hundred lines outright.</li>
 <li><b>The control-plane commands duplicate their own list/paging shape per noun.</b> <span class="mono">newProjectListCmd</span> is cognitive 52 at 14.6% coverage; <span class="mono">newRepoListCmd</span> 56; <span class="mono">org.go:85</span> and <span class="mono">project.go:163</span> are a 37-line verbatim duplicate; 16 dupl findings — the most of any feature. The org × name-filter × pagination branching belongs in one generic helper next to <span class="mono">runCoreList</span>. Also the least-covered shipped area (65%): it talks to a live control plane and has no fake.</li>
 <li><b>Two functions should be split on sight.</b> <span class="mono">newSearchCmd</span> (cognitive 105 — the highest in the tree, a 280-line <span class="mono">RunE</span> closure holding every search mode) and <span class="mono">handleLifecycleTurnEnd</span> (83). Both are hot: 52 and 74 commits in 90 days.</li>
 <li><b>Mechanical duplication worth ten minutes:</b> <span class="mono">strategy/cleanup.go:510–590</span> is the same delete-and-log block three times (52 + 52 + 25 lines); <span class="mono">grant.go:152/265</span> and <span class="mono">setup_search_skill.go:100/129</span> likewise.</li>
 <li><b>Unreachable code, as of this snapshot.</b> The rewind restore machinery (<span class="mono">Rewind</span>, <span class="mono">PreviewRewind</span>, <span class="mono">CanRewind</span>, <span class="mono">checkCanRewindWithWarning</span>, 459 LOC with no caller outside tests) was confirmed dead by this analysis and has since been removed on main (#2178, follow-ups #2186/#2187). <span class="mono">checkpoint/fsstore</span> (326 LOC) is documented as a test-only reference backend and nothing outside its own tests imports it — it could live in an internal test package. The strategy getters <span class="mono">GetSessionInfo</span>, <span class="mono">GetCheckpointLog</span>, <span class="mono">GetTaskCheckpoint*</span> (78 LOC) and <span class="mono">claudecode.FindCheckpointUUID</span> have no callers.</li>
 <li><b>The <span class="mono">cli</span> mega-package is the structural cost.</b> 149 source files, 64k LOC, 2,122 functions in one package importing 66 of the module's 100 packages. Every feature above that lives in it shares one namespace and one test binary; the per-feature numbers here exist only because file naming is disciplined. Moving the largest command families (<span class="mono">trail</span>, <span class="mono">plugin</span>, <span class="mono">setup</span>, control-plane) into packages, as <span class="mono">review</span> and <span class="mono">investigate</span> already are, is what would make these numbers enforceable rather than observable.</li>
 <li><b>Where coverage is lowest, the reason is interactivity, not neglect</b>: pickers, wizards and TUIs (<span class="mono">review/picker.go</span> 32%, <span class="mono">repo_mirror_create_wizard.go</span> 32%, <span class="mono">dispatch_wizard.go</span>, <span class="mono">search_v4.go</span> 55%) and commands that need a live backend (<span class="mono">runner</span> 35%, <span class="mono">api</span> 54%, control-plane 65%). Both have known fixes — a headless form driver and a control-plane fake — and both would move many features at once.</li>
</ol>

<h2 id="rerun">Re-running it</h2>
<p class="note">Everything is scripted; re-running against a new commit gives a diffable set of CSV/JSON files. The tools live in <span class="mono">tools/complexity/</span> (a self-contained Go module) — see its README for the exact commands and flags.</p>
<pre>go test -count=1 -coverprofile=/tmp/cx/unit.out -coverpkg=./... ./...
go test -count=1 -tags=integration -coverprofile=/tmp/cx/int.out -coverpkg=./... \\
  ./cmd/entire/cli/integration_test/... ./cmd/entire/cli/auth/...
cd tools/complexity
go run .        -root ../.. -cover /tmp/cx/unit.out,/tmp/cx/int.out -out /tmp/cx/all
go run ./reach  -root ../.. -out /tmp/cx/reach
golangci-lint run -c dupl-config.yaml --new=false --output.json.path=/tmp/cx/dupl.json ../...
go run ./dupl   -in /tmp/cx/dupl.json -root ../.. -out /tmp/cx/dupl-by-feature.json
python3 render.py /tmp/cx/all /tmp/cx/reach /tmp/cx/dupl-by-feature.json ../.. baseline.html</pre>
<p class="note">To turn observation into a gate: enable <span class="mono">gocognit</span> in <span class="mono">.golangci.yaml</span> with a threshold around 30 and <span class="mono">--new-from-rev</span> so only new violations fail; and diff <span class="mono">features.csv</span> in CI so a feature's coverage or uncovered-cognit figure cannot silently regress.</p>
</div>
<div id="tip" role="tooltip"></div>
<script>
(function(){{
 // sortable tables
 document.querySelectorAll('table thead th[data-k]').forEach(function(th){{
  th.tabIndex=0;
  function sortBy(){{
   var table=th.closest('table'),tb=table.tBodies[0],idx=Array.prototype.indexOf.call(th.parentNode.children,th);
   var dir=th.getAttribute('data-sorted')==='desc'?'asc':'desc';
   table.querySelectorAll('th').forEach(function(x){{x.removeAttribute('data-sorted')}});th.setAttribute('data-sorted',dir);
   var rows=Array.prototype.slice.call(tb.rows);
   rows.sort(function(a,b){{var ca=a.cells[idx],cb=b.cells[idx];var va,vb;
    if(th.dataset.k==='n'){{va=parseFloat(ca.dataset.v!==undefined?ca.dataset.v:ca.textContent.replace(/[^0-9.-]/g,''))||0;vb=parseFloat(cb.dataset.v!==undefined?cb.dataset.v:cb.textContent.replace(/[^0-9.-]/g,''))||0;return dir==='desc'?vb-va:va-vb;}}
    va=ca.textContent.trim().toLowerCase();vb=cb.textContent.trim().toLowerCase();return dir==='desc'?(va<vb?1:va>vb?-1:0):(va<vb?-1:va>vb?1:0);}});
   rows.forEach(function(r){{tb.appendChild(r)}});
  }}
  th.addEventListener('click',sortBy);th.addEventListener('keydown',function(e){{if(e.key==='Enter'||e.key===' '){{e.preventDefault();sortBy();}}}});
 }});
 // area filter
 var btns=document.querySelectorAll('.filters button');
 btns.forEach(function(b){{b.addEventListener('click',function(){{
  btns.forEach(function(x){{x.setAttribute('aria-pressed','false')}});b.setAttribute('aria-pressed','true');
  var f=b.dataset.f;document.querySelectorAll('#ft tbody tr').forEach(function(r){{r.hidden=!(f==='all'||r.dataset.area===f)}});
 }})}});
 // scatter tooltip
 var tip=document.getElementById('tip');
 function show(e,el){{tip.textContent=el.dataset.tip;tip.style.display='block';var r=el.getBoundingClientRect();tip.style.left=Math.min(window.innerWidth-360,r.left+r.width/2+10)+'px';tip.style.top=(r.top-10)+'px';}}
 function hide(){{tip.style.display='none'}}
 document.querySelectorAll('.dot').forEach(function(d){{d.addEventListener('mouseenter',function(e){{show(e,d)}});d.addEventListener('mouseleave',hide);d.addEventListener('focus',function(e){{show(e,d)}});d.addEventListener('blur',hide);}});
}})();
</script>
"""
open(HTML_OUT, "w").write(page)
print(f"wrote {HTML_OUT} ({len(page)//1024} KB)")
