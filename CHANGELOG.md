# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

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
