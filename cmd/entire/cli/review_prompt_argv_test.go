package cli

import (
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
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

// TestJudges_PromptNeverInArgv guards the judge path: any agent can be picked
// as the judge, and its GenerateText gets the synthesis prompt, which carries
// every reviewer's report and the user's --prompt. Each text generator runs
// with a runner that records argv instead of starting the CLI.
func TestJudges_PromptNeverInArgv(t *testing.T) {
	// No real agent CLI may start, and none may read the developer's config.
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	const canary = "entire-argv-guard-canary-judge-91c2"
	checked := 0
	for _, name := range agent.List() {
		ag, err := agent.Get(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, ok := ag.(agent.TextGenerator); !ok {
			continue
		}
		// External agents speak their own protocol, not a CLI argv Entire
		// builds; other tests in this process may have registered one.
		if external.IsExternal(ag) {
			continue
		}
		// A fresh value of the agent's type, so the shared registry instance
		// keeps its real runner.
		fresh := reflect.New(reflect.TypeOf(ag).Elem())
		field := fresh.Elem().FieldByName("CommandRunner")
		if !field.IsValid() || !field.CanSet() {
			t.Fatalf("%s: no CommandRunner field; extend this guard to record its command", name)
		}
		var argvs [][]string
		record := func(ctx context.Context, bin string, args ...string) *exec.Cmd {
			argvs = append(argvs, append([]string{bin}, args...))
			return exec.CommandContext(ctx, filepath.Join(t.TempDir(), "missing"))
		}
		field.Set(reflect.ValueOf(record).Convert(field.Type()))
		gen, ok := fresh.Interface().(agent.TextGenerator)
		if !ok {
			t.Fatalf("%s: fresh %T is not a TextGenerator", name, fresh.Interface())
		}

		_, _ = gen.GenerateText(context.Background(), canary, "") //nolint:errcheck // the recorded command cannot start
		if len(argvs) == 0 {
			t.Errorf("%s: GenerateText started no command through CommandRunner; the guard checked nothing", name)
		}
		for _, argv := range argvs {
			for i, arg := range argv {
				if strings.Contains(arg, canary) {
					t.Errorf("%s: prompt in argv[%d] %q; pipe it via stdin", name, i, arg)
				}
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no text generators found; the guard checked nothing")
	}
}
