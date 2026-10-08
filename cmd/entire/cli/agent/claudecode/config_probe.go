package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// configProbeEnvVar points the config-dir probe at a fake claude binary in
// tests, or switches it off with "off". Under `go test` the probe does not run
// at all unless this names a binary, so no test starts the developer's claude.
const configProbeEnvVar = "ENTIRE_TEST_CLAUDE_CONFIG_PROBE"

// configProbeTimeout bounds the whole probe. The initialize round trip measured
// about 0.25s against claude 2.1.285; the bound only matters when claude hangs.
const configProbeTimeout = 5 * time.Second

// configProbeRequestID tags the initialize request so its reply is recognized
// among whatever else claude streams first (hook progress, system messages).
const configProbeRequestID = "entire-config-dir"

// outputStylesDirName is the directory claude reports in user_output_styles_dir,
// directly under its config home.
const outputStylesDirName = "output-styles"

var errConfigProbeSkipped = errors.New("claude config probe skipped")

// configProbe memoizes the probe for the process: at most one claude is
// started, and a failed probe is not retried.
var configProbe struct {
	mu   sync.Mutex
	done bool
	dir  string
}

// probedConfigDir returns claude's config home as claude itself resolves it, or
// "" when probing is not enabled for this process or the probe failed. See
// agent.EnableHomeProbes for when it is enabled.
func probedConfigDir() string {
	if !agent.HomeProbesEnabled() {
		return ""
	}
	configProbe.mu.Lock()
	defer configProbe.mu.Unlock()
	if !configProbe.done {
		configProbe.done = true
		ctx := context.Background()
		dir, err := probeConfigDir(ctx)
		if err != nil {
			if !errors.Is(err, errConfigProbeSkipped) {
				logging.Debug(ctx, "claude-code: config dir probe failed, falling back to the environment", slog.String("error", err.Error()))
			}
		} else {
			configProbe.dir = dir
		}
	}
	return configProbe.dir
}

// probeConfigDir asks claude for its config home through the SDK initialize
// request, which answers without sending a prompt to the model. The reply's
// user_output_styles_dir is documented in the binary as the output-styles
// folder "of the config home as the CLI resolves it after the settings env
// blocks are applied, so a CLAUDE_CONFIG_DIR set in user or managed settings is
// honored" — which is the case the environment cannot answer. The field is
// internal and absent on older CLIs, so every failure falls back to
// agent.ResolveHome rather than erroring.
//
// The child is confined: hooks are disabled (they could include Entire's own,
// and a SessionStart hook would otherwise fire), no MCP server is started, and
// it runs in a fresh private directory so no repository's project settings are
// read.
func probeConfigDir(ctx context.Context) (string, error) {
	bin, err := configProbeBinary()
	if err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp("", "entire-claude-probe-")
	if err != nil {
		return "", fmt.Errorf("create probe dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	ctx, cancel := context.WithTimeout(ctx, configProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin,
		"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--settings", `{"disableAllHooks":true}`, flagStrictMCP)
	cmd.Dir = tmp
	cmd.Env = os.Environ()
	// Stderr stays nil (the null device) rather than io.Discard: a writer
	// makes Wait block on a copy that only ends once every process holding the
	// pipe exits, and a child claude spawned can outlive the kill below.
	// WaitDelay bounds the same wait on stdout.
	cmd.WaitDelay = time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("probe stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("probe stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start claude: %w", err)
	}
	defer func() {
		_ = cmd.Process.Kill() //nolint:errcheck // the child has served its purpose or failed; either way it goes
		_ = cmd.Wait()         //nolint:errcheck // killed above, so the exit status carries no information
	}()

	request := fmt.Sprintf(`{"type":"control_request","request_id":%q,"request":{"subtype":"initialize"}}`+"\n", configProbeRequestID)
	if _, err := io.WriteString(stdin, request); err != nil {
		return "", fmt.Errorf("send initialize: %w", err)
	}
	// Nothing else will be sent, so say so. claude 2.1.285 answers either way
	// (measured: 0.2s with stdin closed, 0.25s left open), but a CLI that waits
	// for end of input would otherwise sit until the timeout.
	if err := stdin.Close(); err != nil {
		return "", fmt.Errorf("close probe stdin: %w", err)
	}
	return readConfigDirReply(stdout)
}

// configProbeBinary picks the claude binary to probe, or reports the probe
// skipped.
func configProbeBinary() (string, error) {
	switch override := os.Getenv(configProbeEnvVar); {
	case override == "off":
		return "", errConfigProbeSkipped
	case override != "":
		return override, nil
	case testing.Testing():
		return "", errConfigProbeSkipped
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		return "", errConfigProbeSkipped
	}
	// A .cmd/.bat shim runs through cmd.exe, which would reparse the JSON
	// argument; only a native executable gets the argv as passed.
	if runtime.GOOS == "windows" && !strings.EqualFold(filepath.Ext(bin), ".exe") {
		return "", errConfigProbeSkipped
	}
	return bin, nil
}

// readConfigDirReply scans claude's stream for the initialize reply and returns
// the config home it names.
func readConfigDirReply(r io.Reader) (string, error) {
	scanner := bufio.NewScanner(r)
	// The reply lists every command, agent and model, so it outgrows the
	// default 64KiB line limit.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var msg struct {
			Type     string `json:"type"`
			Response struct {
				RequestID string `json:"request_id"`
				Response  struct {
					UserOutputStylesDir string `json:"user_output_styles_dir"`
				} `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal(scanner.Bytes(), &msg) != nil || msg.Type != "control_response" || msg.Response.RequestID != configProbeRequestID {
			continue
		}
		styles := msg.Response.Response.UserOutputStylesDir
		if styles == "" {
			return "", errors.New("claude did not report user_output_styles_dir")
		}
		if !filepath.IsAbs(styles) || filepath.Base(styles) != outputStylesDirName {
			return "", fmt.Errorf("unexpected user_output_styles_dir %q", styles)
		}
		return filepath.Dir(styles), nil
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read claude output: %w", err)
	}
	return "", errors.New("claude exited without answering initialize")
}
