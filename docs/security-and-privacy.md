# Security & Privacy

Entire stores AI session transcripts and metadata in your git repository. This document explains what data is stored, how sensitive content is protected, and how to configure additional safeguards.

## Transcript Storage & Git History

### Where data is stored

When you use Entire with an AI agent (Claude Code, Codex, Gemini CLI, OpenCode, Cursor, Factory AI Droid, Copilot CLI, Pi), session transcripts, user prompts, and checkpoint metadata are committed to a dedicated branch in your git repository (`entire/checkpoints/v1`). This branch is separate from your working branches, your code commits stay clean, but it lives in the same repository.

Entire also creates temporary local branches (e.g., `entire/<short-hash>`) as working storage during a session. Metadata written to these shadow branches — transcripts, prompts, incremental checkpoint data, subagent transcripts — goes through the same redaction pipeline as `entire/checkpoints/v1`. **Code-file snapshots, however, are written as raw blobs of your working tree without redaction**, so any hardcoded secrets in your source code would appear unredacted on the shadow branch. Gitignored files (e.g., `.env`) are filtered out of these snapshots as a partial defense. Shadow branches are **not** pushed by Entire; do not push them manually, because unredacted source content would be visible on the remote. They are cleaned up when session data is condensed into `entire/checkpoints/v1` at commit time.

Anyone with access to your repository can view the transcript data on the `entire/checkpoints/v1` branch. This includes the full prompt/response history and session metadata. Note that transcripts capture all tool interactions — including file contents, MCP server calls, and other data exchanged during the session.

If your repository is **public**, this data is visible to the entire internet.

### What Entire redacts automatically

Entire automatically scans transcript and metadata content before writing it to the `entire/checkpoints/v1` branch. Five always-on secret detection methods run during condensation, plus a conditional sixth pass for user-defined secret rules (see [Customizing redaction](#customizing-redaction) below), an opt-in seventh pass for PII (see [Optional PII redaction](#optional-pii-redaction) below), and an opt-in eighth pass that shells out to the OpenAI Privacy Filter model (see [Optional OpenAI Privacy Filter](#optional-openai-privacy-filter-opf) below):

1. **Entropy scoring** — Identifies high-entropy strings (Shannon entropy > 4.5) that look like randomly generated secrets, even if they don't match a known pattern.
2. **Pattern matching** — Uses [Betterleaks](https://github.com/betterleaks/betterleaks) built-in rules to detect known secret formats.
3. **Credentialed URI detection** — Redacts URLs with embedded passwords, such as `scheme://user:password@host`.
4. **Database connection-string detection** — Redacts JDBC, Postgres keyword DSN, SQL Server, and ODBC-style connection strings containing passwords.
5. **Bounded credential value detection** — Redacts password-like config values such as `DB_PASSWORD=...` and `PGPASSWORD=...` while preserving the surrounding key.

Detected secrets are replaced with `REDACTED` before the data is ever written to a git object. The five secret-detection passes above are **always on** and cannot be disabled. User-defined rules (inline `custom_redactions` and rule packs) add a sixth secret-detection pass that only runs when configured.

### Optional PII redaction

PII redaction is a separate, **opt-in** layer that runs in addition to the always-on secret detection. Disabled by default. Configured under `redaction.pii` in `.entire/settings.json` (team-shared) or `.entire/settings.local.json` (personal, gitignored).

Built-in categories (when `enabled` is `true`):

| Category | Default | Replacement token |
|---|---|---|
| `email` | on | `[REDACTED_EMAIL]` |
| `phone` | on | `[REDACTED_PHONE]` |
| `address` (US street addresses) | off (more false-positive prone) | `[REDACTED_ADDRESS]` |

Common bot/CI email addresses are not redacted (`noreply@*`, `actions@*`, `*@users.noreply.github.com`, `*@noreply.github.com`).

Teams can add their own regex patterns via `custom_patterns`. Each key is a label (uppercased in the replacement token), each value is a regex string. Example: `{"employee_id": "EMP-\\d{6}"}` produces `[REDACTED_EMPLOYEE_ID]`.

```json
{
  "redaction": {
    "pii": {
      "enabled": true,
      "email": true,
      "phone": true,
      "address": false,
      "custom_patterns": {
        "employee_id": "EMP-\\d{6}"
      }
    }
  }
}
```

If a custom pattern itself reveals sensitive structure (e.g. an internal ID format), put it in `.entire/settings.local.json` (gitignored) instead of `.entire/settings.json`.

### Optional OpenAI Privacy Filter (`opf`)

A separate, **opt-in** layer that shells out to the [OpenAI Privacy Filter](https://github.com/openai/privacy-filter) (`opf`) — a 1.5B-parameter token-classification model that finds names, emails, phone numbers, addresses, dates, URLs, account numbers, and secrets that pure regex can miss. Disabled by default. Runs *in addition to* the seven built-in layers, **only at push time** — never per-turn and never at commit time. Local commits stay on the fast 7-layer pipeline so per-commit latency is unchanged; OPF only re-redacts checkpoints right before they leave the machine via `git push`.

Prerequisites:

```bash
pip install opf
```

Verify `opf --help` works; the CLI defaults to resolving the binary via `$PATH`. Override with the `command` setting if you need a specific path.

Enable in `.entire/settings.json`:

```json
{
  "redaction": {
    "openai_privacy_filter": {
      "enabled": true,
      "categories": {
        "private_person": true
      }
    }
  }
}
```

Available categories (set to `true` to enable, `false` or omit to skip):

| Category | Replacement token | Notes |
|---|---|---|
| `private_person` | `[REDACTED_PERSON]` | Person names |
| `private_email` | `[REDACTED_EMAIL]` | Email addresses |
| `private_phone` | `[REDACTED_PHONE]` | Phone numbers |
| `private_address` | `[REDACTED_ADDRESS]` | Street addresses |
| `private_url` | `[REDACTED_URL]` | URLs that may identify a person/account |
| `private_date` | `[REDACTED_DATE]` | Dates (DOB, etc.) |
| `account_number` | `[REDACTED_ACCOUNT_NUMBER]` | Account / card / SSN-shaped numbers |
| `secret` | `REDACTED` | Generic credential-shaped values |

Unknown category names are rejected at settings load time so typos surface immediately instead of silently disabling a category.

Full settings reference:

```json
{
  "redaction": {
    "openai_privacy_filter": {
      "enabled": true,
      "categories": {
        "private_person": true,
        "private_email": true,
        "private_phone": true,
        "private_address": false,
        "private_url": false,
        "private_date": false,
        "account_number": false,
        "secret": false
      },
      "command": "opf",
      "timeout_seconds": 30
    }
  }
}
```

- `command` — path or PATH-resolvable name of the `opf` binary. Defaults to `opf`.
- `timeout_seconds` — per-invocation timeout. Defaults to `30`.
- `prompt_default` — `"ask"` (default), `"never"`, or `"always"`. Controls whether the pre-push hook surfaces an interactive prompt before running OPF. `ENTIRE_OPF=yes` or `ENTIRE_OPF=no` on a single `git push` invocation overrides this for that push only.

The interactive prompt offers three options and reacts to **Ctrl-C** for cancellation:

```
Run OpenAI Privacy Filter on these checkpoints?
Adds ~30s but redacts names/PII the regex layers can't catch.
Ctrl-C to cancel the push.

  ▸ Yes — run OPF this push
    No — skip OPF, push as-is
    Always — run OPF on every push from now on
```

- **Yes** runs OPF for this push only.
- **No** skips OPF for this push only; the 7-layer-redacted content reaches the remote.
- **Always** runs OPF this push AND writes `prompt_default: "always"` to `.entire/settings.local.json` so future pushes don't ask.
- **Ctrl-C** aborts the push entirely — `git push` exits non-zero, no refs go to the remote.

Non-interactive contexts (CI, scripted pipes with no TTY) skip the prompt and run OPF automatically when enabled, printing `→ OpenAI Privacy Filter: scanning checkpoints…` to stderr so the wait isn't silent. Set `ENTIRE_OPF=no` to skip OPF in those contexts without disabling the feature globally.

**CI consideration**: if you've enabled OPF locally and your CI runs `git push` (e.g. an agent-driven workflow), the CI push will attempt to run OPF too. If the `opf` binary isn't installed in CI, the push will abort with `OPFRuntimeFailedError` rather than silently shipping under-redacted content — by design, since "I enabled OPF" should mean "no content leaves my machines without OPF." The remedies are (a) install `opf` in CI, (b) set `ENTIRE_OPF=no` for CI pushes, or (c) set `prompt_default: "never"` if you only want OPF on interactive pushes.

OPF failures at push time are **fail-closed**: if OPF is not on PATH, fails to start, or times out during the pre-push rewrite, the per-process circuit breaker trips and the rewrite aborts the push with `OPF runtime failed; aborting push`. Nothing reaches the remote. The intent is that "the user enabled OPF" means "I do not want unredacted content leaving this machine" — falling back to 7-layer silently on the push path would violate that contract. Fix the install or set `ENTIRE_OPF=no` for a one-off push.

(The circuit breaker is per-process, so a broken install costs one warning instead of one timeout per blob — but the push still aborts.)

Cost note: each shell-out loads the OPF model (~1.5B parameters on CPU). The pre-push rewrite batches **every redactable leaf across every unpushed v1 commit** into a single inference pass, so a typical real-world push pays the model-load cost once (~6s) plus inference (~5s per 100KB of leaf content) — not multiplied by the number of commits or blobs. A 3-commit push with ~250KB of total prose content runs in ~12–15s, not the ~50–100s a per-blob flow would take. Per-commit latency is unaffected because OPF doesn't run at commit time.

#### When OPF actually runs

OPF execution lives in the pre-push hook. The flow:

1. **Post-commit** writes the checkpoint with **7-layer-only** redaction to your local `entire/checkpoints/v1` branch. Fast, predictable, no OPF cost on the hot path.
2. **Pre-push** (`git push`): if OPF is enabled, the hook re-reads each unpushed `entire/checkpoints/v1` commit, runs the OpenAI Privacy Filter over its blobs to add the categories the regex layers don't catch (person names, addresses, etc.), and builds **new commits** carrying an `Entire-OPF-Applied: true` trailer. The local v1 ref fast-forwards atomically to the new tip, and the (now 8-layer-redacted) commits are what get pushed.
3. The original 7-layer-only commits become **unreachable** in the local git object database and eventually get swept by `git gc`.

This means:

- **The remote only ever sees 8-layer-redacted content** when OPF is enabled.
- **Local-only commits are 7-layer-redacted** until the moment you push. If you never push, OPF never runs.
- **Re-running pre-push is idempotent** — commits already carrying the trailer get re-parented into the chain but are not re-redacted.

#### Force-pushed remote, bootstrap, and concurrent pushes

The rewrite refuses to proceed and aborts the push when it detects a divergent state, so checkpoints on the remote are never silently rebased:

- **Diverged remote**: if local `entire/checkpoints/v1` has commits that aren't ancestors of `<remote>/entire/checkpoints/v1`, the hook exits with a `entire/checkpoints/v1 has diverged from remote` error. Fetch the remote and either reset local v1 to the remote tip or resolve manually before pushing.
- **Bootstrap** (remote has no `entire/checkpoints/v1` yet, e.g. first push): the cap is `100` unpushed commits by default. Override via the env var on the push invocation:

  ```fish
  set -x ENTIRE_OPF_BOOTSTRAP_LIMIT 500; git push
  # or fully unbounded:
  set -x ENTIRE_OPF_BOOTSTRAP_LIMIT unlimited; git push
  ```

- **Batch-size cap**: independent of commit count, the rewrite refuses pushes whose cumulative prose-leaf content exceeds `ENTIRE_OPF_BATCH_LIMIT` bytes (default: 2 MiB ≈ ~110s of inference). The two caps protect different failure modes — the bootstrap cap stops "100 throwaway commits", the batch cap stops "one commit with 50 MB of pasted transcript". An `OPF would run inference on …` error means you've hit this; bump the env var or push without OPF for that push:

  ```fish
  set -x ENTIRE_OPF_BATCH_LIMIT 10485760; git push   # 10 MiB
  # or fully unbounded:
  set -x ENTIRE_OPF_BATCH_LIMIT unlimited; git push
  ```

- **Concurrent push** from another worktree: the rewrite uses a CAS to update the local v1 ref. If another process moved the ref while OPF was running, the hook exits with a "concurrent push detected" error and `git push` aborts the whole batch. Fetch and retry.

#### Persistence of un-redacted-by-OPF content

Three places retain content that OPF *didn't* redact, with different lifetimes. Understanding them matters if your threat model goes beyond "what reaches the remote":

| Location | Redaction level | Lifetime | Reaches remote? |
|---|---|---|---|
| `.entire/<session>.jsonl` | **None — raw** | Until session is deleted (managed by the agent) | No |
| Shadow branch `entire/<commit>-<worktree>` | 7-layer | Auto-deleted after the next successful push (only when its session has ended cleanly) | No |
| Unreachable git objects after pre-push rewrite | 7-layer | Until `git gc --prune` (default `gc.pruneExpire` is 2 weeks) | No |
| Reflog `git reflog show entire/checkpoints/v1` | 7-layer tips | Default `gc.reflogExpire` is 90 days | No |
| `<remote>/entire/checkpoints/v1` | 8-layer (after OPF rewrite) | Until you delete the branch on the remote | Yes |

The `.entire/<session>.jsonl` files are raw working state owned by the agent (Claude Code, etc.) — Entire reads from them but does not redact them in place, because the agent is reading and writing them continuously and editing under the agent's feet would corrupt the session.

To aggressively scrub the unreachable git objects from the pre-push rewrite (instead of waiting for the 2-week GC window):

```fish
git reflog expire --expire-unreachable=now refs/heads/entire/checkpoints/v1
git gc --prune=now
```

This is I/O-heavy on large repositories; it's not run automatically. If you want it as part of your push workflow, wrap `git push` in a script that invokes it after a successful push.

Verifying it's working:

```fish
# After enabling OPF, run an agent turn that includes a name in the prompt,
# e.g. "Create notes.txt with: Alice Johnson reviewed the proposal."
# Commit (this stays on the fast 7-layer pipeline), then push. OPF runs
# during the pre-push step:
git commit -m "demo"
git push   # → "→ OpenAI Privacy Filter: scanning N checkpoints (~30s)…"
git log --oneline entire/checkpoints/v1 | head -2
entire checkpoint explain HEAD | grep -i 'REDACTED_PERSON'
```

If `[REDACTED_PERSON]` appears in the prompt or transcript section and the latest `entire/checkpoints/v1` commit carries `Entire-OPF-Applied: true`, OPF is active.

### Recommendations

If your AI sessions will touch sensitive data:

- **Use a private repository.** This is the simplest and most complete protection. Transcripts on `entire/checkpoints/v1` are only visible to collaborators.
- **Avoid passing sensitive files to your agent.** Content that never enters the agent conversation never appears in transcripts.
- **Review before pushing.** You can inspect the `entire/checkpoints/v1` branch locally before pushing it to a remote.

## What Gets Redacted

### Secrets (always on)

Betterleaks pattern matching covers cloud providers (AWS, GCP, Azure), version control platforms (GitHub, GitLab, Bitbucket), payment processors (Stripe, Square), communication tools (Slack, Discord, Twilio), private key blocks (RSA, DSA, EC, PGP, OpenSSH), and generic credentials (bearer tokens, basic auth, JWTs). Dedicated credentialed URI detection covers URLs that embed passwords. Additional database connection-string detection covers DB DSNs and query-parameter passwords not reliably covered by generic secret rules. Entropy scoring catches secrets that don't match any known pattern.

All detected secrets are replaced with `REDACTED`. PII matches are replaced with category-tagged tokens like `[REDACTED_EMAIL]` (see [Optional PII redaction](#optional-pii-redaction)).

To reduce over-redaction, Entire preserves structural transcript fields such as IDs and paths, leaves placeholder values alone, and redacts only credential values for bounded key/value forms. Placeholders are detected by exact match (e.g. `changeme`, `example`, `placeholder`, `your_password`, `your_secret`, prior `REDACTED`/`[REDACTED]`/`<REDACTED>` markers) or by shape: shell expansions like `${DB_PASSWORD}`, bracketed names like `<password>` or `<your-db-password>`, and mask runs of three or more `*`/`x`/`.`/`-` (so `***`, `xxxx`, `....`, `----` all match). When a connection string contains a real password, it is redacted as a unit because partial fragments can still expose sensitive material; connection strings whose passwords are placeholders are left intact.

## Customizing redaction

The built-in detectors handle well-known secret formats. For anything else you don't want stored in transcripts — internal credential shapes the bundled scanners don't know about, project codenames, or specific words and phrases you'd rather keep out of session archives — Entire offers two extension surfaces. Both run plain regex matching against transcript content, so the rules can target any string pattern, not just credentials. Both feed the same engine and run as their own layer between connection-string detection and bounded credential KV detection.

### Surface 1: Inline `redaction.custom_redactions`

Add a label → regex map under `redaction.custom_redactions` in `.entire/settings.json`:

```json
{
  "redaction": {
    "custom_redactions": {
      "acme_token":  "ACME_TOKEN_[A-Za-z0-9]{20,}",
      "internal_id": "INTERNAL_[a-z]{6}_[0-9]{4}"
    }
  }
}
```

- The label is for diagnostics only; matches are replaced with the bare `REDACTED` token (matching the built-in secret layers, not the `[REDACTED_<LABEL>]` token used for PII).
- Regexes follow [Go's RE2 syntax](https://pkg.go.dev/regexp/syntax). No lookarounds, no backreferences.
- A failed compile is logged once at startup and the rule is skipped — it will never crash the redactor.
- Override in `.entire/settings.local.json` for personal additions; entries merge per-key (override replaces the same key, leaves other keys intact).

### Surface 2: Rule packs

Drop a YAML or JSON file into `.entire/redactors/`:

```yaml
# .entire/redactors/acme-internal.yaml
name: acme-internal              # MUST match the filename stem
version: 1.0.0
description: Internal ACME service tokens
rules:
  - id: acme-token
    description: Long-lived ACME service tokens
    regex: 'ACME_TOKEN_[A-Za-z0-9]{20,}'
    samples:
      - { input: "key=ACME_TOKEN_abc123def456ghi789jkl", redacted: true  }
      - { input: "ACME_TOKEN_short",                     redacted: false }
  - id: acme-session
    regex: 'asess_[a-f0-9]{32}'
```

Equivalent JSON form:

```json
{
  "name": "acme-internal",
  "version": "1.0.0",
  "rules": [
    {
      "id": "acme-token",
      "regex": "ACME_TOKEN_[A-Za-z0-9]{20,}",
      "samples": [
        { "input": "key=ACME_TOKEN_abc123def456ghi789jkl", "redacted": true  },
        { "input": "ACME_TOKEN_short",                     "redacted": false }
      ]
    }
  ]
}
```

**Required fields:** `name` (must equal the filename stem — `acme-internal.yaml` → `acme-internal`), `version` (any string; semver recommended), and `rules[]` (at least one entry, each with `id` and `regex`).

**Optional fields:** `description` (pack-level and rule-level), and `rules[].samples[]` (see "Self-tests" below).

A pack does not have to target credentials. The same shape works for any string pattern you don't want stored in transcripts — for example, a project codename or a small word list:

```yaml
# .entire/redactors/local/private-words.yaml
name: private-words
version: 1.0.0
description: Project codenames and personal words to keep out of transcripts
rules:
  - id: codename-falcon
    description: Internal project codename
    regex: '(?i)\bproject[- ]?falcon\b'
    samples:
      - { input: "rolling out Project Falcon next week", redacted: true  }
      - { input: "the falcon flew over",                  redacted: false }
  - id: personal-words
    description: Words I'd rather not see archived
    regex: '(?i)\b(word_one|word_two)\b'
```

Putting personal lists under `.entire/redactors/local/` keeps them out of team commits (see "Distribution" below).

### Self-tests via `samples[]`

Each rule may declare an array of `{input, redacted}` pairs. On the next process startup after editing the pack, Entire runs each sample and emits a `slog.Warn` for any mismatch:

```
WARN  redactor pack sample mismatch  pack=.entire/redactors/acme-internal.yaml
      rule=acme-token sample_index=0 sample_length=42 expected=true got=false
```

A failing sample never disables the rule — sample validation is informational. Use it to catch typos and false positives before they ship.

### Distribution

- **Within a team:** commit `.entire/settings.json` and/or `.entire/redactors/*` to your repo. Teammates pull and the rules apply.
- **Across teams:** copy the pack file or share a link to a gist; recipients drop the file into their `.entire/redactors/`.
- **Personal-only:** put the file in `.entire/redactors/local/` — `entire enable` writes that path into `.entire/.gitignore` so personal rules don't pollute team commits.

### When to write a rule vs. file an issue

Write a rule for:

- Internal service tokens (`ACME_*`, `INTERNAL_*`) and custom env-var prefixes the bundled detectors don't know about.
- Project-specific session formats.
- Project codenames or other identifiers you don't want stored in transcripts.
- Specific words or phrases you'd rather keep out of session archives.

File an issue when the rule would benefit every Entire user (e.g., a major SaaS issued a new token format), when a built-in is producing false positives on common idioms in your codebase, or when a built-in is *not* catching a well-known shared format (we'd rather fix the built-in than have everyone ship the same custom rule).

### Troubleshooting

- **My rule doesn't redact anything.** Warnings about invalid patterns or sample mismatches are emitted by the redaction layer when Entire initializes it. In the hook path (where checkpoints are actually written) these go to `.entire/logs/entire.log` — `grep component=redaction` and look for lines mentioning your label or pack path. When a hard pack-discovery failure happens during an interactive command, Entire also prints a one-line breadcrumb on stderr pointing back at the log.
- **My pack file is silently ignored.** Filenames must end in `.yaml`, `.yml`, or `.json`. Other extensions are skipped.
- **I want to disable a rule temporarily.** Comment it out (prefix the YAML key with `#`) or remove the entry from `custom_redactions`. The rule reloads on the next CLI invocation.

## Limitations

- **Best-effort.** Novel or low-entropy secrets (short passwords, predictable tokens) may not be caught.
- **Filenames and binary data.** Secrets in filenames, binary files, or deeply nested structures may not be detected.
- **JSONL skip rules.** Entire skips scanning fields named `signature`, fields ending in `id`/`ids`, structural-path fields (`filepath`, `file_path`, `cwd`, `root`, `directory`, `dir`, `path`), and objects whose `type` starts with `image` or equals `base64` — all to avoid false positives.
- **Custom PII patterns are user-authored.** Teams own the correctness of their `custom_patterns`. An invalid regex is logged and skipped, not enforced.
- **Users are ultimately responsible** for reviewing what they commit and push. Redaction is a safety net, not a guarantee.

## Telemetry

The CLI captures anonymous usage analytics by default. Sent to PostHog with `DisableGeoIP` enabled. Captured per command: command name, selected agent, whether Entire is enabled in the repo, CLI version, OS/arch, and **names** of flags passed (never their values). The distinct ID is a hashed machine identifier (`machineid.ProtectedID`), not a user identity.

Not captured: flag values, prompt text, transcripts, file paths, repository identifiers, GitHub usernames, source code.

Opt out via any one of:

- `--telemetry=false` on a command that accepts it.
- `"telemetry": false` in `.entire/settings.json` or `.entire/settings.local.json`.
- `ENTIRE_TELEMETRY_OPTOUT=1` in the environment.

## Reporting a vulnerability

For vulnerability disclosure, see [SECURITY.md](../SECURITY.md) at the repo root: email `security@entire.io`, expect acknowledgment within 48 hours and resolution of criticals within 90 days.

## Related

- [Checkpoint commit signing](architecture/checkpoint-signing.md) — best-effort GPG/SSH signing of checkpoint commits, opt-out via `sign_checkpoint_commits: false`.
- External agent plugins are arbitrary executables on `$PATH` invoked by the CLI; only install plugins you trust.
