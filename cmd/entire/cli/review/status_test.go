package review_test

// status_test.go: this file is intentionally a comment-only stub.
//
// headHasReviewCheckpoint lives in the cli package (cmd/entire/cli/
// review_helpers.go) — not here — because it imports checkpoint, which
// transitively imports per-agent reviewer packages, which import review.
// Moving it into review/ would close that cycle. Tests for it live with
// its definition in the cli package's test suite.
//
// This stub's presence signals to reviewers that the status surface was
// considered for this package, not overlooked.
