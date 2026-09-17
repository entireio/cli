# Project trails CLI

There is one user-facing entity: **a trail**, representing intent across
repositories and branches. Internal project-trail and repository-work identities
remain separate, but there is no `trail change` subgroup or change selector.

## Intent

```sh
# Namespace defaults to origin. Explicit project targeting works outside a clone.
entire trail list --project gh/entireio --json
entire trail list --project et/widgets --limit 50 --page-token '<nextPageToken>'

# No selector follows the current branch's parent.
entire trail show
entire trail show 42 --project gh/entireio
entire trail update --body 'Updated intent' --status open

# Creation publishes the local branch, then links it with the new intent.
# No separate commit/push is required. On the base branch, derive a new name
# from the title; on a feature branch, use that branch.
entire trail create --title 'Cross-repository work'
# Intent without a branch is explicit.
entire trail create --project gh/entireio --title 'Plan' --no-branch
# Alternatively, ask the server to create the branch without a local push.
entire trail create --title 'Intent' --branch feature/work --base main --branch-action create
```

Selectors are **project-local numbers or project trail ULIDs**, never repository
numbers or branch names. Use `--branch` for a branch. Project status is
`draft`, `open`, or `closed`; `merged` belongs to branch work. There is no trail
deletion command. Show displays intent plus “Repositories and branches,” not a
second class of user-visible entities.

List returns one page: JSON is `{items, nextPageToken}`. `--status` filters that
page locally because the project API has no status filter. Update combines body
and metadata in one conditional PATCH. `--assignee` replaces the list,
`--assignee=` clears it, and `--add-assignee`/`--remove-assignee` modify the read
list. JSON retains backend resource fields and IDs for automation.

## Repository/branch context

```sh
entire trail checkout 42 --branch feature/work
entire trail resume 42 --branch feature/work --no-resume
entire trail finding list 42 --repo gh/entireio/cli --branch feature/work
entire trail approve 42 --branch feature/work
entire trail approvals 42 --branch feature/work
entire trail watch 42 --branch feature/work

# Add another repository/branch to existing intent. No backing ID is needed.
entire trail link 42 --repo gh/entireio/api --branch feature/api
entire trail unlink 42 --repo gh/entireio/api --branch feature/api
```

Context selection is deterministic: explicit `--branch`, then a matching current
checkout, then the only visible branch in the selected repository. Multiple
matches list the branches and require selection; zero matches fail. An explicit
`--repo` never borrows the local branch silently. Checkout and resume operate in
a local clone; checkout rejects `--repo`, and resume treats it as a local-repo
assertion. Approvals and findings affect only the selected branch, not every
repository in the trail. Confirmation text identifies that scope.

Link defaults to the current branch, or accepts `--branch`; `--branch-action`
can be `link` (default) or `create`. Existing work, reviews, and body survive a
link. Linking a branch owned by another trail fails. Link expands repository
scope atomically. Unlink removes membership without deleting the branch or its
work. Both operations require the parent's read ETag.

## Discussions

```sh
entire trail comment list --trail 42 --project gh/entireio
entire trail comment add --trail 42 --body 'Cross-repository plan'
entire trail comment reply '<discussion-id>' --trail 42 --body 'Agreed'
entire trail comment resolve '<discussion-id>' --trail 42
```

Comments use project-wide `/discussions` routes, including edits and deletion
(`--force` required). Discussion updates use the discussion ETag; message
updates/deletion use that message's ETag, not its parent's. Code-review comments
remain under `finding`. `watch` currently streams the selected repository/branch,
not an aggregation of project discussions and all repositories.

## Routing and safety

- Creation and explicit selectors use Core `/projects/resolve/{host}/{project}`
  and its returned `project.apiUrl`, with the stored `primaryProcessingCell` and
  `region`. No separate catalog lookup is needed, including for hidden assigned
  clusters. Routes use the canonical public reference, not an internal GitHub
  project storage name. Missing fields (older Core), an unassigned project, or
  an unavailable URL fail closed; a catalog or region default is not a fallback.
- Branch discovery follows the backing row's `parent` through the catalog
  (parent references currently carry a cell ID, not an API URL), without
  requiring a project-collection lookup. A missing parent may be hidden,
  stale, or unresolved; it is never a reason to fall back to legacy semantics.
- Branch operations validate membership through the owned project route before
  using the repository-local ID/number for findings, approvals, or streams.
- Parent routing never falls back to a repository or jurisdiction-default cell.
- Permission-filtered detail may be partial. JSON retains `isPossiblyPartial`;
  text warns that the repository/branch list may be incomplete.
- Missing ETags refuse protected mutations; a 412 never causes an unconditional
  retry. Creation and link print an Idempotency-Key before sending the request;
  retry with that key and identical inputs. Local `create` publishes the branch
  before the API request, running push hooks and preserving the local tip. It
  never commits uncommitted work, force-pushes, or deletes branches to compensate
  for an API failure. A rejected push prevents the creation request. `--repo`
  targets remote work without publishing the local clone; `--no-branch` and
  `--branch-action create` also skip local publication.

Project-wide streaming, batch multi-repository creation inputs, and direct
project metadata/scope editing remain follow-up work.
