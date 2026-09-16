#!/usr/bin/env sh
# Prove that `recall` makes no network calls.
#
# The privacy boundary for sensitive repositories is that prompts and
# transcripts never reach a new external service. This script does not assert
# that; it measures it. It runs `recall ingest` (graph disabled, so only the
# recall binary itself is under test) and `recall activate` under strace with
# the network syscall family traced across every child process, and fails if
# a single socket, connect, send or receive is recorded.
#
# Usage:  recall/scripts/verify-offline.sh [path-to-recall-binary]
# Needs:  strace. Exits 2 when it is missing rather than claiming success.
set -eu

here=$(cd "$(dirname "$0")/.." && pwd)
bin=${1:-"$here/target/release/recall"}
if ! command -v strace >/dev/null 2>&1; then
    echo "verify-offline: strace not installed; cannot verify" >&2
    exit 2
fi
if [ ! -x "$bin" ]; then
    echo "verify-offline: no binary at $bin (run: cargo build --release)" >&2
    exit 2
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# The fixture is a bare checkpoint list; wrap it in the ingest envelope with
# the graph disabled.
printf '{"repo_root":"","checkpoints":' >"$work/ingest.json"
cat "$here/fixtures/checkpoints.json" >>"$work/ingest.json"
printf '}' >>"$work/ingest.json"

strace -f -e trace=network -o "$work/ingest.trace" \
    "$bin" ingest --brain "$work/brain" --no-graph <"$work/ingest.json" >/dev/null
strace -f -e trace=network -o "$work/activate.trace" \
    "$bin" activate --brain "$work/brain" --k 3 "why did we drop the retry wrapper" >/dev/null

# strace writes one line per traced syscall plus exit markers; anything that
# is not an exit marker is a network syscall.
calls=$(grep -hvE 'exited with|\+\+\+|--- SIG' "$work/ingest.trace" "$work/activate.trace" || true)
procs=$(cat "$work/ingest.trace" "$work/activate.trace" | grep -c 'exited with')
if [ -n "$calls" ]; then
    echo "verify-offline: FAIL — network syscalls recorded:" >&2
    echo "$calls" >&2
    exit 1
fi
echo "verify-offline: OK — 0 network syscalls across $procs traced processes (ingest + activate)"
