package pi

import (
	"context"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/review"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// preparePiReviewConfig applies a review profile's agent config: the
// checkout's extensions are not discovered (--no-extensions); Entire's own
// extension, written to the run dir so the session is still tracked, and the
// profile's extensions are loaded explicitly. The checkout's skills, prompt
// templates and .pi/settings.json still load; the trust gate lists them.
func preparePiReviewConfig(ctx context.Context, cfg reviewtypes.RunConfig) (reviewtypes.RunConfig, func(), error) {
	if cfg.AgentConfig == nil {
		return cfg, nil, nil
	}
	run, err := review.NewAgentConfigRun(ctx)
	if err != nil {
		return cfg, nil, err //nolint:wrapcheck // already names the step
	}
	if err := review.ValidateAgentConfig("pi", cfg.AgentConfig, run.ForbiddenRoots); err != nil {
		run.Cleanup()
		return cfg, nil, fmt.Errorf("review profile config: %w", err)
	}
	entireExt, err := run.WriteFile("pi/entire/index.ts", []byte(renderExtension()))
	if err != nil {
		run.Cleanup()
		return cfg, nil, err //nolint:wrapcheck // already names the file
	}
	cfg.ExtraArgs = append(cfg.ExtraArgs, "--no-extensions", "--extension", entireExt)
	for _, ext := range cfg.AgentConfig.Extensions {
		cfg.ExtraArgs = append(cfg.ExtraArgs, "--extension", ext)
	}
	return cfg, run.Cleanup, nil
}
