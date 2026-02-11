# Session Context

## User Prompts

### Prompt 1

Implement the following plan:

# Plan: Use `mise run lint` in GitHub Actions

## Problem

The CI lint workflow uses a mix of approaches:
1. Raw `gofmt -l -s .` shell command for formatting checks
2. `golangci/golangci-lint-action@v9` GitHub Action for linting
3. `mise run lint:gomod` for go.mod tidiness

Meanwhile, locally we use `mise run fmt` and `mise run lint`. This is inconsistent — if a developer runs `mise run lint` locally and it passes, CI should behave the same way.

## Changes

### ...

