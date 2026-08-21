package contexts_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/entireio/cli/internal/entireclient/contexts"
)

// The two saved logins these tests resolve between.
const (
	ctxProd    = "prod"
	ctxStaging = "staging"
)

// storeFile is a two-login store: ctxProd is the stored current_context.
func storeFile() *contexts.File {
	return &contexts.File{
		CurrentContext: ctxProd,
		Contexts: []*contexts.Context{
			{Name: ctxProd, CoreURL: "https://core.entire.io", Handle: "me", KeychainService: "kc:prod"},
			{Name: ctxStaging, CoreURL: "https://core.partial.to", Handle: "me", KeychainService: "kc:staging"},
		},
	}
}

// Mutates the process-wide flag override, so these cannot run in parallel with
// each other; t.Setenv already forbids it too.
func TestActive_PrecedenceFlagOverEnvOverCurrent(t *testing.T) {
	f := storeFile()

	t.Run("current_context when nothing is requested", func(t *testing.T) {
		contexts.SetFlagOverrideForTest(t, "")
		t.Setenv(contexts.EnvContextVar, "")
		sel, err := f.Active()
		if err != nil {
			t.Fatalf("Active: %v", err)
		}
		if sel.Context.Name != ctxProd || sel.Explicit() {
			t.Fatalf("got %q explicit=%v, want prod explicit=false", sel.Context.Name, sel.Explicit())
		}
	})

	t.Run("env overrides current_context", func(t *testing.T) {
		contexts.SetFlagOverrideForTest(t, "")
		t.Setenv(contexts.EnvContextVar, ctxStaging)
		sel, err := f.Active()
		if err != nil {
			t.Fatalf("Active: %v", err)
		}
		if sel.Context.Name != ctxStaging || sel.Source != "$"+contexts.EnvContextVar {
			t.Fatalf("got %q from %q, want staging from the env var", sel.Context.Name, sel.Source)
		}
	})

	t.Run("flag beats env", func(t *testing.T) {
		t.Setenv(contexts.EnvContextVar, ctxStaging)
		contexts.SetFlagOverrideForTest(t, ctxProd)
		sel, err := f.Active()
		if err != nil {
			t.Fatalf("Active: %v", err)
		}
		if sel.Context.Name != ctxProd || sel.Source != "--context" {
			t.Fatalf("got %q from %q, want prod from --context", sel.Context.Name, sel.Source)
		}
	})

	t.Run("blank env is not a selection", func(t *testing.T) {
		contexts.SetFlagOverrideForTest(t, "")
		t.Setenv(contexts.EnvContextVar, "   ")
		sel, err := f.Active()
		if err != nil {
			t.Fatalf("Active: %v", err)
		}
		if sel.Context.Name != ctxProd || sel.Explicit() {
			t.Fatalf("blank env should fall through to current_context, got %q", sel.Context.Name)
		}
	})
}

// An explicit request naming nothing must fail loudly. Falling back to
// current_context would run the command as a different identity than the one
// asked for, and succeed while doing it.
func TestActive_UnknownExplicitSelectionErrors(t *testing.T) {
	t.Setenv(contexts.EnvContextVar, "")
	contexts.SetFlagOverrideForTest(t, "typo")

	sel, err := storeFile().Active()
	if err == nil {
		t.Fatalf("want an error for an unknown --context, got selection %+v", sel)
	}
	var unknown *contexts.UnknownContextError
	if !errors.As(err, &unknown) {
		t.Fatalf("want UnknownContextError so callers can tell it from a trust failure, got %T", err)
	}
	if unknown.Name != "typo" || unknown.Source != "--context" {
		t.Fatalf("got name=%q source=%q", unknown.Name, unknown.Source)
	}
	for _, want := range []string{"typo", ctxProd, ctxStaging} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q should name %q", err.Error(), want)
		}
	}
}

func TestActive_NoCurrentContextIsNotAnError(t *testing.T) {
	contexts.SetFlagOverrideForTest(t, "")
	t.Setenv(contexts.EnvContextVar, "")

	f := storeFile()
	f.CurrentContext = ""
	sel, err := f.Active()
	if err != nil {
		t.Fatalf("logged out is not an error condition: %v", err)
	}
	if sel.Context != nil {
		t.Fatalf("want no context, got %q", sel.Context.Name)
	}
}
