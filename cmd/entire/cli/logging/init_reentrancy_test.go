package logging

import (
	"context"
	"testing"
	"time"
)

// Init must not hold mu while calling logLevelGetter. The getter is supplied
// by the cli package and loads settings, and settings loading logs — so a
// getter that logs would deadlock on a non-reentrant RWMutex.
//
// This is a regression test for a real hang: every git hook froze in any repo
// whose committed .entire/settings.json set
// redaction.openai_privacy_filter.command, because the trust gate warned from
// inside settings.Load, which Init calls through this getter.
//
// Not parallel: mutates package-global logger state.
func TestInit_GetterThatLogsDoesNotDeadlock(t *testing.T) {
	t.Setenv(LogLevelEnvVar, "") // force the getter path
	t.Setenv("ENTIRE_LOG_DIR", t.TempDir())

	prev := logLevelGetter
	t.Cleanup(func() { SetLogLevelGetter(prev) })

	SetLogLevelGetter(func() string {
		// Exactly what settings.Load does on some paths.
		Debug(context.Background(), "log emitted from inside the log-level getter")
		return "debug" // case-insensitive; lowercase avoids a goconst hit
	})

	done := make(chan error, 1)
	go func() { done <- Init(context.Background(), "") }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Init returned an error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Init deadlocked: the log-level getter must not run while mu is held")
	}
}
