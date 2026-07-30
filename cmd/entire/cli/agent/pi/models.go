package pi

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

var _ agent.ModelLister = (*PiAgent)(nil)

// ListModels returns Pi's live model catalog by shelling out to
// `pi --list-models`. Unlike the curated lists for claude-code/codex/gemini,
// Pi has a real enumeration command spanning every configured provider, so the
// result reflects what this machine/account can actually use.
func (a *PiAgent) ListModels(ctx context.Context) ([]agent.ModelInfo, error) {
	// Deliberately NOT HandleTextGenResult: that helper builds the summary-path
	// error surface, so a model-listing failure would render as "Pi failed to
	// generate the summary" if it ever reached formatCheckpointSummaryError.
	// Listing models is a different operation and gets a plain error.
	res, runErr := agent.RunIsolatedTextGeneratorCLIRaw(ctx, a.CommandRunner, "pi", []string{"--list-models"}, "")
	if runErr != nil {
		detail := strings.TrimSpace(string(res.Stderr))
		if detail == "" {
			detail = strings.TrimSpace(string(res.Stdout))
		}
		if detail == "" {
			detail = runErr.Error()
		}
		return nil, fmt.Errorf("pi --list-models: %s", agent.TruncateStderr(detail))
	}
	out := strings.TrimSpace(string(res.Stdout))
	if out == "" {
		return nil, errors.New("pi --list-models returned empty output")
	}
	return parsePiModelList(out), nil
}

// parsePiModelList parses the tabular `pi --list-models` output. Each non-header
// row is "<provider> <model> <context> <max-out> <thinking> <images>"; the model
// ID is rendered as "provider/model" (the unambiguous form Pi's --model accepts)
// with the context window kept as a note.
func parsePiModelList(raw string) []agent.ModelInfo {
	var models []agent.ModelInfo
	scanner := bufio.NewScanner(strings.NewReader(raw))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		provider, model := fields[0], fields[1]
		if provider == "provider" && model == "model" {
			continue // header row
		}
		note := ""
		if len(fields) >= 3 {
			note = fields[2] + " ctx"
		}
		models = append(models, agent.ModelInfo{ID: provider + "/" + model, Note: note})
	}
	return models
}
