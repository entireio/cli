package pi

import (
	"bufio"
	"context"
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
	out, err := agent.RunIsolatedTextGeneratorCLI(ctx, nil, "pi", "pi", []string{"--list-models"}, "")
	if err != nil {
		return nil, fmt.Errorf("pi --list-models: %w", err)
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
