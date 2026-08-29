package strategy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// capturedSyncRemotesFileName is the per-clone captured-election state, stored
// in the git common dir (worktree-shared, like the push queue). List-shaped
// from day one: phase 1 caps membership at one remote, and lifting the cap
// (per-remote push-queue tracking) must not need a state migration.
const capturedSyncRemotesFileName = "entire-checkpoint-sync-remotes.json"

// capturedSyncRemotesLockName serializes the read-decide-write in
// commitCapturedSyncRemote. Paired with the state file exactly as
// checkpoint/pushqueue.go pairs its queue and lock in this same directory: the
// atomic write below keeps readers safe, but without the lock two concurrent
// pre-push hooks in different worktrees both observe "nothing captured", both
// write, and last-rename-wins — so each announces a different remote and "first
// capture sticks" becomes a coin flip.
const capturedSyncRemotesLockName = "entire-checkpoint-sync-remotes.lock"

type capturedSyncRemotesFile struct {
	Remotes []string `json:"remotes"`
}

func capturedSyncRemotesPath(ctx context.Context) (string, error) {
	commonDir, err := GetGitCommonDir(ctx)
	if err != nil {
		return "", err
	}
	return filepath.Join(commonDir, capturedSyncRemotesFileName), nil
}

func capturedSyncRemotesLockPath(ctx context.Context) (string, error) {
	commonDir, err := GetGitCommonDir(ctx)
	if err != nil {
		return "", err
	}
	return filepath.Join(commonDir, capturedSyncRemotesLockName), nil
}

// loadCapturedSyncRemotes reads the captured election. Fail-soft: a missing,
// unreadable, or corrupt file reads as "nothing captured" — capture is
// automatic state, so unlike the explicit checkpoint_push_remote setting it
// must never fail sync closed.
func loadCapturedSyncRemotes(ctx context.Context) []string {
	path, err := capturedSyncRemotesPath(ctx)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the git common dir resolved from the repo itself, not user input.
	if err != nil {
		return nil
	}
	var f capturedSyncRemotesFile
	if err := json.Unmarshal(data, &f); err != nil {
		logging.Debug(ctx, "captured sync remotes file unreadable; ignoring",
			slog.String("error", err.Error()))
		return nil
	}
	return f.Remotes
}

// saveCapturedSyncRemote persists the one captured remote. Singular on purpose:
// phase 1 caps membership at one, and a plural writer left that cap resting on a
// single call site passing a one-element literal. The on-disk shape stays
// list-shaped, so lifting the cap in phase 2 needs a new writer rather than a
// migration.
func saveCapturedSyncRemote(ctx context.Context, name string) error {
	path, err := capturedSyncRemotesPath(ctx)
	if err != nil {
		return err
	}
	data, err := json.Marshal(capturedSyncRemotesFile{Remotes: []string{name}})
	if err != nil {
		return fmt.Errorf("encode captured sync remotes: %w", err)
	}
	if err := jsonutil.WriteFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("write captured sync remotes: %w", err)
	}
	return nil
}

// pendingCaptureCheckpointSyncRemote reports the remote this push would elect,
// without writing anything. Phase one of two: the gate consults it so the push
// that elects a remote is also the first to carry checkpoints there, and
// commitCapturedSyncRemote persists it only once those checkpoints arrived.
//
// Splitting the two matters because everything between them can stop delivery —
// the policy gate, the OPF decision, the empty-remote defer, a rejected transfer.
// Persisting on intent meant a push that carried nothing still moved the election
// permanently, and the queued checkpoints could then only drain to the remote that
// had just failed to take them.
//
// The lock covers the read only. Holding it across the network push would
// serialize unrelated worktrees on a remote operation, so two hooks can reach the
// same pending answer; commitCapturedSyncRemote re-checks under the lock, making
// the write idempotent — "first capture sticks" is decided when the state lands,
// not when it was proposed.
func pendingCaptureCheckpointSyncRemote(ctx context.Context, pushRemote string) bool {
	if !isConfiguredRemote(ctx, pushRemote) {
		return false
	}
	lockPath, err := capturedSyncRemotesLockPath(ctx)
	if err != nil {
		logging.Debug(ctx, "capture skipped: cannot resolve lock path",
			slog.String("error", err.Error()))
		return false
	}
	release, err := flock.Acquire(lockPath)
	if err != nil {
		logging.Debug(ctx, "capture skipped: cannot acquire lock",
			slog.String("error", err.Error()))
		return false
	}
	defer release()
	_, ok := captureEligible(ctx, pushRemote)
	return ok
}

// captureEligible answers "would this push elect pushRemote", and if so what the
// election was before. Callers hold the lock.
//
// The rule is that the push is consent-grade evidence that pushRemote is the
// user's own remote: the push target agrees with the branch's declared push
// destination (the config a bare `git push` resolves through). Declaration alone
// was the bug that got the tracking tier dropped from the election (74e239a9 — it
// elected remotes that never receive pushes); behavior alone is the
// pre-single-remote transcript leak. Only their intersection qualifies.
//
// Phase-1 rules: at most one captured remote, and the first capture sticks (a
// mixed-habit repo whose branches push two remotes must not flip the election on
// every push). The default-elected seed is displaceable once; after that,
// re-routing takes the explicit setting until the multi-remote set ships.
//
// One election answers every gate — err covers unreadable settings and a
// fail-closed checkpoint_push_remote, Source covers an explicit override and a
// capture already in force, Name covers already-elected — so there is no second
// copy of the precedence rules to drift from the resolver's. A separate validity
// predicate beside the resolver was how the two came to disagree about a dead
// captured entry in the first place.
func captureEligible(ctx context.Context, pushRemote string) (previouslyElected string, ok bool) {
	elected, err := ResolveCheckpointSyncRemote(ctx)
	if err != nil {
		return "", false
	}
	switch elected.Source {
	case SyncRemoteSourceConfig, SyncRemoteSourceObserved, SyncRemoteSourceOverride:
		// An explicit setting, a capture already in force, or a per-operation
		// override (`entire trust --remote`, never reached from a pre-push):
		// nothing to displace.
		return "", false
	case SyncRemoteSourceDefault, SyncRemoteSourceSole, SyncRemoteSourceFirst:
		// Exactly the tiers a capture may displace. Enumerated rather than left to
		// a default so `exhaustive` turns a new tier into a decision here instead
		// of silently letting capture override it.
	}
	if elected.Name == pushRemote {
		return "", false
	}
	if declaredPushDestination(ctx) != pushRemote {
		return "", false
	}
	return elected.Name, true
}

// commitCapturedSyncRemote persists the election after checkpoints reached
// pushRemote, and announces it. Phase two: reached only on a delivery that
// succeeded, so the announcement can no longer claim a move that carried nothing.
//
// Re-checks eligibility under the lock rather than trusting the pending answer:
// the network push sat in between, and another worktree's hook may have captured
// meanwhile.
func commitCapturedSyncRemote(ctx context.Context, pushRemote string) {
	lockPath, err := capturedSyncRemotesLockPath(ctx)
	if err != nil {
		return
	}
	release, err := flock.Acquire(lockPath)
	if err != nil {
		return
	}
	defer release()

	previouslyElected, ok := captureEligible(ctx, pushRemote)
	if !ok {
		logging.Debug(ctx, "capture no longer eligible after delivery; leaving the election as is",
			slog.String("remote", pushRemote))
		return
	}
	if saveErr := saveCapturedSyncRemote(ctx, pushRemote); saveErr != nil {
		logging.Warn(ctx, "failed to persist captured checkpoint sync remote",
			slog.String("remote", pushRemote),
			slog.String("error", saveErr.Error()))
		return
	}
	// Announced only after the state landed AND the checkpoints did, so the
	// message can claim neither an unpersisted change nor an empty delivery.
	fmt.Fprintf(stderrWriter,
		"[entire] Checkpoints now sync to %q — the remote your branch pushes to. Override with strategy_options.checkpoint_push_remote in .entire/settings.local.json.\n",
		pushRemote)
	logging.Info(ctx, "checkpoint sync remote captured",
		slog.String("remote", pushRemote),
		slog.String("previously_elected", previouslyElected))
}

// declaredPushDestination resolves where a bare `git push` on the current
// branch would go. Empty when HEAD is detached or nothing is declared.
//
// Phase-1 simplification: the pre-push hook receives only the remote name
// (refspecs are not plumbed through), so the declaration is read from HEAD's
// branch rather than the branches actually being pushed. A miss is
// conservative — no capture happens and the gate behaves as before.
func declaredPushDestination(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "git", "symbolic-ref", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return DeclaredPushRemoteForBranch(ctx, strings.TrimSpace(string(out)))
}

// DeclaredPushRemoteForBranch resolves where `git push` would send branch,
// through git's own precedence: branch.<name>.pushRemote, then
// remote.pushDefault, then branch.<name>.remote. Empty when branch is empty or
// nothing is declared — callers supply their own default, since the right one
// differs per caller ("origin" for a command issuing its own push; "no capture"
// for the checkpoint gate).
//
// This is the "where does this branch push" question, which is NOT the question
// ResolveCheckpointSyncRemote answers ("which single remote may carry checkpoint
// data"). Do not substitute one for the other in either direction; that
// resolver's doc comment holds the full argument, including why checkpoint sync
// deliberately does not elect from the declaration returned here. The short
// version: that objection is about a gate deciding whether to piggyback on
// someone else's push, and does not apply to a caller issuing its own.
func DeclaredPushRemoteForBranch(ctx context.Context, branch string) string {
	if branch == "" {
		return ""
	}
	if v := gitConfigValue(ctx, "branch."+branch+".pushRemote"); v != "" {
		return v
	}
	if v := gitConfigValue(ctx, "remote.pushDefault"); v != "" {
		return v
	}
	return gitConfigValue(ctx, "branch."+branch+".remote")
}

// gitConfigValue returns a single git config value, or "" when unset or on
// any error.
func gitConfigValue(ctx context.Context, key string) string {
	out, err := exec.CommandContext(ctx, "git", "config", "--get", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
