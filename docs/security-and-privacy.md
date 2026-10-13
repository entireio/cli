# Security & Privacy

Entire stores AI session transcripts and metadata in your git repository. This document explains what data is stored, how sensitive content is protected, and how to configure additional safeguards.

## Transcript Storage & Git History

### Where data is stored

When you use Entire with an AI agent (Claude Code, Codex, Antigravity, OpenCode, Cursor, Factory AI Droid, Copilot CLI, Pi), session transcripts, user prompts, and checkpoint metadata are committed to **your own git repository**. They stay out of your working branches' history, but they live in the same repo and travel with it.

Exactly where depends on the [checkpoint backend](architecture/ref-checkpoint-backend.md) the repo uses:

| Backend | Committed checkpoints land in | Notes |
|---|---|---|
| `git-refs` (default for new repos) | One ref per checkpoint: `refs/entire/checkpoints/<shard>/<id>` | Not a branch. Pushed individually, and fetched individually on demand |
| `git-branch` (legacy) | Subtrees of one long-lived branch, `entire/checkpoints/v1` | A branch, so it shows up in branch listings, CI branch triggers, and platform previews |

Which one a repo is on is recorded as `checkpoints.primary.type` in `.entire/settings.json` (or `settings.local.json`); an absent `checkpoints` block means `git-branch`. The redaction described in this document applies identically to both — the pipeline is shared, and only the destination differs. Where the distinction matters below, it is called out.

Between commits, Entire writes nothing to git. At the end of each agent turn it records the paths the agent touched and their git blob hashes in local session state (`.git/entire-sessions/`), and keeps the sanitized transcript in `.entire/metadata/<session>/full.jsonl` (see [Local copies of transcript content](#local-copies-of-transcript-content)). Older Entire versions instead wrote local **shadow branches** (`entire/<commit>-<worktree>`) at turn end, whose code-file snapshots were raw, unredacted blobs of your working tree. Current versions do not delete them automatically: `entire doctor` reports them and `entire doctor --force` deletes the `entire/<commit>-<worktree>` form without touching session state. Plain `entire clean` deletes them too, but also clears the state of every session based on HEAD, which is now the only record of pending agent work. `entire disable --uninstall` deletes the `entire/<commit>-<worktree>` form as well, and lists the oldest `entire/<commit>` form by name without deleting it (a user's own branch named like a short SHA matches it); `entire clean --all` deletes that form after you confirm. Deleting a branch leaves its objects unreachable until git prunes them: `git gc` reclaims the space once they are two weeks old (git's default `gc.pruneExpire`). `git gc --prune=now` frees it at once but drops every unreachable object, recently dropped stashes included, and must only run when no other git process is active.

**Antigravity title-tee (machine-global):** setting up the Antigravity agent installs `entire hooks antigravity title-tee` into agy's *global* settings (`~/.gemini/antigravity-cli/settings.json`), because agy exposes token usage only through its window-title/statusline feed. Once installed, the tee runs on every agy state change on the machine — including in repositories where Entire is not enabled — and persists **only** the conversation ID and token counts (`context_window` totals) to Entire's local cache directory; the rest of the payload is discarded and no prompt or file content is captured. Stale per-conversation snapshots are cleaned up after 14 days. Removing the Antigravity agent (`entire agent remove antigravity`) uninstalls the tee.

Anyone with access to your repository can read committed checkpoint data: the full prompt/response history and session metadata. Note that transcripts capture all tool interactions — including file contents, MCP server calls, and other data exchanged during the session. Per-checkpoint refs are less *visible* than a branch, but they are not less accessible: a `git fetch` of `refs/entire/checkpoints/*` reads them just as well.

If your repository is **public**, this data is visible to the entire internet.

### What Entire redacts automatically

Entire automatically scans transcript and metadata content before writing it to a git object. Five always-on secret detection methods plus a configurable scanner layer (pattern matching, method 2 below) run during condensation, plus a conditional seventh pass for user-defined secret rules (see [Customizing redaction](#customizing-redaction) below), and an opt-in eighth pass for PII (see [Optional PII redaction](#optional-pii-redaction) below):

1. **Entropy scoring** — Identifies high-entropy strings (Shannon entropy > 4.5) that look like randomly generated secrets, even if they don't match a known pattern. One exact shape is exempt from this layer only: a string that is, in its entirety, a Claude Code tool-use id (`toolu_01` plus 22 letters or digits). The other layers still scan it, and a secret attached to an id forms a longer string that is not exempt.
2. **Pattern matching** — Runs one or both configurable scanner engines against known secret formats: [Betterleaks](https://github.com/betterleaks/betterleaks) (default on) and/or [goredact](https://github.com/lastpersonlabs/goredact) (default off). See [Choosing secret-scanner engines](#choosing-secret-scanner-engines) below.
3. **Provider token prefixes** — Deterministically redacts known secret-key prefixes (e.g. Supabase `sb_secret_`, `sbp_`) regardless of entropy or surrounding context.
4. **Credentialed URI detection** — Redacts URLs with embedded passwords, such as `scheme://user:password@host`.
5. **Database connection-string detection** — Redacts JDBC, Postgres keyword DSN, SQL Server, and ODBC-style connection strings containing passwords.
6. **Bounded credential value detection** — Redacts password-like config values such as `DB_PASSWORD=...` and `PGPASSWORD=...` while preserving the surrounding key.

Detected secrets are replaced with `REDACTED` before the data is ever written to a git object. Of the six secret-detection passes above, the scanner layer (pass 2) is configurable — see [Choosing secret-scanner engines](#choosing-secret-scanner-engines) below — while the other five are **always on** and cannot be disabled. User-defined rules (inline `custom_redactions` and rule packs) add a seventh secret-detection pass that only runs when configured.

### What Entire does NOT understand: pasted images and screenshots

**Every layer described above — all eight passes, including the opt-in PII layer — reads transcript *text*. There is no OCR pass, no vision-model PII or secret scan, and no gate that holds an image back until someone reviews it. Nothing in Entire ever reads what an image depicts.**

What that means for an image you paste is **not uniform across agents**, because it depends on how the agent writes the image into its transcript. There are three outcomes, and only the first is an exposure:

| Agent | Default | With `redaction.externalize_images` on |
| --- | --- | --- |
| Claude Code | **Stored unredacted**, inline in the transcript as base64. The text scanner skips it (the `type: image` / `type: base64` skip rule below) rather than scanning it. | **Stored unredacted** as a raw binary blob under the checkpoint's `assets/` folder. |
| Codex | **Destroyed.** Codex writes images as `data:` URIs inside `image_url` and tool-output strings, which the skip rule does not match, so the entropy layer treats the base64 as a secret and replaces it. The stored transcript keeps the surrounding message; the image is gone. | **Stored unredacted** under `assets/` (externalization runs before redaction, which is what preserves it). |
| Cursor | **Not stored in the repository at all.** Cursor keeps images in its own per-session SQLite store, never in the transcript Entire reads. | **Stored unredacted** under `assets/`, captured from that store. |
| OpenCode, Copilot CLI, Factory Droid, Pi | Depends on the agent's own transcript shape; Entire has no image handling for these. Assume the Claude Code row unless you have checked. | Unchanged — the setting only affects the three agents above. |

**Do not treat the Codex row as a protection.** It is a side effect of a skip rule not matching a shape, not a deliberate safeguard: it destroys data you may want, it does not apply to the `assets/` path, and a change to either the rule or Codex's format would flip it to the exposure case without notice.

Where an image *is* stored, it is byte-for-byte what you pasted — completely unredacted — because byte-level regex and entropy redaction cannot inspect binary image data without corrupting it, so Entire does not attempt it. That is a deliberate design choice, not a bug.

Two things about *when* those bytes reach git:

- **A checkpoint's copy** lands on `entire/checkpoints/v1` (or the equivalent per-checkpoint ref on the `git-refs` backend). Like all checkpoint data it is written locally and pushed separately, so it reaches your remote only when checkpoint data is pushed — see the **Review before pushing** bullet under [Recommendations](#recommendations).
- **Before you commit**, the image exists only in the agent's own transcript and in Entire's local sanitized copy (`.entire/metadata/<session>/full.jsonl`); nothing is written to git at turn end.

**If you would not commit an image to your repository unredacted, do not paste it into an agent conversation.** This applies equally to a private repository — anyone with read access to checkpoint data can see it — and especially to a public one.

### Choosing secret-scanner engines

Pattern matching (layer 2 above) is served by two independent scanner engines, each of which can be turned on or off:

- **Betterleaks** — a broad rule-set auditor with several hundred built-in detectors for known secret formats (cloud providers, VCS platforms, payment processors, private key blocks, generic credentials, and more). Default: **on**.
- **goredact** — a streaming, validator-based scanner that checks a smaller set of provider/contextual token shapes against structural validators (e.g. checksum or length checks) rather than pure regex. Default: **off**.

Configure them under `redaction.betterleaks` / `redaction.goredact` in `.entire/settings.json`:

```json
"redaction": {
  "betterleaks": { "enabled": true },
  "goredact":   { "enabled": false }
}
```

Omitting either key, or the key's `enabled` field, keeps that engine at its default. All other redaction layers — entropy scoring, provider token prefixes, credentialed URI detection, connection-string detection, custom rules, bounded credential key/value detection, and PII — are unaffected by these two toggles; they keep running exactly as described elsewhere in this document regardless of which scanner engine(s) are selected.

**Fail-closed rules:**

- At least one scanner engine must be enabled. Setting both `betterleaks.enabled` and `goredact.enabled` to `false` is a settings error: Entire refuses to load the (merged) settings rather than run condensation with no pattern-matching coverage at all.
- Scanner selection is honored **only** from the committed `.entire/settings.json`. A `betterleaks` or `goredact` key present in `.entire/settings.local.json` is ignored, and Entire logs a warning naming the ignored key. This is deliberate: unlike most `settings.local.json` overrides, which are personal and don't affect anyone else, the scanner selection changes what gets redacted into checkpoints that every reader of the repository's history will see — so it has to be a team-visible, committed decision, not a per-developer one.
- Disabling Betterleaks narrows layer-2 coverage to whatever engine(s) remain enabled. The first time a hook or CLI command runs with Betterleaks disabled, Entire logs a one-time notice to that effect (printed on the terminal when stderr is a TTY; suppressed on subsequent runs via a marker file under `.entire/tmp/`).

**Runtime degradation:** if goredact is the only enabled scanner and it fails at runtime (a scan error, not a missing finding), Entire treats that as scanner degradation and fails the transcript write rather than persisting content that only received partial pattern-matching coverage. This is a deliberate fail-closed choice: with Betterleaks also enabled, a goredact failure degrades gracefully to Betterleaks-only coverage for that write; with Betterleaks disabled, there is no fallback engine left, so the write itself must fail instead of shipping under-scanned content.

**The coverage trade-off, honestly stated:** Betterleaks' several-hundred-rule set covers a long tail of structured, often low-entropy token formats that a smaller rule set would miss; goredact covers roughly 67 provider/contextual token shapes but checks each one with a dedicated validator, trading breadth for precision. Neither engine is a strict superset of the other — running both (the default plus opting into goredact) gives the widest coverage.

### Optional PII redaction

PII redaction is a separate, **opt-in** layer that runs in addition to the always-on secret detection. Disabled by default. Configured under `redaction.pii` in `.entire/settings.json` (team-shared) or `.entire/settings.local.json` (personal, gitignored).

Built-in categories (when `enabled` is `true`):

| Category | Default | Replacement token |
|---|---|---|
| `email` | on | `[REDACTED_EMAIL]` |
| `phone` (North American / NANP formats) | on | `[REDACTED_PHONE]` |
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

### Local copies of transcript content

Two local copies of transcript content are never redacted and never reach the remote:

`.entire/metadata/<session>/full.jsonl` is Entire's own local working copy of the transcript, written mode `0600`. It is *sanitized* (agent state that cannot be replayed out of a checkpoint is stripped) but **not redacted** — redaction happens on the way into a git object, not on this file. It is the input condensation reads from when the live agent transcript is no longer available.

The agent's own transcript is never modified at all. Entire reads from it and leaves it alone, because the agent is writing to it continuously and editing under the agent's feet would corrupt the session.

### Recommendations

If your AI sessions will touch sensitive data:

- **Use a private repository.** This is the simplest and most complete protection. Committed checkpoints are then only visible to collaborators.
- **Avoid passing sensitive files to your agent.** Content that never enters the agent conversation never appears in transcripts.
- **Never paste a screenshot or image containing secrets or personal data.** Nothing in Entire reads what an image depicts, and on most agents the image is stored unredacted — see [What Entire does NOT understand: pasted images and screenshots](#what-entire-does-not-understand-pasted-images-and-screenshots).
- **Review before pushing.** Checkpoints are written locally — at commit time, or when a session with no file changes is snapshotted with the hidden `entire checkpoint create` — and pushed separately, so there is always a window to inspect them:

  ```fish
  # git-refs: list local checkpoint refs, then read one
  git for-each-ref refs/entire/checkpoints
  entire checkpoint list
  entire checkpoint explain <id>

  # git-branch: inspect the branch directly
  git log --oneline entire/checkpoints/v1
  ```

- **Know your push destination.** Checkpoint data syncs to exactly one elected remote; `entire status` names it and reports how many checkpoints are still unpushed.

## What Gets Redacted

### Secrets (always on)

Secret detection as a whole is always on, though the pattern-matching scanner layer within it is configurable per [Choosing secret-scanner engines](#choosing-secret-scanner-engines). Betterleaks pattern matching covers cloud providers (AWS, GCP, Azure), version control platforms (GitHub, GitLab, Bitbucket), payment processors (Stripe, Square), communication tools (Slack, Discord, Twilio), private key blocks (RSA, DSA, EC, PGP, OpenSSH), and generic credentials (bearer tokens, basic auth, JWTs). Dedicated credentialed URI detection covers URLs that embed passwords. Additional database connection-string detection covers DB DSNs and query-parameter passwords not reliably covered by generic secret rules. Entropy scoring catches secrets that don't match any known pattern.

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
- **JSONL skip rules.** Entire skips scanning fields whose name *ends in* `signature` (so `thinkingSignature` too), fields ending in `id`/`ids`, and structural-path fields (`filepath`, `file_path`, `cwd`, `root`, `directory`, `dir`, `path`) to avoid false positives. Objects whose `type` starts with `image` or equals `base64` are also skipped — and that skip is what leaves a pasted image unredacted rather than merely unflagged. It matches some agents' image shapes and not others, which is why the outcome differs per agent: see [What Entire does NOT understand: pasted images and screenshots](#what-entire-does-not-understand-pasted-images-and-screenshots) above.
- **Built-in PII patterns are US-centric.** `phone` matches North American (NANP) formats only — international formats, including E.164 numbers outside `+1`, are not detected. `address` matches the street line only; city, state, and ZIP/postcode are preserved. If you handle personal data from other regions, add `custom_patterns` for your locale rather than relying on the built-in categories alone.
- **Custom PII patterns are user-authored.** Teams own the correctness of their `custom_patterns`. An invalid regex is logged and skipped, not enforced.
- **Users are ultimately responsible** for reviewing what they commit and push. Redaction is a safety net, not a guarantee.

## Telemetry

The CLI captures anonymous usage analytics by default. Sent to PostHog with `DisableGeoIP` enabled. Captured per command: command name, selected agent, whether Entire is enabled in the repo, CLI version, OS/arch, installed git version (best-effort; omitted if git is absent or unparseable), and **names** of flags passed (never their values). The distinct ID is a hashed machine identifier (`machineid.ProtectedID`), not a user identity.

Not captured: flag values, prompt text, transcripts, file paths, repository identifiers, GitHub usernames, source code.

Opt out via any one of:

- `--telemetry=false` on a command that accepts it.
- `"telemetry": false` in `.entire/settings.json` or `.entire/settings.local.json`.
- `ENTIRE_TELEMETRY_OPTOUT=1` in the environment.

## Why `external_agents` is local-only

`external_agents` turns on the `$PATH` scan that looks for `entire-agent-*`
binaries and runs each one's `info` subcommand, then keeps running the ones it
registered for every hook thereafter. It is an execution grant, not a
preference, so it is honored only when it is genuinely developer-owned: only
from `.entire/settings.local.json`, and only when that file is untracked in both
the index and `HEAD`. Reading it from the committed `.entire/settings.json` would
let an ordinary pull request turn on execution of whatever `entire-agent-*`
binary it could get onto a developer's `$PATH`, and one line of JSON does not
read as executable to a reviewer. There is no prompt in front of this one at all.

The untracked check matters because the filename alone proves nothing:
`.gitignore` does not apply to a path that is already tracked, so
`git add -f .entire/settings.local.json` commits it and a fresh clone
materializes it with the committed content. A tracked
`.entire/settings.local.json` is ignored in its entirety, because it is not
local to your clone — it arrives with the repository and would override project
settings for everyone. Entire warns on stderr and tells you to run
`git rm --cached .entire/settings.local.json`. The load still succeeds using
project settings, so a committed file cannot brick the repository.

Rejection is a downgrade, never an error: discovery simply does not run.
`entire status` names the setting and where it has to move, and the same reason
is logged. The interactive setup flows (`entire enable`, `entire configure`,
`entire agent add`, `entire plugin uninstall`) reach external agents regardless
of the setting, so the remedy stays available from the commands that need it —
and when one of them enables an external agent for you, it writes the setting to
`.entire/settings.local.json`, which is where it takes effect.

Every `$PATH` scanner in the CLI also drops non-absolute entries. A relative
entry resolves against the process's working directory, which for a git hook is
whatever repository the caller was standing in, so a file committed to that
repository would otherwise be a binary Entire executes.

## Why agent instruction fields are local-only

Every review `prompt` (per-agent and judge, in
`review_profiles` and the legacy `review` map) and every review profile's
`task` are placed verbatim in the prompts of agents that
`entire review` spawns with approval checks disabled (claude-code's
`bypassPermissions`, codex's `--dangerously-bypass-approvals-and-sandbox`). The
prompt is the stated control for those spawns, so whoever writes these strings
gets the last word in it. `task` and `prompt` are adjacent sections of the same
composed prompt, so they are gated together — a gate on one alone would just
move the attacker's text to the other field. Honoring any of them from the
committed `.entire/settings.json` would let an ordinary pull request steer an
approvals-disabled agent on every developer who pulls — the same delivery route
as [`external_agents`](#why-external_agents-is-local-only), carrying
instructions instead of an execution grant.

They are therefore honored only from layers that are this developer's own:
clone-local review preferences (stored inside `.git/`, which a clone never
populates) or `.entire/settings.local.json` verified untracked in both the
index and `HEAD`. Rejection is a downgrade, never an error — a dropped task
falls back to the built-in text for conventional profile names, and
`entire review` prints a one-line notice naming the
dropped field and where it has to move (suppressed when the dropped task equals
the built-in default, since that drop changes nothing).

The `entire-investigate` plugin applies the same reasoning to its own
`always_prompt`, against its own `.entire/investigate.local.json`. That file
has no committed counterpart at all, so there is no layer to gate — only a
check that the file itself is untracked. It is outside this repository's
control; see the plugin's `internal/config`.

Deliberately not gated: `skills` (review validates every configured skill
against the locally installed set before spawning, so free text there fails the
run rather than reaching an agent), `agent` and `model` (registry keys and
routing hints, not instruction text), and `review_default_profile` (it only
selects among profiles whose instruction content is itself gated).

## Reporting a vulnerability

For vulnerability disclosure, see [SECURITY.md](../SECURITY.md) at the repo root: email `security@entire.io`, expect acknowledgment within 48 hours and resolution of criticals within 90 days.

## Related

- [Checkpoint commit signing](architecture/checkpoint-signing.md) — best-effort GPG/SSH signing of checkpoint commits, opt-out via `sign_checkpoint_commits: false`.
- External agent plugins are arbitrary executables on `$PATH` invoked by the CLI; only install plugins you trust. Discovery is off unless you turn it on in `.entire/settings.local.json` — see [Why `external_agents` is local-only](#why-external_agents-is-local-only).
