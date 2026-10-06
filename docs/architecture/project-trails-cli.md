# Project trails CLI

Legacy repository-scoped commands remain the default. Set
`ENTIRE_PROJECT_TRAILS=1` (exact value) to enable project trails; unset it to
return to legacy. Help and agent-help reflect the selected mode.

## Code layout

The model is chosen in one place, `newTrailCmdForMode` (`trail_cmd.go`), and
by the first-turn injection, both through `projectTrailsEnabled`.

- Commands whose meaning differs are separate trees: legacy `show`, `list`,
  `create`, `update`, `delete`, `comment` in `trail_*.go`; project `show`,
  `list`, `create`, `update`, `link`, `unlink`, `comment` in
  `project_trail_*.go`.
- Commands that act on one repository branch in both models (`checkout`,
  `resume`, `finding`, `watch`, `approve`, `request-changes`, `approvals`) are
  shared. Each receives a `trailMode` (`trail_mode.go`) that resolves the
  selector to that branch and supplies the model's help text.

Shared command bodies do not check the mode. Retiring legacy means deleting
`legacyTrailMode`, the legacy tree, and the legacy argument of each
`trailMode.help` call, then renaming `project_trail_*.go` to `trail_*.go`.

## Usage

```sh
export ENTIRE_PROJECT_TRAILS=1

entire trail list --project gh/entireio --json
entire trail list --project gh/entireio --limit 50 --page-token '<nextPageToken>'
entire trail show 42 --project gh/entireio
entire trail create --title 'Cross-repository work'
entire trail create --project gh/entireio --title 'Plan' --no-branch
entire trail update 42 --body 'Updated intent'
entire trail link 42 --repo gh/entireio/api --branch feature/api
entire trail unlink 42 --repo gh/entireio/api --branch feature/api
entire trail finding list 42 --repo gh/entireio/cli --branch feature/work
entire trail finding list cli/1503 --project gh/entireio
entire trail comment add --trail 42 --body 'Cross-repository plan'
```

Selectors are project-local numbers or trail ULIDs, or `<repo>/<number>` for one repository's branch work, the form a change's web URL ends in (`…/trails/2074/changes/cli/1503` → `cli/1503`). The repository is looked up in `--project`, or origin's owner without it; the work's parent names the trail, so no collection lookup is needed, and merged work whose branch is gone still resolves. Branch-level commands act on that work; `show`, `update`, and `comment` act on its trail. It cannot be combined with `--branch`, and an explicit `--repo` must name the same repository. Legacy mode does not accept it: there a slash-containing selector is a branch name and a bare number is already repository-local work. Without a selector, commands follow the current branch's parent. `--branch` selects branch work; ambiguous matches require it. Checkout and resume operate on the local clone. Findings, approvals, and watch apply to the selected branch; comments apply to the whole trail. Project mode has `link`/`unlink`, not `delete` or a `change` subgroup.

## API and safety

- List requires `--project` and reads `GET /api/v1/trails?projectId=<ID>` from
  Core's assigned project cell. `--repo` only filters within that project.
  Server pagination tokens pass through unchanged. A numeric selector is one
  `GET /{host}/{project}/trails/{number}` (the detail route accepts a
  project-local number); later requests use the returned ULID. JSON preserves counts, groups, jurisdiction, and capabilities.
- Explicit selectors resolve through Core `/projects/resolve/{host}/{project}`.
  Branch discovery follows the backing row's parent. Neither falls back to
  legacy semantics or another cell on failure.
- Branch operations verify project membership before using repository-local
  IDs for subresource requests. Permission-filtered detail may be incomplete.
- Updates require the resource's ETag; a 412 never triggers an unconditional
  retry. Create/link print an idempotency key for retries with identical inputs.
- Local create publishes the branch but never commits, force-pushes, or deletes
  it on failure. `--repo`, `--no-branch`, and `--branch-action create` skip local
  publication. Unlink preserves the branch and its work.

Project-wide streaming and batch multi-repository creation are not supported.
