# Checkpoint Intelligence

## One-sentence summary
Turn AI coding history captured by Entire Checkpoints into actionable development intelligence — showing what was intended, what was actually implemented, what remains unfinished, and what could be affected.

## Problem, intended user and why it matters
AI coding agents can make many changes across a repository, but after multiple sessions developers may not know what was originally intended, what the agent actually implemented, which requirements are complete or incomplete, what parts of the system are affected by changes, and how another developer can understand and continue the work. Git commits alone do not preserve enough of the development intent and agent-session context. Our product converts Entire Checkpoints context into a developer-facing workflow, enabling developers to quickly audit, understand, and hand off AI-assisted development work.

## Selected Entire track and why Entire is essential
Track 1 — Build a Checkpoint-Native Developer Experience. Entire Checkpoints are an essential input because they provide the critical context (developer intent, agent actions, requirement progression) that is lost in standard Git commits. Without Checkpoints, we cannot reconstruct the full timeline of why and how AI agents modified the codebase.

## Architecture and main workflow
Frontend
    ↓
Backend/API
    ↓
GitHub + Entire CLI/Checkpoint data + Entire Graph
    ↓
Analysis engine
    ↓
Requirement / Risk / Handoff results
    ↓
Frontend

## Entire Graph findings and verification
TBD

## Noon Curveball: what changed and how we adapted
TBD

## Checkpoint links and what each checkpoint proves
| Checkpoint | Purpose | What it proves | Link |
|---|---|---|---|
| TBD | Initial understanding | TBD | TBD |
| TBD | Pre-noon stable state | TBD | TBD |
| TBD | Curveball response | TBD | TBD |
| TBD | Final implementation/verification | TBD | TBD |

## Setup, run and test instructions
TBD

## Databricks use, data sources and limitations (if applicable)
Databricks is not currently part of the MVP.

## Known limitations and next steps
TBD
