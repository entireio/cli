# Self-hosted project Agents

`entire project agent setup` and `serve` extend the existing project Agent group.
They do not change local `entire agent` integrations or restore legacy trail
runners. The backend continues to schedule Agents and validate results.

## Setup

Run setup as a project administrator, from a repository in the target project:

```sh
entire project agent setup --project et/acme --name review-laptop
# Or reuse an account:
entire project agent setup --project et/acme --service-account svc_<id>
```

Setup requires an existing Agent configured with `runs_on: self_hosted`. It
selects the project's active managed self-hosted Environment; it does not create
or reconfigure Agents. It asks for confirmation (use `--yes` in automation),
creates the named account or checks an existing account belongs to the project,
grants read access to the current repository, and binds the account to the
Environment. Existing repository grants are preserved, never downgraded.
`--json` emits project, account and Environment IDs; progress goes to stderr.

**The binding covers every Agent in the Environment**, limited by the account's
repository grants. There is no per-Agent authorization. Additional repositories
need their own grants. Use a different account per machine: the server identifies
claim ownership by service account, not process or machine.

Setup saves non-secret project ID/cell routing under the private Entire user
config root (`agent-serve/projects`). A repository-only service account cannot
resolve the project shell in Core, so serve reuses this operator-approved routing
without granting project read access. Run setup on the worker machine, or securely
provision this connection file alongside its secret in an unattended deployment.
Rerun setup if the project moves cells. If no connection is saved, serve attempts
normal discovery, which requires project inspection permission.

Setup deliberately does not mint, print or store a secret or replace your login.
Provision it through the existing `entire service-account secret create` command
and a secret manager, then supply it as `ENTIRE_TOKEN` for serve. Existing service
account JWTs also work. Secret exchange and refresh reuse the existing auth
client. Setup's account creation, grant and binding are separate operations: on
partial failure it prints the account ID for retry with `--service-account`;
there is no destructive rollback. Inspect grants after an ambiguous grant error.

## Foreground execution

```sh
entire project agent serve --project et/acme
entire project agent serve --project et/acme --once
```

Requires macOS or Linux, Git, the installed `git-remote-entire` helper, and Claude
Code advertising `--restricted`. Claude uses its own local login or explicitly
provided `ANTHROPIC_API_KEY`/`CLAUDE_CODE_OAUTH_TOKEN`; runner credentials never
reach Claude's environment. Windows execution is refused. Unsupported harnesses
are left unclaimed. `--poll-interval` defaults to five seconds, minimum one second.
`--once` exits when no supported job exists, or after one job/submission recovery.

The saved setup connection (or project resolver) chooses the project's API cell. The worker walks all job
pages, filters by project and Claude, and claims by run ID. Claims refused with
404/409 are skipped for the remainder of that queue scan, including across pages.
`--once` exits if the scan has no remaining supported jobs. Continuous workers
clear the skip list after processing a job or exhausting the scan, so a temporarily paused repository
can become eligible again. The same claim is retried after uncertain responses. Previously
claimed runs belonging to this account are recoverable from the server's job
list. Transient requests receive bounded retries. Continuous workers wait for
the poll interval and resume after transient polling or claim failures, without
skipping an uncertain claim. `--once` returns the error after bounded retries.
Authorization and claim identity mismatches stop the worker without publishing
against an unverified run. A matching run/project with an unsupported harness or
missing deadline is never executed: the worker saves and submits a failure, then
continues serving (`--once` returns the execution error). An uncertain terminal
submission stops with a retained payload.

Each run:

1. Makes a private workspace under the user cache root and clones the claim's
   credential-free `entire://` URL using the service-account credentials.
2. Checks out the full pinned head commit, not a mutable branch, with symlinks
   represented as ordinary files. Git ignores global/system configuration,
   templates and hooks; external diff/textconv are disabled. Submodules are not
   initialized. A failed clone/checkout/diff prevents model execution.
3. Writes the pinned base/head diff outside the untrusted checkout.
4. Runs Claude with restricted Read/Grep/Glob tools, strict empty MCP configuration,
   hooks explicitly disabled, no settings sources (`--setting-sources ""`),
   and no session persistence. The capability probe also disables settings sources.
   Prompt text is stdin;
   schema/model come from the claim. No shell or repository command is run by the
   model. Restricted mode confines file reads to the checkout and diff directory
   and ignores user/project/local settings. There is no unrestricted fallback.
5. Stops execution five seconds before the server deadline, saves the completion
   payload, and publishes the structured result. Failures publish a generic error
   rather than subprocess output. Output is bounded to 8 MiB. Workspaces are
   removed on normal return; abrupt termination may leave cache artifacts.

The `agent/claudecode` restricted-review entry point owns executable discovery,
capability checks, argv, the credential allowlist and structured response parsing.
The CLI worker owns checkout, scheduling and bounded subprocess transport. The
restricted path reuses Claude response types and error classification, not the
ordinary reviewer permissions or text-generation auth-helper injection.

Installed executables, absolute PATH directories and Claude's managed policy are
operator-trusted. Isolation relies on Claude's restricted-mode enforcement, not
an OS sandbox. The model may read repository instructions as untrusted content;
it cannot execute them. The server validates the returned result against its
schema and review-domain rules.

## Recovery and shutdown

A rooted, private `agent-serve` directory under the Entire user config root holds
one pending submission per identity/cell/project and a process lock. Writes are
atomic with mode 0600. Files contain review results, not runner credentials, and
must be treated as sensitive source-derived data. The lock prevents duplicate
local workers for the same identity/project. It cannot coordinate the same
account running on two different machines; provision separate accounts there.

Restarting serve replays the exact pending complete/fail payload before checking
harness availability or claiming another job. Successful submissions remove it.
404/409 terminal refusals clear it and report that the run can no longer accept
it; other refusals (including 422 validation errors) retain it for inspection or
manual repair. Submission failures print the exact pending-file path without
printing its contents. For 422, stop the worker, inspect that JSON file, and
correct `body.result` (for a completion) according to the API validation error;
keep `run_id` and `action` unchanged. Restart serve to replay the corrected body
before taking new work. Restarting without correcting it cannot fix validation
errors. Once the run expires, its terminal 409 clears the pending file on replay.
Never delete a pending file merely to retry a transient failure: that loses the
saved result and can cause the claimed review to execute again.

Ctrl-C/SIGTERM cancel execution and subprocess groups, then allow up to 15 seconds
to report failure with a fresh context. An interrupted submission remains saved.
An abrupt kill before a submission exists can require re-executing the claimed
review; the protocol does not promise exactly-once model execution.

## Verification

Tests use fake API servers and fake subprocesses: pagination, claim conflicts,
claimed-job recovery, bounded retry with identical payloads, deadlines, save
failure, submission retention, clone/checkout/diff failure, output bounds,
credential filtering and cancellation. No paid model calls or live setup are
part of these tests.
