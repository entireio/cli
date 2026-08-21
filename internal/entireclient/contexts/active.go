package contexts

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"
)

// EnvContextVar selects the acting login context for one process — the
// environment counterpart to `--context`.
//
// Both exist because they reach different entry points. A flag can't reach git
// operations at all: git invokes the `git-remote-entire` helper itself, so
// `ENTIRE_CONTEXT=staging git push` is the only way to scope a push or fetch to
// a login other than the active one. The env var also survives into hooks and
// subprocesses, which is what makes a whole shell session scopable without
// mutating shared state.
const EnvContextVar = "ENTIRE_CONTEXT"

// flagOverride records an explicit `--context` selection for this process. It is
// process-global for the same reason auth's insecure-HTTP opt-in is: the flag is
// parsed at the CLI edge but consumed deep inside resolution, and threading it
// through every resolver signature would touch every command that authenticates.
// One CLI invocation acts as one identity, so a process-wide value is the honest
// scope.
var flagOverride atomic.Pointer[string]

// SetFlagOverride records the `--context` value for this process. Call it once,
// during flag handling, before anything resolves a token. An empty or
// whitespace-only name clears the override rather than selecting a nameless
// context.
func SetFlagOverride(name string) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		flagOverride.Store(nil)
		return
	}
	flagOverride.Store(&trimmed)
}

// SetFlagOverrideForTest sets the override for one test and restores the previous
// value when it ends.
//
// Tests using it MUST NOT call t.Parallel(): the override is process-wide, so a
// parallel test would resolve against another test's selection. That is also why
// this restores the prior value rather than clearing — nesting stays honest.
func SetFlagOverrideForTest(t interface {
	Helper()
	Cleanup(restore func())
}, name string,
) {
	t.Helper()
	prev := flagOverride.Load()
	SetFlagOverride(name)
	t.Cleanup(func() { flagOverride.Store(prev) })
}

// requestedContext returns the explicitly requested context name and the source
// to name in errors, or ("", "") when nothing was requested. The flag wins over
// the environment so an explicit argument always beats inherited state.
func requestedContext() (name, source string) {
	if override := flagOverride.Load(); override != nil {
		return *override, "--context"
	}
	if env := strings.TrimSpace(os.Getenv(EnvContextVar)); env != "" {
		return env, "$" + EnvContextVar
	}
	return "", ""
}

// Selection is a resolved acting identity plus where the choice came from.
//
// Source is what lets callers name the right remedy, which differs by origin: a
// wrong `--context` is fixed by correcting the argument, a wrong
// current_context by `entire auth use`. Telling someone to run `auth use` when
// they passed an explicit flag sends them to change the wrong thing.
type Selection struct {
	// Context is the login to act as, or nil when there is none (logged out, or
	// current_context unset/dangling). Never nil when Source is non-empty: an
	// unmatched explicit request is an error instead.
	Context *Context
	// Source names the mechanism that chose it: "--context", "$ENTIRE_CONTEXT",
	// or "" for the stored current_context.
	Source string
}

// Explicit reports whether the identity was requested for this invocation
// rather than inherited from current_context.
func (s Selection) Explicit() bool { return s.Source != "" }

// Active resolves which stored login this process should act as: an explicit
// `--context`, else $ENTIRE_CONTEXT, else the stored current_context.
//
// An explicit request naming no stored context is an error, never a silent
// fallback to current_context. Acting as an identity other than the one asked
// for is precisely the failure the explicit selection exists to prevent, and it
// would be invisible — the command would succeed as the wrong account.
//
// A missing current_context is NOT an error: that is just "logged out", and the
// caller renders its own hint. So a nil Context with a nil error means no
// identity is available, and callers must handle it.
func (f *File) Active() (Selection, error) {
	name, source := requestedContext()
	if source == "" {
		return Selection{Context: f.Find(f.CurrentContext)}, nil
	}
	if c := f.Find(name); c != nil {
		return Selection{Context: c, Source: source}, nil
	}
	return Selection{}, &UnknownContextError{Name: name, Source: source, Available: f.Names()}
}

// UnknownContextError reports an explicit context selection that names no stored
// login. It carries the available names so the caller can print them without
// re-reading the store, and is a distinct type so callers can tell "you asked
// for something that doesn't exist" from "this login isn't trusted here" —
// different mistakes with different fixes.
type UnknownContextError struct {
	Name      string
	Source    string
	Available []string
}

func (e *UnknownContextError) Error() string {
	if len(e.Available) == 0 {
		return fmt.Sprintf("%s selected login context %q, but no logins are saved; run `entire login` first", e.Source, e.Name)
	}
	return fmt.Sprintf("%s selected login context %q, which is not saved.\nSaved contexts: %s\nRun `entire auth contexts` to list them.",
		e.Source, e.Name, strings.Join(e.Available, ", "))
}

// Names returns the stored context names in on-disk order, for listing in
// messages. On-disk order is stable across saves, so the output is too.
func (f *File) Names() []string {
	if f == nil {
		return nil
	}
	out := make([]string, 0, len(f.Contexts))
	for _, c := range f.Contexts {
		if c != nil && c.Name != "" {
			out = append(out, c.Name)
		}
	}
	return out
}
