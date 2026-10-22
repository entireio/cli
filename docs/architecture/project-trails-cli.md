# Project trails CLI

Project trails are the only trail model the CLI offers; `ENTIRE_PROJECT_TRAILS`
is no longer read. The legacy repository-scoped tree is still in the code,
reachable only through `newTrailCmdForMode(false)` in tests, until it is
deleted.

**Selector migration.** A bare number is now a project trail number. Under the
old default, `entire trail merge 7` (or `approve 7`, `finding list 7`) meant
repository trail #7; it now means project trail #7, which a command resolves to
that trail's sole open change, in whichever repository it is. To name one
repository's work, pass `<repo>/<number>` (`cli/7`) with `--project` when the
project is not origin's owner, or `--branch`.

## Code layout

The model is chosen in one place, `newTrailCmdForMode` (`trail_cmd.go`), and
by the first-turn injection, both through `projectTrailsEnabled`.

- Commands whose meaning differs are separate trees: legacy `show`, `list`,
  `create`, `update`, `delete`, `comment` in `trail_*.go`; project `show`,
  `list`, `create`, `update`, `link`, `unlink`, `comment` in
  `project_trail_*.go`.
- Commands that act on one repository branch in both models (`checkout`,
  `resume`, `finding`, `watch`, `approve`, `approvals`, `merge`) are
  shared. Each receives a `trailMode` (`trail_mode.go`) that resolves the
  selector to that branch and supplies the model's help text.

Shared command bodies do not check the mode. Retiring legacy means deleting
`legacyTrailMode`, the legacy tree, and the legacy argument of each
`trailMode.help` call, then renaming `project_trail_*.go` to `trail_*.go`.

## Usage

```sh
entire trail list --project gh/entireio --json
entire trail list --project gh/entireio --limit 50 --page-token '<nextPageToken>'
entire trail show 42 --project gh/entireio
entire trail create --title 'Cross-repository work'
entire trail create --title 'Repository-scoped plan' --no-branch
entire trail create --repo gh/entireio/cli --title 'Repository-scoped plan' --no-branch
entire trail create --project gh/entireio --title 'Unscoped plan' --no-branch
entire trail update 42 --body 'Updated intent'
entire trail link 42 --repo gh/entireio/api --branch feature/api
entire trail unlink 42 --repo gh/entireio/api --branch feature/api
entire trail finding list 42 --repo gh/entireio/cli --branch feature/work
entire trail finding list cli/1503 --project gh/entireio
entire trail comment add --trail 42 --body 'Cross-repository plan'
```

Selectors are project-local numbers or trail ULIDs, or `<repo>/<number>` for one repository's branch work, the form a change's web URL ends in (`…/trails/2074/changes/cli/1503` → `cli/1503`). The repository is looked up in `--project`, or origin's owner without it; the work's parent names the trail, so no collection lookup is needed, and merged work whose branch is gone still resolves. Branch-level commands act on that work; `show`, `update`, and `comment` act on its trail. It cannot be combined with `--branch`, and an explicit `--repo` must name the same repository. Legacy mode does not accept it: there a slash-containing selector is a branch name and a bare number is already repository-local work. Without a selector, commands follow the current branch's parent. `--branch` selects branch work. Given a project trail, a command acts on its only open change, in any repository; with several, it lists them as `<repo>/<number> - <title>` with each reviewer's latest decision (`approved by …`, `changes requested by …`, or `no reviews yet`) and names the command to re-run with one; for `approve` that is the first change nobody has approved. Review state is read only when listing, from each change's repository approvals route, at most 20 lookups within 3 seconds; a change whose lookup fails is listed without state. `--repo` narrows the candidates to that repository. Read commands first prefer the change on the checked-out branch; `approve`, `unlink`, and `merge` never do, since they change the work they select. Checkout and resume consider only this clone's repository and list changes elsewhere as needing another clone. Findings, approvals, and watch apply to the selected branch; comments apply to the whole trail. Project mode has `link`/`unlink`, not `delete` or a `change` subgroup. `merge` merges one change through its repository's merge route; merging a project trail never merges its other changes. Messages name the change as `change <repo>/<number> of trail #N`, and every retry hint passes `<repo>/<number> --project <forge>/<project>`: a bare number there means a project trail, and a change selector without `--project` resolves in origin's owner, which a fork does not share. `--json` reports `change`, `project`, and `trail` alongside the change's repository-local `number`.

## API and safety

- List requires `--project` and reads `GET /api/v1/trails?projectId=<ID>` from
  Core's assigned project cell. `--repo` only filters within that project.
  Server pagination tokens pass through unchanged. A numeric selector is one
  `GET /{host}/{project}/trails/{number}` (the detail route accepts a
  project-local number); later requests use the returned ULID. JSON preserves counts, groups, jurisdiction, and capabilities.
- Explicit selectors normally resolve through Core `/projects/resolve/{host}/{project}`.
  Creation and `link` instead use the destination repository's placement and
  ownership, so repo-only writers need no project inspection. Branch discovery
  follows the backing row's parent. None falls back to legacy semantics or
  another cell on failure.
- Branch operations verify project membership before using repository-local
  IDs for subresource requests. Permission-filtered detail may be incomplete.
- Project commands retain the standard login and repository-onboarding hints.
  Branch reads refresh the local trails-enablement cache only when their
  repository is known to be this clone; foreign change selectors never do.
  `link` takes a project trail number or ULID, not `<repo>/<number>`.
- `create` first creates a Trail scoped to the current or explicit `--repo`
  repository (`repoIds` plus verified `project_id`, no initial Changes or
  idempotency header). It then creates/links a branch-backed Change using the
  parent's ETag and a Change idempotency key. Both steps support repo writers;
  creating a Change cannot extend scope without project management.
  `--no-branch` skips the Change. `--no-branch --project` without `--repo`
  instead creates unscoped intent, which requires project management.
- Project intent has only `open` and `closed` lifecycle states. `draft` is
  rejected locally by project create/update/link/list; legacy branch-work
  lifecycle values are unchanged.
- Assignee add/remove flags compare canonical account identities, not a login
  against the `ulid:<account id>` references returned by project reads. GitHub
  logins resolve through the active control plane before the conditional PATCH;
  known account IDs need no lookup. A failed lookup aborts the entire write.
  `--assignee=` still clears the list without resolving identities.
- Project discussion write JSON includes only resources the server returned:
  reply/edit can contain a message without a discussion. ETags remain internal.
- Finding creation derives a bounded, deterministic review-start
  `Idempotency-Key` from the finding client IDs. The server replays that review
  for 24 hours; its review-local `client_id` checks then deduplicate the findings.
  Retrying `finding add --client-id <key>` with the same fields reuses both
  resources, including after a batch failure. A changed payload with the same
  key conflicts; use a new key for new work. In-flight conflicts are returned
  to the caller, never retried without the key.
- Updates and linking require the resource's ETag; a 412 never triggers an
  unconditional retry. With a branch, `--idempotency-key` applies only to the
  Change, not to parent creation. If linking fails, the command reports the
  created Trail ID and a complete, shell-quoted `trail link` retry command,
  including the resolved repository (even when originally implicit), project,
  branch action, base, creation fields and retry key. Copy that command instead
  of rerunning `trail create`; it preserves the Change request hash. The hint
  names the target shell: POSIX on Unix, PowerShell on Windows.
  A successful link is followed by a fresh parent read for the final output.
  Neither stage retries automatically or deletes the parent/branch on failure.
  After an uncertain parent-create result, inspect Trails before creating again.
  For intent-only creation, an explicit key retains the manager-only idempotent
  request; the CLI never downgrades a denied request.
- Local create publishes the branch but never commits, force-pushes, or deletes
  it on failure. `--repo`, `--no-branch`, and `--branch-action create` skip local
  publication. Unlink preserves the branch and its work.

Project-wide streaming and batch multi-repository creation are not supported.
