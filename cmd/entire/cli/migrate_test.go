package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

const migrateDeprecationMessage = "Migration to checkpoints v2 has been halted for now."

// TestMigrateCmd_DeprecationStub verifies the migrate command is a stub that
// prints the deprecation message to stderr and returns a SilentError for any
// invocation (no args, legacy flags, or unknown flags).
func TestMigrateCmd_DeprecationStub(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
	}{
		{"no args", []string{"migrate"}},
		{"legacy --checkpoints v2", []string{"migrate", "--checkpoints", "v2"}},
		{"unknown flag", []string{"migrate", "--bogus"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := NewRootCmd()
			var stderr bytes.Buffer
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&stderr)
			root.SetArgs(tc.args)

			err := root.Execute()
			if err == nil {
				t.Fatal("expected non-nil error from deprecated migrate command")
			}
			var silent *SilentError
			if !errors.As(err, &silent) {
				t.Fatalf("error = %T %q, want *SilentError", err, err)
			}
			if !strings.Contains(stderr.String(), migrateDeprecationMessage) {
				t.Errorf("stderr = %q, want containing %q", stderr.String(), migrateDeprecationMessage)
			}
		})
	}
}
