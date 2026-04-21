# Checkpoint Commit Signing

Entire can sign checkpoint commits (shadow branch, metadata branch) using the same key configured for regular git commits. Signing is **best-effort**: if the signer is unavailable or fails, the commit is created unsigned and a warning is logged to `.entire/logs/`.

## Requirements

All of the following must be true for checkpoint commits to be signed:

1. **`commit.gpgsign = true`** in git config at **global** or **system** scope. 
2. **A supported signer is available**: SSH (via `ssh-agent`) or GPG.
3. **The Entire setting `sign_checkpoint_commits`** is either `true` or not set (defaults to `true`).

## Supported Signing Formats

| Format | Config value (`gpg.format`) | Notes |
|--------|----------------------------|-------|
| GPG | `openpgp` (default) | Uses the GPG keyring |
| SSH | `ssh` | Requires a running `ssh-agent` (`SSH_AUTH_SOCK`) |

The format is determined by the `gpg.format` git config key. When unset, GPG is used.

## Disabling Signing

Users may opt-out from checkpoint commit signing. This does not affect signing of regular user commits.

Add to `.entire/settings.json` (shared with the team) or `.entire/settings.local.json` (personal, gitignored):

```json
{
  "sign_checkpoint_commits": false
}
```

## Best-Effort Behavior

`SignCommitBestEffort` never blocks a commit from being created. If any step in the signing pipeline fails — signer unavailable, encoding error, signing error — the failure is logged and the commit proceeds unsigned. This ensures that:

- Users with hardware tokens that require a touch are not blocked during automated checkpoint saves.
- Temporary `ssh-agent` unavailability does not cause data loss.
- CI environments without signing keys continue to work.
