# Your First Contribution to Entire

If this is your first time contributing to an open source project, welcome! This guide walks you through making your first contribution to Entire, from finding an issue to opening a pull request.

If you've contributed to other projects and just want the workflow, jump to [CONTRIBUTING.md](../CONTRIBUTING.md) instead.

---

## Who this guide is for

You're in the right place if any of this sounds like you:

- You've never opened a pull request on someone else's project before.
- You've used `git` for your own work but haven't navigated a fork-and-PR workflow.
- You've contributed to other projects but want to know what's specific to Entire: the `Entire-Checkpoint` trailers, the `mise` toolchain, the agent integration tests.

If you get stuck at any point, ask in [Discord](https://discord.gg/jZJs3Tue4S). First-time contributor questions are welcome.

---
## Find a first issue

A few good places to look:

- **The [issue tracker](https://github.com/entireio/cli/issues).** Skim the open issues for something that looks small, well-scoped, or interesting to you. When you find one, comment on it (see [Claim the issue](#claim-the-issue) below) so a maintainer can confirm it's a good fit before you start.
- **The [`#looking-for-contributors`](https://discord.com/channels/1468014954689855716/1499864669920297081) channel on [Discord](https://discord.gg/jZJs3Tue4S).** Maintainers post issues that they are ready for someone to pick up.
- **Something you noticed yourself.** A typo, an unclear error message, a function in `cmd/entire/cli/` without test coverage. Small improvements like these are always welcome and don't need a prior issue.

---

## Claim the issue

Before you start coding, comment on the issue so a maintainer knows you're working on it. This avoids two people duplicating each other's work.

A good first comment looks like:

> First time contributor here! I see this is about [restate the issue in your own words, e.g., "a typo in the README quickstart section"]. Does that sound right? If so, I'd like to take it.

This shows you've read the issue and gives a maintainer a chance to course-correct before you spend time on the wrong thing.

Wait for a maintainer reply before opening a PR, usually within a day or two. If it's been longer, ping in [Discord](https://discord.gg/jZJs3Tue4S).

---

## Fork, clone, and branch

### 1. Fork the repository

Click **Fork** on [github.com/entireio/cli](https://github.com/entireio/cli). This creates a copy under your account that you have write access to.

### 2. Clone your fork

```bash
git clone https://github.com/YOUR-USERNAME/cli.git
cd cli
```

### 3. Create a branch

Use a descriptive name that references the issue:

```bash
git checkout -b fix/readme-typo-issue-123
```

When you pick a branch name, choose a prefix that reflects the kind of change you're making. The vocabulary below is borrowed from [Conventional Commits](https://www.conventionalcommits.org/):

- `fix/<short-description>` for bug fixes
- `feat/<short-description>` for new features
- `docs/<short-description>` for documentation

---

## Set up your dev environment

Entire is a Go project managed by `mise`. Three commands and you're set up:

```bash
# Install mise (skip if you already have it)
curl https://mise.run | sh

# Trust this repo's mise config and install Go 1.26
mise trust
mise install

# Build the CLI and run the tests
mise run build
mise run test
```

If `mise run test` passes, you're good to go. If something failed, see [Troubleshooting](#troubleshooting) below.

> Detailed setup notes live in [CONTRIBUTING.md](../CONTRIBUTING.md#local-setup), and architecture notes live in [AGENTS.md](../AGENTS.md).

---

## Working with agents

Entire exists to help you work with AI coding agents, so it would be odd if you weren't using one to contribute. There's no need to tell us you did. Our general thinking: use whatever agent and methodology you like, but until the robot revolution comes, you are responsible for the final code. Before submitting a PR for review, make sure you have reviewed it yourself. We'll close PRs that obviously skipped this step.

Entire supports agents including Claude Code, Codex, Gemini CLI, OpenCode, Cursor, Factory AI Droid, Copilot CLI, and Pi, so feel free to use whichever one you're most comfortable with. Whichever you choose, your session will be captured the same way (see [Using Entire while you contribute](#using-entire-while-you-contribute) below).

One thing to watch out for is LLM eagerness. Agents like to please and they're in a hurry. A few common failure modes to push back on:

- **Think first:** Agents tend to jump straight to writing code. Explain the architecture you want first, based on your own understanding, or have the agent explore the code and propose approaches before any edits happen. If the first implementation doesn't look right, just start over and use what you learned to do better next time. Re-rolling is cheaper than untangling.
- **Spot the laziness:** LLMs will make their own job easy. They write trivial tests, make types wide and optional so the compiler doesn't complain, catch exceptions and log instead of handling errors, and copy local patterns whether or not they fit. When you notice this happening, push back and ask the agent to do the work properly.
- **Spot the uncertainty:** As much as the bots declare "I see the issue now clearly," they often don't. Call them on it if you see the agent flailing. Another telltale sign: the agent starts listing the many ways it fixed an issue, or starts writing overly defensive code.
- **Spot the bloat:** Agents like to insert redundant comments, or worse, comments that describe the change at hand rather than the resulting code. They write loads of tests that don't really test anything, and when they do, they test the implementation rather than the intention. They also like to log anything, just in case. When you see this in the diff, trim it back before opening your PR.

---

## Make your change and commit

Edit the file(s), then verify locally:

```bash
mise run check   # runs fmt, lint, and the full test suite
```

CI runs the same thing. If `mise run check` passes locally, you're most of the way to a green PR.

Then commit:

```bash
git add path/to/changed-file.go
git commit -m "Fix typo in README quickstart"
```

### Using Entire while you contribute

We use Entire on Entire, and we encourage you to use it too. It helps maintainers see the agent context behind your change during review, which makes your PR easier to understand and approve.

You can install Entire as you work by following the [installation docs](https://docs.entire.io/cli/installation).

If you have Entire installed and running, you may notice a line like `Entire-Checkpoint: a3b2c4d5e6f7` appended to your commit messages. That's the trailer in action, linking the commit to the session that produced it so maintainers can pull up the agent context during review.

---

## Push and open a pull request

```bash
git push -u origin fix/readme-typo-issue-123
```

GitHub will print a link to open a PR. Click it, then:

1. **Write a descriptive title.** Same shape as a good commit message.
2. **Reference the issue** in the description (e.g., "Closes #123").
3. **Describe what you changed and why.** One paragraph is plenty for a first PR.
4. **Submit.**

<details>
<summary>Push failed with an authentication error?</summary>

GitHub no longer accepts password authentication over HTTPS. You'll need either:

- **A personal access token.** [Create one](https://github.com/settings/tokens) and use it as your password when prompted.
- **SSH keys.** Follow [GitHub's SSH setup guide](https://docs.github.com/en/authentication/connecting-to-github-with-ssh), then update your remote: `git remote set-url origin git@github.com:YOUR-USERNAME/cli.git`.

</details>

---

## What happens next

1. **Automated checks run.** CI (lint, tests, build) runs in a few minutes. If it goes red, click through to the failure, usually a `gofmt` or lint issue. Push a fix to the same branch and the PR updates automatically.

2. **Copilot reviews your PR.** We run Copilot review on every PR. It leaves inline comments. Read each one and either fix the issue or reply explaining why you disagree. PRs with unaddressed Copilot comments don't move to maintainer review.

3. **A maintainer reviews.** This usually happens within a few days, though open source review queues vary and sometimes things take a little longer, so please bear with us. If it's been more than a week with no response, feel free to leave a friendly bump comment on the PR.

4. **Address feedback and merge.** You may go through a round or two of changes. That's normal and not a sign anything is wrong. Once approved, a maintainer merges your code.

Congratulations, you're a contributor.

---

## If something goes wrong

- **`mise run test` fails on a fresh clone.** Make sure `mise install` finished cleanly and you ran `mise trust`. If still broken, ask in [Discord](https://discord.gg/jZJs3Tue4S) with the error output.
- **CI fails but local tests pass.** Re-run `mise run check` locally. `mise run test` alone doesn't check formatting or linting. CI runs the full check.
- **You pushed to the wrong branch or made a mess of commits.** Don't panic and don't force-push to `main`. Ask in Discord. We can almost always sort it out without losing your work.
- **You changed your mind and want to abandon the PR.** Close it with a brief comment ("decided not to pursue this"). No hard feelings.
- **You feel stuck or unsure.** Please ask, we really do mean it. First-time contributor questions are some of the easiest to answer, and we'd much rather walk you through something than have you walk away frustrated.

---

## Troubleshooting

### `mise install` fails

```bash
# Reload your shell after installing mise
source ~/.zshrc  # or ~/.bashrc

# Then retry
mise install
```

### `go mod download` times out

```bash
# Force direct mode (skip the module proxy)
GOPROXY=direct go mod download
```

### `entire` command not found after build

```bash
# Check which binary your shell is finding
which entire
type -a entire

# You may have multiple installs; make sure the freshly-built one is on your PATH first
```

---

## Where to go next

Once you've landed your first PR:

- [CONTRIBUTING.md](../CONTRIBUTING.md): the full contribution guide, including PR conventions and the Entire-specific workflow notes.
- [AGENTS.md](../AGENTS.md): architecture and development reference. Read this before tackling a non-trivial change.
- [Discord](https://discord.gg/jZJs3Tue4S): say hi, hang out, help the next first-time contributor.
