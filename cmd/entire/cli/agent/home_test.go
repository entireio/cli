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

func TestLookupOverride(t *testing.T) {
	abs := t.TempDir()
	tests := []struct {
		name    string
		env     string
		want    string
		wantOK  bool
		wantErr bool
	}{
		{name: "unset is not ok", env: ""},
		{name: "blank is not ok", env: "  "},
		{name: "absolute is returned as set", env: abs + " ", want: abs + " ", wantOK: true},
		{name: "relative is refused", env: filepath.Join("relative", "dir"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PI_CODING_AGENT_SESSION_DIR", tt.env)
			got, ok, err := LookupOverride("PI_CODING_AGENT_SESSION_DIR")
			if (err != nil) != tt.wantErr {
				t.Fatalf("LookupOverride() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("LookupOverride() = %q, %v; want %q, %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestLookupOverride_RefusesAnUnlistedVariable(t *testing.T) {
	t.Parallel()
	if _, _, err := LookupOverride("NOT_A_RELOCATION_VARIABLE"); err == nil || !strings.Contains(err.Error(), "relocationEnvVars") {
		t.Fatalf("LookupOverride() error = %v; want a refusal pointing at the list", err)
	}
}

func TestRefusedRelocationEnvVars(t *testing.T) {
	for _, envVar := range relocationEnvVars {
		t.Setenv(envVar, "")
	}
	if refused := RefusedRelocationEnvVars(); len(refused) != 0 {
		t.Fatalf("RefusedRelocationEnvVars() = %v with every variable unset, want none", refused)
	}

	t.Setenv("CODEX_HOME", filepath.Join("relative", "codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	refused := RefusedRelocationEnvVars()
	if len(refused) != 1 || !strings.Contains(refused[0].Error(), "CODEX_HOME") {
		t.Fatalf("RefusedRelocationEnvVars() = %v, want exactly the relative CODEX_HOME", refused)
	}
}

func TestLookupOverride_ExpandsTildeWhereTheAgentDoes(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, envVar, value, want string
		wantErr                   bool
	}{
		{name: "pi expands ~/", envVar: "PI_CODING_AGENT_DIR", value: "~/pi-home", want: filepath.Join(home, "pi-home")},
		{name: "pi expands a bare ~", envVar: "PI_CODING_AGENT_SESSION_DIR", value: "~", want: home},
		{name: "pi leaves ~user alone, so it stays relative", envVar: "PI_CODING_AGENT_DIR", value: "~someone/pi", wantErr: true},
		{name: "claude does not expand ~", envVar: "CLAUDE_CONFIG_DIR", value: "~/claude", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.envVar, tt.value)
			got, ok, err := LookupOverride(tt.envVar)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("LookupOverride() = %q, want an error", got)
				}
				return
			}
			if err != nil || !ok || got != tt.want {
				t.Errorf("LookupOverride() = %q, %v, %v; want %q", got, ok, err, tt.want)
			}
		})
	}
}

func TestTildeExpandingEnvVars_AreRelocationEnvVars(t *testing.T) {
	t.Parallel()
	for _, envVar := range tildeExpandingEnvVars {
		if !slices.Contains(relocationEnvVars, envVar) {
			t.Errorf("%s expands ~ but is not in relocationEnvVars", envVar)
		}
	}
}
