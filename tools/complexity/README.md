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
| `gate/main.go` | the CI check. Reads the CSVs the others write, by header name rather than column position. Blocking mode diffs two `functions.csv` files and fails on functions that are both over `-cognit-warn` and new or worsened; advisory mode compares `features.csv` coverage against a committed baseline and never fails. See "CI gate" below |
| `internal/cx/` | the one home of the feature mapping (rule matching, rank policy), module-path reading, and CSV writing — shared by all four binaries |
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

## CI gate

`gate/` turns these tables into a check. It has two modes with opposite failure semantics, and they are separate invocations for that reason.

### What blocks

The `complexity-gate` job in `.github/workflows/ci.yml` runs on every pull request and fails it when a function is over cognitive complexity 30 **and** is new or worse than at the merge base. A function that is over the threshold and did not move does not fail: the tree has 105 of those, and a gate that fired on all of them would be switched off within a week. The three failing shapes are a function absent from the base tree (so a moved or renamed file counts as entirely new — the identity is file plus function name, since the CSVs carry no stable symbol ID), a function whose base complexity was within the threshold, and a function that grew.

Locally, the same comparison:

```sh
mise run complexity:gate                          # working tree vs the merge base with origin/main
BASE_REF=origin/some-branch mise run complexity:gate
COGNIT_WARN=25 mise run complexity:gate
```

It builds the head tree's `cxtool` and `gate`, exports the merge-base tree with `git archive`, and runs `cxtool` over both — deliberately with no `-cover` and no churn window, which keeps each run to about a second. Both sides are measured by the *head* version of the tool and of `features.json`, so a change to `cxtool` itself never reads as a change in complexity.

This is deliberately **not** the `gocognit` linter. golangci-lint has no per-linter new-only mode: enabling `gocognit` in `.golangci.yaml` would fail `mise run lint` locally on all 105 pre-existing violations, and the alternative — `only-new-issues: true` on the golangci-lint action — is workflow-wide, so it would silently flip every other linter to new-only in CI as well. The threshold lives in the gate's flags (`-cognit-warn`, `COGNIT_WARN`), not in `.golangci.yaml`.

### What advises

The second mode compares each ranked feature's statement coverage against a committed baseline snapshot and reports drops past a tolerance. It never fails: it exits 0 on a finding, and exits 0 even when it cannot read its inputs (emitting a `::warning::` instead), because the snapshot is one commit's numbers and goes stale as the tree moves — features get renamed, files change hands, suites get resharded.

```sh
# after taking coverage profiles as in "Run" above
go run . -root ../.. -cover /tmp/cx/unit.out,/tmp/cx/int.out -out /tmp/cx/all
go run ./gate -features /tmp/cx/all/features.csv \
  -baseline baseline/2026-08-31/features.csv -cov-drop 2.0 -min-stmts 300
```

`-min-stmts` skips small features, whose coverage swings several points on one helper, and unranked features (test infrastructure, generated code) are skipped along with any feature missing from either side or without a coverage figure.

This half is **not wired into CI yet**, and wiring it needs a decision first: the profiles the CI test jobs could produce are not comparable with the committed baseline. `mise run test:ci:core` runs with `-tags=integration -race` over every package but `integration_test`, and the three `test:ci:integration:shard` jobs cover that one package under `-run` filters; the baseline's `cov_pct` column came from the untagged, unraced two-command recipe in "Run" above. Feeding CI's profiles into a comparison against this baseline would report the recipe change as a coverage regression on the first run. The two ways out are to refresh the baseline with the CI recipe (and keep the two in step from then on), or to give the advisory its own non-blocking job that reproduces the documented recipe — which costs a duplicate coverage run but keeps the numbers apples-to-apples and keeps instrumentation off the blocking test path.

### Refreshing the baseline

Refreshing `baseline/<date>/` is manual and meant to be deliberate: it is the reference the coverage advisory measures against, so re-taking it silently accepts whatever has drifted since. Take a new dated directory (following the "Run" recipe, then copying in `report.md`, the four rollup CSVs, `dupl-by-feature.json`, and a `COMMIT` file naming the tree), and say in the pull request what moved and why that is acceptable. The blocking half needs none of this — it always diffs against the merge base, so it cannot go stale.

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
