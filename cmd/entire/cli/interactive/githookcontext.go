package interactive

import "sync/atomic"

// gitHookContext records that this process was invoked as a git hook. It is a
// one-way latch set once during command setup, so reads are cheap and cannot
// race with a later change.
var gitHookContext atomic.Bool

// MarkGitHookContext declares that this process is running as a git hook, which
// changes what a controlling terminal implies: git was run by something else —
// possibly a TUI client that gave us a private terminal it only reads from — so
// a terminal we can open is not necessarily one a user can answer on. See
// jobcontrol_unix.go for the check this enables and why it is scoped here rather
// than applied to every `entire` invocation.
//
// Called from the `entire hooks git` command tree, which every git hook funnels
// through. Direct CLI invocations never set it and keep the plain terminal
// detection.
func MarkGitHookContext() { gitHookContext.Store(true) }

// inGitHookContext reports whether MarkGitHookContext was called.
func inGitHookContext() bool { return gitHookContext.Load() }
