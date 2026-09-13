# BTW Buildathon 2026 — Project Handoff Notes

Context document for the AI coding agent picking this up. This captures every decision made during planning, before any implementation started.

---

## Event basics

- **Event:** Bengaluru Tech Week Buildathon 2026, powered by Entire + Databricks
- **Date:** Sunday, 6 September 2026, 9:00 AM–5:00 PM IST
- **Venue:** Scaler School of Technology, Electronic City, Bengaluru
- **Submission deadline:** 3:00 PM IST — hard cutoff
- **Judging:** 3:00–5:00 PM
- **Team:** Solo builder, full-stack (GitHub: `Rohithvishnukumar`). Same person is submission owner and demo owner.

---

## Track selected: Track 1 — Build a Checkpoint-Native Developer Experience

- **Build on:** `entireio/cli` (fork this — NOT `entire-graph` [Track 2] or `external-agents` [Track 3])
- **Core idea:** turn context preserved in real Entire Checkpoints into a useful developer workflow. Git shows *what* changed; checkpoints should preserve *why*, what was attempted, assumptions made, what failed, what's unresolved.
- **Official use cases** (targeting all five):
  1. Review an implementation against its stated intent
  2. Identify unfinished requirements or unresolved risks
  3. Create a change-risk or release-readiness report
  4. Hand work to another developer or agent without losing original reasoning
  5. Resume an agent-assisted workflow with confidence after a break
- **Success bar:** checkpoint context must be an *essential* input — the tool should let someone complete a task more accurately/efficiently than from a git diff alone. Using Entire only to track dev without using its context does not qualify.

---

## Product: "Checkpoint Resume & Drift Assistant"

Three CLI subcommands sharing one core (build in this priority order — 1 and 2 are must-haves, 3 is a stretch goal only if time remains):

### Shared core (build first)
- **CheckpointReader** — parses the `entire/checkpoints/v1` branch into structured session data: session id, timestamps, prompts/decisions text, files touched, stated intent.
- **GraphClient** — thin wrapper shelling out to `entire graph` commands (`search`, `diff`, `impact`) and parsing their output.
- **DatabricksSync** — pushes parsed checkpoint records into a Delta table.

### 1. `entire resume` — covers use cases 4 & 5, partially 1
- Pulls the checkpoint narrative (intent, decisions, files touched).
- Adds a Graph impact/blast-radius section: for files/symbols the last session touched, show what else depends on them.
- Adds one Databricks aggregate number (e.g. coverage %).
- Renders to terminal **and** a single self-contained HTML file (for demo visual appeal, no server needed).
- This is the command we dogfood at noon to reconstruct our own context in a fresh session.

### 2. `entire drift` — covers use cases 1 & 2
- Compares the *original plan* (from the first checkpoint) against *current* code state.
- Uses `entire graph diff` (entity-level) rather than raw text diff — a function can move without meaningfully changing; raw diff would false-flag it as drift, Graph won't.
- Outputs a list of unfinished/dropped requirements.

### 3. `entire assess <commit-ish>` — stretch goal, covers use case 3
- Only build after 1 and 2 are solid and running on real data.
- Reuses the same core. Scopes Graph impact analysis to one specific commit/checkpoint.
- Cross-references that change against the nearest checkpoint's stated intent — flags things like "this touches files the plan didn't mention."
- Cheap to add (~30–45 min) since it's composition of existing pieces, not new build.

---

## Databricks integration (Best Use of Databricks award)

- **Free Edition account** — create/open before the event (setup ≠ implementation, this is allowed prep).
- **Constraints to design around:** one serverless 2X-Small SQL warehouse total, up to 5 concurrent job tasks, up to 3 Apps, one workspace/metastore per account. Keep scope narrow — don't overbuild.
- **Plan:** one Delta table storing structured checkpoint records (via DatabricksSync) + exactly one working SQL query computing something meaningful — requirement-coverage % or file-churn/risk score. `entire drift` (and/or `resume`) queries this back.
- **Meaningful-use test:** removing Databricks should break the trend/aggregate analysis, leaving only a single-point-in-time view. That's the bar for "essential," not "superficial storage."
- **Must document in BUILDATHON.md:** capabilities used, why essential, workspace/app/endpoint/demo URL, relevant repo paths, reproduction steps, data provenance, and how the Noon Curveball affected the Databricks workflow.

---

## Repos — which to fork (30+ repos in the org, most are noise)

| Repo | What it is | Relevant? |
|---|---|---|
| `entireio/cli` | Core Entire CLI — checkpoints, git integration | **Fork this one** |
| `entireio/entire-graph` | Semantic code-graph plugin | Track 2 only |
| `entireio/external-agents` | Plugin binaries for more coding agents | Track 3 only |
| `entireio/cli-checkpoints` | Small, unrelated repo | Ignore |
| `entireio/entire-judge` | Looks like organizers' internal judging tooling | Ignore — not for participants |
| Everything else (`git-sync`, `auth-go`, `homebrew-tap`, `scoop-bucket`, `forgemark`, `devcontainer-features`, etc.) | Internal infra / distribution tooling | Ignore |

---

## Exact setup commands (run ONLY after 9:00 AM official start)

```bash
# 1. Fork entireio/cli to your account
gh repo fork entireio/cli --clone=false
# (no GitHub CLI? click "Fork" manually on github.com/entireio/cli)

# 2. Authenticate Entire CLI
entire login

# 3. Create the mirror — select your fork + India region when prompted
entire repo mirror create

# 4. Clone through Entire's mirror workflow
entire repo clone /gh/Rohithvishnukumar/cli

# 5. Move into the cloned directory
cd cli

# 6. Enable checkpoints for Claude Code specifically
entire enable -y --agent claude-code

# 7. Confirm it's tracking
entire status

# 8. Install and activate the Graph plugin
entire plugin install graph
entire graph version
entire graph init-agents --repo .
```

**Important:** after `init-agents`, close the current Claude Code session and start a fresh one (`claude`) in that directory. Graph instructions are only loaded into sessions started *after* `init-agents` runs — an already-open session won't pick them up.

---

## Required checkpoint milestones (minimum 4, non-negotiable)

1. Initial understanding and intended architecture
2. Last stable state before the Noon Curveball
3. Response to the Noon Curveball
4. Final implementation and verification

Checkpoint **quality matters more than quantity** — capture decisions, rejected options, failures, assumptions, open risks, and the evidence that changed the approach. Not "wip" commit messages.

## Required Graph evidence (at least 3 distinct, demonstrated actions)

1. A graph search or definition lookup — do early, right after `init-agents`.
2. A relationship/impact analysis **before** a high-risk change — do before touching the curveball-affected area.
3. A final semantic-diff analysis of the submitted implementation — do at the end, before the final checkpoint.

Treat Graph output as evidence, not an oracle — verify findings against real source/tests. Never present incomplete or uncertain graph output as fact.

---

## Hour-by-hour plan

Real build time is only ~5 hours total (9–12 and 1–3); the noon hour is curveball + lunch combined, not extra build time.

| Time | What happens |
|---|---|
| 9:00–9:15 | Fork, mirror, clone (commands above) |
| 9:15–9:35 | Enable Checkpoints + Graph. Write a substantive first checkpoint: user, problem, why git diff isn't enough, planned architecture. Run one `entire graph search` (1st required Graph evidence). |
| 9:35–11:00 | Build shared core (CheckpointReader, GraphClient). Wire minimal Databricks: one Delta table, sync script, one SQL query. |
| 11:00–11:45 | Ship `entire resume` v1 and `entire drift` v1 — must run on real data from this repo, not mocked. |
| 11:45–12:00 | **Stop new features.** Clean commit. Write the required pre-noon checkpoint: intent, architecture, done/unresolved work, risks. |
| 12:00–1:00 | Close session for real. Receive curveball (Track 1-specific only). Think while eating. Open a **fresh** session, use own `entire resume` output to reconstruct context — this is the dogfooding moment and best demo proof. Run Graph impact analysis on the affected area before editing (2nd required Graph evidence). |
| 1:00–2:20 | Implement the smallest complete curveball response. Update Databricks table/query if data shape changed (scores real points on the Databricks rubric). Write/adjust tests. Run final semantic-diff analysis via Graph (3rd required evidence). Confirm no regressions in resume/drift. Commit final checkpoint. |
| 2:20–3:00 | Fill `BUILDATHON.md`. Run final 20-minute checklist. Submit all required fields before 3:00 PM sharp. |
| 3:00–5:00 | Judging — keep project runnable, don't touch main branch, rehearse demo script. |

---

## BUILDATHON.md required outline

```
# Project name
## One-sentence summary
## Problem, intended user and why it matters
## Selected Entire track and why Entire is essential
## Architecture and main workflow
## Entire Graph findings and verification
## Noon Curveball: what changed and how we adapted
## Checkpoint links and what each checkpoint proves
## Setup, run and test instructions
## Databricks use, data sources and limitations (if applicable)
## Known limitations and next steps
```

Don't skip "known limitations" — it's explicitly judged (Demonstration and future potential, 10 pts).

---

## Final 20-minute submission checklist

- [ ] Final commit pushed, SHA matches submission
- [ ] Project launches from a clean checkout / documented setup path
- [ ] Required checkpoints open and clearly explain the 4 milestones
- [ ] Entire Graph evidence + final semantic-diff analysis are recorded
- [ ] BUILDATHON.md complete, readable, free of secrets
- [ ] Tests covering critical + Curveball behavior pass
- [ ] Databricks resource links + data notes included
- [ ] Demo owner can sign in, open every resource, run the critical path
- [ ] Fallback screenshot/recording saved locally
- [ ] Submitted before 3:00 PM IST

## Required submission fields

- Selected Entire track (Track 1)
- GitHub fork URL and final commit SHA
- Entire mirror/project URL
- Links to the required Entire Checkpoints
- Clear setup, run, test instructions
- Working product demo or a reliable fallback recording
- Complete `BUILDATHON.md` in repo root
- If opting into Databricks: capabilities used, why essential, workspace/app/endpoint/demo URL, relevant repo paths, reproduction steps, data provenance, how the Curveball affected the Databricks workflow

---

## Judging rubric (use this to prioritize effort under time pressure)

### Entire main challenge — 100 points
| Criterion | Points |
|---|---|
| Problem and innovation | 20 |
| Technical implementation | 25 |
| Response to the Curveball | 15 |
| Use of Entire Checkpoints | 15 |
| Use of Entire Graph | 15 |
| Demonstration and future potential | 10 |

### Best Use of Databricks — 100 points
| Criterion | Points |
|---|---|
| Meaningful use | 30 |
| Working implementation and reliability | 25 |
| User value and product decisions | 20 |
| Data quality, provenance, responsible use | 15 |
| Curveball response | 10 |

**Takeaway:** Technical implementation + Problem/innovation = 45% of the Entire score → fewer things, all of them actually working, beats broad-but-flaky. Databricks "meaningful use" alone is 30% of that award → keep it narrow but genuinely load-bearing, never superficial storage.

---

## Final demo script (for 3:00–5:00 judging)

1. State the user and problem in one sentence.
2. Show the working product and critical path live — not a slide walkthrough.
3. Explain why Entire is essential to the solution.
4. Show one useful checkpoint and one graph finding that changed or verified a decision.
5. Explain the Noon Curveball: what changed, what behavior changed, the test that proves it.
6. Show the essential Databricks function and evidence that it's working.
7. Close with known limitations and the next step toward production readiness.

---

## Key strategic decisions made during planning

- **Solo builder** — no team coordination overhead, but no parallelization either; scope is deliberately cut tight for a ~5-hour build window.
- **CLI-first interface**, not a web app — matches Entire's own convention (`entire graph search`, etc.), cheap to build solo. Each command also writes a self-contained HTML report purely for demo visual appeal, no backend needed.
- **Dogfooding is the core strategy**: our own build-process checkpoints ARE the real data the tool analyzes. At noon we literally run `entire resume` on ourselves to reconstruct context — this satisfies the mandatory workflow AND is our strongest, unstaged demo proof point.
- **The tool must stay generic** — accept a repo path as a parameter, don't hardcode assumptions about its own project structure. Otherwise it reads as a party trick rather than a real reusable tool during judging.
- **Allowed before 9:00 AM:** research, user discovery, problem framing, sketches, planning, and testing Entire CLI commands on a *throwaway scratch repo* (not the submission repo) to learn syntax. **Not allowed:** any actual implementation of the submission itself, or arriving with a prebuilt feature branch.

## Open decisions to make on the day

- Exact SQL query for the Databricks layer — requirement-coverage % vs. file-churn risk score. Pick whichever is fastest to compute correctly from real checkpoint data once it exists.
- Whether to attempt `entire assess` — only after `resume` and `drift` are both solid and running on real data, not before.
- Exact mechanism for `drift`'s "requirements" baseline — likely a lightweight `requirements.md` maintained alongside checkpoints, or parsed directly from the first checkpoint's stated intent text.
