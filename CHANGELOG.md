# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

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
