# tools/complexity — deterministic complexity baseline

Answers, per feature and per command: how much code is there, how hard is it
to read (gocyclo / gocognit), how much of it is exercised by tests, how often
it changes, and what each command exclusively owns.

This is a separate Go module (`cxtool`) so its dependencies stay out of the
CLI's `go.mod`; `go list ./...` from the repo root does not descend into it,
and it in turn skips nested modules when walking the tree.

## Pieces

| file | what |
| --- | --- |
| `main.go` | parses every non-test Go file (in parallel); per function LOC, cyclomatic (`fzipp/gocyclo`), cognitive (`uudashr/gocognit`); folds `go test -coverprofile` files in a single streaming pass (merged per block, max count) and `git log --numstat` churn (one git run, bucketed per window); rolls up by `features.json` |
| `reach/main.go` | builds SSA + a VTA call graph (`golang.org/x/tools/go/callgraph/vta`); every `*cobra.Command`-returning func is a root, plus `<init>` and one `<main:pkg>`; reports exclusive vs shared LOC per root and declarations no root reaches. Its precision policies (`-maxfanout` cutoff for unresolved dynamic calls, closures only via their creator, generic instantiations rolled up to their origin) are documented at the top of the file; `-who <substr>` explains why a symbol is or isn't reached |
| `dupl/main.go` | rolls a golangci-lint `dupl` JSON report up by feature. golangci reports `Pos.Filename` relative to its **config file's** directory while the path quoted in the issue text is repo-relative, so `-base` (default: the working directory) says what the former resolves against and each path is resolved against whichever base names a file that exists |
| `internal/cx/` | the one home of the feature mapping (rule matching, rank policy), module-path reading, and CSV writing — shared by all three binaries |
| `features.json` | path → feature rules, resolved in three ordered steps, first match winning: a rule naming the exact path (so an explicit `_test.go` rule always wins); then, for a test file, its source name (`foo_test.go` → `foo.go`) so tests follow their source; then globs and `dir/` prefixes. Exact paths beat globs regardless of list order — otherwise a broad rule like `setup*.go` silently absorbs a later one naming `setup_import.go`, and the file lands in a plausible wrong feature instead of in `_unmapped`, where nothing reports it. A pattern `path.Match` cannot parse is rejected at load rather than matching nothing. `norank: true` keeps a feature out of rankings and headline totals while still measuring it. Edit this when a command family moves; unmapped files are reported and counted in `report.json` meta |
| `render.py` | renders the HTML report from `report.json` + `reach.json` + `dupl-by-feature.json`. Its "Reading the numbers" section is snapshot-bound interpretation and says so |
| `dupl-config.yaml` | golangci config for the duplication scan (same recipe as `mise run dup`) |
| `baseline/<date>/` | snapshot of that day's small, diffable outputs — `report.md`, the four rollup CSVs, `dupl-by-feature.json` — plus a `COMMIT` file naming the tree they were taken at. The per-function and per-root dumps (`functions.csv`, `files.csv`, `reach.md`) and the `.json` payloads are regenerable and left out |

## Run

From the repo root:

```sh
mkdir -p /tmp/cx
# 1. coverage profiles (≈ 5 min; unit + in-process integration)
go test -count=1 -coverprofile=/tmp/cx/unit.out -coverpkg=./... ./...
go test -count=1 -tags=integration -coverprofile=/tmp/cx/int.out -coverpkg=./... \
  ./cmd/entire/cli/integration_test/... ./cmd/entire/cli/auth/...

cd tools/complexity
# 2. metrics + rollup (seconds)
go run .       -root ../.. -cover /tmp/cx/unit.out,/tmp/cx/int.out -out /tmp/cx/all
# 3. command reachability (seconds); -who SomeFunc explains one symbol
go run ./reach -root ../.. -out /tmp/cx/reach
# 4. duplication per feature
# run the scan from the repo root: golangci-lint typechecks against the module
# it is invoked in, and `../...` from here is outside this nested module
(cd ../.. && golangci-lint run -c tools/complexity/dupl-config.yaml --new=false \
  --max-issues-per-linter=0 --max-same-issues=0 \
  --output.json.path=/tmp/cx/dupl.json ./...)
# -base defaults to the working directory, which is what the config-relative
# paths above resolve against when this is run from tools/complexity
go run ./dupl  -in /tmp/cx/dupl.json -root ../.. -out /tmp/cx/dupl-by-feature.json
# 5. page
python3 render.py /tmp/cx/all /tmp/cx/reach /tmp/cx/dupl-by-feature.json ../.. /tmp/cx/baseline.html
```

All flags have `-h` help; thresholds are flags (`-cyclo-warn`, `-cognit-warn`,
`-cov-warn`), and their effective values travel in `report.json` meta so the
rendered page labels itself from data.

Outputs: `report.md` (human summary), `features.csv` / `areas.csv` /
`packages.csv` / `files.csv` / `functions.csv` (diffable tables), `report.json`
(everything, including meta), `reach.md` / `commands.csv` / `reach.json`.

## Reading the numbers

- **Σ cognit** — sum of cognitive complexity over a feature's functions; the
  best single "how much is there to understand" number.
- **cog/100** — cognitive complexity per 100 production lines; density.
  Parsers and renderers run high by nature; a command surface running high is a
  smell.
- **uncovered cognit** — cognitive complexity in functions with < `-cov-warn`
  statement coverage; the "complex and untested" number.
- **exclusive LOC** (reach) — code reachable from exactly one root; deleting
  that command frees this much. "reached LOC" is everything it can touch and
  is generous because of interface dispatch.
- **reached by no root** — nothing in the shipped binaries *calls* it (tests
  excluded; reflection not modelled). That is not the same as "safe to
  delete": a method can be required by a Go interface and never called, and
  this tool counts calls, not declarations. Before deleting a method, pair
  `-who <name>` with a grep for the name in interface declarations — an
  interface method with zero callers is structurally load-bearing, and removing
  it breaks every implementation. `agent.Agent`'s `ReadSession` and
  `GetSessionID` are exactly this shape: 0 owners, 11 implementations.

## Known limits

- Generated code (`ast.IsGenerated`) is counted in LOC but excluded from
  complexity.
- Coverage is statement coverage from the unit and integration suites; the
  e2e suite spawns binaries and is not included.
- VTA over-approximates dynamic dispatch; see the policy comment at the top of
  `reach/main.go` for how that is contained and what remains invisible
  (reflection, template lookups).
- The graph records calls, not declarations, so an uncalled method that an
  interface requires looks identical to a dead one. Interface satisfaction is
  structural in Go and nothing in the callgraph represents it; the grep above
  is the check, and there is no plan to infer it here.
- Churn does not follow renames.
