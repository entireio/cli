# OPF real-runtime throughput findings

Follows the branch that shipped in PR #2533 (dedup fix, per-ref cap scoping,
detached worker, status visibility). That PR fixed the *measurement* and
*blast-radius* bugs. This file records what real benchmarking of the actual
`opf` binary found afterward, which is why a fourth piece of work
(`docs/superpowers/plans/2026-09-18-opf-per-ref-delivery.md`) exists.

## The finding

`batchDefaultLimit` (2 MiB) was sized from a comment's estimate of
"~5.4s/100KB." Benchmarking the real `opf` binary — the exact invocation
`redact/opf.go`'s `shellOut.RedactBatch` uses
(`opf --device cpu --output-mode typed --format json --no-print-color-coded-text`,
fed via stdin) — against generated prose text of several sizes gave:

| Size | Wall time | Rate |
| --- | --- | --- |
| 1 KB | 1.2s | — |
| 10 KB | 9.0s | 0.90 s/KB |
| 50 KB | 42.7s | 0.85 s/KB |
| 100 KB | 86.0s | 0.86 s/KB |
| 250 KB | 284.6s | 1.14 s/KB |
| 500 KB | 568.3s | 1.14 s/KB |

Real rate is **~16x slower** than the code's own estimate, and it does not
stay at the small-input rate — it degrades to a sustained **~1.14 s/KB**
beyond roughly 250KB and holds there through 500KB. Treat 1.14 s/KB as the
number to design around, not the optimistic 0.85s/KB from small inputs.

At this rate the *current* 2 MiB cap would cost roughly **29 minutes** of
inference if it were ever allowed to run to completion, and a real multi-MB
session transcript costs on the order of an hour. No cap value makes typical
real-session content redact quickly — the original assumption behind sizing
the cap for push-latency was never true at this throughput.

## MPS acceleration: checked, not viable

This machine has Apple Silicon with MPS available and built
(`torch.backends.mps.is_available()` → `True`). Two blockers:

1. The model's MoE layers use fused kernels gated on the `triton` package
   (`OPF_MOE_TRITON`), which is not installed and is not a supported
   Apple-Silicon package (Triton targets NVIDIA/AMD).
2. With `OPF_MOE_TRITON=0` to fall back to the non-fused path, MPS is
   **slower than CPU**, not faster — confirmed at two sizes:
   - 1 KB: MPS 1.3s vs CPU 1.2s
   - 100 KB: MPS 133.3s vs CPU 86.0s (55% slower)

Conclusion: CPU is the fastest option available on this stack today. There
is no hardware-acceleration escape hatch to lean on when picking new
numbers — the fix has to be architectural (don't let a slow-but-working ref
block delivery of its siblings), not a faster runtime.

## What this changes about the plan

- `batchDefaultLimit` should stop trying to answer "will this be fast" (it
  can't) and instead answer "is this plausibly a real session or a
  runaway/corrupted one" — a much larger sanity ceiling, not a tuned number.
- The OPF shell-out's fixed 30s `timeout_seconds` default is already too
  short for real content at this rate (100KB alone takes ~86s) — it needs to
  scale with input size or it will misclassify legitimately-slow-but-working
  redaction as a broken runtime, which trips the process-wide circuit
  breaker and is worse than a plain cap rejection.
- The actual fix for "checkpoints don't ship" is not the cap or the timeout
  — it's that today the whole flush is withheld if *any* queued ref isn't
  redacted yet, and given real timing, "still redacting" will be the normal
  state on any active repo. Delivery needs to ship per-ref: already-redacted
  refs go out, still-cooking or perma-failing ones stay queued, without
  holding the rest hostage.

See `docs/superpowers/plans/2026-09-18-opf-per-ref-delivery.md` for the
concrete implementation plan.
