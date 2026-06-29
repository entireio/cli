# `entire review`

Experimental review command for running one configured review profile.

## Basic use

```sh
entire review --configure       # create or edit a profile
entire review --list            # list profiles
entire review <profile>         # run a profile
entire review --profile <name>  # same as positional form
entire review --agent <name>    # run one reviewer from the profile
entire review --findings        # view local findings
```

Useful run flags:

```sh
entire review --prompt "focus on auth"
entire review --timeout 15m
entire review --agent claude-code --model opus
```

## Profiles

Profiles live in:

- `.entire/settings.json` for shared project config
- `.entire/settings.local.json` for local, git-ignored config

A profile contains:

- `task`: what to review for
- `agents`: reviewer slots
- `judge`: optional final consolidating reviewer
- `output`: `local` or `trail`

Minimal example:

```json
{
  "review_default_profile": "general",
  "review_profiles": {
    "general": {
      "task": "Review this change for correctness, regressions, tests, and maintainability.",
      "agents": {
        "claude-code": {"skills": ["/review"]},
        "codex": {"skills": ["/review"]}
      },
      "judge": {"agent": "claude-code"},
      "output": "local"
    }
  }
}
```

## Behavior

- Reviewers run the profile task.
- Multi-reviewer profiles run reviewers concurrently, then run one judge.
- Results are printed and saved locally; `output: "trail"` also posts findings to the branch trail.
- A bare non-interactive `entire review` does not auto-run a profile. Automation should pass a profile name.

## Key files

- `cmd/entire/cli/review/cmd.go`
- `cmd/entire/cli/review/picker.go`
- `cmd/entire/cli/review/profile.go`
- `cmd/entire/cli/review/run.go`
- `cmd/entire/cli/review/run_multi.go`
- `cmd/entire/cli/review/synthesis_sink.go`
- `cmd/entire/cli/review_bridge.go`
- `cmd/entire/cli/settings/settings.go`
