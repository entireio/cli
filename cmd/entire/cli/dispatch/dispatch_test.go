package dispatch

import (
	"context"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
)

func TestRun_ServerAllowsRepos(t *testing.T) {
	old := newDispatchCellClient
	newDispatchCellClient = func(context.Context, bool, string) (*api.Client, string, error) {
		return nil, "", auth.ErrNotLoggedIn
	}
	t.Cleanup(func() { newDispatchCellClient = old })

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
