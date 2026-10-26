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

Entire automatically scans transcript and metadata content before writing it to a git object. Six always-on secret detection methods plus a configurable scanner layer (pattern matching, method 2 below) run during condensation, plus a conditional eighth pass for user-defined secret rules (see [Customizing redaction](#customizing-redaction) below), and an opt-in ninth pass for PII (see [Optional PII redaction](#optional-pii-redaction) below):

1. **Entropy scoring** — Identifies high-entropy strings (Shannon entropy > 4.5) that look like randomly generated secrets, even if they don't match a known pattern. Three shapes are exempt from this layer only: a string that is, in its entirety, a Claude Code tool-use id (`toolu_01` plus 22 letters or digits), a string that lies entirely inside a content digest of its exact encoded length (a go.sum `h1:` hash, or a `sha1-`, `sha256-`, `sha384-`, or `sha512-` integrity value as used by npm lockfiles, Subresource Integrity, and CSP), and an identifier assignment: a key ending in `_id` or `_account` (e.g. `GOOGLE_ADSENSE_ACCOUNT=pub-1234567890123456`, `tenant_id=...`) whose value is not itself high-entropy, unless the key names a credential (contains a `key`, `token`, `secret`, or `pass`/`password`/`pwd` segment) or a bearer-style id (`session`, `sid`, `request`, `nonce`, `csrf`, `cookie`), so `aws_access_key_id=...` and `session_id=...` are still scored. The identifier exemption narrows coverage for low-entropy values under such keys. The other layers still scan all three, and a secret attached to an id or digest forms a longer string that is not exempt. A base64 value that contains `/` is scored as one string rather than piece by piece, unless it reads as a file path: a word, a long hex name, or a version with a build suffix (`10+incompatible`) between slashes. The text before the value does not matter, since secrets often follow a flag, a key prefix or a name (`-p<secret>`, `token_<secret>`, `user@<secret>`).
2. **Pattern matching** — Runs one or both configurable scanner engines against known secret formats: [Betterleaks](https://github.com/betterleaks/betterleaks) (default on) and/or [goredact](https://github.com/lastpersonlabs/goredact) (default off). See [Choosing secret-scanner engines](#choosing-secret-scanner-engines) below. Betterleaks multi-part rules (such as an AWS key ID with its secret access key) are adjusted for redaction: each credential part is redacted on its own, whether or not its partner appears nearby.
3. **Provider token shapes** — Deterministically redacts known secret-key prefixes (Supabase `sb_secret_`, `sbp_`) regardless of entropy or surrounding context, plus bounded token shapes with boundary checks and false-positive filters: short `sk-` keys, AWS access key IDs (documentation `EXAMPLE` keys stay visible), Salesforce access tokens, and Tableau personal access tokens. These shapes are caught without a vendor keyword nearby.
4. **Credentialed URI detection** — Redacts URLs with embedded passwords, such as `scheme://user:password@host`. A URL whose password is a placeholder (see [Secrets (always on)](#secrets-always-on) below) is left intact, unless its query also carries a real password. Only the password is checked; a credential used as the username is left to the other layers. In database URLs (`postgres://`, `mysql://`, `mariadb://`, `mongodb://`, `redis://`), a password containing `/` or `'` is caught too.
5. **Database connection-string detection** — Redacts JDBC, Postgres keyword DSN (including Unix-socket DSNs without `host=`), SQL Server, and ODBC-style connection strings containing passwords, where values may contain spaces. Also redacts Oracle `user/password@` logins in `jdbc:oracle:` URLs and `sqlplus` commands.
6. **Shell-stdin secret literals** — Redacts literals `printf`-piped into `secrets put/set/add/create` commands, unless the target key is a plain identifier like `CLIENT_ID`, such as `printf 'value' | mycli secrets put API_KEY`, while preserving the command itself.
7. **Bounded credential value detection** — Redacts credential-shaped config values such as `DB_PASSWORD=...`, `PGPASSWORD=...`, `GITHUB_CLIENT_SECRET=...`, and `AUTH_TOKEN=...` while preserving the surrounding key, the `sig` signature of Azure SAS URLs and tokens, and the credential of an `Authorization` or `Proxy-Authorization` header written out in a request, code, or a headers map (`Authorization: Bearer <token>`), whatever its entropy. A credential without a digit, or a shell or template reference such as `$TOKEN` or `{{token}}`, stays visible.

Detected secrets are replaced with `REDACTED` before the data is ever written to a git object. Of the seven secret-detection passes above, the scanner layer (pass 2) is configurable — see [Choosing secret-scanner engines](#choosing-secret-scanner-engines) below — while the other six are **always on** and cannot be disabled. User-defined rules (inline `custom_redactions` and rule packs) add an eighth secret-detection pass that only runs when configured.

### What Entire does NOT understand: pasted images and screenshots

**Every layer described above — all nine passes, including the opt-in PII layer — reads transcript *text*. There is no OCR pass, no vision-model PII or secret scan, and no gate that holds an image back until someone reviews it. Nothing in Entire ever reads what an image depicts.**

What that means for an image you paste is **not uniform across agents**, because it depends on how the agent writes the image into its transcript. There are three outcomes, and only the first is an exposure:

| Agent | Default | With `redaction.externalize_images` on |
| --- | --- | --- |
| Claude Code | **Stored unredacted**, inline in the transcript as base64. The text scanner skips it (the `type: image` / `type: base64` skip rule below) rather than scanning it. | **Stored unredacted** as a raw binary blob under the checkpoint's `assets/` folder. |
| Codex | **Destroyed.** Codex writes images as `data:` URIs inside `image_url` and tool-output strings, which the skip rule does not match, so the entropy layer treats the base64 as a secret and replaces it. The stored transcript keeps the surrounding message; the image is gone. | **Stored unredacted** under `assets/` (externalization runs before redaction, which is what preserves it). |
| Cursor | **Not stored in the repository at all.** Cursor keeps images in its own per-session SQLite store, never in the transcript Entire reads. | **Stored unredacted** under `assets/`, captured from that store. |
| Pi | **Stored unredacted**, inline in the transcript as base64. Pi writes images as `{"type": "image", "data": …}` items, which the same skip rule matches. | **Stored unredacted** under `assets/`. |
| OpenCode, Copilot CLI, Factory Droid | Depends on the agent's own transcript shape; Entire has no image handling for these. Assume the Claude Code row unless you have checked. | Unchanged — the setting only affects the four agents above. |

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

Omitting either key, or the key's `enabled` field, keeps that engine at its default. All other redaction layers — entropy scoring, provider token shapes, credentialed URI detection, connection-string detection, custom rules, shell-stdin literal detection, bounded credential key/value detection, and PII — are unaffected by these two toggles; they keep running exactly as described elsewhere in this document regardless of which scanner engine(s) are selected.

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
| `ip` (IPv4 and IPv6 addresses, see below) | on | `[REDACTED_IP]` |

Common bot/CI email addresses are not redacted (`noreply@*`, `actions@*`, `*@users.noreply.github.com`, `*@noreply.github.com`), and neither is the `git@` login of a git remote: a remote URL (`ssh://git@host/org/repo`), an scp-style remote whose path contains `/` or ends in `.git` (`git@host:org/repo.git`), or a bare `git@` on GitHub, GitLab, Bitbucket, Codeberg, or Azure DevOps (`ssh -T git@github.com`). Any other `git@` address, such as `git@company.com` in prose or `ssh -T git@gitlab.company.com`, is redacted like any other email.

Phone and address matches must stand on their own: digits that are part of a longer number (`2024-123-4567890`) are not a phone number, and a street suffix must be a whole word, so `Plan` or `Started` does not end an address.

IP addresses are redacted when they look like an address in use, because version numbers (`1.17.2.3`) and user agents (`Chrome/154.0.0.0`) share the dotted-quad shape. An address is redacted when any of these hold:

- it is private, link-local, or shared address space (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `169.254.0.0/16`, `100.64.0.0/10`, `fc00::/7`, `fe80::/10`);
- network syntax surrounds it: a scheme (`https://<ip>`), userinfo (`deploy@<ip>`), a port (`<ip>:443`, which stays visible as `[REDACTED_IP]:443`), a CIDR prefix (`<ip>/32`), or a connection arrow (`->`);
- a network word (such as `ip`, `host`, `server`, `ssh`, `tcp`, `dns`, `connect`) appears shortly before or after it on the same line, or it is the value of a JSON key that names an address (`host`, `remote_addr`, `peerIP`, `HOST_IP`).

These are never redacted:

- loopback (`127.0.0.1`, `::1`), unspecified (`0.0.0.0`, `::`), `0.0.0.0/8`, and `255.255.255.255`;
- documentation addresses, which no network uses (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`, `2001:db8::/32`, `3fff::/20`);
- the first address of each private block, as written in code that handles those ranges (`10.0.0.0/8`, `172.16.0.0/12`, `fe80::/10`), and any `N.0.0.0`;
- the value of a version label, even in a private range (`Version=10.0.0.0`, `AssemblyVersion("10.1.2.3")`, `"version": "10.1.2.3"`), unless the same text is redacted elsewhere in the line, since a value redacted in one field is redacted wherever it appears;
- an address that continues a longer token (`v1.2.3.4`, `go1.2.3.4`, `1.2.3.4-rc1`, an OID such as `1.3.6.1.4.1`, or code such as `u64::from` and `a[1::2]`).

A public address printed bare with no such context, such as one line in a plain list, is not redacted; add a `custom_patterns` entry if you need every address. Path fields in transcripts (`cwd`, `file_path`, `path`) are not scanned by any redaction layer, so an address in a Windows share path there (`\\10.0.0.5\share`) stays. A version number that sits next to a network word on the same line, or in a private range without a version label (`from 10.2.0.0 to 10.3.0.0`), can be redacted.

Older CLI versions reject unknown `pii` keys, so set `"ip": false` in a committed `.entire/settings.json` only after everyone on the team has upgraded. Turning on `enabled` is enough to get IP redaction.

Teams can add their own regex patterns via `custom_patterns`. Each key is a label (uppercased in the replacement token), each value is a regex string. Example: `{"employee_id": "EMP-\\d{6}"}` produces `[REDACTED_EMPLOYEE_ID]`. A pattern that fails to compile or can match the empty string is skipped with a warning, the same as for [custom redaction rules](#customizing-redaction).

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
- **Keep checkpoints out of a public repository.** If the code must stay public, upload checkpoints to a separate private repository with `entire configure --local --checkpoint-remote github:<owner>/<repo>` (`strategy_options.checkpoint_remote`, see [Checkpoint Remote](../README.md#checkpoint-remote)), or stop uploading them with `entire configure --local --skip-push-sessions` (`strategy_options.push_sessions: false`). `--local` makes the setting take effect for your clone: a project `checkpoint_remote` owned by a different account than `origin` is ignored, and a local `push_sessions` overrides the project one.
- **Avoid passing sensitive files to your agent.** Content that never enters the agent conversation never appears in transcripts.
- **Never paste a screenshot or image containing secrets or personal data.** Nothing in Entire reads what an image depicts, and on most agents the image is stored unredacted — see [What Entire does NOT understand: pasted images and screenshots](#what-entire-does-not-understand-pasted-images-and-screenshots).
- **Review before pushing.** Checkpoints are written locally — at commit time, or when a session with no file changes is snapshotted with the hidden `entire checkpoint create` — and pushed separately, so there is always a window to inspect them:

  ```fish
  # git-refs: what your next git push sends, read locally with no network call
  entire checkpoint list --outgoing
  entire checkpoint explain <id>

  # git-refs: list local checkpoint refs, then read one
  git for-each-ref refs/entire/checkpoints
  entire checkpoint list
  entire checkpoint explain <id>

  # git-branch: inspect the branch directly
  git log --oneline entire/checkpoints/v1
  ```

- **Know your push destination.** Checkpoint data syncs to exactly one elected remote; `entire status` names it and reports how many checkpoints are still unpushed, and on the git-refs backend `entire checkpoint list --outgoing` lists them. `entire enable` also names the destination every time it runs.

## What Gets Redacted

### Secrets (always on)

Secret detection as a whole is always on, though the pattern-matching scanner layer within it is configurable per [Choosing secret-scanner engines](#choosing-secret-scanner-engines). Betterleaks pattern matching covers cloud providers (AWS, GCP, Azure), version control platforms (GitHub, GitLab, Bitbucket), payment processors (Stripe, Square), communication tools (Slack, Discord, Twilio), private key blocks (RSA, DSA, EC, PGP, OpenSSH), and generic credentials (bearer tokens, basic auth, JWTs). Dedicated credentialed URI detection covers URLs that embed passwords. Additional database connection-string detection covers DB DSNs and query-parameter passwords not reliably covered by generic secret rules. Entropy scoring catches secrets that don't match any known pattern.

All detected secrets are replaced with `REDACTED`. PII matches are replaced with category-tagged tokens like `[REDACTED_EMAIL]` (see [Optional PII redaction](#optional-pii-redaction)).

To reduce over-redaction, Entire preserves structural transcript fields such as IDs and paths, leaves placeholder values alone, and redacts only credential values for bounded key/value forms. In structured JSON, such as a tool call's input, a value under a key that names a credential (for example `api_key`, `token`, `client_secret` or `password`) is also scanned together with its key, so `{"api_key": "…"}` is caught by the same key-based rules that catch `api_key=…` in prose. When only the key reveals a secret, the whole value is replaced. Placeholders are detected by exact match (e.g. `changeme`, `example`, `placeholder`, `your_password`, `your_secret`, prior `REDACTED`/`[REDACTED]`/`<REDACTED>` markers) or by shape: shell expansions like `${DB_PASSWORD}`, template expressions such as `%s`, `%(password)s`, `{}`, `{password}`, or `$DB_PASSWORD`, bracketed names like `<password>` or `<your-db-password>`, and mask runs of three or more `*`/`x`/`.`/`-` (so `***`, `xxxx`, `....`, `----` all match). A shell expansion must be one complete expression, so `${A}hunter2${B}` is redacted, and one with a fallback value, such as `${DB_PASSWORD:-hunter2}`, is a placeholder only when the fallback is empty or itself a placeholder. Format-field names must be lowercase and shell variables uppercase, so `{hunter2}` and `$hunter2` are still redacted. Bounded key/value captures additionally treat as placeholders: command substitutions that run a known secret-fetching command without quotes or redirection (`$(cat token)`, `$(gh auth token)`) unless single-quoted, documentation fill-ins that name a credential (`your_api_key_here`), short letter-only vendor prefixes ending in an ellipsis (`sk-ant-...`), uppercase bracketed names (`<ENTIRE_TOKEN>`), Go template references, and, for non-password keys only, code literals (`None`, `null`, `false`) and the exact masks `hidden` / `(value hidden)`; inside single quotes only the literal placeholder words and shapes apply. Secret-scanner findings, credentialed URLs and connection strings use only the shared placeholder rules. When a connection string contains a real password, it is redacted as a unit because partial fragments can still expose sensitive material; connection strings and credentialed URLs whose passwords are placeholders are left intact. In a Postgres keyword DSN, an uppercase `PWD=` whose value is an absolute path (`/app`, `~/app`) is the shell's working directory, so `env` output is not mistaken for a connection string, while a lowercase `pwd=` always counts as a password; bounded key/value detection reads the value from the same line only, so a blank `DB_PASSWORD=` leaves the next line alone.

### Redaction counts in checkpoint metadata

Each checkpoint records how many values redaction replaced in its stored transcript (`full.jsonl`) and how many matches each detection layer found. That transcript holds the whole session up to the checkpoint, so a session's counts grow from one checkpoint to the next; to total what a push sends, take each session's latest checkpoint rather than adding its checkpoints together. The layers are `entropy`, `betterleaks`, `goredact`, `provider_token`, `credentialed_uri`, `connection_string`, `custom`, `shell_stdin` (a literal piped into a secret-management command), `credential_value`, `pii`, `key_context` (a JSON value redacted because of its key), and `credential_key` (a password value under a credential key). A layer that ran and found nothing reports 0. A layer that did not run, such as custom rules when none are configured, is left out. A layer counts each place it flags once, however many of its rules match there. Overlapping matches count in every layer that found them but are replaced once, so the layer counts can add up to more than the number of replacements. A value found in one JSON field is also replaced where the same text appears in a field redaction does not scan, such as an ID, so the layer counts can also add up to less.

The counts never include the redacted values. Custom rules are reported as one total plus the number of rules that ran, without their names, because a rule's label can name something a team keeps out of shared metadata. PII is counted per built-in category (`EMAIL`, `PHONE`, `ADDRESS`, `IP`), and custom PII patterns share one `custom` bucket, except a pattern named after a built-in category (such as `ip`), which counts under that category.

`entire checkpoint explain` shows the counts in its header, for example `redacted 5 replacements in this session so far · matches: entropy 2, betterleaks 1, custom 2 (1 rule)`, and `--json` includes them for each session and for the whole checkpoint. Checkpoints written by older versions have no counts and show no row. Prompts (`prompt.txt`), summaries, and subagent transcripts are not counted.

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
- A pattern that can match the empty string is skipped the same way. That covers `""`, `x*`, a trailing `|` as in `ACME-[0-9]{8}|`, an optional group like `(EMP-\d{6})?`, and a bare `\b`. Left active, such a pattern would insert `REDACTED` between characters throughout the transcript. The warning names the label and the reason, never the pattern.
- Override in `.entire/settings.local.json` for personal additions; entries merge per-key (override replaces the same key, leaves other keys intact).
- An empty value is not a supported way to disable a rule. It is a pattern that matches the empty string, so it is skipped with a warning each time Entire loads the rules. Because the override replaces the rule with the same label, an empty value in `.entire/settings.local.json` still turns that `.entire/settings.json` rule off for you, and text only that rule would catch is stored unredacted.

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

A rule whose regex fails to compile or can match the empty string is skipped with a warning, like an inline rule. The other rules in the pack still load.

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

- **My rule doesn't redact anything.** Warnings about invalid patterns or sample mismatches are emitted by the redaction layer when Entire initializes it. In the hook path (where checkpoints are actually written) these go to `.entire/logs/entire.log` — `grep component=redaction` and look for lines mentioning your label or pack path. `error="pattern can match the empty string"` means the rule was skipped because it can match zero characters; change it so every match consumes at least one character, for example by dropping a trailing `|` or `?`, or using `+` instead of `*`. When a hard pack-discovery failure happens during an interactive command, Entire also prints a one-line breadcrumb on stderr pointing back at the log. `entire checkpoint explain` on a recent checkpoint tells the cases apart: `custom 0 (2 rules)` means your rules ran and matched nothing, and no `custom` entry means no rule loaded.
- **My pack file is silently ignored.** Filenames must end in `.yaml`, `.yml`, or `.json`. Other extensions are skipped.
- **I want to disable a rule temporarily.** Comment it out (prefix the YAML key with `#`) or remove the entry from `custom_redactions`. The rule reloads on the next CLI invocation.

## Limitations

- **Best-effort.** Novel or low-entropy secrets (short passwords, predictable tokens) may not be caught.
- **Filenames and binary data.** Secrets in filenames, binary files, or deeply nested structures may not be detected.
- **JSONL skip rules.** Entire skips scanning the string values of fields whose name *ends in* `signature` (so `thinkingSignature` too), fields ending in `id`/`ids`, and structural-path fields (`filepath`, `file_path`, `cwd`, `root`, `directory`, `dir`, `path`) to avoid false positives. That includes strings in an array under such a field; objects nested under one are still scanned. Because the rule matches by suffix, a string directly under an unrelated name such as `valid` or `android` is skipped too. Objects whose `type` starts with `image` or equals `base64` are also skipped — and that skip is what leaves a pasted image unredacted rather than merely unflagged. It matches some agents' image shapes and not others, which is why the outcome differs per agent: see [What Entire does NOT understand: pasted images and screenshots](#what-entire-does-not-understand-pasted-images-and-screenshots) above.
- **JSON keys are not scanned.** Only values are. A secret used as an object key, such as a token as a header name, is stored as written.
- **A secret recognized only by its key is redacted only there.** A value such as a hex string under `api_key` is replaced where it sits. The same value elsewhere, in another field, in prose, or on another line, is redacted only if a detector flags it there on its own.
- **Built-in PII patterns are US-centric.** `phone` matches North American (NANP) formats only — international formats, including E.164 numbers outside `+1`, are not detected. `address` matches the street line only; city, state, and ZIP/postcode are preserved. If you handle personal data from other regions, add `custom_patterns` for your locale rather than relying on the built-in categories alone.
- **Custom PII patterns are user-authored.** Teams own the correctness of their `custom_patterns`. An invalid regex, or one that can match the empty string, is logged and skipped, not enforced.
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
