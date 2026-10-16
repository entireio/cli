# Debug Entire CLI via E2E Artifacts

Diagnose Entire CLI bugs using captured artifacts from the E2E test suite. Artifacts are written to `e2e/artifacts/` locally or downloaded from CI via GitHub Actions.

## Inputs

The user provides either:
- **A test run directory:** `e2e/artifacts/{timestamp}/` -- triage all failures
- **A specific test directory:** `e2e/artifacts/{timestamp}/{TestName}-{agent}/` -- debug one test

## Artifact Layout

```
e2e/artifacts/{timestamp}/
├── report.nocolor.txt          # Pass/fail/skip summary with error lines
├── test-events.json            # Raw Go test events (NDJSON)
├── entire-version.txt          # CLI version under test
└── {TestName}-{agent}/
    ├── PASS or FAIL            # Status marker
    ├── console.log             # Full operation transcript
    ├── git-log.txt             # git log --decorate --graph --all
    ├── git-tree.txt            # ls-tree HEAD + checkpoint branch
    ├── entire-logs/entire.log  # CLI structured JSON logs
    ├── checkpoint-metadata/    # Checkpoint + session metadata
    └── repo -> /tmp/...        # Symlink to preserved repo (E2E_KEEP_REPOS=1 only)
```

## Preserved Repo

When the test run was executed with `E2E_KEEP_REPOS=1`, each test's artifact directory contains a `repo` symlink pointing to the preserved temporary git repository. This is the actual repo the test operated on -- you can inspect it directly.

**Navigate via the symlink** (e.g., `{artifact-dir}/repo/`) rather than resolving the `/tmp/...` path. The symlink lives inside the artifact directory so permissions and paths stay consistent.

The preserved repo contains:
- Full git history with all branches (main, `entire/checkpoints/v1`)
- The `.entire/` directory with CLI state, config, and raw logs
- The `.claude/` directory (if Claude Code was the agent)
- All files the agent created or modified, in their final state

This is the most powerful debugging tool -- you can run `git log`, `git diff`, `git show`, inspect `.entire/` internals, and see exactly what the CLI left behind.

## Debugging Workflow

### 1. Triage (if given a run directory)

Read `report.nocolor.txt` to identify failures and their error messages. Each entry shows the test name, agent, duration, and failure output with file:line references.

### 2. Read console.log (most important)

Full transcript of every operation:
- `> claude -p "..." ...` -- agent prompts with stdout/stderr
- `> git add/commit/...` -- git commands
- `> send: ...` -- interactive session inputs

This tells you what happened chronologically.

### 3. Read test source code

Use the file:line from the report to find the test in `e2e/tests/`. Understand what the test expected to happen vs what console.log shows actually happened.

### 4. Diagnose the CLI behavior

Cross-reference console.log (what happened) with the test (what should have happened). Focus on CLI-level issues:

| Symptom | CLI Investigation |
|---------|-------------------|
| Checkpoint not created / timeout | Check `entire-logs/entire.log` for hook invocations, phase transitions, errors |
| Wrong checkpoint content | Check `git-tree.txt` for checkpoint branch files, `checkpoint-metadata/` for session info |
| Hooks didn't fire | Check `entire-logs/entire.log` for missing hook entries (session-start, user-prompt-submit, stop, post-commit) |
| Stash/unstash problems | Check `entire-logs/entire.log` for stash-related log lines, `git-log.txt` for commit ordering |
| File-tracking issues | Check `checkpoint-metadata/` for `files_touched` and the session metadata's `files_touched` |
| Strategy mismatch | Check `entire-logs/entire.log` for `strategy` field, verify auto-commit vs manual-commit behavior |

### 5. Deep dive files

- **entire-logs/entire.log**: Structured JSON logs -- hook lifecycle, session phases (`active` -> `idle` -> `ended`), warnings, errors. Key fields: `component`, `hook`, `strategy`, `session_id`.
- **git-log.txt**: Commit graph showing main branch, `entire/checkpoints/v1`, checkpoint initialization.
- **git-tree.txt**: Files at HEAD vs checkpoint branch (separated by `--- entire/checkpoints/v1 ---`).
- **checkpoint-metadata/**: `metadata.json` has `checkpoint_id`, `strategy`, `files_touched`, `token_usage`, and `sessions` array. Session subdirs have per-session details.

### 6. Report findings

Identify whether the issue is in:
- **CLI hooks** (prepare-commit-msg, commit-msg, post-commit)
- **Session management** (phase transitions, session tracking)
- **Checkpoint creation** (branch management, metadata writing)
- **File tracking** (files touched, prompt correlation)
- **Strategy logic** (auto-commit vs manual-commit behavior)
