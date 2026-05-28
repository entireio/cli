// Package oppassword reads credentials from 1Password via the `op` CLI.
//
// It exists so the entire-core and entiredb break-glass commands share a
// single implementation of the exec / stderr-capture / signin-hint
// pattern instead of duplicating it. Callers handle their own debug
// logging around the call.
package oppassword

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Read returns the trimmed credential at opRef (e.g.
// "op://partial.to/entire-break-glass-eu.auth.partial.to/credential").
//
// On failure, the returned error includes op's stderr (when non-empty)
// and a hint about running `op signin` for stale sessions, plus the
// op:// ref the caller tried.
func Read(ctx context.Context, opRef string) (string, error) {
	cmd := exec.CommandContext(ctx, "op", "read", opRef)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("1Password CLI failed reading %s: %s\n\nIf your 1Password session is stale, run `op signin`. Otherwise install the op CLI: https://developer.1password.com/docs/cli", opRef, detail)
	}
	tok := strings.TrimSpace(stdout.String())
	if tok == "" {
		return "", fmt.Errorf("1Password returned empty credential for %s", opRef)
	}
	return tok, nil
}
