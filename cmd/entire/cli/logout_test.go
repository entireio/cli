package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

const testLogoutToken = "tok123"

func TestRunLogout_RevokesServerSideThenRemovesLogin(t *testing.T) {
	t.Parallel()

	revokeCalled, cleared := false, false
	revoke := func(context.Context) error {
		revokeCalled = true
		return nil
	}

	var out, errOut bytes.Buffer
	err := runLogout(context.Background(), &out, &errOut, testLogoutToken, revoke, func() error { cleared = true; return nil })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !revokeCalled {
		t.Error("revoke should be called when a token exists")
	}
	if !cleared {
		t.Fatal("expected the active context to be removed")
	}
	if !strings.Contains(out.String(), "Logged out.") {
		t.Fatalf("stdout = %q, want to contain %q", out.String(), "Logged out.")
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", errOut.String())
	}
}

func TestRunLogout_NoTokenSkipsRevoke(t *testing.T) {
	t.Parallel()

	revokeCalled, cleared := false, false
	revoke := func(context.Context) error {
		revokeCalled = true
		return nil
	}

	var out, errOut bytes.Buffer
	err := runLogout(context.Background(), &out, &errOut, "", revoke, func() error { cleared = true; return nil })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if revokeCalled {
		t.Fatal("revoke should not be called without a token")
	}
	if !cleared {
		t.Fatal("the login should still be removed locally")
	}
	if !strings.Contains(out.String(), "Logged out.") {
		t.Fatalf("stdout = %q, want to contain %q", out.String(), "Logged out.")
	}
}

func TestRunLogout_RevokeFailureWarnsButSucceeds(t *testing.T) {
	t.Parallel()

	revoke := func(context.Context) error {
		return errors.New("connection refused")
	}

	cleared := false
	var out, errOut bytes.Buffer
	err := runLogout(context.Background(), &out, &errOut, testLogoutToken, revoke, func() error { cleared = true; return nil })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !cleared {
		t.Fatal("the login should still be removed when server revoke fails")
	}
	if !strings.Contains(errOut.String(), "server-side session revocation failed") {
		t.Fatalf("stderr = %q, want warning about revoke failure", errOut.String())
	}
	if !strings.Contains(errOut.String(), "connection refused") {
		t.Fatalf("stderr = %q, want underlying error message", errOut.String())
	}
	if !strings.Contains(out.String(), "Logged out.") {
		t.Fatalf("stdout = %q, want to contain %q", out.String(), "Logged out.")
	}
}

func TestRunLogout_RevokeUnauthorizedIsSilent(t *testing.T) {
	t.Parallel()

	revoke := func(context.Context) error {
		return &api.HTTPError{StatusCode: http.StatusUnauthorized, Message: "Not authenticated"}
	}

	cleared := false
	var out, errOut bytes.Buffer
	err := runLogout(context.Background(), &out, &errOut, testLogoutToken, revoke, func() error { cleared = true; return nil })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !cleared {
		t.Fatal("the login should still be removed after silent 401")
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q, want empty for already-invalid token", errOut.String())
	}
	if !strings.Contains(out.String(), "Logged out.") {
		t.Fatalf("stdout = %q, want to contain %q", out.String(), "Logged out.")
	}
}

func TestRunLogout_ReturnsErrorOnClearFailure(t *testing.T) {
	t.Parallel()

	revoke := func(context.Context) error { return nil }

	var out, errOut bytes.Buffer
	err := runLogout(context.Background(), &out, &errOut, testLogoutToken, revoke, func() error { return errors.New("keyring locked") })
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "keyring locked") {
		t.Fatalf("error = %q, want to contain %q", err.Error(), "keyring locked")
	}
	if strings.Contains(out.String(), "Logged out.") {
		t.Fatal("should not print success message when local removal fails")
	}
}

func TestLogoutCmd_IsRegistered(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	found := false
	for _, c := range root.Commands() {
		if c.Use == "logout" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("logout command not registered on root")
	}
}

func TestLogoutCmd_HasSimpleScope(t *testing.T) {
	t.Parallel()
	cmd := newLogoutCmd()
	if cmd.Flags().Lookup("everywhere") == nil {
		t.Fatal("logout should keep --everywhere")
	}
	if cmd.Flags().Lookup("all-contexts") != nil {
		t.Fatal("logout should not expose --all-contexts")
	}
}
