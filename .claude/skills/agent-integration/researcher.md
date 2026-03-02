# Research Command

Assess whether a target AI coding agent's hook/lifecycle model is compatible with the Entire CLI before writing any Go code.

## Procedure

### Phase 1: Understand Entire's Expectations

Read `docs/architecture/agent-guide.md` to understand what Entire expects from agents: EventType names, required interfaces, hook patterns, and lifecycle flow. This gives you the vocabulary to map the target agent's native hooks to Entire's event model.

You may also reference `cmd/entire/cli/agent/agent.go` and `cmd/entire/cli/agent/event.go` for exact interface signatures and EventType constants. Prefer `agent-guide.md` as the primary source, but the source files are the ground truth if the guide is out of date. Avoid reading other internal files (`hook_registry.go`, `lifecycle.go`, strategy code) — the implementer handles those, and architecture tests enforce this boundary at the code level.

### Phase 2: Static Capability Checks

Non-destructive CLI probing. Record PASS/WARN/FAIL for each:

| Check | Command | PASS | FAIL |
|-------|---------|------|------|
| Binary present | `command -v $AGENT_BIN` | Found | Not found (blocker) |
| Help output | `$AGENT_BIN --help` or `$AGENT_BIN help` | Available | No help |
| Version info | `$AGENT_BIN --version` or `$AGENT_BIN version` | Available | N/A |
| Hook keywords | Scan help for: hook, lifecycle, callback, event, trigger, pre-, post-, plugin, extension | Found | None found |
| Session keywords | Scan help for: session, resume, continue, history, transcript, context | Found | None found |
| Config directory | Check `~/.$AGENT_SLUG/`, `~/.config/$AGENT_SLUG/`, `./$AGENT_SLUG/`, `./.${AGENT_SLUG}/` | Found | None found |
| Documentation | Web search for hook/plugin/extension docs | Found | None found |

### Phase 3: Test Script Creation

Based on Phase 2 findings, create an **agent-specific** test script:

```
scripts/test-$AGENT_SLUG-agent-integration.sh
```

The script is tailored to the specific agent's hook mechanism (not a generic template). Adapt the hook wiring section based on what Phase 2 discovered.

**Script structure:**

```bash
#!/usr/bin/env bash
set -euo pipefail

AGENT_NAME="..."
AGENT_SLUG="..."
AGENT_BIN="..."
PROBE_DIR=".entire/tmp/probe-${AGENT_SLUG}-$(date +%s)"
```

**Required sections:**

1. **Static checks** — Re-runnable binary/version/help checks
2. **Hook wiring** — Create workspace-local config that intercepts hooks and dumps stdin JSON to `$PROBE_DIR/captures/<event-name>-<timestamp>.json`
3. **Run modes:**
   - `--run-cmd '<cmd>'` — Automated: launch agent, wait, collect
   - `--manual-live` — Interactive: user runs agent manually, presses Enter
4. **Capture collection** — List and pretty-print all payload files
5. **Cleanup** — Restore original config (unless `--keep-config`)
6. **Verdict** — PASS/WARN/FAIL per lifecycle event + COMPATIBLE/PARTIAL/INCOMPATIBLE

### Phase 4: Execution & Analysis

Run the script and analyze:

1. **Execute**: `chmod +x scripts/test-$AGENT_SLUG-agent-integration.sh && scripts/test-$AGENT_SLUG-agent-integration.sh --manual-live`
2. **For each captured payload**: show command, artifact path, decoded JSON
3. **Lifecycle mapping**: native hook name → Entire EventType

### Phase 5: Implementation One-Pager

Write the research findings to `cmd/entire/cli/agent/$AGENT_PACKAGE/AGENT.md` as a structured one-pager that the test-writer and implementer phases will use as their single source of agent-specific information.

**Create the agent package directory first** (if it doesn't exist):

```bash
mkdir -p cmd/entire/cli/agent/$AGENT_PACKAGE
```

**Write the one-pager using this template:**

```markdown
# $AGENT_NAME — Integration One-Pager

## Verdict: COMPATIBLE / PARTIAL / INCOMPATIBLE

## Static Checks
| Check | Result | Notes |
|-------|--------|-------|
| Binary present | PASS/FAIL | path |
| Help available | PASS/FAIL | |
| Version info | PASS/FAIL | version string |
| Hook keywords | PASS/FAIL | keywords found |
| Session keywords | PASS/FAIL | keywords found |
| Config directory | PASS/FAIL | path |
| Documentation | PASS/FAIL | URL |

## Binary
- Name: `$AGENT_BIN`
- Version: ...
- Install: ... (how to install if not present)

## Hook Mechanism
- Config file: `~/.config/$AGENT_SLUG/settings.json` (exact path)
- Config format: JSON / YAML / TOML
- Hook registration: ... (how hooks are declared — JSON objects, env vars, etc.)
- Hook names and when they fire:
  | Native Hook Name | When It Fires | Entire EventType |
  |-----------------|---------------|-----------------|
  | `on_session_start` | Agent session begins | `SessionStart` |
  | ... | ... | ... |
- Valid Entire EventTypes: `SessionStart`, `TurnStart`, `TurnEnd`, `Compaction`, `SessionEnd`, `SubagentStart`, `SubagentEnd`
- Hook input (stdin JSON): ... (exact fields with example payload)

## Transcript
- Location: `~/.config/$AGENT_SLUG/sessions/<id>/transcript.jsonl`
- Format: JSONL / JSON array / other
- Session ID extraction: ... (from hook payload field or directory name)
- Example entry: `{"role": "user", "content": "..."}`

## Config Preservation
- Keys to preserve when modifying: ... (or "use read-modify-write on entire file")
- Settings that affect hook behavior: ...

## CLI Flags
- Non-interactive prompt: `$AGENT_BIN --prompt "..." --no-confirm`
- Interactive mode: `$AGENT_BIN` (or "not supported")
- Relevant env vars: ...

## Gaps & Limitations
- ... (anything that doesn't map cleanly)

## Captured Payloads
- See `.entire/tmp/probe-$AGENT_SLUG-*/captures/` for raw JSON captures
```

**Key points about the one-pager:**

- The **Entire EventType mapping** (which native hook → which EventType) uses the event names learned from `agent-guide.md` in Phase 1. The researcher can do this mapping because it's a simple table — it doesn't need Entire source code.
- Fill in every section with concrete values from Phases 2-4. Don't leave placeholders.
- If a section doesn't apply (e.g., no transcript support), say so explicitly.
- This file persists as development documentation — future maintainers will reference it.

### Phase 6: Commit

Use `/commit` to commit all files.

## Blocker Handling

If blocked at any point (auth, sandbox, binary not found):

1. State the exact blocker
2. Provide the exact command for the user to run manually
3. Explain what output to paste back
4. Continue with provided output

## Constraints

- **No Go code.** This command produces a one-pager and test script only.
- **Non-destructive.** All artifacts go under `.entire/tmp/` (gitignored). The one-pager goes in the agent package directory.
- **Agent-specific scripts.** Adapt based on Phase 2 findings, not a generic template.
- **Ask, don't assume.** If the hook mechanism is unclear, ask the user.
- **External focus.** Prefer `agent-guide.md` for Entire's expectations. You may reference `agent.go` and `event.go` for exact names, but avoid deeper internals — architecture tests enforce the boundary at the code level.
