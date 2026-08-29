# Global Tracking

Global tracking lets Entire capture agent sessions in every git repository on
a machine without running `entire enable` in each one. It is configured only
in the user's settings file — there is no CLI flag that turns it on and no
wizard — and it is built from two inputs that already exist: the repository's
own `.entire/settings*.json` and the user's `~/.config/entire/settings.json`.
Nothing under `.git/` is a policy record; the only files Entire writes there
in global mode are runtime data and one stamp.

## The settings file

`~/.config/entire/settings.json` (or `$ENTIRE_CONFIG_DIR/settings.json`) is
decoded with **per-block strictness**: each block this binary knows (`global`,
`redaction`) rejects unknown keys — an unknown key inside one, or malformed
JSON, turns the tier off machine-wide (fail closed; an older binary must never
misread recorded consent or an executable name) and `entire doctor` says so —
while unknown **top-level** blocks are ignored and preserved across writes, so
a newer `entire` on the same machine can add a block without switching the
tier off for older binaries. New features add top-level blocks, not keys
inside existing ones (`repopolicy.userSettingsBlocks` is the registry).

```json
{
  "global": {
    "enabled": true,
    "exclude_paths": ["~/scratch/**"],
    "exclude_paths_exact": ["~"],
    "exclude_origins": ["github.com/acme/private-*"],
    "trust_all": false,
    "trusted_origins": ["github.com/acme/widgets"],
    "trusted_paths": ["/Users/me/code/notes"]
  },
  "redaction": {
    "openai_privacy_filter": {
      "command": "/opt/opf/bin/opf",
      "timeout_seconds": 120,
      "prompt_default": "ask"
    }
  }
}
```

- `enabled` — both `true` and `false` count as *configured*; an absent block is
  *unconfigured* (zero cost, no user-level hooks).
- `exclude_paths` — `~`-expanded doublestar globs against the worktree root (a
  bare directory pattern excludes its subtree). `exclude_paths_exact` matches
  one root only, no cascade — the way to exclude `$HOME` as a dotfiles repo
  without excluding everything under it. `exclude_origins` — globs against the
  origin URL normalized to `host/owner/repo`. An unusable pattern, or an origin
  that is present but cannot be normalized, deactivates the tier for that repo
  (fail closed).
- `trust_all`, `trusted_origins`, `trusted_paths` — checkpoint-egress consent
  (below). `entire trust` writes these; users may edit them by hand.
- `redaction.openai_privacy_filter` — how the OpenAI Privacy Filter runs on
  *this machine*: `command` (the executable; honored from here with no
  ownership probe, because no repository can deliver content into this file),
  `timeout_seconds`, `prompt_default`. Whether OPF runs and what it redacts
  (`enabled`, `categories`) is team policy and stays in the repository's
  `.entire/settings.json`; those keys are unknown here and fail closed. See
  [Security & Privacy](../security-and-privacy.md#why-command-is-user-only).

## Activation (derived, never recorded)

`repopolicy.ClassifyRepoPolicy` derives one `RepoPolicy` per process from the
repository's own settings files first and the user file second
(`cmd/entire/cli/settings/repopolicy/policy.go`):

| Repo settings | User `global.enabled` | Result |
| --- | --- | --- |
| project `settings.json` (default enabled) or `settings.local.json` with an explicit `enabled` key | any | **Local** activation if enabled; an explicit `enabled: false` is a veto that also blocks the tier. While `global.enabled` is `true`, the user's exclude lists outrank a committed `settings.json` (it is repository content and arrives by cloning) — only an untracked `settings.local.json` with `enabled: true`, the developer's own action on this clone, still activates an excluded repo |
| none | `true`, repo not excluded | **Global** activation |
| none | `false`, absent, or repo excluded | inactive |

This is main's `IsSetUpAndEnabled` contract for repo-enabled repos, with one
deliberate refinement: a `settings.local.json` written by an unrelated feature
(investigate config, …) without an `enabled` key does not pin a globally
tracked repo into local mode. Committed `.entire/settings.json` activates a
fresh clone exactly as it does today — consent for *egress* is a separate
question (below), so cloned content never opens the sync path by itself. A
*tracked* (or symlinked) `settings.local.json` is ignored wholesale, as the
merged loader ignores it: a clone cannot force activation, or bypass the user's
exclusions, by shipping a "local" file (`repopolicy.LocalSettingsTrusted`,
installed by the `settings` package).

Hooks classify once (`prepareHookPolicy`) and carry the snapshot on `ctx`; a
repo-enabled repo whose settings the full loader rejects (`ErrScannerConfig`)
stays inactive for hooks, as on main. Classification errors fail closed for
Entire's work and never fail the user's git or agent operation.

## Runtime layout (a function of activation source)

`RepoPolicy.RuntimeRoot()` decides where `.entire/{metadata,logs,tmp}` live:

- **Local** → `<worktree>/.entire/…` (main's layout, unchanged).
- **Global** → `<git-common-dir>/entire/worktree/<worktree-hash>/…` — nothing in
  the checked-out tree, so `git status --porcelain` stays byte-clean; linked
  worktrees get separate roots.

`paths.AbsPath` routes only those three prefixes; configuration
(`.entire/settings*.json`) always resolves to the literal worktree. A repo that
was captured globally and is later `entire enable`d switches to the worktree
layout for new activity; its earlier git-side runtime data is left in place
(no migration — `full.jsonl` is regenerated each Stop, and session state lives
in `.git/entire-sessions` regardless). `disable --uninstall` removes it.

`entire disable` in a globally tracked repo writes nothing: there is no
repo-level setup to disable, and a veto file would be the tier's first worktree
footprint. It points at `exclude_paths` (or `global.enabled: false`) instead.
Repo-enabled repos keep today's disable behavior.

## Lazy per-repo setup

The first hook in a globally tracked repo (`strategy.MaybeEnsureGlobalSetup`,
from both the agent-hook and git-hook routes) installs Entire's git hooks and
ensures the checkpoint metadata ref. Git-hook presence is re-checked on every
hook (a file read), so deleted or outdated hooks come back on the next
activity; the ref step is guarded by a `primary-ref-ready` stamp in the
runtime root. When `core.hooksPath` resolves inside the worktree (a committed
`.husky/`), the hooks half is skipped — global tracking never writes into the
checked-out tree; agent-side capture still works and `entire doctor` explains
it. Every failure logs at Debug and returns.

## User-level agent hooks

Claude Code (`~/.claude/settings.json`) and Gemini CLI
(`~/.gemini/settings.json`) support user-level hooks with the same inventory
and command form as the repo-level ones, so each agent deduplicates them.
`globalPostRun` (root `PersistentPostRun`, plus the installer) reconciles them
against the tier on every foreground `entire` command: installed while
`global.enabled` is true for agents present on the machine, removed when the
tier is configured but off, untouched while unconfigured. That is how a hand
edit of the settings file takes effect without a dedicated command. Hook
processes are hidden commands and never reconcile.

## Checkpoint egress and trust

`settings.CheckpointEgressAllowed` (`repopolicy.DecideEgress`) is the single
predicate for whether checkpoint data may leave the machine:

- Tier off or unconfigured → an active repo syncs as on main.
- Tier on → **every** active repo, repo-enabled or globally tracked, needs
  `trust_all`, every URL of the **elected checkpoint sync remote** normalized
  to a key in `trusted_origins`, or its canonical path in `trusted_paths`.
  Consent is keyed on where checkpoints actually go, not on `origin`: since
  the sync-remote election (`checkpoint_push_remote` → captured election →
  `origin` → sole → first remote), `origin` may be an unpushable base repo or
  a local mirror while checkpoints go to a fork, so `repopolicy.ResolveSyncRemote`
  — installed by `strategy/trust_sync_remote.go` at init, the same seam as
  `LocalSettingsTrusted`; the leaf's default is `origin` — supplies the
  remote whose fetch+push URLs become the keys (the dedicated
  `checkpoint_remote` store's derived URL in that mode). A pre-push that is
  about to *capture* a new remote re-derives the policy with that remote
  pending, so the electing push asks about the destination it is opening; a
  later re-election changes the key and re-asks (new destination, new
  consent). Because the election is persisted only once checkpoints land on
  the captured remote, a *non-interactive* push that is about to capture a
  new remote holds with a message naming `entire trust --remote <name>` —
  plain `entire trust` would record consent for the still-elected remote and
  the next push would hold again. `trusted_origins` keeps its historical name. The path key applies
  when there is no remote **or** when any of the remote's URLs does not
  reduce to `host/owner/repo` (a bare local path, `file://`): the identity
  flips to the path as a whole — partial keys would fail open on a multi-URL
  push — so such a repo can still be trusted, as "this folder only".
  `trust_all` is checked before any identity is resolved: consent for every
  repo must not depend on being able to name this one. An election error
  (unreadable settings, `checkpoint_push_remote` naming a missing remote)
  disables sync and the gate fails closed with it. The prompt, `entire trust`,
  `enable`, and `status` name the remote (`status --json`:
  `global_tracking.sync_remote`). Consent is evaluated for the remote a push
  actually targets: `PushQueuedCheckpointRefs` (`doctor migrate --remote`)
  re-derives the policy for that remote when it is a configured remote other
  than the election, so a trusted election never releases refs to an
  unconsented destination. `disable --uninstall` locates leftover global-mode
  runtime data from the repository's identity (`Repository.GlobalRuntimeRoot`),
  not its current policy, so it is removed even after the tier was turned off
  or the repo excluded.

Consent is recorded three ways, all into the same file: `entire enable`
records it for the repo being enabled; the pre-push prompt offers **Yes**
(this repo / all clones of its origin), **Not now** (re-ask next push), and
**Always** (`trust_all`) — through the terminal, never Git's stdin, and never
implicitly in accessible mode; `entire trust [--revoke] [--remote <name>]` grants or withdraws
it. Revoke removes the repo's current origin keys and its current path; an
entry from a *previous* origin URL is not recognized and stays. A hold never
blocks the user's own push: the branch lands, checkpoint data stays local (the
refs backend keeps its queue), one stderr line explains, and the first trusted
push drains the backlog. The SessionStart banner names the hold in an
untrusted repo, and in a globally tracked repo that will sync only because
`trust_all` is on it says so and points at `exclude_paths` — captured *and*
synced in silence is the one state the user never chose for that repo;
per-repo consent and repo-enabled repos stay quiet. Both egress entry points are gated
(`ManualCommitStrategy.prePush` and `PushQueuedCheckpointRefs`).

## Status and doctor

`entire status` prints the global-tracking line (on/off, agents covered, why
this repo is inactive) and the per-repo trust state; `--json` exposes
`global_tracking.{enabled, settings_path, activation_source, active_here,
inactive_reason, trust_state, trust_source, held_checkpoints}`. A repo with
no repo-level settings that the tier captures renders as enabled — header
`● Tracked globally · branch <b>`, active sessions, the agent-help hint;
`--json` reports `enabled: true` with `activation_source: "global"` and no
`error` — never as `○ not set up (run entire enable)`, which would tell the
user to do the one thing the tier makes unnecessary and would read to an
agent as Entire being off while its session is captured. An excluded or
unclassifiable repo, or a tier that is off, keeps the not-set-up shape. A repo
that *has* repo-level setup but is carved out by the user's exclude lists
renders `○ Excluded on this machine` (and `--json` `enabled: false`,
`inactive_reason: "repo_excluded"`) rather than `● Enabled` — status sides with
the classifier, not the raw file — and `entire enable` in such a folder says
why it stays inactive and points at `entire enable --local` for a per-clone
override.
`held_checkpoints` (and the enabled repo's unpushed count) count checkpoint
commits only: the orphan `Initialize metadata ref` commit that seeds
`entire/checkpoints/v1` is excluded, so a fresh repo does not read "1 held"
before any session was captured. `entire doctor`
reports an unreadable user file, missing or unverifiable user-level hooks
(report-only — the post-run installs them), unusable exclude patterns, an
origin that cannot be normalized, a held repo, and a globally tracked repo
whose git hooks are absent (drift, deliberate `core.hooksPath` skip, or an
unresolvable hooks dir).

## Asking about another repository

A hook runs in one worktree, but session binding (trail 1018) has to ask
whether a repository the agent merely *touched* is one Entire captures. That
is `settings.IsActiveAtRoot(ctx, root)` → `repopolicy.ClassifyActivationAt`:
the same precedence as above (repo settings, then the tier with its exclusions
and vetoes) for an explicit root, with **no egress decision** — the checkpoint
sync remote election is scoped to the current worktree, so deciding trust for
another root would read the wrong repository's remotes. File presence
(`IsSetUpAtRoot`-style checks) is the wrong question once the tier is on: a
globally tracked repo has no settings files, and a check like that silently
excludes every such repo from cross-repo attribution.

Key files: `settings/repopolicy/{types,policy,activation,repository,trust,user_settings}.go`,
`settings/{global,trust}.go`, `paths/invisible.go`, `hook_policy.go`,
`strategy/global_setup.go`, `strategy/trust_prompt.go`,
`strategy/manual_commit_push.go`, `global_warn.go`, `trust_cmd.go`,
`agent/user_hooks.go`, `agent/{claudecode,geminicli}/hooks_user.go`.
