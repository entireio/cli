package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

func TestTokensCmd_CallerResolutionOutput(t *testing.T) {
	// These cases change CWD and environment, so they cannot run in parallel.
	for _, mode := range []string{"text", "json", "agent-brief"} {
		t.Run(mode, func(t *testing.T) {
			for _, scenario := range []string{"untracked", "untracked-ambiguous", "tracked-ambiguous", "caller", "explicit"} {
				t.Run(scenario, func(t *testing.T) {
					setupStopTestRepo(t)
					clearCallerSessionEnv(t)
					ctx := context.Background()
					const id = "tokens-resolution-session"
					untracked := strings.HasPrefix(scenario, "untracked")
					ambiguous := strings.Contains(scenario, "ambiguous")
					explicit := scenario == "explicit"
					if !untracked {
						state := makeSessionState(id, session.PhaseActive)
						if err := strategy.SaveSessionState(ctx, state); err != nil {
							t.Fatalf("SaveSessionState: %v", err)
						}
					}
					t.Setenv("PI_SESSION_ID", id)
					if ambiguous {
						t.Setenv("CODEX_SESSION_ID", "another-untracked-session")
					}

					var args []string
					if explicit {
						args = append(args, id)
					}
					if mode != "text" {
						args = append(args, "--"+mode)
					}
					cmd := newTokensCmd()
					var stdout, stderr bytes.Buffer
					cmd.SetOut(&stdout)
					cmd.SetErr(&stderr)
					cmd.SetArgs(args)
					err := cmd.ExecuteContext(ctx)
					if untracked {
						if mode != "text" {
							if err == nil || stdout.Len() != 0 {
								t.Fatalf("untracked machine output: err=%v stdout=%q", err, stdout.String())
							}
						} else if err != nil {
							t.Fatalf("untracked text diagnostic: %v", err)
						}
						output := stdout.String() + stderr.String()
						if !strings.Contains(output, "entire status") || strings.Contains(output, "Session not found") {
							t.Fatalf("missing untracked diagnostic: %s", output)
						}
						if ambiguous && (!strings.Contains(output, "could not be determined") || strings.Contains(output, "is running inside")) {
							t.Fatalf("untracked ambiguity lost: %s", output)
						}
						if !ambiguous && !strings.Contains(output, id) {
							t.Fatalf("diagnostic omitted caller ID: %s", output)
						}
						return
					}
					if err != nil {
						t.Fatalf("ExecuteContext: %v", err)
					}
					want := strategy.ResolutionCallerEnv
					if ambiguous {
						want = strategy.ResolutionCallerAmbiguous
						if !strings.Contains(stderr.String(), "Confirm the session ID") {
							t.Fatalf("missing ambiguity warning: %s", stderr.String())
						}
					} else if explicit {
						want = strategy.ResolutionNone
					}
					if mode == "json" {
						var report sessionTokensReport
						if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
							t.Fatalf("invalid JSON: %v", err)
						}
						if report.Resolution != want || report.SessionID != id {
							t.Fatalf("unexpected report: %+v", report)
						}
					} else if want != strategy.ResolutionNone {
						// Text mode renders the human label, not the enum —
						// the same label `session current` prints, so one
						// resolution reads identically wherever it surfaces.
						// The enum belongs to --json.
						wantLine := "Resolved: " + sessionResolutionLabel(want)
						if !strings.Contains(stdout.String(), wantLine) {
							t.Fatalf("missing %q in %s output: %s", wantLine, mode, stdout.String())
						}
						if strings.Contains(stdout.String(), "Resolved: "+string(want)) {
							t.Fatalf("text output leaked the raw enum: %s", stdout.String())
						}
					}
					if explicit && (strings.Contains(stdout.String(), `"resolution":`) || strings.Contains(stdout.String(), "Resolved:")) {
						t.Fatalf("explicit selection should omit resolution: %s", stdout.String())
					}
				})
			}
		})
	}
}
