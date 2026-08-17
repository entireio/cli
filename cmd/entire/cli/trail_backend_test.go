package cli

import "testing"

func TestConfiguredTrailBackend(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  trailBackend
	}{
		{name: "default", want: trailBackendLegacy},
		{name: "legacy", value: "legacy", want: trailBackendLegacy},
		{name: "bff alias", value: "bff", want: trailBackendLegacy},
		{name: "entire-api", value: "entire-api", want: trailBackendEntireAPI},
		{name: "native alias", value: "native", want: trailBackendEntireAPI},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(trailBackendEnvVar, tt.value)
			got, err := configuredTrailBackend()
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("backend = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConfiguredTrailBackendRejectsUnknownValue(t *testing.T) {
	t.Setenv(trailBackendEnvVar, "something-else")
	if _, err := configuredTrailBackend(); err == nil {
		t.Fatal("expected invalid backend error")
	}
}

func TestTrailBackendScopedAPIBaseIncludesBackend(t *testing.T) {
	t.Setenv(trailBackendEnvVar, "legacy")
	legacy, err := trailBackendScopedAPIBase()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(trailBackendEnvVar, "entire-api")
	native, err := trailBackendScopedAPIBase()
	if err != nil {
		t.Fatal(err)
	}
	if legacy == native {
		t.Fatalf("backend cache scopes must differ: %q", legacy)
	}
}
