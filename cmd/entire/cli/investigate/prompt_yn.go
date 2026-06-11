package investigate

import (
	"context"

	"github.com/entireio/cli/cmd/entire/cli/uiform"
)

func realPromptYN(ctx context.Context, question string, def bool) (bool, error) {
	return uiform.PromptYN(ctx, question, def) //nolint:wrapcheck // uiform already wraps
}
