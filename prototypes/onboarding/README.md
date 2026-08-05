# Onboarding prototype (throwaway)

A **mocked** prototype of the "one-keypress" `entire enable` onboarding UX — a
single inline review screen where every value is pre-decided and editable, and
**Enter** builds the folder. Every side effect (login, mirror, hooks, git) is
faked so we can iterate on flow/wording fast.

This is a **separate Go module** on purpose: the main module's build, lint, and
CI skip it, so the prototype never has to satisfy production lint rules.

Nothing here imports `cmd/entire/cli` — it only shares cobra/bubbletea/lipgloss
so the look and feel transfers to a real implementation.

## Run

```bash
cd prototypes/onboarding
go run .                       # auto-detect the current folder
go run . --state repo-gh       # simulate a GitHub repo
go run . --state repo-no-origin
go run . --state empty          # empty folder (add --github to publish+mirror)
go run . --state repo-gh --yes  # non-interactive (skip the review screen)
```

`--state`: `auto` (default, real detection) · `repo-gh` · `repo-no-origin` · `empty`.
Other flags: `--yes`, `--fast` (no simulated delays), `--github`, `--no-telemetry`,
`--agent`, `--region`.

Keys: `↑↓` move · `←→`/`space` change a row (Agents expands to multi-select) ·
`enter` accept · `esc`/`q` cancel.
