<img src=".github/assets/gh-repo-cover.png" alt="Entire CLI" width="100%">

# Entire CLI

[Entire](https://entire.com) is a Git forge built for agent–human collaboration. This CLI is how you and your agents use Entire from the terminal — anything you do on entire.com, you can do here:

- **Distributed Git hosting** — host repositories on Entire's Git-compatible server, or mirror your GitHub ones, and replicate them in regions around the world for data residency and the scale of agent traffic. → [Repositories & Git Hosting](#repositories--git-hosting)
- **Trails** — instead of reviewing a diff, get a heads-up display for every change, with customizable evals that agents score on every push. → [Trails](#trails)
- **Continuous memory** — capture every agent session as a checkpoint linked to your commits; [entire-brain](https://docs.entire.com/guides/brain/overview) combines them with [entire-graph](https://docs.entire.com/guides/graph/overview)'s code map into project memory. → [Agent Sessions](#agent-sessions)

Works with Claude Code, Codex, Cursor, Copilot CLI, OpenCode, Antigravity, Factory AI Droid, Pi, and [more](https://docs.entire.com/agents/agent-integration-protocol/overview), and extends through [plugins](#plugins). Pushes through a GitHub mirror still trigger your GitHub Actions, and Trails can also show CI results from Buildkite and Depot.

Open source under the MIT license. Docs: **[docs.entire.com](https://docs.entire.com)**.

## Contents

- [Install](#install)
- [Quick Start](#quick-start)
- [Repositories & Git Hosting](#repositories--git-hosting)
- [Trails](#trails)
- [Agent Sessions](#agent-sessions)
- [Search, Activity & the API](#search-activity--the-api)
- [Plugins](#plugins)
- [Security & Privacy](#security--privacy)
- [Troubleshooting](#troubleshooting)
- [Documentation](#documentation)

## Install

```bash
brew install --cask entireio/tap/entire          # macOS and Linux
curl -fsSL https://entire.com/install.sh | bash  # macOS and Linux, without Homebrew
irm https://entire.com/install.ps1 | iex         # Windows PowerShell 5.1+
```

For the nightly channel (prerelease builds with the latest changes):

```bash
brew install --cask entireio/tap/entire@nightly
curl -fsSL https://entire.com/install.sh | bash -s -- --channel nightly
iex "& {$(irm https://entire.com/install.ps1)} -Channel nightly"
```

Requires Git on macOS, Linux, or Windows. Scoop, `go install`, and installing during a GitHub outage are covered in [Installation](https://docs.entire.com/installation).

## Quick Start

**1. Capture agent sessions** — works in any Git repository, no account needed:

```bash
cd your-project
entire enable                    # pick your agents; installs their hooks and Entire's git hooks
entire status                    # current sessions and where checkpoints are uploaded
```

**2. Put a repository on Entire** — sign in, then mirror a GitHub repository into one or more clusters, or create an Entire-native one. Mirroring needs the [Entire GitHub App](https://docs.entire.com/guides/repositories/github-app-access) installed for that repository.

```bash
entire login

entire repo mirror add                          # wizard: pick repos and clusters
entire repo clone /gh/<owner>/<repo> --nearest

entire project create                           # wizard: owner, name, region
entire repo create <name> --project <project>
entire repo clone /et/<project>/<name>
```

Then `cd` into the clone and run `entire enable` there to capture sessions in it.

**3. Open a Trail** — in a repository on Entire, on the branch you're working on:

```bash
git switch -c add-rate-limiting
entire trail create --title "Add rate limiting"
entire trail show
```

## Repositories & Git Hosting

Entire hosts Git repositories on clusters in several regions, so agents can clone, fetch, and push against a copy near where they run.

- **Region** — where an organization's or project's data stays: a jurisdiction such as `us` or `eu`, set with `--region` when you create it.
- **Cluster** — an Entire Git host, such as `aws-eu-central-1.entire.io`. `entire cluster list` shows them; a repository can have copies on several.
- **Project** — holds Entire-native repositories and belongs to an organization or a personal account. Repository and project commands take its plain name (`--project acme`); Trail commands take `et/<project>`, and treat a GitHub owner as the project for its mirrors (`gh/<owner>`).
- **Mirror** — a copy of a repository on one cluster. GitHub repositories are mirrored from their GitHub upstream, and native repositories can be mirrored to more clusters. `entire repo mirror` manages both.

Repository references name their forge: `/et/<project>/<repo>` for an Entire-native repository, `/gh/<owner>/<repo>` for a GitHub mirror, or a full `entire://<cluster>/…` clone URL.

```bash
entire repo create web --project acme                   # Entire-native, private by default
entire repo mirror add /gh/acme/web --cluster aws-us-east-2.entire.io,aws-eu-central-1.entire.io
entire repo clone /gh/acme/web --nearest                # time each cluster and use the fastest
entire repo remote add entire                           # add Entire to an existing checkout, beside origin
```

A GitHub mirror keeps GitHub as the source of truth: clones and fetches come from Entire, and pushes are forwarded to GitHub, where your GitHub permissions, branch protection, and required checks still apply. Clones use `entire://` URLs through the `git-remote-entire` helper that ships with the packaged installs; after that, it is plain Git.

See [Repositories](https://docs.entire.com/guides/repositories/overview) and [Mirrors in the CLI](https://docs.entire.com/guides/repositories/mirrors-in-cli) for regions, mirroring, unmirrored branches, moving a mirror off GitHub, branch protection, and access.

## Trails

A **Trail** follows a change from intent to merge. Instead of reviewing a diff, you get a heads-up display for it — the Trail's page on entire.com, or `entire trail show` and `entire trail watch` here — with findings from evals that agents score on every push. Each Trail has a number in its project, such as `#42`.

```bash
entire trail create --title "Add rate limiting" --type feature   # publishes the current branch
entire trail list --project et/acme --status open
entire trail show                       # the Trail for the current branch
entire trail watch                      # stream reviews, findings, and runner events live

entire trail finding                    # findings dashboard for this branch
entire trail finding apply <id>         # apply a finding's suggested fix
entire trail finding resolve <id> -m "fixed in abc123"

entire trail comment add -m "Should this cover the admin API too?"
entire trail approve
entire trail merge                      # merge this branch's change into its base
entire trail resume 42                  # continue Trail #42: restore its branch's sessions in your agent
```

Trail commands use the current repository's project (`gh/<owner>` or `et/<project>`); pass `--project` or `--repo` to work on another one without a checkout. `entire trail list` always takes `--project`. Other subcommands are `checkout`, `link`, `unlink`, `update`, and `approvals`.

**Runners** are those evals: per-repository checks such as risk, confidence, drift, security, and review, which score each push to a Trail and raise its findings. They live in `.entire/runners/`; `entire runner setup` (experimental) creates the default set and can tailor its prompts to your repository using its docs, history, and past findings.

## Agent Sessions

`entire enable` connects your agents and installs Entire's git hooks, keeping any hooks you already have. Use your agent and commit as usual: each commit that includes agent work gets a **checkpoint** — the prompts, transcript, and files touched — linked to the commit by an `Entire-Checkpoint` trailer. Entire never adds commits to your branch; by default, checkpoints are stored as their own Git refs (`refs/entire/checkpoints/…`) and pushed alongside your code.

```bash
entire checkpoint list                # checkpoints on this branch
entire checkpoint explain <id>        # the prompt, transcript, and files behind a checkpoint
entire session resume <branch>        # restore a branch's sessions and continue in your agent
entire agent add <name>               # add an agent later; entire configure for other settings
entire disable                        # turn capture off (--uninstall removes Entire from the repo)
```

See [Sessions](https://docs.entire.com/guides/sessions/overview) and [Checkpoint Storage](https://docs.entire.com/guides/checkpoints/checkpoint-storage) for how capture works and migrating from legacy branch storage, [Settings](https://docs.entire.com/guides/configuration/settings-layers) and [Git Hooks](https://docs.entire.com/guides/configuration/git-hooks) for configuration, and [Checkpoint Destination](https://docs.entire.com/guides/checkpoints/checkpoint-destination) for choosing the remote or a [separate checkpoint repository](https://docs.entire.com/guides/checkpoints/store-checkpoints-in-another-repo).

Checkpoints are also the raw material for continuous memory: [entire-brain](https://docs.entire.com/guides/brain/overview), installed separately, combines them with [entire-graph](https://docs.entire.com/guides/graph/overview)'s code map into project memory your agents can query. From the CLI, `entire search` and `entire checkpoint explain` are the way in.

## Search, Activity & the API

- **Search** — `entire search` runs hybrid (semantic + keyword) search over checkpoints, commits, and sessions, scoped to the current repo or `--all-repos`. `--json --compact` returns a trimmed shape suited to agents, and `entire checkpoint explain <id>` fetches the full detail of a hit.
- **Activity and dispatches** — `entire activity` shows your activity overview, repository breakdown, and recent sessions from entire.com. `entire dispatch` writes a summary of recent agent work; `--local` generates it with your locally installed agent CLI instead of the Entire server.
- **Raw API access** — `entire api <path>` makes an authenticated request to an Entire API and prints the JSON response, like `gh api`. It attaches your token and picks the host: `--to core` (the default) reaches the control plane (orgs, projects, repos, mirrors, clusters), `--to cell` reaches your home data cell (activity, repo aggregates), and `--jurisdiction <slug>` reaches another jurisdiction's cell. `{owner}`, `{repo}`, and `{repo_id}` in the path are filled from the current repository's `origin`; `{repo_id}` needs a GitHub repository with an active Entire mirror. These endpoints are internal and can change without notice; where a purpose-built command exists, prefer it.

```bash
entire api /api/v1/clusters
entire api --to cell "/api/v1/me/recap?repo={repo_id}"
entire api /api/v1/me
```

## Plugins

Any executable named `entire-<name>` on `$PATH` runs as `entire <name>`. Install plugins from the [plugin index](https://github.com/entireio/plugin-index) or any Git host:

```bash
entire plugin search
entire plugin install <name>
entire plugin list
```

Installs from a Git host are verified against the release's `checksums.txt`. See [`entire plugin`](https://docs.entire.com/cli-reference/plugin).

## Security & Privacy

**Session transcripts are pushed to your Git remote** — as checkpoint refs, not commits on your branch. If that repository is public, anyone can read them. Entire redacts detected secrets before writing a checkpoint, but redaction is best effort; `entire checkpoint list --outgoing` shows what your next push will upload, and `entire checkpoint explain <id>` shows a checkpoint's contents.

To keep transcripts out of a public repository, upload them to a separate private repository (`entire configure --local --checkpoint-remote github:<owner>/<repo>`) or stop uploading them (`entire configure --local --skip-push-sessions`). `entire enable` names the checkpoint destination every time it runs. See [Security](https://docs.entire.com/security) for redaction, scanners, deleting checkpoints, and cleaning up data from older versions.

## Troubleshooting

| Issue | Solution |
| --- | --- |
| "Not a git repository" | Navigate to a Git repository first |
| "Entire is disabled" | Run `entire enable` |
| Agent hooks outdated after a CLI upgrade | Run `entire agent add <name>`; add `--force` if that does not fix them |
| A session stuck ACTIVE, or leftover session state | Run `entire doctor`, then `entire clean --force` if it persists |
| Anything else | Run `entire doctor`; `entire doctor bundle` produces a redacted diagnostic bundle for a bug report |

Set `ENTIRE_LOG_LEVEL=debug` for debug logs, and `ACCESSIBLE=1` for screen-reader-friendly prompts. Headless servers and CI: see [`entire login`](https://docs.entire.com/cli-reference/login).

## Documentation

- **[docs.entire.com](https://docs.entire.com)** — guides for Entire and the CLI, including [how the CLI and entire.com fit together](https://docs.entire.com/platforms/cli-and-entire-com)
- `entire help`, `entire <command> --help`, and the [CLI reference](https://docs.entire.com/cli-reference); `entire agent-help` is the version written for coding agents
- **Issues:** https://github.com/entireio/cli/issues · **Contributing:** [CONTRIBUTING.md](CONTRIBUTING.md)

## License

MIT License — see [LICENSE](LICENSE).
