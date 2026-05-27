# Entire Sem

`entire-sem` is an Entire CLI plugin for entity-level checkpoint context.

Entire already knows a checkpoint touched `auth.py`. This plugin answers the next question:
which functions, classes, types, or methods changed inside that file?

Once built as `entire-sem` and installed as an Entire plugin, it is invoked as:

```sh
entire sem commit HEAD
entire sem checkpoint abc123def456
entire sem diff --base HEAD~1 --head HEAD
entire sem analyze --json
```

## Status

This plugin implements the semantic checkpoint context proposed in
[entireio/cli#589](https://github.com/entireio/cli/issues/589). It intentionally
does not vendor or copy Ataraxy Labs' `inspect` / `sem` projects.

The MVP uses a tree-sitter-backed parser for:

- Go
- Python
- JavaScript / TypeScript
- Rust

The parser is isolated behind `internal/sem`, so the command surface can stay stable
while the semantic model gets richer.

## Install Locally

```sh
cd entire-sem
mise run build
entire plugin install ./entire-sem --force
```

## Commands

Compare one commit against its first parent:

```sh
entire sem commit HEAD
```

Compare two arbitrary refs:

```sh
entire sem diff --base main --head HEAD
```

Emit JSON:

```sh
entire sem diff --base main --head HEAD --json
```

Analyze the commit associated with an Entire checkpoint trailer:

```sh
entire sem checkpoint abc123def456
```

Run without installing through Entire:

```sh
ENTIRE_REPO_ROOT=/path/to/repo ./entire-sem diff --base HEAD~1 --head HEAD
```

## Example Output

```text
Semantic changes HEAD~1..HEAD

auth.py
  ~ function validate_token signature changed (14 dependents)
  + class TokenClaims added
  - function parse_token removed (0 dependents)
```

## Why This Exists

Issue [entireio/cli#589](https://github.com/entireio/cli/issues/589) proposes showing
checkpoint context at the entity level instead of stopping at "this file changed."
`entire-sem` is a plugin-shaped implementation of that idea:

- parse the before and after git trees with tree-sitter
- extract named entities like functions, classes, methods, structs, traits, and types
- compare signatures and normalized bodies
- build a heuristic dependent count from parsed references in the target tree
- report added, removed, renamed, signature-changed, and body-changed entities

The implementation does not copy or vendor Ataraxy Labs code. The parser dependency is
`github.com/smacker/go-tree-sitter`, which is MIT-licensed.

## Current Limits

- Dependent counts are heuristic, not compiler/type-checker accurate.
- Rename detection is heuristic.
- Native `entire explain` / `entire rewind` rendering requires the matching Entire CLI
  bridge that invokes this plugin when it is installed.
