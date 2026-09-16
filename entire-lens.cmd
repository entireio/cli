@echo off
REM Entire external-command shim: `entire lens ...` resolves to `entire-lens`
REM on PATH via the CLI's kubectl-style external-command lookup.
python -m tools.checkpoint_lens.cli %*
