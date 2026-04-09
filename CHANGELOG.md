# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

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
