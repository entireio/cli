package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/spf13/cobra"
)

// Session resolution and the snapshot write are `checkpoint create`'s, and are
// covered by its own tests; what follows is only what sharing adds on top.

// Without a remote there is nowhere to share to. Checked before the checkpoint
// is written, so the user is not left with a checkpoint and no explanation.
func TestResolveShareRemote_NoRemotesIsAnError(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	_, err := resolveShareRemote(context.Background())
	if err == nil {
		t.Fatal("resolveShareRemote() = nil error, want a failure when no remote is configured")
	}
	if !strings.Contains(err.Error(), "nowhere to share") {
		t.Errorf("the error should say why sharing cannot proceed, got: %v", err)
	}
}

// Share must not offer a remote override. Reads resolve through the same
// election as the push (the elected remote, then origin), so a flag moving
// only the push would strand the checkpoint somewhere the printed resume
// command never looks.
func TestSessionShare_HasNoRemoteOverride(t *testing.T) {
	t.Parallel()

	if f := newSessionShareCmd().Flags().Lookup("remote"); f != nil {
		t.Errorf("session share must not expose --remote; reads follow the elected remote, so an override strands the handoff")
	}
}

// The printed line is the whole point of the command: it is what the user
// hands over, so it must be a command the recipient can actually run.
func TestPrintShareResult_PrintsARunnableResumeCommand(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("abc123def456")
	root := &cobra.Command{Use: "entire"}
	share := &cobra.Command{Use: "share"}
	root.AddCommand(share)

	var out bytes.Buffer
	printShareResult(&out, share, cpID, strategy.ShareCheckpointResult{Pushed: 1})

	want := "entire session resume " + cpID.String()
	if !strings.Contains(out.String(), want) {
		t.Errorf("share must print %q, got: %s", want, out.String())
	}
	if !strings.Contains(out.String(), "Pushed 1 checkpoint ref(s)") {
		t.Errorf("a counted push should report how many refs went out, got: %s", out.String())
	}
}

// push_sessions=false must not read as a successful share: the checkpoint is
// local, so the resume line has to come with that caveat attached.
func TestPrintShareResult_PushDisabledSaysNothingLeftTheMachine(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	printShareResult(&out, nil, id.MustCheckpointID("abc123def456"),
		strategy.ShareCheckpointResult{PushDisabled: true})

	got := out.String()
	if !strings.Contains(got, "disabled in settings") {
		t.Errorf("a disabled push must say so, got: %s", got)
	}
	if strings.Contains(got, "Pushed") {
		t.Errorf("a disabled push must not claim it pushed, got: %s", got)
	}
}

// Unparented commands fall back to the canonical binary name rather than
// printing a resume line nobody can type.
func TestShareRootName_FallsBackWhenUnparented(t *testing.T) {
	t.Parallel()

	if got := shareRootName(nil); got != cmdRoot {
		t.Errorf("shareRootName(nil) = %q, want %q", got, cmdRoot)
	}
	if got := shareRootName(&cobra.Command{Use: "share"}); got != cmdRoot {
		t.Errorf("shareRootName(unparented) = %q, want %q", got, cmdRoot)
	}
}

// The snapshot writer refuses a session with pending file changes. That is a
// decision, not a fault, so share must turn it into something actionable
// rather than surfacing the sentinel's bare condition.
func TestDescribeShareCheckpointError(t *testing.T) {
	t.Parallel()

	t.Run("pending files become commit-first guidance", func(t *testing.T) {
		t.Parallel()
		err := describeShareCheckpointError(strategy.ErrPendingFileChanges)
		if err == nil {
			t.Fatal("describeShareCheckpointError() = nil, want an error")
		}
		if !strings.Contains(err.Error(), "Commit first") {
			t.Errorf("the refusal should say what to do, got: %v", err)
		}
		if !strings.Contains(err.Error(), "attribution") {
			t.Errorf("the refusal should say why the commit's checkpoint is better, got: %v", err)
		}
	})

	t.Run("anything else passes through unchanged", func(t *testing.T) {
		t.Parallel()
		sentinel := errors.New("disk on fire")
		if err := describeShareCheckpointError(sentinel); !errors.Is(err, sentinel) {
			t.Errorf("describeShareCheckpointError() = %v, want the original error", err)
		}
	})
}
