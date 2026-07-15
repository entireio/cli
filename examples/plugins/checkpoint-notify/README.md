# checkpoint-notify (example Lua plugin)

A minimal observer plugin demonstrating hooks, durable `entire.kv` storage, a
contributed command, and an optional capability-gated `entire.exec` call.

## Install

```bash
entire plugin install ./examples/plugins/checkpoint-notify
```

## Enable

Installing only places files — the plugin stays inert until you allow-list it.
Add to `.entire/settings.json`:

```json
{
  "plugins": {
    "checkpoint-notify": { "enabled": true, "capabilities": ["exec"] }
  }
}
```

The `exec` capability is optional: without it the desktop notification is
skipped (the call is guarded with `pcall`) and the plugin still logs and counts.

## Use

```bash
entire notify-stats   # prints how many checkpoints the plugin has observed
```

Checkpoint/commit activity is logged to `.entire/logs/` (set `log_level` to
`DEBUG` for the full trace).
