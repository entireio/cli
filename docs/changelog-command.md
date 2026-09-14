# Product changelog

`entire changelog` reads the latest five entries from the [Entire product
changelog](https://entire.io/blog?category=Changelog), which covers all Entire
products, and the [CLI release changelog](https://github.com/entireio/cli/blob/main/CHANGELOG.md). It works without a login, Git repository, or Entire setup.

```sh
entire changelog
entire changelog --limit 10
entire changelog --json
entire changelog --only-cli
entire changelog search "Windows" --only-cli --json
entire changelog search subagent
entire changelog search "git network" --limit 10 --json
```

`--only-cli` lists or searches only CLI releases and skips the product feed.
The filter applies before `--limit`.

Search matches a case-insensitive literal substring in the title, description,
or Markdown body. Quote multiword queries to search for a phrase. Results are
merged by publication date, newest first, with slug ascending for ties. Both
sources publish day-level dates, so no time of day is inferred. Undated
`Unreleased` sections are excluded. CLI entries have titles such as `Entire CLI
0.10.6` and slugs such as `cli-0.10.6`. `--limit` must be positive and
applies after matching. Search does not support regular expressions or relevance
ranking.

Each result includes its title, date, web link, and full body, without YAML
frontmatter. Terminals show styled Markdown. Pipes, `NO_COLOR`, and accessibility
mode (`ACCESSIBLE=1`) receive plain Markdown. There is no pager or prompt.

Plain text and JSON preserve MDX components as source. Use the web link to view
videos and other media; the CLI does not execute components or fetch embedded
resources.

JSON is a single array, with `slug`, `title`, `date` (`YYYY-MM-DD`), `description`,
`category`, `url`, `markdown_url`, and `content` fields. No matches returns `[]`
in JSON or `No changelog entries found.` in text, with a successful exit status.
If either feed or a required post cannot be fetched or parsed, the command returns an error
before printing any results.

There is no persistent cache. By default, each invocation fetches the product index and the CLI changelog Markdown file.
With `--only-cli`, only the CLI changelog file is fetched.
Listing downloads only the selected product posts; a
search with no matches may download every changelog post. Downloads use at most
four concurrent post requests, each with a 20-second timeout and a 2 MiB response
limit. Post URLs and redirects must stay on `https://entire.io/blog/` and name
Markdown files. The CLI feed is fetched from
`https://raw.githubusercontent.com/entireio/cli/refs/heads/main/CHANGELOG.md`
with the same timeout and size limit; redirects must retain that exact URL.
