# entire-agent-hummin

An [Entire](https://github.com/entireio/cli) external agent plugin for the
[hummin](https://github.com/rennf93/hummin) coding agent (a fork of pi with
`.pi` → `.hummin`, `pi` → `hummin`). Single-file Node script, Node >= 20, no
npm dependencies.

Design notes, command reference, hook mapping, and limitations live in the
CLI docs: [docs/architecture/hummin-agent.md](../../docs/architecture/hummin-agent.md).
The wire format it implements is the
[external agent protocol](../../docs/architecture/external-agent-protocol.md).

## Install

```sh
cp entire-agent-hummin /usr/local/bin/    # any dir on $PATH
chmod +x /usr/local/bin/entire-agent-hummin
entire agent add hummin
```

## Test

```sh
./test.sh
```

The harness prints PASS/FAIL per protocol command against a synthetic hummin
session, checks a real hummin session store read-only when one exists, and
(optionally, with Go installed) runs `entire hooks hummin <event>` through the
real CLI. See the script header for details.
