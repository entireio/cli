# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [0.6.0] - 2026-05-04

### Added

- Detection of pushes to protected branches, with a clearer warning before the push proceeds ([#1033](https://github.com/entireio/cli/pull/1033))
- Improved auth token management in the CLI ([#1050](https://github.com/entireio/cli/pull/1050))
- `entire explain --generate` now supports external agents for summary generation ([#1044](https://github.com/entireio/cli/pull/1044))
- `entire search` TUI gains a unified palette with the activity view, markdown snippet rendering, and shell completions ([#1053](https://github.com/entireio/cli/pull/1053))
- Homebrew now prompts users to update when a new version is released, and Homebrew cask completions are generated at install time ([#1057](https://github.com/entireio/cli/pull/1057), [#1085](https://github.com/entireio/cli/pull/1085))
- Nested perf spans render in traces for richer debugging ([#1073](https://github.com/entireio/cli/pull/1073))

### Changed

- CLI restructured around `session` / `checkpoint` / `agent` / `auth` / `doctor` command groups ([#1062](https://github.com/entireio/cli/pull/1062))
- Charm TUI stack upgraded to v2; dispatch repo links added to the dispatch wizard ([#1048](https://github.com/entireio/cli/pull/1048))
- TUI navigation help aligned across `entire activity` and `entire search` ([#1058](https://github.com/entireio/cli/pull/1058), [#1064](https://github.com/entireio/cli/pull/1064))
- `entire explain` generated summary is now formatted ([#1078](https://github.com/entireio/cli/pull/1078))
- Auto-update prompt aligned across all installers ([#1083](https://github.com/entireio/cli/pull/1083))
- TTY detection simplified via `testing.Testing()` and OS-level process detachment ([#1029](https://github.com/entireio/cli/pull/1029))
- Switched secret scanning back to betterleaks, with tightened redaction coverage and improved database credential redaction ([#1043](https://github.com/entireio/cli/pull/1043), [#1045](https://github.com/entireio/cli/pull/1045), [#1068](https://github.com/entireio/cli/pull/1068))
- Checkpoints v2 (work in progress): expose CLI version to external agents for compact transcripts; cleaner migration output and completion message; use checkpoint creation time for generation calc with a lower default retention; push v2 refs in parallel ([#1032](https://github.com/entireio/cli/pull/1032), [#1059](https://github.com/entireio/cli/pull/1059), [#1088](https://github.com/entireio/cli/pull/1088), [#1089](https://github.com/entireio/cli/pull/1089), [#1094](https://github.com/entireio/cli/pull/1094))

### Fixed

- Cursor sessions no longer get mis-identified as Claude Code ([#1082](https://github.com/entireio/cli/pull/1082))
- `entire explain` works with partial-clone checkpoint repositories ([#1069](https://github.com/entireio/cli/pull/1069))
- Agent-neutral wording in the `entire explain` empty-state message ([#1086](https://github.com/entireio/cli/pull/1086))
- False PATH conflict detection in the installer ([#1038](https://github.com/entireio/cli/pull/1038))
- Checkpoints v2 migration: preserve attribution metadata; clean up v1-named transcript files on `/full/current`; handle missing v1 sessions; check archived v2 transcripts; correct generation packing ([#1035](https://github.com/entireio/cli/pull/1035), [#1034](https://github.com/entireio/cli/pull/1034), [#1071](https://github.com/entireio/cli/pull/1071), [#1080](https://github.com/entireio/cli/pull/1080), [#1091](https://github.com/entireio/cli/pull/1091))

### Housekeeping

- Centralized TUI keybindings via `bubbles/key` in preparation for Bubble Tea v2 ([#1060](https://github.com/entireio/cli/pull/1060))
- Expanded checkpoints v2 validation coverage and pruned subsumed tests in the strategy package ([#1012](https://github.com/entireio/cli/pull/1012), [#1077](https://github.com/entireio/cli/pull/1077))
- Dependency bumps: go-dependencies group (incl. `posthog-go` 1.12.1 → 1.12.4), `goreleaser/goreleaser-action` 7.1.0 → 7.2.1, `slackapi/slack-github-action` 3.0.1 → 3.0.3 ([#1031](https://github.com/entireio/cli/pull/1031), [#1087](https://github.com/entireio/cli/pull/1087), [#1055](https://github.com/entireio/cli/pull/1055), [#1016](https://github.com/entireio/cli/pull/1016), [#1095](https://github.com/entireio/cli/pull/1095))

### Thanks

Thanks to @KuaaMU for making the `entire explain` empty-state message agent-neutral!

## [0.5.6] - 2026-04-24

### Added

- `entire activity` command to show recent session activity ([#999](https://github.com/entireio/cli/pull/999))
- `entire dispatch` command to generate dispatches from checkpoints, using the `/api/v1/repositories` endpoint for the dispatch wizard ([#1004](https://github.com/entireio/cli/pull/1004), [#1023](https://github.com/entireio/cli/pull/1023))
- `entire explain` accepts a checkpoint ID or commit SHA as a positional argument ([#990](https://github.com/entireio/cli/pull/990))
- `entire explain --generate` summary provider with improved observability ([#887](https://github.com/entireio/cli/pull/887))
- `--json` output for `entire status` ([#975](https://github.com/entireio/cli/pull/975))
- Checkpoint commit signing (SSH/GPG), with object signer checks moved before registration and fixes for 1Password and bare public-key setups ([#960](https://github.com/entireio/cli/pull/960), [#1020](https://github.com/entireio/cli/pull/1020), [#1002](https://github.com/entireio/cli/pull/1002))
- Filtered fetches for checkpoint refs to reduce clone/fetch size ([#996](https://github.com/entireio/cli/pull/996))
- Session linkage preserved across `git rebase`, `git commit --amend`, and `git reset` ([#947](https://github.com/entireio/cli/pull/947), [#948](https://github.com/entireio/cli/pull/948))
- External agents can register in the `entire attach` flow ([#986](https://github.com/entireio/cli/pull/986))
- VS Code-compatible payloads for Copilot hooks ([#888](https://github.com/entireio/cli/pull/888))
- Actionable, classified error messages for Claude CLI failures ([#963](https://github.com/entireio/cli/pull/963))
- Inline auto-update prompt after version notification ([#997](https://github.com/entireio/cli/pull/997))
- Warning when `entire enable` runs but the CLI is not installed in agent hooks ([#929](https://github.com/entireio/cli/pull/929))
- Devcontainer setup for GitHub Codespaces / VS Code ([#940](https://github.com/entireio/cli/pull/940))
- Vercel branch deploy config to exclude `entire/*` branches ([#904](https://github.com/entireio/cli/pull/904))
- Checkpoints v2 (work in progress): `attach` command support, health checks in `entire doctor`, `checkpoints_version` setting with v2-only option, retention-based cleanup in `entire clean`, external-agent transcript compaction, transcript blob reuse across turn-end checkpoints, and `full.jsonl` renamed to `raw_transcript` ([#955](https://github.com/entireio/cli/pull/955), [#946](https://github.com/entireio/cli/pull/946), [#1001](https://github.com/entireio/cli/pull/1001), [#970](https://github.com/entireio/cli/pull/970), [#972](https://github.com/entireio/cli/pull/972), [#980](https://github.com/entireio/cli/pull/980), [#984](https://github.com/entireio/cli/pull/984), [#944](https://github.com/entireio/cli/pull/944))

### Changed

- Improved `entire enable` flow for folders that are not yet git repositories ([#978](https://github.com/entireio/cli/pull/978))
- Reduced duplication between `enable` and `configure` flows ([#950](https://github.com/entireio/cli/pull/950))
- Consolidated TTY detection into the `interactive` package; honor `PI_CODING_AGENT` to skip interactive prompts ([#1011](https://github.com/entireio/cli/pull/1011), [#926](https://github.com/entireio/cli/pull/926))
- Guard `entire attach` against overwriting checkpoints created on other machines ([#1014](https://github.com/entireio/cli/pull/1014))
- Strategy now guards against writing empty-session metadata stubs ([#1022](https://github.com/entireio/cli/pull/1022))
- Hook messages renamed from "Powered by Entire" to "Entire CLI" ([#965](https://github.com/entireio/cli/pull/965))
- Consistent rewind/resume continuation wording across agents ([#987](https://github.com/entireio/cli/pull/987))
- More descriptive output checkpoints are pushed during normal `git push` ([#927](https://github.com/entireio/cli/pull/927))
- Refactored checkpoint remote URL resolution and `ENTIRE_CHECKPOINT_TOKEN` handling ([#989](https://github.com/entireio/cli/pull/989))

### Fixed

- Codex token usage normalization ([#1021](https://github.com/entireio/cli/pull/1021))
- Factory AI Droid fallback tool-use IDs ([#942](https://github.com/entireio/cli/pull/942))
- `entire explain` fetches metadata from the remote when missing locally ([#953](https://github.com/entireio/cli/pull/953))
- Fetch checkpoint blobs from `checkpoint_remote` instead of `origin` ([#976](https://github.com/entireio/cli/pull/976))
- Checkpoints v2: dual-write and preserve task metadata; skip empty sessions to prevent phantom checkpoint paths ([#962](https://github.com/entireio/cli/pull/962), [#958](https://github.com/entireio/cli/pull/958))
- Hanging summary TTY in local test runs ([#968](https://github.com/entireio/cli/pull/968))
- Nightly release workflow now fails loudly instead of silently skipping when the tag already exists ([#966](https://github.com/entireio/cli/pull/966))
- Build fix: qualify `isTerminalWriter` in `activity_cmd.go` ([#1013](https://github.com/entireio/cli/pull/1013))

### Housekeeping

- Single `mise run check` command runs fmt, lint, and full test suite needed for PRs to be green ([#949](https://github.com/entireio/cli/pull/949))
- Require `mise run lint` before pushing any remote code update ([#1003](https://github.com/entireio/cli/pull/1003))
- Refactored git commands and increased test coverage ([#995](https://github.com/entireio/cli/pull/995))
- Prefer `testutil.InitRepo` in trivial git test setup ([#979](https://github.com/entireio/cli/pull/979))
- Stabilized TTY-dependent local CI tests, OpenCode E2E, and Factory AI Droid pre/post-tool-call E2E tests ([#969](https://github.com/entireio/cli/pull/969), [#967](https://github.com/entireio/cli/pull/967), [#959](https://github.com/entireio/cli/pull/959), [#1000](https://github.com/entireio/cli/pull/1000), [#1025](https://github.com/entireio/cli/pull/1025))
- Removed nightly Windows E2E schedule ([#925](https://github.com/entireio/cli/pull/925))
- Added `entire sessions` command reference to docs ([#1010](https://github.com/entireio/cli/pull/1010))
- Updated Code of Conduct community platform from Slack to Discord ([#810](https://github.com/entireio/cli/pull/810))
- Dependency bumps: `go-git/v6` 6.0.0-alpha.1 → 6.0.0-alpha.2, `posthog-go` 1.11.2 → 1.12.1, `goreleaser-action` 7.0.0 → 7.1.0, `actions/create-github-app-token` 3.0.0 → 3.1.1 ([#977](https://github.com/entireio/cli/pull/977), [#951](https://github.com/entireio/cli/pull/951), [#992](https://github.com/entireio/cli/pull/992), [#991](https://github.com/entireio/cli/pull/991), [#943](https://github.com/entireio/cli/pull/943))

### Thanks

Thanks to @areporeporepo for updating the Code of Conduct community link!

## [0.5.5] - 2026-04-13

### Added

- Checkpoints v2 (work in progress): `--force` flag for `entire migrate-v2` to rerun migrations that previously completed, and `checkpoint_transcript_start` support for compact `transcript.jsonl` files ([#885](https://github.com/entireio/cli/pull/885), [#877](https://github.com/entireio/cli/pull/877))

### Changed

- Hide `entire search` command from the menu while it stabilizes ([#928](https://github.com/entireio/cli/pull/928))
- Condensation logic refactored with type-enforced redaction boundaries for safer session data handling ([#922](https://github.com/entireio/cli/pull/922))

### Fixed

- Fetch checkpoint refs by URL to avoid polluting `origin` git config ([#934](https://github.com/entireio/cli/pull/934))
- Support Claude JSON array responses in `explain` summary generation ([#921](https://github.com/entireio/cli/pull/921))
- GoReleaser using the wrong tag during concurrent releases ([#918](https://github.com/entireio/cli/pull/918))

### Housekeeping

- Stabilize flaky Cursor and OpenCode E2E behavior and transcript prep timing ([#923](https://github.com/entireio/cli/pull/923))
- More hermetic separation for Gemini auth config files in E2E tests ([#915](https://github.com/entireio/cli/pull/915))
- Bump `actions/upload-artifact` from 7.0.0 to 7.0.1 ([#920](https://github.com/entireio/cli/pull/920))

## [0.5.4] - 2026-04-10

### Added

- Checkpoints v2 (work in progress): v2-aware `explain` with compact transcript support, push logic for v2 refs, compact transcript format for Factory AI Droid, Codex, and Copilot CLI, and `entire migrate-v2` migration command ([#864](https://github.com/entireio/cli/pull/864), [#821](https://github.com/entireio/cli/pull/821), [#852](https://github.com/entireio/cli/pull/852), [#862](https://github.com/entireio/cli/pull/862), [#891](https://github.com/entireio/cli/pull/891))
- `entire search` command is now available, with improved TUI usability and managed search subagents ([#907](https://github.com/entireio/cli/pull/907), [#856](https://github.com/entireio/cli/pull/856), [#833](https://github.com/entireio/cli/pull/833))
- Stale session indicator in `entire status` output ([#853](https://github.com/entireio/cli/pull/853))
- `entire status` now shows active agents ([#847](https://github.com/entireio/cli/pull/847))
- `entire configure --remove-agent` to remove agent configurations ([#851](https://github.com/entireio/cli/pull/851))
- Codex support for `explain --generate` with summary timeout ([#875](https://github.com/entireio/cli/pull/875), [#876](https://github.com/entireio/cli/pull/876))
- Nightly releases via GoReleaser and Homebrew tap, with `install.sh` nightly support ([#825](https://github.com/entireio/cli/pull/825), [#911](https://github.com/entireio/cli/pull/911))
- Hook overwrite detection during running agent prompts ([#791](https://github.com/entireio/cli/pull/791))
- Binary file detection in PR diffs ([#897](https://github.com/entireio/cli/pull/897))

### Changed

- `entire clean` fully replaces deprecated `entire reset` ([#858](https://github.com/entireio/cli/pull/858))
- Checkpoint branch alignment with remote now uses rebase instead of force-push ([#863](https://github.com/entireio/cli/pull/863))

### Fixed

- Windows: reject absolute and malformed paths in git tree writes ([#902](https://github.com/entireio/cli/pull/902))
- `entire attach` using wrong path for Codex sessions ([#894](https://github.com/entireio/cli/pull/894))
- External agents detection during hook execution ([#893](https://github.com/entireio/cli/pull/893))
- Gitignore now respected for shadow branch tree writes ([#890](https://github.com/entireio/cli/pull/890))
- Model field always written to checkpoint metadata.json ([#882](https://github.com/entireio/cli/pull/882))
- Multi parallel sessions causing conflicts on the same shadow branch ([#879](https://github.com/entireio/cli/pull/879))
- Codex resume failing to restore session state ([#878](https://github.com/entireio/cli/pull/878))
- Checkpoint transcript start offset when agent continues writing logs after checkpoint ([#873](https://github.com/entireio/cli/pull/873))
- Attribution inflation from intermediate commits during squash workflows ([#870](https://github.com/entireio/cli/pull/870))
- Codex single-line start message rendering with extra spaces ([#857](https://github.com/entireio/cli/pull/857))
- Token count omitted from status when no token data exists ([#854](https://github.com/entireio/cli/pull/854))
- `entire clean --all` now cleans all sessions, not just orphaned ones ([#846](https://github.com/entireio/cli/pull/846))
- `entire status` blank line formatting ([#848](https://github.com/entireio/cli/pull/848))
- Skip transcript redaction when checkpoints v2 is disabled ([#896](https://github.com/entireio/cli/pull/896))
- Clarified external checkpoint discovery warning copy ([#889](https://github.com/entireio/cli/pull/889))

### Housekeeping

- E2E test improvements: OpenCode boot time, Cursor/Gemini harness fixes, debug tooling, attach test timeout ([#914](https://github.com/entireio/cli/pull/914), [#912](https://github.com/entireio/cli/pull/912), [#892](https://github.com/entireio/cli/pull/892), [#835](https://github.com/entireio/cli/pull/835))
- Speed up unit tests ([#901](https://github.com/entireio/cli/pull/901))
- Pinned all GitHub Actions to commit SHAs for supply chain security ([#872](https://github.com/entireio/cli/pull/872))
- Updated README to consolidate agent instructions ([#899](https://github.com/entireio/cli/pull/899))
- Added Codex mentions to documentation ([#816](https://github.com/entireio/cli/pull/816))
- Dependency bumps: Go 1.26.2 + ulikunitz/xz v0.5.15 (fixes 6 vulns), golang.org/x/sys, charmbracelet/bubbles, go-dependencies group ([#910](https://github.com/entireio/cli/pull/910), [#905](https://github.com/entireio/cli/pull/905), [#874](https://github.com/entireio/cli/pull/874), [#850](https://github.com/entireio/cli/pull/850))
- Copilot CLI E2E tests can now use GitHub Actions token for authentication ([#900](https://github.com/entireio/cli/pull/900))

## [0.5.3] - 2026-04-03

### Added

- `entire sessions` subcommands (`list`, `info`, `stop`) for managing active and ended sessions ([#822](https://github.com/entireio/cli/pull/822), [#739](https://github.com/entireio/cli/pull/739))
- `entire attach` command to manually link untracked sessions ([#688](https://github.com/entireio/cli/pull/688), [#743](https://github.com/entireio/cli/pull/743))
- Gemini CLI transcript support for session logs and condensation ([#819](https://github.com/entireio/cli/pull/819))
- Checkpoints v2 (work in progress): compact `transcript.jsonl` file and metadata on `/main` ref ([#828](https://github.com/entireio/cli/pull/828))
- `ENTIRE_CHECKPOINT_TOKEN` environment variable for authenticated checkpoint push/fetch ([#818](https://github.com/entireio/cli/pull/818), [#827](https://github.com/entireio/cli/pull/827))

### Changed

- Deprecated `entire reset` command in favor of `entire clean` ([#720](https://github.com/entireio/cli/pull/720))

### Fixed

- Resume failing when checkpoints aren't fetched locally yet ([#796](https://github.com/entireio/cli/pull/796))
- OpenCode transcript export resilient to stdout truncation ([#832](https://github.com/entireio/cli/pull/832))
- Fail-closed content detection in `prepare-commit-msg` to prevent dangling checkpoint trailers from stale sessions ([#826](https://github.com/entireio/cli/pull/826))

### Housekeeping

- Scoop installation instructions for Windows ([#808](https://github.com/entireio/cli/pull/808))
- Eliminated real-time waits causing test suite hangs ([#823](https://github.com/entireio/cli/pull/823))
- Sped up slow unit tests in strategy and external packages ([#830](https://github.com/entireio/cli/pull/830))
- Dependency bumps: go-git/go-git v6 alpha.1, jdx/mise-action 4 ([#831](https://github.com/entireio/cli/pull/831), [#809](https://github.com/entireio/cli/pull/809))

## [0.5.2] - 2026-03-30

### Added

- Codex CLI agent integration with lifecycle hooks, e2e runner, transcript parsing, and token tracking. note: subagent tracking is not yet supported due to missing `pre-task`/`post-task` hooks in codex ([#772](https://github.com/entireio/cli/pull/772), [#794](https://github.com/entireio/cli/pull/794))
- Windows support: cross-platform path handling, CRLF-safe git parsing, detached process spawning, and `WINDOWS.md` guide ([#766](https://github.com/entireio/cli/pull/766))
- Checkpoints v2 (work in progress): dual-write behind `checkpoints_v2` feature flag with `/main` and `/full/current` ref layout, generation rotation to bound transcript growth, and unified `transcript.jsonl` condensation for Claude Code and OpenCode ([#742](https://github.com/entireio/cli/pull/742), [#759](https://github.com/entireio/cli/pull/759), [#781](https://github.com/entireio/cli/pull/781), [#788](https://github.com/entireio/cli/pull/788))
- `entire configure --checkpoint-remote` for setting the checkpoint remote interactively ([#798](https://github.com/entireio/cli/pull/798))
- `entire logout` command to remove stored credentials ([#740](https://github.com/entireio/cli/pull/740))
- E2E triage CI workflow with Slack integration for automated failure analysis ([#741](https://github.com/entireio/cli/pull/741))
- Diagnostic logging for checkpoint linking failures and session content filtering ([#785](https://github.com/entireio/cli/pull/785))

### Changed

- Redirect questions and support links from GitHub Discussions to Discord ([#761](https://github.com/entireio/cli/pull/761))

### Fixed

- Cursor mid-turn condensation and Gemini interactive prompt hang ([#780](https://github.com/entireio/cli/pull/780))
- Copilot CLI E2E fixes: Edit mode handling, subagent reliability, slash command dismissal ([#782](https://github.com/entireio/cli/pull/782), [#797](https://github.com/entireio/cli/pull/797))
- Attribution handling for long sessions ([#792](https://github.com/entireio/cli/pull/792))
- Cross-platform `files_touched` path normalization with `filepath.ToSlash` ([#803](https://github.com/entireio/cli/pull/803))
- OpenCode system-reminder messages appearing in transcript parser ([#671](https://github.com/entireio/cli/pull/671))
- External agent plugin discovery during git hook execution, ensuring token usage data in metadata ([#716](https://github.com/entireio/cli/pull/716))
- Local-dev hooks path resolution for non-Claude agents ([#745](https://github.com/entireio/cli/pull/745))
- Gemini subagent commits missing `Entire-Checkpoint` trailer in `prepare-commit-msg` ([#780](https://github.com/entireio/cli/pull/780))
- E2E timing flakiness with hardened assertions and carry-forward checkpoint condensation ([#787](https://github.com/entireio/cli/pull/787))

### Housekeeping

- Windows-compatible external agent name derivation and binary discovery ([#729](https://github.com/entireio/cli/pull/729))
- Linux PATH instruction for `go install` in README ([#764](https://github.com/entireio/cli/pull/764))
- Bumped go-git to fix `index decoder: invalid checksum` on some repos using the `TREE` extension ([#801](https://github.com/entireio/cli/pull/801))
- Dependency bumps: posthog-go 1.11.2, go-keyring 0.2.8, slackapi/slack-github-action 3.0.1 ([#786](https://github.com/entireio/cli/pull/786), [#755](https://github.com/entireio/cli/pull/755), [#695](https://github.com/entireio/cli/pull/695))

### Thanks

Thanks to @keyu98 for Windows-compatible agent name derivation and fixing external agent plugin discovery in git hooks! Thanks to @sheikhlimon for the Linux install docs, @erezrokah for the CLAUDE.md fix, and @mvanhorn for fixing OpenCode transcript parsing!

## [0.5.1] - 2026-03-19

### Added

- Sparse metadata fetch with on-demand blob resolution for reduced memory and network cost ([#680](https://github.com/entireio/cli/pull/680), [#721](https://github.com/entireio/cli/pull/721))
- `entire trace` command for diagnosing slow performance hooks and lifecycle events ([#652](https://github.com/entireio/cli/pull/652))
- Opt-in PII redaction with typed tokens ([#397](https://github.com/entireio/cli/pull/397))
- Auto-discover external agents during `entire enable`, `entire rewind`, and `entire resume` ([#678](https://github.com/entireio/cli/pull/678))
- Preview support for dedicated remote repository for checkpoint data, onboarded the CLI repository ([#677](https://github.com/entireio/cli/pull/677), [#732](https://github.com/entireio/cli/pull/732))
- E2E tests for external agents with roger-roger canary ([#700](https://github.com/entireio/cli/pull/700), [#702](https://github.com/entireio/cli/pull/702))
- hk hook manager detection ([#657](https://github.com/entireio/cli/pull/657))

### Changed

- Bumped go-git with improved large packfile memory efficiency ([#731](https://github.com/entireio/cli/pull/731))
- Use transcript size instead of line count for new content detection ([#726](https://github.com/entireio/cli/pull/726))
- Improved traversal resistance with `os.OpenRoot` ([#704](https://github.com/entireio/cli/pull/704))
- Upgraded to Go 1.26.1 and golangci-lint 2.11.3 ([#699](https://github.com/entireio/cli/pull/699))
- CLI command output consistency improvements ([#709](https://github.com/entireio/cli/pull/709))

### Fixed

- Gemini CLI 0.33+ hook validation by stripping non-array values from hooks config ([#714](https://github.com/entireio/cli/pull/714))
- Copilot checkpoint token scoping, session token backfill, and modelMetrics struct ([#717](https://github.com/entireio/cli/pull/717))
- Cursor 2026.03.11 transitioning from flat to nested path during a session ([#707](https://github.com/entireio/cli/pull/707))
- Rewind file path resolution when running from a subdirectory ([#663](https://github.com/entireio/cli/pull/663))
- `GetWorktreeID` handling `.bare/worktrees/` layout in bare repos ([#669](https://github.com/entireio/cli/pull/669))
- OpenCode over-redaction in session transcripts ([#703](https://github.com/entireio/cli/pull/703))
- Factory AI Droid prompt fallback to script parsing when hooks don't provide it ([#705](https://github.com/entireio/cli/pull/705))
- Resume fetching metadata branch on fresh clones where `entire/checkpoints/v1` doesn't exist locally ([#680](https://github.com/entireio/cli/pull/680))
- Remote branch detection for v6 metadata merging ([#662](https://github.com/entireio/cli/pull/662))
- Mise install detection for update command ([#659](https://github.com/entireio/cli/pull/659))
- Cursor-cli E2E flakiness with isolated config dir ([#654](https://github.com/entireio/cli/pull/654))

### Housekeeping

- Factory AI Droid added to all documentation ([#655](https://github.com/entireio/cli/pull/655))
- Copilot CLI added to all documentation ([#653](https://github.com/entireio/cli/pull/653))
- Updated Discord release message to include installation link ([#646](https://github.com/entireio/cli/pull/646))
- Dependency bumps: actions/create-github-app-token 3.0.0, jdx/mise-action 4, gitleaks 8.30.1 ([#706](https://github.com/entireio/cli/pull/706), [#694](https://github.com/entireio/cli/pull/694), [#689](https://github.com/entireio/cli/pull/689))
- Added tests for git remote related flows ([#696](https://github.com/entireio/cli/pull/696))
- "Why Entire" section in README ([#331](https://github.com/entireio/cli/pull/331))

### Thanks

Thanks to @mvanhorn for multiple contributions including hk hook manager detection, bare repo worktree ID fix, rewind subdirectory path fix, and mise install detection!

## [0.5.0] - 2026-03-06

### Added

- External agent plugin system with lazy discovery, timeout protection, feature-flag gating, and stdin/stdout caps ([docs](https://docs.entire.io/cli/external-agents), [#604](https://github.com/entireio/cli/pull/604))
- Vogon deterministic fake agent for cost-free E2E canary testing ([#619](https://github.com/entireio/cli/pull/619))
- `entire resume` now supports squash-merged commits by parsing checkpoint IDs from the metadata branch ([#534](https://github.com/entireio/cli/pull/534), [#602](https://github.com/entireio/cli/pull/602), [#593](https://github.com/entireio/cli/pull/593))
- `entire rewind` now supports squash-merged commits ([#612](https://github.com/entireio/cli/pull/612))
- Model name tracking and display in session info for Claude Code, Gemini CLI, Cursor, and Droid ([#595](https://github.com/entireio/cli/pull/595), [#581](https://github.com/entireio/cli/pull/581))
- Performance measurement (`perf` package) with span-based instrumentation across all lifecycle hooks ([#614](https://github.com/entireio/cli/pull/614))
- Cursor session metrics: duration, turns, model, and attribution captured via hooks ([#613](https://github.com/entireio/cli/pull/613))
- Commit hook perf benchmark with control baseline and scaling analysis ([#549](https://github.com/entireio/cli/pull/549))
- Discord notifications for new releases ([#624](https://github.com/entireio/cli/pull/624))
- Changelog-based release notes with CI enforcement ([#635](https://github.com/entireio/cli/pull/635))

### Changed

- Replaced O(N) go-git tree walks with `git diff-tree` in post-commit hook for faster commits ([#594](https://github.com/entireio/cli/pull/594))
- Removed `context.md` and scoped `prompt.txt` to checkpoint-only prompts; prompt source of truth is now shadow branch/filesystem, never transcript ([#572](https://github.com/entireio/cli/pull/572))
- Consolidated transcript file extraction behind `resolveFilesTouched` and `hasNewTranscriptWork` ([#597](https://github.com/entireio/cli/pull/597))
- Reconcile disconnected local/remote metadata branches automatically at read/write time and during `entire enable` ([#533](https://github.com/entireio/cli/pull/533))

### Fixed

- `entire explain` showing "(no prompt)" for multi-session checkpoints ([#633](https://github.com/entireio/cli/pull/633))
- Two-turn bug where second turn committed different files than first turn, causing carry-forward failure ([#578](https://github.com/entireio/cli/pull/578))
- Phantom file carry-forward causing lingering shadow branches ([#537](https://github.com/entireio/cli/pull/537))
- Spurious task checkpoints for agents without `SubagentStart` hook ([#577](https://github.com/entireio/cli/pull/577))
- OpenCode session end detection via `server.instance.disposed` ([#584](https://github.com/entireio/cli/pull/584))
- OpenCode turn-end hook chain for reliable checkpoints ([#541](https://github.com/entireio/cli/pull/541))
- Cursor `modified_files` forwarding from subagent-stop and transcript position tracking ([#598](https://github.com/entireio/cli/pull/598))
- Session state with nil `LastInteractionTime` causing immortal sessions ([#617](https://github.com/entireio/cli/pull/617))
- Dispatch test leaking session state into real repo ([#625](https://github.com/entireio/cli/pull/625))
- Error propagation in push, doctor, and post-commit paths ([#533](https://github.com/entireio/cli/pull/533))

### Housekeeping

- Droid E2E tests stabilized for CI ([#607](https://github.com/entireio/cli/pull/607))
- E2E tests show rerun command on failure ([#621](https://github.com/entireio/cli/pull/621))
- Added "Git in Tests" section to CLAUDE.md ([#625](https://github.com/entireio/cli/pull/625))
- Flaky external agent test fix with `ETXTBSY` retry ([#638](https://github.com/entireio/cli/pull/638))
- E2E workflow dynamically builds agent matrix for single-agent dispatch ([#609](https://github.com/entireio/cli/pull/609), [#616](https://github.com/entireio/cli/pull/616))
- E2E test failure alerting on main branch ([#603](https://github.com/entireio/cli/pull/603))
- tmux PATH propagation in E2E interactive tests ([#629](https://github.com/entireio/cli/pull/629))

### Thanks

Thanks to @erezrokah for contributing to this release!

## [0.4.9] - 2026-03-02

### Added

- Factory AI Droid agent integration with full checkpoint, resume, rewind, and session transcript support ([#435](https://github.com/entireio/cli/pull/435), [#552](https://github.com/entireio/cli/pull/552))
- `--absolute-git-hook-path` flag for `entire enable` to set up git hooks with absolute paths to the entire binary ([#495](https://github.com/entireio/cli/pull/495))
- Architecture tests enforcing agent package boundaries ([#569](https://github.com/entireio/cli/pull/569))

### Changed

- Improved TTY handling consolidated into a single location ([#543](https://github.com/entireio/cli/pull/543))
- Simplified PATH setup message in install script ([#566](https://github.com/entireio/cli/pull/566))
- Skip version check for dev builds instead of all prereleases ([#401](https://github.com/entireio/cli/pull/401))
- Skip fully-condensed ENDED sessions in PostCommit to avoid redundant work ([#556](https://github.com/entireio/cli/pull/556), [#568](https://github.com/entireio/cli/pull/568))
- Don't update LastInteraction when only git hooks were triggered ([#550](https://github.com/entireio/cli/pull/550))

### Fixed

- `entire explain` hanging on repos with many checkpoints ([#551](https://github.com/entireio/cli/pull/551))
- `prepare-commit-msg` hook performance for large repos ([#553](https://github.com/entireio/cli/pull/553))
- Don't wait for sessions older than 120s during transcript flush ([#545](https://github.com/entireio/cli/pull/545))

### Housekeeping

- Updated agent-integration skill docs ([#555](https://github.com/entireio/cli/pull/555))

## [0.4.8] - 2026-02-27

### Added

- Full checkpoint support for Cursor agent in IDE and CLI. Note: resume and rewind are missing for now ([#392](https://github.com/entireio/cli/pull/392), [#493](https://github.com/entireio/cli/pull/493), [#525](https://github.com/entireio/cli/pull/525), [#527](https://github.com/entireio/cli/pull/527))
- Consolidated E2E test suite moved into `e2e/` with per-agent filtering, transient error retry, preflight checks, and test report generation ([#474](https://github.com/entireio/cli/pull/474), [#508](https://github.com/entireio/cli/pull/508), [#539](https://github.com/entireio/cli/pull/539))
- Agent integration Claude skill for multi-phase agent onboarding ([#498](https://github.com/entireio/cli/pull/498))
- Post-commit cache to avoid redundant work on consecutive commits ([#500](https://github.com/entireio/cli/pull/500))
- `entire enable` now creates local metadata branch from remote when available, preserving checkpoints on fresh clones ([#511](https://github.com/entireio/cli/pull/511))
- `entire --version` now works as an alias for `entire version` ([#526](https://github.com/entireio/cli/pull/526))
- Mise linting to keep `mise.toml` clean; scripts moved into task files ([#530](https://github.com/entireio/cli/pull/530))
- `commit_linking` setting replaces the Strategy interface abstraction, with `[Y/n/a]` prompt on commit ([#531](https://github.com/entireio/cli/pull/531))

### Changed

- Extracted magic numbers to named constants ([#276](https://github.com/entireio/cli/pull/276))
- Removed auto-commit strategy entirely, making manual-commit the only strategy ([#405](https://github.com/entireio/cli/pull/405))
- Upgraded to Go 1.26 and golangci-lint 2.10.1 ([#458](https://github.com/entireio/cli/pull/458))
- O(depth) tree surgery replaces O(N) flatten-and-rebuild for both metadata branch and shadow branch writes ([#473](https://github.com/entireio/cli/pull/473), [#503](https://github.com/entireio/cli/pull/503))
- Renamed `paths.RepoRoot()` to `paths.WorktreeRoot()` for clarity ([#486](https://github.com/entireio/cli/pull/486))
- Local and CI linting now use the same configuration ([#504](https://github.com/entireio/cli/pull/504))
- Consistent context.Context threading through all function call chains (~25 `context.Background()`/`context.TODO()` replaced) ([#507](https://github.com/entireio/cli/pull/507), [#512](https://github.com/entireio/cli/pull/512))
- Unified `CalculateTokenUsage` into a single `agent.CalculateTokenUsage()` function ([#509](https://github.com/entireio/cli/pull/509))
- Removed backward-compatibility fallbacks for unknown agent types ([#515](https://github.com/entireio/cli/pull/515))
- Removed Strategy interface abstraction — `ManualCommitStrategy` is now used directly everywhere ([#531](https://github.com/entireio/cli/pull/531))
- Replaced `fmt.Fprintf(os.Stderr)` with structured logging in agent hook paths ([#538](https://github.com/entireio/cli/pull/538))
- Moved `AgentName` and `AgentType` to `agent/types` package to break import cycle ([#542](https://github.com/entireio/cli/pull/542))

### Fixed

- Carry-forward false positive when user replaces agent content before committing ([#502](https://github.com/entireio/cli/pull/502))
- Isolate integration tests from global git config ([#513](https://github.com/entireio/cli/pull/513))
- Using OpenCode with Codex models now correctly handle `apply_patch` events ([#521](https://github.com/entireio/cli/pull/521))
- Compaction resetting transcript offset, causing Gemini carry-forward to re-send already-condensed content ([#535](https://github.com/entireio/cli/pull/535))
- Handle OpenCode `database is locked` errors during parallel E2E tests ([#540](https://github.com/entireio/cli/pull/540))

### Docs

- Agent integration guide and checklist updated for Cursor and OpenCode ([#410](https://github.com/entireio/cli/pull/410), [#510](https://github.com/entireio/cli/pull/510))
- E2E test README and debug skill ([#474](https://github.com/entireio/cli/pull/474))
- Cursor agent documentation ([#493](https://github.com/entireio/cli/pull/493), [#525](https://github.com/entireio/cli/pull/525))

### Thanks

Thanks to @ishaan812 for contributing to this release!

Thanks to @9bany ([#260](https://github.com/entireio/cli/pull/260)) for their Cursor PR! We've now merged our Cursor integration. While we went with our own implementation, your PR were valuable in helping us validate our design choices and ensure we covered the right scenarios. We appreciate the effort you put into this!

## [0.4.7] - 2026-02-24

### Fixed

- Commits hanging for up to 3s per session while waiting for transcript updates that were already flushed ([#482](https://github.com/entireio/cli/pull/482))

### Housekeeping

- Updated README to include OpenCode in the supported agent list ([#476](https://github.com/entireio/cli/pull/476))

## [0.4.6] - 2026-02-24

### Added

- OpenCode agent support with resume, rewind, and session transcripts ([#415](https://github.com/entireio/cli/pull/415), [#428](https://github.com/entireio/cli/pull/428), [#439](https://github.com/entireio/cli/pull/439), [#445](https://github.com/entireio/cli/pull/445), [#461](https://github.com/entireio/cli/pull/461), [#465](https://github.com/entireio/cli/pull/465))
- `IsPreview()` on Agent interface to replace hardcoded name checks ([#412](https://github.com/entireio/cli/pull/412))
- Stale session file cleanup ([#438](https://github.com/entireio/cli/pull/438))
- Redesigned `entire status` with styled output and session cards ([#448](https://github.com/entireio/cli/pull/448))
- Benchmark utilities for performance testing ([#449](https://github.com/entireio/cli/pull/449))

### Changed

- Refactored Agent interface: moved hook methods to `HookSupport`, removed unused methods ([#360](https://github.com/entireio/cli/pull/360), [#425](https://github.com/entireio/cli/pull/425), [#427](https://github.com/entireio/cli/pull/427), [#429](https://github.com/entireio/cli/pull/429))
- `entire enable` now uses multi-select for agent selection with re-run awareness ([#362](https://github.com/entireio/cli/pull/362), [#443](https://github.com/entireio/cli/pull/443))
- Use Anthropic API key for Claude Code agent detection ([#396](https://github.com/entireio/cli/pull/396))
- Don't track gitignored files in session metadata ([#426](https://github.com/entireio/cli/pull/426))
- Performance optimizations for `entire status` and `entire enable`: cached git paths, pure Go git operations, reftable support ([#436](https://github.com/entireio/cli/pull/436), [#454](https://github.com/entireio/cli/pull/454))
- Streamlined `entire enable` setup flow with display names and iterative agent handling ([#440](https://github.com/entireio/cli/pull/440))
- Git hooks are now a no-op if Entire is not enabled in the repo ([#445](https://github.com/entireio/cli/pull/445))
- Resume sessions now sorted by creation time ascending ([#447](https://github.com/entireio/cli/pull/447))

### Fixed

- Secret redaction hardened across all checkpoint persistence paths ([#395](https://github.com/entireio/cli/pull/395))
- Gemini session restore following latest Gemini pattern ([#403](https://github.com/entireio/cli/pull/403))
- Transcript path stored in checkpoint metadata breaking location independence ([#403](https://github.com/entireio/cli/pull/403))
- Integration tests hanging on machines with a TTY ([#414](https://github.com/entireio/cli/pull/414))
- Stale ACTIVE/IDLE/ENDED sessions incorrectly condensed into every commit ([#418](https://github.com/entireio/cli/pull/418))
- Gemini TTY handling when called as a hook ([#430](https://github.com/entireio/cli/pull/430))
- Deselected agents reappearing as pre-selected on re-enable ([#443](https://github.com/entireio/cli/pull/443))
- UTF-8 truncation producing garbled text for CJK/emoji characters ([#444](https://github.com/entireio/cli/pull/444))
- Git refs resembling CLI flags causing errors ([#446](https://github.com/entireio/cli/pull/446))
- Over-aggressive secret redaction in session transcripts ([#471](https://github.com/entireio/cli/pull/471))

### Docs

- Security and privacy documentation ([#398](https://github.com/entireio/cli/pull/398))
- Agent integration checklist for validating new agent integrations ([#442](https://github.com/entireio/cli/pull/442))
- Clarified README wording and agent-agnostic troubleshooting ([#453](https://github.com/entireio/cli/pull/453))

### Thanks

Thanks to @AlienKevin for contributing to this release!

Thanks to @ammarateya ([#220](https://github.com/entireio/cli/pull/220)), @Avyukth ([#257](https://github.com/entireio/cli/pull/257)), and @MementoMori123 ([#315](https://github.com/entireio/cli/pull/315)) for their OpenCode PRs! We've now merged our OpenCode integration. While we went with our own implementation, your PRs were valuable in helping us validate our design choices and ensure we covered the right scenarios. We appreciate the effort you put into this!

## [0.4.5] - 2026-02-17

### Added

- Detect external hook managers (Husky, Lefthook, Overcommit) and warn during `entire enable` ([#373](https://github.com/entireio/cli/pull/373))
- New E2E test workflow running on merge to main ([#350](https://github.com/entireio/cli/pull/350), [#351](https://github.com/entireio/cli/pull/351))
- Subagent file modifications are now properly detected ([#323](https://github.com/entireio/cli/pull/323))
- Content-aware carry-forward for 1:1 checkpoint-to-commit mapping ([#325](https://github.com/entireio/cli/pull/325))

### Changed

- Consolidated duplicate JSONL transcript parsers into a shared `transcript` package ([#346](https://github.com/entireio/cli/pull/346))
- Replaced `ApplyCommonActions` with `ActionHandler` interface for cleaner hook dispatch ([#332](https://github.com/entireio/cli/pull/332))

### Fixed

- Extra shadow branches accumulating when agent commits some files and user commits the rest ([#367](https://github.com/entireio/cli/pull/367))
- Attribution calculation for worktree inflation and mid-turn agent commits ([#366](https://github.com/entireio/cli/pull/366))
- All IDLE sessions being incorrectly added to a checkpoint ([#359](https://github.com/entireio/cli/pull/359))
- Hook directory resolution now uses `git --git-path hooks` for correctness ([#355](https://github.com/entireio/cli/pull/355))
- Gemini transcript parsing: array content format and trailer blank line separation for single-line commits ([#342](https://github.com/entireio/cli/pull/342))

### Docs

- Added concurrent ACTIVE sessions limitation to contributing guide ([#326](https://github.com/entireio/cli/pull/326))

### Thanks

Thanks to @AlienKevin for contributing to this release!

## [0.4.4] - 2026-02-13

### Added

- `entire explain` now fully supports Gemini transcripts ([#236](https://github.com/entireio/cli/pull/236))

### Changed

- Improved git hook auto healing, also working for the auto-commit strategy now ([#298](https://github.com/entireio/cli/pull/298))
- First commit in the `entire/checkpoints/v1` branch is now trying to lookup author info from local and global git config ([#262](https://github.com/entireio/cli/pull/262))

### Fixed

- Agent settings.json parsing is now safer and preserves unknown hook types ([#314](https://github.com/entireio/cli/pull/314))
- Clarified `--local`/`--project` flags help text to indicate they reference `.entire/` settings, not agent settings ([#306](https://github.com/entireio/cli/pull/306))
- Removed deprecated `entireID` references ([#285](https://github.com/entireio/cli/pull/285))

### Docs

- Added requirements section to contributing guide ([#231](https://github.com/entireio/cli/pull/231))

## [0.4.3] - 2026-02-12

### Added

- Layered secret detection using gitleaks patterns alongside entropy-based scanning ([#280](https://github.com/entireio/cli/pull/280))
- Multi-agent rewind and resume support for Gemini CLI sessions ([#214](https://github.com/entireio/cli/pull/214))

### Changed

- Git hook installation now uses hook chaining instead of overwriting existing hooks ([#272](https://github.com/entireio/cli/pull/272))
- Hidden commands are excluded from the full command chain in help output ([#238](https://github.com/entireio/cli/pull/238))

### Fixed

- "Reference not found" error when enabling Entire in an empty repository ([#255](https://github.com/entireio/cli/pull/255))
- Deleted files in task checkpoints are now correctly computed ([#218](https://github.com/entireio/cli/pull/218))

### Docs

- Updated sessions-and-checkpoints architecture doc to match codebase ([#217](https://github.com/entireio/cli/pull/217))
- Fixed incorrect resume documentation ([#224](https://github.com/entireio/cli/pull/224))
- Added `mise trust` to first-time setup instructions ([#223](https://github.com/entireio/cli/pull/223))

### Thanks

Thanks to @fakepixels, @jaydenfyi, and @kserra1 for contributing to this release!

## [0.4.2] - 2026-02-10

<!-- Previous release -->
