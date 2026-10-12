# Project trails CLI

Legacy repository-scoped commands remain the default. Set
`ENTIRE_PROJECT_TRAILS=1` (exact value) to enable project trails; unset it to
return to legacy. Help and agent-help reflect the selected mode.

## Code layout

The model is chosen in one place, `newTrailCmdForMode` (`trail_cmd.go`), and
by the first-turn injection, both through `projectTrailsEnabled`.

- Commands whose meaning differs are separate trees: legacy `show`, `list`,
  `create`, `update`, `delete`, `merge`, `comment` in `trail_*.go`; project `show`,
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

Selectors are project-local numbers or trail ULIDs, or `<repo>/<number>` for one repository's branch work, the form a change's web URL ends in (`…/trails/2074/changes/cli/1503` → `cli/1503`). The repository is looked up in `--project`, or origin's owner without it; the work's parent names the trail, so no collection lookup is needed, and merged work whose branch is gone still resolves. Branch-level commands act on that work; `show`, `update`, and `comment` act on its trail. It cannot be combined with `--branch`, and an explicit `--repo` must name the same repository. Legacy mode does not accept it: there a slash-containing selector is a branch name and a bare number is already repository-local work. Without a selector, commands follow the current branch's parent. `--branch` selects branch work; ambiguous matches require it. Checkout and resume operate on the local clone. Findings, approvals, and watch apply to the selected branch; comments apply to the whole trail. Project mode has `link`/`unlink`, not `delete`, `merge`, or a `change` subgroup. Server-side `merge` remains a legacy-only command until it uses project-aware branch selection.

## Local agent logs

`trail logs publish [<trail>] --share-with-trail --transcript-agent <agent>` reads explicitly selected agent JSONL from stdin;
`trail logs watch [<trail>] --stream <ULID>` watches the stream ID printed on stderr.
No agent is launched, and no transcript files or environment variables are read
for capture. These commands are conservatively classified as **unlisted,
user-owned** in agent-help; agents must not start publishing on their own.

```sh
entire trail logs publish 42 --project gh/entireio --share-with-trail --transcript-agent claude-code < selected-transcript.jsonl
# Plain terminal output is an explicit fallback, not inferred conversation data:
entire trail logs publish 42 --project gh/entireio --share-with-trail --format text < selected-output.txt
entire trail logs watch 42 --project gh/entireio --stream <STREAM_ULID>
```

Logs are shared with everyone who can read the Trail, not just a linked repo's
readers. Use only output safe for that audience. The publisher applies configured
local secret/PII scanners, common credential filters, and repeatable `--redact`
regexes. Transcript input is sanitized and redacted before the existing compact
converter runs. Images and hidden/runtime blocks are not published. A transcript
scanner failure stops capture rather than joining across missing context; plain
text mode withholds the affected line. The checkpoint-only OpenAI Privacy
Filter is **not** run, and redaction is not comprehensive secret detection.
Never pipe private transcripts or secrets on the assumption they will be removed.

The default `--format transcript` supports Claude Code, Codex, and Pi JSONL.
The existing converter runs on a bounded redacted prefix at most every 250 ms
and once at EOF, publishing changed compact messages with stable IDs. Later tool
results replace their earlier pending messages. Harness identity and model (when
supported by the agent's existing extractor) accompany the messages; model names
pass through the same redaction filters. The reader accepts at most 1 MiB / 4096 input lines (64 KiB per line); converted
messages remain limited to 8 KiB / 64 content blocks. Invalid input, overload,
oversized messages, or a transcript branch change emit a gap and stop capture;
start a new stream instead of silently mixing incomplete histories. Terminal
watch prints these compact message updates as JSON lines.

Capture has a 64-record output queue, bounded batches/retries, and explicit gap
records. Plain text mode has a 4 KiB output-line cap. If upload fails permanently, it reports the failure
and drains input without uploading, so a failed server does not indefinitely
block an agent's output pipe. Keep a local copy when completeness matters.
This is a volatile prototype: logs live only in DO memory, for at most one hour,
and disappear on restart or eviction—even after an acknowledgement. The entire
Trail buffer is capped at 4 MiB / 4096 records across 32 streams. When it is lost,
start a new publishing stream. Completion means input EOF, not agent success.
Watch sanitizes terminal output and resumes only while the buffer survives.

Publishing and watching deliberately use the BFF, which routes to the existing
project editor DO without loading its editor. This is an exception to ordinary
project cell-direct requests. Numeric selectors use the assigned cell's lightweight
`.../trails/{number}/logs/access` route instead of an editor-backed detail read;
explicit ULIDs need no detail read. Optional branch/change discovery still uses
the shared project-context resolver.

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
- Project commands retain the standard login and repository-onboarding hints.
  Branch reads refresh the local trails-enablement cache only when their
  repository is known to be this clone; foreign change selectors never do.
  `link` takes a project trail number or ULID, not `<repo>/<number>`.
- Updates require the resource's ETag; a 412 never triggers an unconditional
  retry. Create/link print an idempotency key for retries with identical inputs.
- Local create publishes the branch but never commits, force-pushes, or deletes
  it on failure. `--repo`, `--no-branch`, and `--branch-action create` skip local
  publication. Unlink preserves the branch and its work.

Linked-repository event aggregation and batch multi-repository creation are not supported.
Local logs are project-scoped; existing `trail watch` keeps its branch-scoped contract.
