# models-updater

An example Lua plugin that keeps a repo's cached model-pricing table
(`.entire/models.json`) fresh, and makes it obvious when it has gone stale — so
embedded pricing never rots silently.

It exercises the whole plugin surface in one place: a **command**
(`entire.command`), an observer **hook** (`entire.on`), the **http** and **fs**
capabilities, durable **kv** state, and **capability-gating**.

## What it does

### `entire models-update`

1. Fetches LiteLLM's public
   [`model_prices_and_context_window.json`](https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json)
   over `entire.http` (the `http` capability).
2. If `.entire/models.json` already exists, reads it (`entire.fs`) and **diffs**
   it against the fetched data, reporting per-model input/output **rate drift**.
3. **Preserves local-only models** — any id present in your cached file but
   absent upstream (your own newer or internal ids) is kept verbatim, never
   erased, and listed as manually maintained.
4. Writes the merged result back to `.entire/models.json` (`entire.fs`) and
   records the refresh via `entire.kv`.
5. Prints a concise summary: models seen, rate changes, local-only preserved.

```
$ entire models-update
Fetching https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json ...
Rate drift detected (2 change(s)):
  gpt-4o  input 5e-06 -> 2.5e-06
  gpt-4o  output 1.5e-05 -> 1e-05
Keeping 1 local-only model id(s) (manually maintained, absent upstream):
  - entire-internal-model
Wrote .entire/models.json — 312 models, 2 rate change(s), 1 local-only preserved (refreshed at session #7).
```

### `entire models-update --check`

Fetch and report drift **without writing**, and **exit non-zero** when anything
drifted. Drop it in CI to fail the build when the committed pricing table no
longer matches upstream.

```
$ entire models-update --check && echo up-to-date
```

`--write` is accepted as the explicit form of the default (write) behavior. A
trailing positional argument overrides the source URL:

```
$ entire models-update https://example.com/my-prices.json
```

### `session_start` nudge

On each session start the plugin emits a single gentle log line **only when the
cache looks stale** — either it has never been fetched, or it has not been
refreshed in a while — pointing you at `entire models-update`. It is quiet on
the common (fresh) path and nudges at most once per session. This is the "so it
never gets forgotten" piece.

Staleness is measured in **sessions since the last refresh**, not days: the
plugin sandbox opens only `base`/`string`/`table`/`math`, so a plugin has no
wall clock (there is no `os` library and event payloads carry no timestamp). A
durable logical session counter in `entire.kv` is the honest staleness signal
achievable with only the `http` + `fs` capabilities. Day-based staleness (and a
real "last updated" timestamp) would need the host to expose a clock to Lua —
see the plugin's own comments.

## Required capabilities

| Capability | Used for                                                  |
| ---------- | --------------------------------------------------------- |
| `http`     | fetching the upstream pricing JSON (`entire.http.get`)    |
| `fs`       | reading/writing `.entire/models.json` (`entire.fs.*`)     |

`entire.kv` (the session clock + refresh marker) needs no capability.

## Enabling it

Installing a plugin only places files; it stays inert until you allow-list it.

```bash
entire plugin install ./examples/plugins/models-updater
```

Then grant it in settings. A **user-installed** plugin may be enabled from
either `.entire/settings.json` or `.entire/settings.local.json`:

```json
{
  "plugins": {
    "models-updater": { "enabled": true, "capabilities": ["http", "fs"] }
  }
}
```

A **repo-local** plugin (shipped under `.entire/plugins/`) can only be enabled
from your personal, uncommitted `.entire/settings.local.json` — a committed team
`.entire/settings.json` can never auto-run it. See the trust model in
[docs/architecture/plugins-lua.md](../../../docs/architecture/plugins-lua.md).
