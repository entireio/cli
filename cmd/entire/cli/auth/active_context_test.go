package auth

import (
	"errors"
	"testing"

	"github.com/entireio/cli/internal/entireclient/contexts"
)

// These tests drive process-global state (ENTIRE_CONFIG_DIR) so they cannot run
// in parallel.

func TestActiveContext_ReturnsTheActingContext(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", configDir)
	writeActiveContext(t, configDir, "alice@core", "https://ctx-core.example", "alice", "svc")

	c, ok, err := ActiveContext()
	if err != nil || !ok {
		t.Fatalf("ActiveContext() ok = %v, error = %v", ok, err)
	}
	if c.Name != "alice@core" || c.CoreURL != "https://ctx-core.example" {
		t.Fatalf("context = %+v, want the acting one", c)
	}
}

// The context object itself is returned, not just its name, so callers needing
// its CoreURL do not re-find it by looping over the full list. Contexts() must
// still agree about which one is acting.
func TestActiveContext_AgreesWithContexts(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", configDir)
	other := &contexts.Context{Name: "staging", CoreURL: "https://staging.example"}
	acting := &contexts.Context{Name: "prod", CoreURL: "https://prod.example"}
	if err := contexts.Save(configDir, &contexts.File{
		CurrentContext: "prod",
		Contexts:       []*contexts.Context{other, acting},
	}); err != nil {
		t.Fatalf("write contexts.json: %v", err)
	}

	c, ok, err := ActiveContext()
	if err != nil || !ok {
		t.Fatalf("ActiveContext() ok = %v, error = %v", ok, err)
	}
	_, current, err := Contexts()
	if err != nil {
		t.Fatalf("Contexts: %v", err)
	}
	if c.Name != current {
		t.Fatalf("ActiveContext name = %q, Contexts current = %q; they must not disagree", c.Name, current)
	}
}

// A context with no CoreURL is an unusable pointer: reporting it as acting means
// dialing an empty host instead of telling the user to log in.
func TestActiveContext_BlankCoreURLIsNotActing(t *testing.T) {
	for _, coreURL := range []string{"", "   "} {
		configDir := t.TempDir()
		t.Setenv("ENTIRE_CONFIG_DIR", configDir)
		writeActiveContext(t, configDir, "broken", coreURL, "alice", "svc")

		c, ok, err := ActiveContext()
		if err != nil {
			t.Fatalf("CoreURL %q: unexpected error %v", coreURL, err)
		}
		if ok || c != nil {
			t.Fatalf("CoreURL %q: ok = %v, context = %+v, want no acting context", coreURL, ok, c)
		}
	}
}

func TestActiveContext_NoCurrentContext(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", configDir)

	c, ok, err := ActiveContext()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok || c != nil {
		t.Fatalf("ok = %v, context = %+v, want no acting context", ok, c)
	}
}

// An explicit --context/$ENTIRE_CONTEXT naming no saved context is a hard error,
// not "nobody is acting": it must not degrade into the `entire login` hint.
func TestActiveContext_UnknownSelectionIsAnError(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", configDir)
	t.Setenv(contexts.EnvContextVar, "nope")
	writeActiveContext(t, configDir, "alice@core", "https://ctx-core.example", "alice", "svc")

	c, ok, err := ActiveContext()
	if err == nil {
		t.Fatalf("error = nil, want an unknown-context error; got ok = %v, context = %+v", ok, c)
	}
	var unknown *contexts.UnknownContextError
	if !errors.As(err, &unknown) {
		t.Fatalf("error = %v, want *contexts.UnknownContextError", err)
	}
}
