package cli

import (
	"errors"
	"testing"

	"github.com/entireio/cli/internal/coreapi"
)

func TestContextCommandSurface(t *testing.T) {
	t.Parallel()
	cmd := newContextCmd()
	want := map[string]bool{"enable": false, "disable": false, "status": false, "query": false, "inspect": false}
	for _, child := range cmd.Commands() {
		if _, ok := want[child.Name()]; ok {
			want[child.Name()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("context command missing %q", name)
		}
	}
}

func TestOrgContextSharingCommandSurface(t *testing.T) {
	t.Parallel()
	cmd := newOrgContextSharingCmd()
	want := map[string]bool{"get": false, "set": false}
	for _, child := range cmd.Commands() {
		if _, ok := want[child.Name()]; ok {
			want[child.Name()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("org context-sharing command missing %q", name)
		}
	}
}

func TestContextSharingCommandErrorIdentifiesOldCore(t *testing.T) {
	t.Parallel()

	got := contextSharingCommandError(&coreapi.ErrorModelStatusCode{StatusCode: 404})
	if !errors.Is(got, errContextSharingUnsupported) {
		t.Fatalf("404 error = %v, want unsupported Core error", got)
	}
	domainNotFound := &coreapi.ErrorModelStatusCode{
		StatusCode: 404,
		Response: coreapi.ErrorModel{
			Detail: coreapi.NewOptString("org not found"),
		},
	}
	if got := contextSharingCommandError(domainNotFound); !errors.Is(got, domainNotFound) {
		t.Fatalf("domain 404 error = %v, want original", got)
	}
	original := errors.New("network unavailable")
	if got := contextSharingCommandError(original); !errors.Is(got, original) {
		t.Fatalf("non-404 error = %v, want original", got)
	}
}
