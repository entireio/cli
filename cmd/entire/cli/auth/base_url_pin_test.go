package auth

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

// Not parallel: sets ENTIRE_API_BASE_URL.
func TestBaseURLOverridePinsCell(t *testing.T) {
	tests := []struct {
		name     string
		override string
		want     bool
	}{
		{"unset", "", false},
		{"loopback dev cell", "http://127.0.0.1:8082", true},
		{"localhost dev cell", "http://localhost:8082/", true},
		{"direct cell", "https://aws-eu-central-1.api.entire.io", true},
		{"staging apex", "https://partial.to", false},
		{"prod apex", "https://entire.io", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(api.BaseURLEnvVar, tt.override)
			if got := BaseURLOverridePinsCell(); got != tt.want {
				t.Errorf("BaseURLOverridePinsCell() with %q = %v, want %v", tt.override, got, tt.want)
			}
		})
	}
}
