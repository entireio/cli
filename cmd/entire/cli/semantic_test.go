package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

func TestBuildSemanticChangesMarkdown(t *testing.T) {
	result := &semanticDiffResult{
		Base: "HEAD~1",
		Head: "HEAD",
		Files: []semanticFileChange{{
			Path:     "auth.py",
			Language: "Python",
			Changes: []semanticEntityChange{{
				Type:            "signature_changed",
				Kind:            "function",
				Name:            "validate_token",
				DependentsCount: 14,
			}},
		}},
	}

	got := buildSemanticChangesMarkdown(result)
	for _, want := range []string{
		"## Semantic Changes",
		"`auth.py` _Python_",
		"validate_token signature changed (14 dependents)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("markdown missing %q:\n%s", want, got)
		}
	}
}

func TestFormatCheckpointOutputIncludesSemanticChanges(t *testing.T) {
	content := &checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			SessionID:    "session-1",
			FilesTouched: []string{"auth.py"},
		},
		Prompts: "update auth",
	}
	semantic := &semanticDiffResult{
		Base: "HEAD~1",
		Head: "HEAD",
		Files: []semanticFileChange{{
			Path: "auth.py",
			Changes: []semanticEntityChange{{
				Type:            "renamed",
				Kind:            "function",
				Name:            "format_date",
				OldName:         "format_dt",
				NewName:         "format_date",
				DependentsCount: 0,
			}},
		}},
	}

	output := formatCheckpointOutput(nil, content, id.MustCheckpointID("abc123def456"), nil, checkpoint.Author{}, false, false, &bytes.Buffer{}, semantic)
	if !strings.Contains(output, "## Semantic Changes") {
		t.Fatalf("output missing semantic section:\n%s", output)
	}
	if !strings.Contains(output, "format_date renamed from format_dt (0 dependents)") {
		t.Fatalf("output missing semantic rename:\n%s", output)
	}
}

func TestRunSemanticPluginDiffGracefullyHandlesMissingPlugin(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv(pluginEnvPluginDir, t.TempDir())
	result, err := defaultRunSemanticPluginDiff(context.Background(), "HEAD~1", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if result != nil {
		t.Fatalf("result = %#v, want nil", result)
	}
}

func TestSemanticRewindPreviewSkipsPluginFailure(t *testing.T) {
	oldRunner := runSemanticPluginDiff
	runSemanticPluginDiff = func(context.Context, string, string) (*semanticDiffResult, error) {
		return nil, errors.New("boom")
	}
	defer func() { runSemanticPluginDiff = oldRunner }()

	t.Setenv(semanticDisableEnv, "1")
	var out bytes.Buffer
	printSemanticRewindPreview(context.Background(), &out, strategy.RewindPoint{ID: "abc123"})
	if out.Len() != 0 {
		t.Fatalf("preview wrote output despite disabled semantic integration: %q", out.String())
	}
}

func TestSemanticPluginEnvDisablesRecursiveSemanticBridge(t *testing.T) {
	env := semanticPluginEnv(t.TempDir())
	if !envContains(env, semanticDisableEnv+"=1") {
		t.Fatalf("semantic plugin env missing %s=1", semanticDisableEnv)
	}
}

func envContains(env []string, want string) bool {
	for _, item := range env {
		if item == want {
			return true
		}
	}
	return false
}
