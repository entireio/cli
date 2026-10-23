package opencode

import (
	"context"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"

	"golang.org/x/mod/semver"
)

// openCodeCLIProbeTimeout bounds `opencode --version`. Hooks run inside git
// hooks with their own budgets, so a hung probe must not consume them.
const openCodeCLIProbeTimeout = 5 * time.Second

// openCodeCLI selects the session commands for the installed OpenCode.
// OpenCode 2 moved `export` under `session` and attaches it to a shared
// background service unless `--standalone` is passed; OpenCode 1 rejects
// `--standalone`. Import and delete are only used on OpenCode 1 (see
// importSessionIntoOpenCode).
type openCodeCLI struct {
	legacy bool // OpenCode 1.x
}

func (c openCodeCLI) exportArgs(sessionID string) []string {
	if c.legacy {
		return []string{"export", sessionID}
	}
	return []string{"session", "export", "--standalone", sessionID}
}

var (
	openCodeCLIMu     sync.Mutex
	openCodeCLICached *openCodeCLI
)

// detectOpenCodeCLI probes `opencode --version` once per process. Only a
// version that parses as 0.x or 1.x selects the legacy commands; OpenCode 2,
// an unreadable version or a failed probe use the OpenCode 2 commands. A
// missing binary fails either way, with the command's own not-found error.
var detectOpenCodeCLI = func(ctx context.Context) openCodeCLI {
	openCodeCLIMu.Lock()
	defer openCodeCLIMu.Unlock()
	if openCodeCLICached != nil {
		return *openCodeCLICached
	}
	cli, ok := probeOpenCodeCLI(ctx)
	if ok {
		openCodeCLICached = &cli
	}
	return cli
}

// probeOpenCodeCLI reports the selected commands and whether the answer is
// worth caching: a cancelled caller or a failed probe is not evidence about
// the binary.
func probeOpenCodeCLI(ctx context.Context) (openCodeCLI, bool) {
	probeCtx, cancel := context.WithTimeout(ctx, openCodeCLIProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, "opencode", "--version")
	cmd.Env = gitrepo.EnvWithoutRepoOverrides()
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return openCodeCLI{}, false
	}
	if err != nil {
		// Not cached: a slow or crashing probe says nothing lasting about the
		// binary, and a wrong guess would fail every export in this process.
		logging.Warn(logging.WithComponent(ctx, "opencode"),
			"opencode version probe failed; using OpenCode 2 session commands",
			slog.String("error", err.Error()))
		return openCodeCLI{}, false
	}
	version := openCodeVersion(string(out))
	if version == "" {
		logging.Debug(logging.WithComponent(ctx, "opencode"),
			"opencode version unreadable; using OpenCode 2 session commands")
		return openCodeCLI{}, true
	}
	major := semver.Major(version)
	return openCodeCLI{legacy: major == "v0" || major == "v1"}, true
}
