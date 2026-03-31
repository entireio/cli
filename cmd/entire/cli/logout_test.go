package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

type mockTokenDeleter struct {
	deleted  map[string]bool
	failWith error
}

func newMockTokenDeleter() *mockTokenDeleter {
	return &mockTokenDeleter{deleted: make(map[string]bool)}
}

func (m *mockTokenDeleter) DeleteToken(baseURL string) error {
	if m.failWith != nil {
		return m.failWith
	}
	m.deleted[baseURL] = true
	return nil
}

func TestRunLogout_DeletesTokenAndPrintsMessage(t *testing.T) {
	t.Parallel()

	store := newMockTokenDeleter()
	var out bytes.Buffer

	err := runLogout(&out, store, "https://entire.io")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !store.deleted["https://entire.io"] {
		t.Fatal("expected token to be deleted for https://entire.io")
	}

	if !strings.Contains(out.String(), "Logged out.") {
		t.Fatalf("output = %q, want to contain %q", out.String(), "Logged out.")
	}
}

func TestRunLogout_ReturnsErrorOnDeleteFailure(t *testing.T) {
	t.Parallel()

	store := newMockTokenDeleter()
	store.failWith = errors.New("keyring locked")
	var out bytes.Buffer

	err := runLogout(&out, store, "https://entire.io")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !strings.Contains(err.Error(), "keyring locked") {
		t.Fatalf("error = %q, want to contain %q", err.Error(), "keyring locked")
	}

	if strings.Contains(out.String(), "Logged out.") {
		t.Fatal("should not print success message on error")
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
