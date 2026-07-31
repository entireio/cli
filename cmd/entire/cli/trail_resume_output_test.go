package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/strategy"

	"github.com/spf13/cobra"
)

func TestPrintTrailResumeIdentityLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		found api.TrailResource
		want  string
	}{
		{
			name:  "numbered trail with title",
			found: api.TrailResource{Number: 42, Title: "Improve trail resume", Branch: "trail-resume"},
			want:  "Resuming trail #42 \"Improve trail resume\" → branch trail-resume\n",
		},
		{
			name:  "id-only trail",
			found: api.TrailResource{ID: "tr_abc", Title: "Fix bug", Branch: "fix"},
			want:  "Resuming trail tr_abc \"Fix bug\" → branch fix\n",
		},
		{
			name:  "no title",
			found: api.TrailResource{Number: 7, Branch: "feat"},
			want:  "Resuming trail #7 → branch feat\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			printTrailResumeIdentity(&out, tt.found)
			if out.String() != tt.want {
				t.Errorf("printTrailResumeIdentity() = %q, want %q", out.String(), tt.want)
			}
		})
	}
}

// TestDisplayTrailResumeContinuation_SingleSession pins the non-interactive
// act-path contract: the final stdout line is the default session's resume
// command, bare, so callers can lift it without parsing prose.
func TestDisplayTrailResumeContinuation_SingleSession(t *testing.T) {
	t.Parallel()

	session := strategy.RestoredSession{
		SessionID:    "sess-solo",
		Agent:        "Claude Code",
		CheckpointID: "cafe12cafe34",
		Prompt:       "Implement auth",
		CreatedAt:    time.Date(2026, 2, 2, 12, 0, 0, 0, time.UTC),
	}

	var out bytes.Buffer
	if err := displayTrailResumeContinuation(&out, []strategy.RestoredSession{session}); err != nil {
		t.Fatalf("displayTrailResumeContinuation() error = %v", err)
	}

	if !strings.Contains(out.String(), "✓ Restored checkpoint cafe12cafe34 (1 session).") {
		t.Errorf("output = %q, want restore summary", out.String())
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if got := lines[len(lines)-1]; got != "claude -r sess-solo" {
		t.Errorf("final line = %q, want bare resume command", got)
	}
	if strings.Contains(out.String(), "To continue") {
		t.Errorf("output = %q, want no prose menu", out.String())
	}
}

// TestDisplayTrailResumeContinuation_MultiSessionListsOthers pins the pointer
// line: other restored session ids are listed (there is no table to discover
// them from anymore), and the default (work over review) session's command is
// still the final line.
func TestDisplayTrailResumeContinuation_MultiSessionListsOthers(t *testing.T) {
	t.Parallel()

	work := strategy.RestoredSession{
		SessionID:    "sess-work",
		Agent:        "Claude Code",
		CheckpointID: "cafe12cafe34",
		Prompt:       "Implement auth",
		CreatedAt:    time.Date(2026, 2, 2, 12, 0, 0, 0, time.UTC),
	}
	review := strategy.RestoredSession{
		SessionID:    "sess-review",
		Agent:        "Claude Code",
		CheckpointID: "beef56beef78",
		Prompt:       "Review the code changes on this branch",
		CreatedAt:    time.Date(2026, 2, 2, 13, 0, 0, 0, time.UTC),
		Kind:         "agent_review",
	}

	var out bytes.Buffer
	if err := displayTrailResumeContinuation(&out, []strategy.RestoredSession{review, work}); err != nil {
		t.Fatalf("displayTrailResumeContinuation() error = %v", err)
	}

	if !strings.Contains(out.String(), "1 other session restored (sess-review); use --session <id> to choose") {
		t.Errorf("output = %q, want pointer line listing the other session id", out.String())
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if got := lines[len(lines)-1]; got != "claude -r sess-work" {
		t.Errorf("final line = %q, want the default work session's command", got)
	}
}

// TestContinueTrailRestoredSessionsNonInteractiveEndsWithContinuation pins the
// wiring: the non-interactive act path uses the slim continuation display.
func TestContinueTrailRestoredSessionsNonInteractiveEndsWithContinuation(t *testing.T) {
	t.Parallel()

	session := strategy.RestoredSession{
		SessionID:    "sess-wire",
		Agent:        "Claude Code",
		CheckpointID: "beef56beef78",
		CreatedAt:    time.Date(2026, 2, 2, 12, 0, 0, 0, time.UTC),
	}

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := continueTrailRestoredSessions(context.Background(), cmd, []strategy.RestoredSession{session}, "", false); err != nil {
		t.Fatalf("continueTrailRestoredSessions() error = %v", err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if got := lines[len(lines)-1]; got != "claude -r sess-wire" {
		t.Errorf("final line = %q, want bare resume command", got)
	}
}
