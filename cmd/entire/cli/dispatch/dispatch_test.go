package dispatch

import (
	"context"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/auth"
)

func TestRun_ServerAllowsRepos(t *testing.T) {
	oldResource := lookupResourceToken
	lookupResourceToken = func(_ context.Context, _ string) (string, error) {
		return "", auth.ErrNotLoggedIn
	}
	t.Cleanup(func() {
		lookupResourceToken = oldResource
	})

	_, err := Run(context.Background(), Options{
		Mode:      ModeServer,
		RepoPaths: []string{"entireio/cli"},
	})
	if err == nil {
		t.Fatal("expected login error")
	}
	if strings.Contains(err.Error(), "--repos") {
		t.Fatalf("did not expect repos validation error: %v", err)
	}
	if !strings.Contains(err.Error(), "dispatch requires login") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMode_String(t *testing.T) {
	t.Parallel()

	if got := ModeServer.String(); got != "server" {
		t.Fatalf("expected server string, got %q", got)
	}
	if got := ModeLocal.String(); got != "local" {
		t.Fatalf("expected local string, got %q", got)
	}
}
