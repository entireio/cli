# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

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
