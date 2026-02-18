# Security & Privacy

Entire stores AI session transcripts and metadata in your git repository. This document explains what data is stored, how sensitive content is protected, and how to configure additional safeguards.

## Transcript Storage & Git History

### Where data is stored

When you use Entire with an AI agent (Claude Code, Gemini CLI), session transcripts, user prompts, and checkpoint metadata are committed to a dedicated branch in your git repository (`entire/checkpoints/v1`). This branch is separate from your working branches, your code commits stay clean, but it lives in the same repository.

Entire also creates temporary local branches (e.g., `entire/<short-hash>`) as working storage during a session. These shadow branches store file snapshots and transcripts **without redaction**. They are cleaned up when session data is condensed (with redaction) into `entire/checkpoints/v1` at commit time. Shadow branches are **not** pushed by Entire — do not push them manually, as unredacted content would be visible on the remote.

Anyone with access to your repository can view the transcript data on the `entire/checkpoints/v1` branch. This includes the full prompt/response history and session metadata. Note that transcripts capture all tool interactions — including file contents, MCP server calls, and other data exchanged during the session.

If your repository is **public**, this data is visible to the entire internet.

### What Entire redacts automatically

Entire automatically scans transcript and metadata content before writing it to the `entire/checkpoints/v1` branch. Two detection methods run during condensation:

1. **Entropy scoring** — Identifies high-entropy strings (Shannon entropy > 4.5) that look like randomly generated secrets, even if they don't match a known pattern.
2. **Pattern matching** — Uses [gitleaks](https://github.com/gitleaks/gitleaks) built-in rules to detect known secret formats.

Detected secrets are replaced with `REDACTED` before the data is ever written to a git object. This is **always on** and cannot be disabled.

### Recommendations

If your AI sessions will touch sensitive data:

- **Use a private repository.** This is the simplest and most complete protection. Transcripts on `entire/checkpoints/v1` are only visible to collaborators.
- **Avoid passing sensitive files to your agent.** Content that never enters the agent conversation never appears in transcripts.
- **Review before pushing.** You can inspect the `entire/checkpoints/v1` branch locally before pushing it to a remote.

## What Gets Redacted

### Secrets (always on)

Gitleaks pattern matching covers cloud providers (AWS, GCP, Azure), version control platforms (GitHub, GitLab, Bitbucket), payment processors (Stripe, Square), communication tools (Slack, Discord, Twilio), database connection strings, private key blocks (RSA, DSA, EC, PGP), and generic credentials (bearer tokens, basic auth, JWTs). Entropy scoring catches secrets that don't match any known pattern.

All detected secrets are replaced with `REDACTED`.

## Limitations

- **Best-effort.** Novel or low-entropy secrets (short passwords, predictable tokens) may not be caught.
- **Filenames and binary data.** Secrets in filenames, binary files, or deeply nested structures may not be detected.
- **JSONL skip rules.** Entire skips scanning fields named `signature` or ending in `id`/`ids`, and objects whose `type` starts with `image` or equals `base64`, to avoid false positives.
- **Users are ultimately responsible** for reviewing what they commit and push. Redaction is a safety net, not a guarantee.
