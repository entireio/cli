package cli

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/review"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// TestReviewers_PromptNeverInArgv guards every registered review runner: the
// composed prompt goes on stdin, never in argv, which other local users can
// read through the process table (ps, /proc/<pid>/cmdline). The only other
// copy allowed is ENTIRE_REVIEW_PROMPT, since a process's environment is
// readable only by its own user.
func TestReviewers_PromptNeverInArgv(t *testing.T) {
	t.Parallel()

	const canary = "entire-argv-guard-canary-7f3a"
	cases := map[string]reviewtypes.RunConfig{
		"composed": {Skills: []string{"/review"}, Task: "Review the change.", AlwaysPrompt: canary, PerRunPrompt: canary},
		"override": {PromptOverride: canary},
	}

	checked := 0
	for _, name := range agent.List() {
		reviewer := launchableReviewerFor(string(name))
		if reviewer == nil {
			continue
		}
		tmpl, ok := reviewer.(*reviewtypes.ReviewerTemplate)
		if !ok {
			t.Fatalf("%s: reviewer is %T, not a ReviewerTemplate; extend this guard to inspect its command", name, reviewer)
		}
		checked++
		for label, cfg := range cases {
			cmd := tmpl.BuildCmd(context.Background(), cfg)
			for i, arg := range cmd.Args {
				if strings.Contains(arg, canary) {
					t.Errorf("%s/%s: prompt in argv[%d] %q; pipe it via stdin", name, label, i, arg)
				}
			}
			for _, kv := range cmd.Env {
				if strings.Contains(kv, canary) && !strings.HasPrefix(kv, review.EnvPrompt+"=") {
					t.Errorf("%s/%s: prompt in env %q", name, label, kv)
				}
			}
			if cmd.Stdin == nil {
				t.Errorf("%s/%s: stdin is nil, want the prompt", name, label)
				continue
			}
			stdin, err := io.ReadAll(cmd.Stdin)
			if err != nil {
				t.Fatalf("%s/%s: read stdin: %v", name, label, err)
			}
			if !strings.Contains(string(stdin), canary) {
				t.Errorf("%s/%s: stdin %q does not carry the prompt", name, label, stdin)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no review runners found; the guard checked nothing")
	}
}
