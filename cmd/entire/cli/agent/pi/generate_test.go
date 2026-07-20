package pi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

const piGenerateHelperTest = "TestPiGenerateTextHelperProcess"
const piGenerateHelperMarker = "__entire_pi_generate_helper__"

// TestPiGenerateTextHelperProcess is a subprocess entrypoint. It cannot call
// t.Parallel because its helper modes call os.Exit.
func TestPiGenerateTextHelperProcess(_ *testing.T) {
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator == -1 {
		return
	}

	args := os.Args[separator+1:]
	if len(args) == 0 || args[0] != piGenerateHelperMarker {
		return
	}
	args = args[1:]
	if len(args) == 0 {
		os.Exit(2)
	}
	switch args[0] {
	case "auth":
		_, _ = fmt.Fprint(os.Stderr, "HTTP 401 Unauthorized")
		os.Exit(17)
	case "empty":
		_, _ = fmt.Fprint(os.Stderr, "diagnostic detail")
		os.Exit(0)
	default:
		os.Exit(2)
	}
}

func piHelperRunner(mode string) agent.TextCommandRunner {
	return func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, os.Args[0], "-test.run=^"+piGenerateHelperTest+"$", "--", piGenerateHelperMarker, mode)
	}
}

func TestPiAgentGenerateText_ClassifiesFallbackFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		runner agent.TextCommandRunner
		kind   agent.TextGenerationErrorKind
	}{
		{
			name: "missing CLI",
			runner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "definitely-not-installed-pi-binary")
			},
			kind: agent.TextGenerationErrorCLIMissing,
		},
		{name: "auth stderr", runner: piHelperRunner("auth"), kind: agent.TextGenerationErrorAuth},
		{name: "empty stdout with stderr", runner: piHelperRunner("empty"), kind: agent.TextGenerationErrorUnknown},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			piAgent := &PiAgent{CommandRunner: test.runner}
			_, err := piAgent.GenerateText(context.Background(), "prompt", "")
			if err == nil {
				t.Fatal("expected error")
			}
			var failure *agent.TextGenerationError
			if !errors.As(err, &failure) {
				t.Fatalf("err = %T %v, want *TextGenerationError", err, err)
			}
			if failure.Kind != test.kind {
				t.Errorf("Kind = %v, want %v", failure.Kind, test.kind)
			}
			if failure.Provider != agent.AgentNamePi {
				t.Errorf("Provider = %q, want %q", failure.Provider, agent.AgentNamePi)
			}
			if test.name == "empty stdout with stderr" {
				if failure.Stderr != "diagnostic detail" {
					t.Errorf("Stderr = %q, want retained diagnostic", failure.Stderr)
				}
				if !strings.Contains(failure.Message, "diagnostic detail") {
					t.Errorf("Message = %q, want retained diagnostic", failure.Message)
				}
			}
			if count := countPiTextGenerationErrors(err); count != 1 {
				t.Errorf("TextGenerationError count = %d, want 1 (chain: %v)", count, err)
			}
		})
	}
}

func countPiTextGenerationErrors(err error) int {
	count := 0
	targetType := reflect.TypeFor[*agent.TextGenerationError]()
	for err != nil {
		if reflect.TypeOf(err) == targetType {
			count++
		}
		err = errors.Unwrap(err)
	}
	return count
}
