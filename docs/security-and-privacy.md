# Security & Privacy

Entire stores AI session transcripts and metadata in your git repository. This document explains what data is stored, how sensitive content is protected, and how to configure additional safeguards.

## Transcript Storage & Git History

### Where data is stored

When you use Entire with an AI agent (Claude Code, Codex, Gemini CLI, OpenCode, Cursor, Factory AI Droid, Copilot CLI), session transcripts, user prompts, and checkpoint metadata are committed to a dedicated branch in your git repository (`entire/checkpoints/v1`). This branch is separate from your working branches, your code commits stay clean, but it lives in the same repository.

Entire also creates temporary local branches (e.g., `entire/<short-hash>`) as working storage during a session. Metadata written to these shadow branches — transcripts, prompts, incremental checkpoint data, subagent transcripts — goes through the same redaction pipeline as `entire/checkpoints/v1`. **Code-file snapshots, however, are written as raw blobs of your working tree without redaction**, so any hardcoded secrets in your source code would appear unredacted on the shadow branch. Gitignored files (e.g., `.env`) are filtered out of these snapshots as a partial defense. Shadow branches are **not** pushed by Entire; do not push them manually, because unredacted source content would be visible on the remote. They are cleaned up when session data is condensed into `entire/checkpoints/v1` at commit time.

Anyone with access to your repository can view the transcript data on the `entire/checkpoints/v1` branch. This includes the full prompt/response history and session metadata. Note that transcripts capture all tool interactions — including file contents, MCP server calls, and other data exchanged during the session.

If your repository is **public**, this data is visible to the entire internet.

### What Entire redacts automatically

Entire automatically scans transcript and metadata content before writing it to the `entire/checkpoints/v1` branch. Five secret detection methods run during condensation, plus an opt-in sixth pass for PII (see [Optional PII redaction](#optional-pii-redaction) below):

1. **Entropy scoring** — Identifies high-entropy strings (Shannon entropy > 4.5) that look like randomly generated secrets, even if they don't match a known pattern.
2. **Pattern matching** — Uses [Betterleaks](https://github.com/betterleaks/betterleaks) built-in rules to detect known secret formats.
3. **Credentialed URI detection** — Redacts URLs with embedded passwords, such as `scheme://user:password@host`.
4. **Database connection-string detection** — Redacts JDBC, Postgres keyword DSN, SQL Server, and ODBC-style connection strings containing passwords.
5. **Bounded credential value detection** — Redacts password-like config values such as `DB_PASSWORD=...` and `PGPASSWORD=...` while preserving the surrounding key.

Detected secrets are replaced with `REDACTED` before the data is ever written to a git object. The five secret-detection passes are **always on** and cannot be disabled.

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
