# Auto-Update

After the Entire CLI's daily version check detects a newer release, the
standard notification is followed by an interactive Y/N prompt to run the
installer.

## UX

```
A newer version of Entire CLI is available: v1.2.3 (current: v1.0.0)
Run 'brew upgrade --cask entire' to update.

? Install the new version now?  (Y/n)
```

- Declining simply skips the upgrade. The 24-hour version-check cache means
  the prompt will not reappear until the next day.
- The installer command is whatever `versioncheck.updateCommand(current)`
  returns — `brew upgrade --cask ...`, `mise upgrade entire`,
  `scoop update entire/cli`, or the curl-pipe-bash fallback — including the
  `--channel nightly` variant for nightly builds.
- stdin, stdout and stderr are wired through so the user sees installer
  output and can answer any password prompt.

## Guardrails

The prompt is skipped silently when any of the following holds:

- stdout is not a terminal.
- `CI` environment variable is set.
- `ENTIRE_NO_AUTO_UPDATE` environment variable is set (kill switch).

In those cases the user still sees the existing notification line pointing
to the installer command.

## Not in scope

- No "silent auto-install" mode — the prompt is always interactive.
- No persisted preference — the kill-switch env is the escape hatch.
- No dedicated `entire auto-update` / `entire update` subcommands; the
  notification + prompt replaces them.
