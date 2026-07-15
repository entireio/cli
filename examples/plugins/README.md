# Example Lua plugins

No-build-step plugins for the Entire CLI. See
[docs/architecture/plugins-lua.md](../../docs/architecture/plugins-lua.md) for
the full reference, and [`entire.lua`](entire.lua) for editor type hints
(EmmyLua / lua-language-server).

| Plugin                                     | Shows                                                    |
| ------------------------------------------ | ------------------------------------------------------- |
| [`checkpoint-notify`](checkpoint-notify)   | observer hooks, `entire.kv`, a command, optional `exec`  |
| [`models-updater`](models-updater)         | a command using the `http` + `fs` capabilities           |

## Try one

```bash
entire plugin install ./examples/plugins/checkpoint-notify
# then allow-list it (installing alone does not run it):
#   .entire/settings.json → "plugins": { "checkpoint-notify": { "enabled": true, "capabilities": ["exec"] } }
entire notify-stats
```

## Security

Enabling a plugin runs its code in your `entire` process. Plugins are inert
until allow-listed, repo-local plugins never auto-run, and privileged APIs are
capability-gated — but the allow-list is the trust boundary. Only enable plugins
you trust. See the trust model in the architecture doc.
