# Security & Privacy

Entire stores AI session transcripts and metadata in your git repository. This document explains what data is stored, how sensitive content is protected, and how to configure additional safeguards.

## Transcript Storage & Git History

### Where data is stored

When you use Entire with an AI agent (Claude Code, Codex, Gemini CLI, OpenCode, Cursor, Factory AI Droid, Copilot CLI, Pi), session transcripts, user prompts, and checkpoint metadata are committed to **your own git repository**. They stay out of your working branches' history, but they live in the same repo and travel with it.

Exactly where depends on the [checkpoint backend](architecture/ref-checkpoint-backend.md) the repo uses:

| Backend | Committed checkpoints land in | Notes |
|---|---|---|
| `git-refs` (default for new repos) | One ref per checkpoint: `refs/entire/checkpoints/<shard>/<id>` | Not a branch. Pushed individually, and fetched individually on demand |
| `git-branch` (legacy) | Subtrees of one long-lived branch, `entire/checkpoints/v1` | A branch, so it shows up in branch listings, CI branch triggers, and platform previews |

Which one a repo is on is recorded as `checkpoints.primary.type` in `.entire/settings.json` (or `settings.local.json`); an absent `checkpoints` block means `git-branch`. The redaction described in this document applies identically to both — the pipeline is shared, and only the destination differs. Where the distinction matters below, it is called out.

Entire also creates temporary local **shadow branches** (e.g. `entire/<commit>-<worktree>`) as working storage during a session, on both backends. Metadata written there — transcripts, prompts, incremental checkpoint data, subagent transcripts — goes through the same redaction pipeline as a committed checkpoint. **Code-file snapshots, however, are written as raw blobs of your working tree without redaction**, so any hardcoded secrets in your source code would appear unredacted on the shadow branch. Gitignored files (e.g., `.env`) are filtered out of these snapshots as a partial defense. Shadow branches are **not** pushed by Entire; do not push them manually, because unredacted source content would be visible on the remote. They are cleaned up when session data is condensed into a checkpoint at commit time.

Anyone with access to your repository can read committed checkpoint data: the full prompt/response history and session metadata. Note that transcripts capture all tool interactions — including file contents, MCP server calls, and other data exchanged during the session. Per-checkpoint refs are less *visible* than a branch, but they are not less accessible: a `git fetch` of `refs/entire/checkpoints/*` reads them just as well.

If your repository is **public**, this data is visible to the entire internet.

### What Entire redacts automatically

Entire automatically scans transcript and metadata content before writing it to a git object. Five always-on secret detection methods plus a configurable scanner layer (pattern matching, method 2 below) run during condensation, plus a conditional seventh pass for user-defined secret rules (see [Customizing redaction](#customizing-redaction) below), an opt-in eighth pass for PII (see [Optional PII redaction](#optional-pii-redaction) below), and an opt-in ninth pass that shells out to the OpenAI Privacy Filter model (see [Optional OpenAI Privacy Filter](#optional-openai-privacy-filter-opf) below):

1. **Entropy scoring** — Identifies high-entropy strings (Shannon entropy > 4.5) that look like randomly generated secrets, even if they don't match a known pattern.
2. **Pattern matching** — Runs one or both configurable scanner engines against known secret formats: [Betterleaks](https://github.com/betterleaks/betterleaks) (default on) and/or [goredact](https://github.com/lastpersonlabs/goredact) (default off). See [Choosing secret-scanner engines](#choosing-secret-scanner-engines) below.
3. **Provider token prefixes** — Deterministically redacts known secret-key prefixes (e.g. Supabase `sb_secret_`, `sbp_`) regardless of entropy or surrounding context.
4. **Credentialed URI detection** — Redacts URLs with embedded passwords, such as `scheme://user:password@host`.
5. **Database connection-string detection** — Redacts JDBC, Postgres keyword DSN, SQL Server, and ODBC-style connection strings containing passwords.
6. **Bounded credential value detection** — Redacts password-like config values such as `DB_PASSWORD=...` and `PGPASSWORD=...` while preserving the surrounding key.

Detected secrets are replaced with `REDACTED` before the data is ever written to a git object. Of the six secret-detection passes above, the scanner layer (pass 2) is configurable — see [Choosing secret-scanner engines](#choosing-secret-scanner-engines) below — while the other five are **always on** and cannot be disabled. User-defined rules (inline `custom_redactions` and rule packs) add a seventh secret-detection pass that only runs when configured.

### What Entire does NOT understand: pasted images and screenshots

**Every layer described above — all nine passes, including the opt-in PII and OpenAI Privacy Filter layers — reads transcript *text*. There is no OCR pass, no vision-model PII or secret scan, and no gate that holds an image back until someone reviews it. Nothing in Entire ever reads what an image depicts.**

What that means for an image you paste is **not uniform across agents**, because it depends on how the agent writes the image into its transcript. There are three outcomes, and only the first is an exposure:

| Agent | Default | With `redaction.externalize_images` on |
| --- | --- | --- |
| Claude Code | **Stored unredacted**, inline in the transcript as base64. The text scanner skips it (the `type: image` / `type: base64` skip rule below) rather than scanning it. | **Stored unredacted** as a raw binary blob under the checkpoint's `assets/` folder. |
| Codex | **Destroyed.** Codex writes images as `data:` URIs inside `image_url` and tool-output strings, which the skip rule does not match, so the entropy layer treats the base64 as a secret and replaces it. The stored transcript keeps the surrounding message; the image is gone. | **Stored unredacted** under `assets/` (externalization runs before redaction, which is what preserves it). |
| Cursor | **Not stored in the repository at all.** Cursor keeps images in its own per-session SQLite store, never in the transcript Entire reads. | **Stored unredacted** under `assets/`, captured from that store. |
| Gemini CLI, OpenCode, Copilot CLI, Factory Droid, Pi | Depends on the agent's own transcript shape; Entire has no image handling for these. Assume the Claude Code row unless you have checked. | Unchanged — the setting only affects the three agents above. |

**Do not treat the Codex row as a protection.** It is a side effect of a skip rule not matching a shape, not a deliberate safeguard: it destroys data you may want, it does not apply to the `assets/` path, and a change to either the rule or Codex's format would flip it to the exposure case without notice.

Where an image *is* stored, it is byte-for-byte what you pasted — completely unredacted — because byte-level regex and entropy redaction cannot inspect binary image data without corrupting it, so Entire does not attempt it. That is a deliberate design choice, not a bug.

Two things about *when* those bytes reach git, both of which narrow the window you have to catch a mistake:

- **A checkpoint's copy** lands on `entire/checkpoints/v1` (or the equivalent per-checkpoint ref on the `git-refs` backend). Like all checkpoint data it is written locally and pushed separately, so it reaches your remote only when checkpoint data is pushed — see the **Review before pushing** bullet under [Recommendations](#recommendations).
- **A shadow-branch copy** is written earlier, at the end of the agent turn, before you commit anything and regardless of the setting above. The shadow write does not externalize images, so for Claude Code the base64 is committed into your local git objects at turn end. Shadow branches are local-only and are never pushed (see [Where data is stored](#where-data-is-stored) above), but `entire checkpoint list` will not show this copy — it exists before any checkpoint does.

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

### Optional OpenAI Privacy Filter (`opf`)

A separate, **opt-in** layer that shells out to the [OpenAI Privacy Filter](https://github.com/openai/privacy-filter) (`opf`) — a 1.5B-parameter token-classification model that finds names, emails, phone numbers, addresses, dates, URLs, account numbers, and secrets that pure regex can miss. Disabled by default. Runs *in addition to* the eight built-in layers, **only at push time** — never per-turn and never at commit time. Local commits stay on the fast 8-layer pipeline so per-commit latency is unchanged; OPF only re-redacts checkpoints right before they leave the machine via `git push`.

Prerequisites:

```bash
pip install opf
```

Verify `opf --help` works; the CLI defaults to resolving the binary via `$PATH`. If you need a specific path, set `command` in `.entire/settings.local.json` — it is deliberately not honored from the committed `.entire/settings.json`. See [Why `command` is local-only](#why-command-is-local-only).

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

The filter needs at least one enabled category to run. This is enforced at push time, not settings load: with `enabled: true` and no effective category (`categories` omitted, empty, or all-false) the model scan cannot run. Rather than tagging commits as OPF-applied without a scan, it fails closed the same way a runtime failure does: `git-branch` aborts the push, `git-refs` withholds the checkpoint refs and lets your push through. Enable a category, set `enabled: false`, or pass `ENTIRE_OPF=no` on a push to skip the filter for that push only.

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
      "timeout_seconds": 30
    }
  }
}
```

- `command` — path or PATH-resolvable name of the `opf` binary. Defaults to `opf`. **Only read from `.entire/settings.local.json`**, and only when that file is untracked; see [Why `command` is local-only](#why-command-is-local-only).
- `timeout_seconds` — per-invocation timeout. Defaults to `30`.
- `prompt_default` — `"ask"` (default), `"never"`, or `"always"`. Controls whether the pre-push hook surfaces an interactive prompt before running OPF. `ENTIRE_OPF=yes` or `ENTIRE_OPF=no` on a single `git push` invocation overrides this for that push only.

### Why `command` is local-only

`command` becomes `argv[0]` of a process Entire executes during `git push`, so whoever controls that string controls what runs on the developer's machine. `.entire/settings.json` is version-controlled, which would let an ordinary pull request pair a `command` with a payload committed alongside it — and a JSON settings diff does not read as executable to a reviewer. The pre-push prompt is no defense either: it never names the command, `prompt_default: "always"` skips it, and non-TTY pushes (CI, agent-driven) auto-run.

Entire therefore honors `command` only when it is genuinely developer-owned:

- it must come from `.entire/settings.local.json`, not `.entire/settings.json`
- that file must be **untracked** — absent from both the git index and `HEAD`
- that file must not sit inside a **submodule or nested repository** mounted at `.entire` (an index/`HEAD` entry named `.entire`, or an `.entire/.git` on disk) — `git clone --recurse-submodules` delivers such a file exactly like a committed one
- the command must **not resolve inside the repository worktree** (no `./…`, no absolute path under the repo root): an executable that ships with the repo is what this rule exists to keep out, so install the binary elsewhere or use a bare `$PATH` name

The second check matters because the filename alone proves nothing: `.gitignore` does not apply to a path that is already tracked, so `git add -f .entire/settings.local.json` commits it and a fresh clone materializes it with the committed content.

This is enforced for the whole file, not just this setting: a tracked `.entire/settings.local.json` is ignored in its entirety, because it is not local to your clone — it arrives with the repository and would override project settings for everyone. Entire warns on stderr and tells you to run `git rm --cached .entire/settings.local.json`. The load still succeeds using project settings, so a committed file cannot brick the repository.

The two checks also differ in depth. The layer check looks at the git index; the `command` check also looks at `HEAD`. A pull request that commits the file puts it in the index of every clone that checks the branch out, so the index is what catches a delivered attack — and checkout cannot produce a file that is absent from the index, so "committed, then `git rm --cached`" is a state you created locally, not one that arrived with the repository. Reading `HEAD` is the expensive half, so it is reserved for the setting that gets executed.

The two checks fail in opposite directions on purpose. If the repository cannot be read at all, the local layer is still applied — losing every local preference over an unreadable repo is worse than the risk. The executed `command` is dropped in that case, because being wrong there means running someone else's binary. With no repository at all, nothing can have arrived by cloning, so the file is treated as yours.

When a `command` fails these checks it is ignored with a warning in `.entire/logs/entire.log` and OPF falls back to resolving `opf` on `$PATH`. If that binary is missing, the pre-push rewrite fails closed rather than pushing content you believed OPF had scanned. Everything else in the OPF block (`enabled`, `categories`, `timeout_seconds`, `prompt_default`) is ordinary configuration and still works from the shared project file.

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
- **No** skips OPF for this push only; the 8-layer-redacted content reaches the remote.
- **Always** runs OPF this push AND writes `prompt_default: "always"` to `.entire/settings.local.json` so future pushes don't ask.
- **Ctrl-C** cancels OPF. What that costs depends on the backend: on `git-branch` your `git push` aborts and exits non-zero; on `git-refs` your push completes and the checkpoint refs stay queued for a later push. Either way nothing under-redacted reaches the remote.

Non-interactive contexts (CI, scripted pipes with no TTY) skip the prompt and run OPF automatically when enabled, printing `→ OpenAI Privacy Filter: scanning checkpoints before push (may take ~30s)…` to stderr so the wait isn't silent. Progress and completion are reported as `→ OpenAI Privacy Filter: scanning checkpoints…` and `✓ OpenAI Privacy Filter: done (12.4s, 37 blobs)`. Set `ENTIRE_OPF=no` to skip OPF in those contexts without disabling the feature globally.

**CI consideration**: if you've enabled OPF locally and your CI runs `git push` (e.g. an agent-driven workflow), the CI push will attempt to run OPF too. If the `opf` binary isn't installed in CI, the failure is fail-closed rather than silently shipping under-redacted content — by design, since "I enabled OPF" should mean "no content leaves my machines without OPF." On `git-branch` that aborts the CI push; on `git-refs` the push succeeds but the checkpoints don't ship, which means they can accumulate unpushed until someone notices. The remedies are (a) install `opf` in CI, (b) set `ENTIRE_OPF=no` for CI pushes, or (c) set `prompt_default: "never"` if you only want OPF on interactive pushes.

OPF failures at push time are **fail-closed**: if OPF is not on PATH, fails to start, or times out during the pre-push rewrite, the per-process circuit breaker trips and no under-redacted content reaches the remote. The intent is that "the user enabled OPF" means "I do not want unredacted content leaving this machine" — falling back to 8-layer silently on the push path would violate that contract. Fix the install or set `ENTIRE_OPF=no` for a one-off push.

How that is enforced depends on the checkpoint backend, because they have different escape hatches:

- **git-branch**: the rewrite aborts the push with `OPF runtime failed during pre-push rewrite (command=…); aborting push so regex-only content isn't tagged as OPF-applied`. Your `git push` exits non-zero. The checkpoint branch travels with that push, so refusing the push is the only way to withhold it.
- **git-refs**: checkpoint refs are pushed separately from your branch and stay queued when they are not flushed, so the failure withholds the checkpoint push and lets your own `git push` succeed. Nothing under-redacted ships either way. This is not silent: a warning names the failure and states that checkpoint refs stayed queued for the next push.

(The circuit breaker is per-process, so a broken install costs one warning instead of one timeout per blob — but the push still aborts.)

Cost note: each shell-out loads the OPF model (~1.5B parameters on CPU). The pre-push rewrite batches **every redactable leaf across every unpushed commit** — v1 commits on git-branch, every unpushed commit on every queued ref on git-refs — into a single inference pass, so a typical real-world push pays the model-load cost once (~6s) plus inference (~5s per 100KB of leaf content) — not multiplied by the number of commits or blobs. A 3-commit push with ~250KB of total prose content runs in ~12–15s, not the ~50–100s a per-blob flow would take. Per-commit latency is unaffected because OPF doesn't run at commit time.

#### When OPF actually runs

OPF execution lives in the pre-push hook. The flow:

1. **Post-commit** writes the checkpoint with **8-layer-only** redaction to local git objects — per-checkpoint refs on `git-refs`, the `entire/checkpoints/v1` branch on `git-branch`. Fast, predictable, no OPF cost on the hot path.
2. **Pre-push** (`git push`): if OPF is enabled, the hook re-reads each not-yet-OPF'd commit, runs the OpenAI Privacy Filter over its blobs to add the categories the regex layers don't catch (person names, addresses, etc.), and builds **new commits** carrying an `Entire-OPF-Applied: true` trailer. Each backend then points its own local ref at the new tip with a compare-and-swap, and the (now 9-layer-redacted) commits are what get pushed.
3. The original 8-layer-only commits become **unreachable** in the local git object database and eventually get swept by `git gc`.

The two backends differ in **how they find the commits to rewrite**:

| | `git-refs` | `git-branch` |
|---|---|---|
| What it walks | Every ref in the push-discovery queue | The `entire/checkpoints/v1` commit chain |
| Where it stops | The first ancestor already carrying `Entire-OPF-Applied: true` — the trailer is the watermark | The remote's v1 tip, fetched live (not a stale tracking ref) |
| Already-OPF'd commits | Left byte-identical; they *are* the boundary | Re-parented onto the new chain, but not re-redacted |
| Ref update | Each queued ref rewritten in place, CAS'd individually | One CAS on the local v1 ref |

In steady state the `git-refs` walk stops at the last commit the previous push OPF'd. That is usually the tip's parent, but not always: each write to a checkpoint advances its ref by one commit, so a turn that creates a checkpoint and then backfills its transcript and summary leaves a three-commit chain to rewrite.

Note that the local ref move is **not** a fast-forward on either backend — the rewritten chain replaces the old commits rather than descending from them, which is exactly why step 3 leaves them unreachable. The *push* is still fast-forward-only and never forced, because the new chain is parented on a commit the remote already has.

This means:

- **The remote only ever sees 9-layer-redacted content** when OPF is enabled.
- **Local-only commits are 8-layer-redacted** until the moment you push. If you never push, OPF never runs.
- **Re-running pre-push is idempotent** — commits already carrying the trailer are never re-redacted.

#### Divergence, caps, and concurrent pushes

The rewrite refuses to proceed in a divergent or oversized state rather than silently rebasing or shipping unscanned content. As always, `git-branch` enforces this by aborting your push and `git-refs` by withholding the checkpoint refs.

**Divergence.** On `git-branch`, if local `entire/checkpoints/v1` has commits that aren't ancestors of the remote's v1, the hook exits with a `entire/checkpoints/v1 has diverged from remote` error. Fetch the remote and either reset local v1 to the remote tip or resolve manually before pushing.

`git-refs` has no divergence pre-check, and doesn't need one: each checkpoint is its own ref, so divergence shows up at push time as a non-fast-forward rejection for that one ref. Recovery fetches the remote ref and replays the local-only commits on top, then retries — still without forcing, so the remote commit is preserved as an ancestor. A ref that can't be replayed stays queued.

**Un-OPF'd commit cap: `100` by default.** Override per push:

```fish
set -x ENTIRE_OPF_BOOTSTRAP_LIMIT 500; git push
# or fully unbounded:
set -x ENTIRE_OPF_BOOTSTRAP_LIMIT unlimited; git push
```

The two backends trip this differently. On `git-branch` it applies **only on bootstrap** — the first push, when the remote has no v1 yet — counted across all unpushed commits. On `git-refs` it applies on **every** push, counted **per queued ref** over that ref's un-trailered ancestry. In practice the `git-refs` trigger is "OPF was enabled late" or "checkpoints were just migrated from the branch", not "first push".

**Batch cap: `2 MiB` of cumulative prose-leaf content by default** (≈110s of inference), on both backends. An `OPF would run inference on …` error means you've hit it:

```fish
set -x ENTIRE_OPF_BATCH_LIMIT 10485760; git push   # 10 MiB
# or fully unbounded:
set -x ENTIRE_OPF_BATCH_LIMIT unlimited; git push
```

**Raw-byte cap: `200 MiB` of blob content buffered in memory**, on both backends. It has no env var of its own — it is derived as 100× the batch cap, so raising `ENTIRE_OPF_BATCH_LIMIT` raises it too. It is checked incrementally as blobs load, so on a pathological push (one commit carrying a huge pasted transcript) this is the cap that fires first.

The three caps protect different failure modes: the commit cap stops "100 throwaway commits", the batch cap stops "one commit with 50 MB of prose", and the raw-byte cap stops the loader exhausting memory before either of the others can be evaluated. On `git-refs` the two byte caps are cumulative across the whole flush (all queued refs together) while the commit cap is per ref.

**Concurrent push** from another worktree: both backends compare-and-swap the local ref. If another process moved it while OPF was running, `git-branch` exits with `entire/checkpoints/v1 moved during OPF rewrite …; re-run 'git push' (no fetch needed; the move was local)` and aborts. On `git-refs` the affected ref simply stays queued and the next push picks it up. Note that the `git-refs` rewrite rebuilds every commit before touching any ref, but the ref updates themselves are not atomic *across* refs: a conflict partway through leaves the earlier refs already rewritten. They stay queued and push OPF-applied next time, so this is safe — just not "nothing moved".

#### Persistence of un-redacted-by-OPF content

Several places retain content that OPF *didn't* redact, with different lifetimes. Understanding them matters if your threat model goes beyond "what reaches the remote":

| Location | Redaction level | Lifetime | Reaches remote? |
|---|---|---|---|
| `.entire/metadata/<session>/full.jsonl` | **None** — sanitized, not redacted | Until the session's data is cleaned up (`entire clean`) | No |
| The agent's own transcript (e.g. `~/.claude/projects/…`) | **None — raw** | Owned and managed by the agent | No |
| Shadow branch `entire/<commit>-<worktree>` | 8-layer | Auto-deleted after the next successful push (only when its session has ended cleanly) | No |
| Unreachable git objects after the pre-push rewrite | 8-layer | Until `git gc --prune` (default `gc.pruneExpire` is 2 weeks) | No |
| Local `refs/entire/checkpoints/*` (`git-refs`) | 8-layer before push, 9-layer after the rewrite | Kept indefinitely — these refs are never deleted after pushing | Yes, once pushed |
| Local `entire/checkpoints/v1` (`git-branch`) | 8-layer before push, 9-layer after the rewrite | Until you delete the branch | Yes, once pushed |
| Reflog of `entire/checkpoints/v1` (`git-branch` only) | 8-layer tips | Default `gc.reflogExpire` is 90 days | No |
| A configured `git-branch` **mirror** | 8-layer, indefinitely | Until you delete the branch | No — mirrors aren't pushed at pre-push |
| A leftover `entire/checkpoints/v1` after migrating to `git-refs` | 8-layer | Until you delete the branch | No |
| The remote's copy of the pushed refs/branch | 9-layer (after OPF rewrite) | Until you delete them on the remote | Yes |

Two notes on the `git-refs` rows:

- **There is no reflog concern for `refs/entire/checkpoints/*`.** Git's `core.logAllRefUpdates` default only auto-logs `refs/heads/*`, `refs/remotes/*`, `refs/notes/*`, and `HEAD`, and Entire's checkpoint-ref writes don't append reflog entries themselves. The reflog row above is genuinely branch-only (unless you've set `logAllRefUpdates=always`).
- **The push queue holds no content.** `entire-checkpoint-push-queue.jsonl` in the git common dir stores one ref *name* per line, mode `0600`. Nothing in it is redactable.

Two `git-branch`-shaped leftovers are easy to miss once a repo has moved on:

- **A `git-branch` mirror never gets OPF'd.** Mirrors receive best-effort write fan-out only and never ref-level mutations, so a mirror branch keeps 8-layer content indefinitely. It isn't pushed at pre-push, so it doesn't reach the remote — but it is local content, and it has a `refs/heads/` reflog.
- **Migration doesn't delete the old branch.** `entire doctor migrate-checkpoints` imports from `entire/checkpoints/v1` and leaves it in place, 8-layer, along with its reflog. Migrated refs also carry no OPF trailer, so the first push after a migration re-OPFs every migrated ref — one commit per ref keeps the commit cap happy, but the 2 MiB batch cap is the realistic trip point.

`.entire/metadata/<session>/full.jsonl` is Entire's own local working copy of the transcript, written mode `0600`. It is *sanitized* (agent state that cannot be replayed out of a checkpoint is stripped) but **not redacted** — redaction happens on the way into a git object, not on this file. It is the input the shadow-branch walk and condensation read from.

The agent's own transcript is never modified at all. Entire reads from it and leaves it alone, because the agent is writing to it continuously and editing under the agent's feet would corrupt the session.

To aggressively scrub the unreachable git objects from the pre-push rewrite (instead of waiting for the 2-week GC window):

```fish
# git-refs
git reflog expire --expire-unreachable=now --all
git gc --prune=now

# git-branch
git reflog expire --expire-unreachable=now refs/heads/entire/checkpoints/v1
git gc --prune=now
```

This is I/O-heavy on large repositories; it's not run automatically. If you want it as part of your push workflow, wrap `git push` in a script that invokes it after a successful push.

#### Verifying OPF is working

After enabling OPF, run an agent turn whose prompt contains a name — e.g. *"Create notes.txt with: Alice Johnson reviewed the proposal."* — then commit (which stays on the fast 8-layer pipeline) and push, which is when OPF runs.

```fish
git commit -m "demo"
git push   # → "OpenAI Privacy Filter: scanning checkpoints…"
```

Then confirm the redaction landed and the trailer is present. Note that `git log --oneline` **cannot** show a trailer — it prints only the subject line — so use a format that includes the body:

```fish
# git-refs: which checkpoint refs exist, and which are OPF-applied
git for-each-ref --sort=-committerdate \
  --format='%(refname)  %(trailers:key=Entire-OPF-Applied)' refs/entire/checkpoints/

# git-branch: the trailer on the latest v1 commit
git log -1 --format='%H%n%B' entire/checkpoints/v1

# either backend: confirm the content itself was redacted
entire checkpoint list
entire checkpoint explain <checkpoint-id> | grep -i 'REDACTED_PERSON'
```

If `[REDACTED_PERSON]` appears in the prompt or transcript section and the checkpoint's commit carries `Entire-OPF-Applied: true`, OPF is active.

On `git-refs` you can also confirm nothing was withheld — an absent or empty queue file means everything flushed:

```fish
cat "$(git rev-parse --git-common-dir)/entire-checkpoint-push-queue.jsonl"
```

### Recommendations

If your AI sessions will touch sensitive data:

- **Use a private repository.** This is the simplest and most complete protection. Committed checkpoints are then only visible to collaborators.
- **Avoid passing sensitive files to your agent.** Content that never enters the agent conversation never appears in transcripts.
- **Never paste a screenshot or image containing secrets or personal data.** Nothing in Entire reads what an image depicts, and on most agents the image is stored unredacted — see [What Entire does NOT understand: pasted images and screenshots](#what-entire-does-not-understand-pasted-images-and-screenshots).
- **Review before pushing.** Checkpoints are written locally at commit time and pushed separately, so there is always a window to inspect them:

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

## Global tracking and checkpoint-sync trust

Global tracking (`"global": {"enabled": true}` in `~/.config/entire/settings.json`)
captures agent sessions in every git repository you touch with Claude Code or
Gemini CLI, without `entire enable` in each one. In a repo you never enabled,
all runtime data lives under `.git/` (`entire/worktree/<hash>/`), so the
worktree stays byte-clean; `exclude_paths`, `exclude_paths_exact`, and
`exclude_origins` carve repos out and fail closed on unusable patterns.

Capture and sync are separate consents. While global tracking is on, a repo's
checkpoints leave this machine only after you trust it — whether the repo is
tracked globally or was enabled with `entire enable`. Trust is recorded in the
same settings file (`trust_all`, `trusted_origins`, `trusted_paths`) by:

- `entire enable` in a repo (the explicit enable is the consent);
- the pre-push prompt — **Yes** (this repo, or every clone of its origin),
  **Not now** (keep capturing locally, ask again next push), **Always**
  (`trust_all`). It reads the terminal, never Git's stdin, and never grants
  implicitly (accessible mode holds);
- `entire trust`, and `entire trust --revoke` to withdraw it.

A held repo never blocks your own `git push`: the branch lands, checkpoint data
stays local, one stderr line explains, and the first trusted push syncs the
backlog. Non-interactive pushes hold silently apart from that line. With the
tier off or unconfigured nothing changes from today. See
`docs/architecture/global-tracking.md` for the activation and layout rules.

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
preference, so it is honored under exactly the same rule as the OPF
[`command`](#why-command-is-local-only): only from `.entire/settings.local.json`,
and only when that file is untracked in both the index and `HEAD`. Reading it
from the committed `.entire/settings.json` would let an ordinary pull request
turn on execution of whatever `entire-agent-*` binary it could get onto a
developer's `$PATH`, and — as with `command` — one line of JSON does not read as
executable to a reviewer. There is no prompt in front of this one at all.

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

## Reporting a vulnerability

For vulnerability disclosure, see [SECURITY.md](../SECURITY.md) at the repo root: email `security@entire.io`, expect acknowledgment within 48 hours and resolution of criticals within 90 days.

## Related

- [Checkpoint commit signing](architecture/checkpoint-signing.md) — best-effort GPG/SSH signing of checkpoint commits, opt-out via `sign_checkpoint_commits: false`.
- External agent plugins are arbitrary executables on `$PATH` invoked by the CLI; only install plugins you trust. Discovery is off unless you turn it on in `.entire/settings.local.json` — see [Why `external_agents` is local-only](#why-external_agents-is-local-only).
