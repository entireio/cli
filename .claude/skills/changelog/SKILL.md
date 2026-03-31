---
name: changelog
description: >
  Generate or update the CHANGELOG.md for a new release version.
  Use when the user says "generate changelog", "update changelog", "write release notes",
  or asks to prepare a changelog for a version like "changelog for 0.5.3".
---

# Changelog Generation

Generate a changelog entry for a new release by analyzing merged PRs since the last tag.

## Parameters

The user provides:
- **Version number** -- e.g., `0.5.3`
- **Additional PRs** -- optionally, PRs not yet merged that should be included

## Step 1: Gather Data

1. Find the previous release tag: `git tag --sort=-version:refname | head -1`
2. List all merge commits since that tag: `git log <tag>..HEAD --merges --format='%s' | grep -v "Merge branch"`
3. Get PR details with: `gh pr list --state merged --base main --search "merged:><tag-date>" --json number,title,author --limit 50`
4. List contributors: `git log <tag>..HEAD --format='%an' --no-merges | sort | uniq -c | sort -rn`

## Step 2: Identify External Contributors

To determine if a contributor is external, check for an `@entire.io` email:

```bash
git log --all --format='%an <%ae>' --author="<name>" | sort -u
```

If they have an `@entire.io` email anywhere in git history, they are **internal**. Only list external contributors in the Thanks section.

Also check the memory file at `memory/project_team_members.md` for known internal/external mappings.

## Step 3: Write the Entry

Insert the new version section at the top of `CHANGELOG.md`, after the header and before the previous version.

### Format

Follow the existing style in CHANGELOG.md exactly:

```markdown
## [X.Y.Z] - YYYY-MM-DD

### Added

- Feature description with context ([#123](https://github.com/entireio/cli/pull/123))

### Changed

- Change description ([#456](https://github.com/entireio/cli/pull/456))

### Fixed

- Bug fix description ([#789](https://github.com/entireio/cli/pull/789))

### Housekeeping

- Maintenance item ([#101](https://github.com/entireio/cli/pull/101))

### Thanks

Thanks to @contributor for description of contribution!
```

### Style Rules

- **Sections**: Added, Changed, Fixed, Housekeeping, Thanks. Omit empty sections.
- **Each bullet**: starts with a dash, concise description, PR link(s) at the end
- **Group related PRs** into a single bullet when they're part of the same feature/fix
- **Work-in-progress features**: call out explicitly, e.g., "Feature X (work in progress): ..."
- **Known limitations**: note inline, e.g., "Note: subagent tracking is not yet supported due to..."
- **Thanks section**: only external contributors. Name what they contributed specifically.
- **Dependency bumps**: group into a single Housekeeping bullet unless a bump fixes a notable bug
- **PR links**: always use full URL format `[#N](https://github.com/entireio/cli/pull/N)`
- **No trailing period** on bullet items
- **Date**: use the current date in YYYY-MM-DD format

### Categorization Guide

- **Added**: new features, new commands, new agent integrations, new CI workflows
- **Changed**: behavior changes, API changes, UX changes, migrations
- **Fixed**: bug fixes, E2E fixes, agent-specific fixes
- **Housekeeping**: dependency bumps, docs, refactors, CI improvements, test improvements
