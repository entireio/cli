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

type mockTokenStore struct {
	tokens     map[string]string
	deleted    map[string]bool
	getErr     error
	deleteErr  error
	getCalls   int
	deleteCall int
}

func newMockTokenStore() *mockTokenStore {
	return &mockTokenStore{
		tokens:  make(map[string]string),
		deleted: make(map[string]bool),
	}
}

func (m *mockTokenStore) GetToken(baseURL string) (string, error) {
	m.getCalls++
	if m.getErr != nil {
		return "", m.getErr
	}
	return m.tokens[baseURL], nil
}

func (m *mockTokenStore) DeleteToken(baseURL string) error {
	m.deleteCall++
	if m.deleteErr != nil {
		return m.deleteErr
	}
	m.deleted[baseURL] = true
	return nil
}

func TestRunLogout_RevokesServerSideThenDeletesLocally(t *testing.T) {
	t.Parallel()

	store := newMockTokenStore()
	store.tokens["https://entire.io"] = testLogoutToken

	var gotToken string
	revoke := func(_ context.Context, token string) error {
		gotToken = token
		return nil
	}

	var out, errOut bytes.Buffer
	err := runLogout(context.Background(), &out, &errOut, store, revoke, "https://entire.io")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotToken != testLogoutToken {
		t.Errorf("revoke called with token %q, want %q", gotToken, testLogoutToken)
	}
	if !store.deleted["https://entire.io"] {
		t.Fatal("expected token to be deleted for https://entire.io")
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

	store := newMockTokenStore() // no token stored

	revokeCalled := false
	revoke := func(context.Context, string) error {
		revokeCalled = true
		return nil
	}

	var out, errOut bytes.Buffer
	err := runLogout(context.Background(), &out, &errOut, store, revoke, "https://entire.io")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if revokeCalled {
		t.Fatal("revoke should not be called when no local token exists")
	}
	if !store.deleted["https://entire.io"] {
		t.Fatal("expected DeleteToken to be called even when no token was stored")
	}
	if !strings.Contains(out.String(), "Logged out.") {
		t.Fatalf("stdout = %q, want to contain %q", out.String(), "Logged out.")
	}
}

func TestRunLogout_RevokeFailureWarnsButSucceeds(t *testing.T) {
	t.Parallel()

	store := newMockTokenStore()
	store.tokens["https://entire.io"] = testLogoutToken

	revoke := func(context.Context, string) error {
		return errors.New("connection refused")
	}

	var out, errOut bytes.Buffer
	err := runLogout(context.Background(), &out, &errOut, store, revoke, "https://entire.io")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !store.deleted["https://entire.io"] {
		t.Fatal("local token should still be deleted when server revoke fails")
	}
	if !strings.Contains(errOut.String(), "server-side token revocation failed") {
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

	store := newMockTokenStore()
	store.tokens["https://entire.io"] = testLogoutToken

	revoke := func(context.Context, string) error {
		return &api.HTTPError{StatusCode: http.StatusUnauthorized, Message: "Not authenticated"}
	}

	var out, errOut bytes.Buffer
	err := runLogout(context.Background(), &out, &errOut, store, revoke, "https://entire.io")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !store.deleted["https://entire.io"] {
		t.Fatal("local token should still be deleted after silent 401")
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q, want empty for already-invalid token", errOut.String())
	}
	if !strings.Contains(out.String(), "Logged out.") {
		t.Fatalf("stdout = %q, want to contain %q", out.String(), "Logged out.")
	}
}

func TestRunLogout_GetTokenErrorWarnsAndFallsThrough(t *testing.T) {
	t.Parallel()

	store := newMockTokenStore()
	store.getErr = errors.New("keyring locked for read")

	revokeCalled := false
	revoke := func(context.Context, string) error {
		revokeCalled = true
		return nil
	}

	var out, errOut bytes.Buffer
	err := runLogout(context.Background(), &out, &errOut, store, revoke, "https://entire.io")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if revokeCalled {
		t.Fatal("revoke should not be called when token read fails")
	}
	if !store.deleted["https://entire.io"] {
		t.Fatal("DeleteToken should still be attempted after GetToken failure")
	}
	if !strings.Contains(errOut.String(), "failed to read token before revocation") {
		t.Fatalf("stderr = %q, want warning about read failure", errOut.String())
	}
}

func TestRunLogout_ReturnsErrorOnDeleteFailure(t *testing.T) {
	t.Parallel()

	store := newMockTokenStore()
	store.tokens["https://entire.io"] = testLogoutToken
	store.deleteErr = errors.New("keyring locked")

	revoke := func(context.Context, string) error { return nil }

	var out, errOut bytes.Buffer
	err := runLogout(context.Background(), &out, &errOut, store, revoke, "https://entire.io")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "keyring locked") {
		t.Fatalf("error = %q, want to contain %q", err.Error(), "keyring locked")
	}
	if strings.Contains(out.String(), "Logged out.") {
		t.Fatal("should not print success message when local delete fails")
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
