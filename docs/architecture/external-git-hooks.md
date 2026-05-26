# External Git Hooks Backend

Entire's default mode (`direct`) writes wrapper scripts into `.git/hooks/` so
git invokes Entire on the canonical events (commit, push, rebase). That works
out of the box in plain repositories, but it conflicts with projects that
already use a hook manager — Husky, Rush, Lefthook, pre-commit — because the
manager owns either `.git/hooks/` directly or via `core.hooksPath`. Whoever
writes last wins, and Entire's hooks silently disappear after the next
`npm install`.

The `external` backend solves this by inverting the responsibility: Entire
stops writing hooks entirely and instead reads them from a user-managed
directory. The user wires the Entire dispatcher into their own scripts; Entire
verifies the wiring via a marker comment but never touches the files.

## Why

`.git/hooks/` is a contested space:

- Husky stages user scripts in `.husky/` and links them from `.git/hooks/`
  on every `npm install`. Anything Entire wrote there gets overwritten.
- Rush ships hook templates in `common/git-hooks/` and copies them to
  `.git/hooks/` during `rush install`.
- pre-commit / Lefthook / Overcommit follow similar patterns.

Direct mode treats this conflict with a backup chain (`<hook>.pre-entire` +
inline chaining), which works for plain installs but breaks when the hook
manager itself re-runs and overwrites the chained script. External mode
sidesteps the whole class of bugs by yielding ownership of the hook files
back to whoever was managing them first.

## Configuration

Add a `git_hooks` block to `.entire/settings.json`:

```json
{
  "enabled": true,
  "git_hooks": {
    "backend": "external",
    "external_dir": ".husky"
  }
}
```

`external_dir` is repo-relative. Common values:

| Hook manager | `external_dir` |
| --- | --- |
| Husky v9 | `.husky` |
| Rush | `common/git-hooks` |
| Custom | any repo-relative path you control |

Validation rules:

- `backend` must be `"direct"` or `"external"`. Typos are rejected at
  load time so silent misconfiguration is impossible.
- `external_dir` is required when `backend = "external"`.
- `external_dir` must be repo-relative (no leading `/`, no `..` segments).

`external_dir` existence is NOT checked at parse time — that is a runtime
health concern surfaced by `entire enable` and `entire doctor`.

## User contract

In external mode, Entire considers a hook "installed" iff
`<external_dir>/<hook>` contains the marker comment `# Entire CLI hooks`
somewhere in the file. Detection reads only this marker; the actual
dispatch invocation is the user's responsibility.

The five managed hooks Entire wants to see (these match direct-mode
installs byte-for-byte except they live in your directory):

```
<external_dir>/prepare-commit-msg
<external_dir>/commit-msg
<external_dir>/post-commit
<external_dir>/post-rewrite
<external_dir>/pre-push
```

Each script must:

1. Be executable (`chmod +x`).
2. Contain the marker comment `# Entire CLI hooks`.
3. Invoke Entire's dispatcher with the matching event name.

The recommended dispatch invocations (matching what direct mode installs):

```sh
# .husky/prepare-commit-msg
#!/bin/sh
# Entire CLI hooks
entire hooks git prepare-commit-msg "$1" "$2" 2>/dev/null || true
```

```sh
# .husky/commit-msg
#!/bin/sh
# Entire CLI hooks
entire hooks git commit-msg "$1" || true
```

```sh
# .husky/post-commit
#!/bin/sh
# Entire CLI hooks
entire hooks git post-commit 2>/dev/null || true
```

```sh
# .husky/post-rewrite
#!/bin/sh
# Entire CLI hooks
entire hooks git post-rewrite "$1" 2>/dev/null || true
```

```sh
# .husky/pre-push
#!/bin/sh
# Entire CLI hooks
entire hooks git pre-push "$1" || true
```

You can intermix these calls with your existing hook content — the marker
just needs to be present somewhere in each file. Add error suppression,
guard against missing `entire` binaries, or call other tooling before/after
the dispatch as needed.

## What Entire does and doesn't do

**Does:**

- Detect marker presence in `<external_dir>/<hook>` via
  `IsGitHookInstalled` (read-only).
- Report `external_dir` health in `entire doctor` (exists/missing).
- Abort `entire enable` with the instructional message when
  `external_dir` doesn't exist on disk.
- Silence the hook-manager warning that fires in direct mode (you've
  already opted in to coexistence).

**Does not:**

- Write anything to `external_dir` or `.git/hooks/` in external mode.
- Append, edit, or normalize user-owned hook scripts.
- Validate the exact dispatch command string — only the marker is
  checked. If you change the invocation form, detection still passes.
- Remove hooks on `entire disable` — `RemoveGitHook` is a no-op in
  external mode regardless of what files exist.

## Compatibility

Tested layouts:

- **Husky v9** — `.husky/_/<hook>` (Husky-generated stubs, untouched) +
  `.husky/<hook>` (user scripts with the marker).
- **Rush** — `common/git-hooks/<hook>` with the marker.
- **Generic** — any repo-relative directory you control.

## Migration: direct → external

1. Pick or create the hook directory your manager controls
   (e.g. `.husky/` after `npx husky init`).
2. For each of the five managed hooks, create a script in that directory
   containing the marker comment and the dispatch invocation (see
   examples above). `chmod +x` each script.
3. Add the `git_hooks` block to `.entire/settings.json`:
   ```json
   { "git_hooks": { "backend": "external", "external_dir": ".husky" } }
   ```
4. Run `entire doctor` — expect
   `✓ External git hooks: external_dir ".husky" exists`.
5. The previously-installed `.git/hooks/<hook>` files from direct mode
   are now stale. Entire will leave them alone (detection-only contract);
   remove them manually if your hook manager doesn't already do so.

## Migration: external → direct

1. Remove the `git_hooks` block from `.entire/settings.json` (the
   default is `direct`).
2. Run `entire enable --force` to reinstall hooks into `.git/hooks/`.
3. Decide whether to keep the scripts in your external directory. Entire
   no longer cares about them but your hook manager probably still
   invokes them, so empty/delete them if they'd duplicate Entire's
   direct-mode work.

## Troubleshooting

**"external_dir not found in repo root"** — the directory you named in
`external_dir` doesn't exist on disk. Either create it (with the hook
scripts inside) or set `backend` back to `direct`.

**"Marker present but hook doesn't fire"** — check that the script is
executable (`chmod +x`) and that your hook manager actually links it
into `.git/hooks/` (Husky v9 does this via `.husky/_/`; Rush does it on
`rush install`).

**"Husky stubs in `.husky/_/` are being detected"** — they aren't.
Detection looks at `<external_dir>/<hook>`, not `<external_dir>/_/<hook>`.
The `_/` directory is Husky's plumbing, ignored by Entire's marker check.

**`entire enable` says "Entire is already enabled" without the
external hint** — your `external_dir` does not exist; the precondition
check at the top of `enable` aborts before the success surface runs. The
error message lists the required scripts inline.
