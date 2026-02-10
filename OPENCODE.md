## OpenCode Integration (Preview)

Entire can capture OpenCode sessions via a lightweight plugin that forwards events to `entire hooks opencode`.

### What’s supported

- Event-driven hooks (no polling):
  - `prompt-submit` when OpenCode sends `chat.message`
  - `stop` when OpenCode reports `session.status` with `status.type = "idle"`
- Auto-installable plugin at `.opencode/plugins/entire.js` (installed by `entire enable --agent opencode`).

### Installing

```bash
entire enable --agent opencode
```

- Creates/updates `.opencode/plugins/entire.js` with the bridge plugin.
- Installs git hooks and `.entire/` settings like other agents.

### How it works

The plugin listens to OpenCode `event` callbacks and spawns:

- `entire hooks opencode prompt-submit` with JSON `{"session_id": <id>, "prompt": <text>}`
- `entire hooks opencode stop` with JSON `{"session_id": <id>}` when the session becomes idle.

### Current limitations

- Transcript capture is stubbed (no transcript copy yet). New/deleted files are recorded; modified files rely on future transcript parsing.
- Resume command is best-effort: `opencode --session <id>`.

### Uninstalling

```bash
entire disable --uninstall --force
```

Removes the plugin file and Entire metadata (git hooks, `.entire/`, session state). To remove just the plugin, delete `.opencode/plugins/entire.js`.
