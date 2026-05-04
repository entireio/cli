//go:build integration && unix

package integration

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/creack/pty"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// RunCommandInteractive executes a CLI command with a pty, allowing interactive
// prompt responses. The respond function receives the pty for reading output
// and writing input, and should return the output it read.
func (env *TestEnv) RunCommandInteractive(args []string, respond func(ptyFile *os.File) string) (string, error) {
	env.T.Helper()

	cmd := exec.Command(getTestBinary(), args...)
	cmd.Dir = env.RepoDir
	cmd.Env = append(testutil.GitIsolatedEnv(),
		"ENTIRE_TEST_CLAUDE_PROJECT_DIR="+env.ClaudeProjectDir,
		"ENTIRE_TEST_TTY=1",
		"TERM=xterm",
		"ACCESSIBLE=1",      // Required: makes huh read from stdin instead of /dev/tty
		"ENTIRE_TEST_TTY=1", // Force CanPromptInteractively()=true: the subprocess has a real pty but may inherit CI=true from the runner, which would otherwise short-circuit the interactive gate.
	)

	// Start command with a pty
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return "", fmt.Errorf("failed to start pty: %w", err)
	}
	defer ptmx.Close()

	// Let the respond function interact with the pty and collect output
	var respondOutput string
	respondDone := make(chan struct{})
	go func() {
		defer close(respondDone)
		respondOutput = respond(ptmx)
	}()

	// Wait for respond function with timeout
	select {
	case <-respondDone:
		// respond completed
	case <-time.After(10 * time.Second):
		env.T.Log("Warning: respond function timed out")
	}

	// Collect any remaining output after respond is done
	var remaining bytes.Buffer
	remainingDone := make(chan struct{})
	go func() {
		defer close(remainingDone)
		_, _ = io.Copy(&remaining, ptmx)
	}()

	// Wait for process to complete with timeout
	cmdDone := make(chan error, 1)
	go func() {
		cmdDone <- cmd.Wait()
	}()

	var cmdErr error
	select {
	case cmdErr = <-cmdDone:
		// process completed
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		cmdErr = fmt.Errorf("process timed out")
	}

	// Give remaining output goroutine time to finish after process exits
	select {
	case <-remainingDone:
	case <-time.After(1 * time.Second):
	}

	return respondOutput + remaining.String(), cmdErr
}

// WaitForPromptAndRespond reads from the pty until it sees the expected prompt text,
// then writes the response. Returns the output read so far.
func WaitForPromptAndRespond(ptyFile *os.File, promptSubstring, response string, timeout time.Duration) (string, error) {
	var output bytes.Buffer
	buf := make([]byte, 1024)
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		// Set read deadline to avoid blocking forever
		_ = ptyFile.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, err := ptyFile.Read(buf)
		if n > 0 {
			output.Write(buf[:n])
			if strings.Contains(output.String(), promptSubstring) {
				// Found the prompt, send response
				_, _ = ptyFile.WriteString(response)
				return output.String(), nil
			}
		}
		if err != nil && !os.IsTimeout(err) {
			return output.String(), err
		}
	}
	return output.String(), fmt.Errorf("timeout waiting for prompt containing %q", promptSubstring)
}
