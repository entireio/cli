# Settings testdata

Canonical example settings.json files. Loaded by `TestLoadV2_TestdataExamples`
in `load_v2_test.go`; doubles as hand-readable documentation of the v2 shape.

## Files

| File | Description |
| --- | --- |
| `v2-minimal.json` | Smallest meaningful schema-v2 file: `schema` + a primary backend. |
| `v2-with-gmeta-mirror.json` | Primary v2 + a gmeta mirror (write-only fan-out). |
| `v2-with-git-config.json` | Primary v2 + a git destination override (separate checkpoint repo). |
| `v2-kitchen-sink.json` | Every documented field populated — useful as a reference. |
| `legacy-equivalent.json` | Legacy-shape settings that synthesizes to the same struct as `v2-kitchen-sink.json`. |

## Migration mapping

`legacy-equivalent.json` and `v2-kitchen-sink.json` are paired: loading either
through `LoadV2FromBytes` produces an identical `*Settings` value (modulo
defaults that round-trip correctly). The pairing is the most concrete way
to read the legacy → v2 mapping.

When adding a new legacy field that the synthesizer should translate, add it
to both files and the equivalence test confirms the round-trip.
