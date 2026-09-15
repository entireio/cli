package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestResolveHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	abs := t.TempDir()
	tests := []struct {
		name    string
		env     string
		rel     string
		want    string
		wantErr string
	}{
		{name: "unset joins the default onto the home", env: "", rel: ".claude", want: filepath.Join(home, ".claude")},
		{name: "unset with no default is the home itself", env: "", rel: "", want: home},
		{name: "blank counts as unset", env: "  ", rel: ".claude", want: filepath.Join(home, ".claude")},
		{name: "absolute override wins", env: abs, rel: ".claude", want: abs},
		{name: "surrounding whitespace is kept, as the agents keep it", env: abs + " ", rel: ".claude", want: abs + " "},
		{name: "relative override is refused", env: filepath.Join("relative", "dir"), rel: ".claude", wantErr: "CLAUDE_CONFIG_DIR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", tt.env)
			got, err := ResolveHome("CLAUDE_CONFIG_DIR", tt.rel)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ResolveHome() = %q, %v; want an error naming %s", got, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("ResolveHome() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveHome_RefusesAnUnlistedVariable(t *testing.T) {
	t.Parallel()
	_, err := ResolveHome("NOT_A_RELOCATION_VARIABLE", ".x")
	if err == nil || !strings.Contains(err.Error(), "relocationEnvVars") {
		t.Fatalf("ResolveHome() error = %v; want a refusal pointing at the list", err)
	}
}

func TestRelocationEnvVars_ReturnsACopy(t *testing.T) {
	t.Parallel()
	first := RelocationEnvVars()
	if len(first) == 0 {
		t.Fatal("RelocationEnvVars() is empty")
	}
	first[0] = "MUTATED"
	if slices.Contains(RelocationEnvVars(), "MUTATED") {
		t.Error("RelocationEnvVars() shares its backing array with the package-level list")
	}
}
