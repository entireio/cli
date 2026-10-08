//go:build !windows

package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// The child has a controlling terminal but piped stdin. Both prompt modes must
// read the terminal answer and leave every byte of the plugin's input intact.
func TestPluginConfirmationRedirectedInput(t *testing.T) {
	t.Parallel()
	const marker = "ENTIRE_TEST_PLUGIN_CONFIRM_CHILD"
	const question = "Install test plugin?"
	const payload = "plugin input that must survive\n"
	if os.Getenv(marker) == "1" {
		answer, err := runPluginConfirm(t.Context(), os.Stderr, question, true)
		if err != nil || !answer {
			t.Fatalf("confirmation: answer=%v err=%v", answer, err)
		}
		data, err := io.ReadAll(os.Stdin)
		if err != nil || string(data) != payload {
			t.Fatalf("plugin stdin=%q err=%v", data, err)
		}
		fmt.Fprintln(os.Stderr, "INPUT_PRESERVED")
		return
	}
	for _, accessible := range []string{"", "1"} {
		t.Run("accessible="+accessible, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPluginConfirmationRedirectedInput$")
			cmd.Env = append(os.Environ(), marker+"=1", "ACCESSIBLE="+accessible, "TERM=xterm-256color")
			cmd.Stdin = strings.NewReader(payload)
			terminal, err := pty.StartWithAttrs(cmd, &pty.Winsize{Rows: 24, Cols: 100}, &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer terminal.Close()
			output := make(chan string, 1)
			go func() {
				var transcript strings.Builder
				answered := false
				buf := make([]byte, 4096)
				for {
					n, readErr := terminal.Read(buf)
					transcript.Write(buf[:n])
					if !answered && strings.Contains(transcript.String(), question) {
						answered = true
						if _, err := io.WriteString(terminal, "\r"); err != nil {
							cancel()
						}
					}
					if readErr != nil {
						output <- transcript.String()
						return
					}
				}
			}()
			err = cmd.Wait()
			transcript := <-output
			if err != nil || !strings.Contains(transcript, "INPUT_PRESERVED") {
				t.Fatalf("child: %v\n%s", err, transcript)
			}
		})
	}
}
