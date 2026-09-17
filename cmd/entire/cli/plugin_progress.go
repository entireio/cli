package cli

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
)

type pluginProgressKey struct{}

// withPluginProgress opts the command into progress reporting. The context
// carries the writer through dependency installs without making library callers
// print to the process's terminal.
func withPluginProgress(ctx context.Context, out io.Writer) context.Context {
	return context.WithValue(ctx, pluginProgressKey{}, out)
}

// startPluginStep reports work before it starts. Stop the spinner before any
// prompt, warning or result is printed. The returned stop is idempotent so a
// deferred cleanup can also cover early returns.
func startPluginStep(ctx context.Context, message string) func() {
	out, ok := ctx.Value(pluginProgressKey{}).(io.Writer)
	if !ok {
		return func() {}
	}
	if IsAccessibleMode() || !interactive.ShouldStyle(out) {
		fmt.Fprintln(out, message)
		return func() {}
	}
	stop := startSpinner(out, message)
	var once sync.Once
	return func() { once.Do(func() { stop(false) }) }
}
