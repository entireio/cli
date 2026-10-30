package strategy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/telemetry"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// autoCommentChar is the core.commentChar value that lets `git commit` pick
// the prefix.
const autoCommentChar = "auto"

// commentConfig is what git's config says about comment lines in a commit
// message.
type commentConfig struct {
	prefix   string            // core.commentChar or core.commentString (aliases; the last one set wins), else git's default
	cleanup  string            // commit.cleanup, "" when unset
	status   bool              // commit.status: git writes its comment block into a message it opens in the editor
	autoRule trailers.AutoRule // how this git resolves "auto"; read only when prefix is "auto"
}

// commentConfigKey holds a commit hook's comment config (withCommentConfig).
type commentConfigKey struct{}

type commentConfigMemo struct {
	mu   sync.Mutex
	read bool
	cfg  commentConfig
}

// withCommentConfig gives a commit hook one successful read of git's comment
// config: prepare-commit-msg's squash, redo, amend and stamping steps each
// need it, and git commit itself reads its config only once.
func withCommentConfig(ctx context.Context) context.Context {
	if _, ok := ctx.Value(commentConfigKey{}).(*commentConfigMemo); ok {
		return ctx
	}
	return context.WithValue(ctx, commentConfigKey{}, &commentConfigMemo{})
}

// gitCommentConfig returns git's comment config; every read of it goes
// through here. Only the configured values count: the hooks cannot
// see `git commit --cleanup=<mode>`. Inside a commit hook (withCommentConfig)
// git is asked until it answers once; a failed read yields git's defaults.
func gitCommentConfig(ctx context.Context) commentConfig {
	memo, ok := ctx.Value(commentConfigKey{}).(*commentConfigMemo)
	if !ok {
		cfg, _ := readGitCommentConfig(ctx)
		return cfg
	}
	memo.mu.Lock()
	defer memo.mu.Unlock()
	if !memo.read {
		cfg, ok := readGitCommentConfig(ctx)
		if !ok {
			return cfg // not cached: a later call asks again
		}
		memo.cfg, memo.read = cfg, true
	}
	return memo.cfg
}

// readGitCommentConfig reads commentConfig in one git config call, and one
// git --version call when "auto" is set. ok is false when either call failed,
// leaving git's defaults in their place; config that is not set is a
// successful read.
func readGitCommentConfig(ctx context.Context) (cfg commentConfig, ok bool) {
	cfg = commentConfig{prefix: trailers.DefaultCommentPrefix, status: true}
	out, err := exec.CommandContext(ctx, "git", "config", "--get-regexp", `^(core\.comment(char|string)|commit\.(cleanup|status))$`).Output()
	if err != nil {
		var exitErr *exec.ExitError
		return cfg, errors.As(err, &exitErr) && exitErr.ExitCode() == 1 // 1: none set
	}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		key, value, hasValue := strings.Cut(line, " ")
		switch {
		case key == "commit.cleanup":
			cfg.cleanup = value
		case key == "commit.status":
			cfg.status = gitConfigBool(value, hasValue)
		case value == "":
			cfg.prefix = trailers.DefaultCommentPrefix
		default:
			cfg.prefix = value
		}
	}
	if cfg.prefix == autoCommentChar {
		version := telemetry.GitVersion(ctx)
		cfg.autoRule = autoRuleForGitVersion(version)
		return cfg, version != ""
	}
	return cfg, true
}

// autoRuleForGitVersion returns the core.commentChar=auto rule of git at
// version (telemetry.GitVersion). Git 2.51 changed the rule; a version that
// cannot be read is taken as current.
func autoRuleForGitVersion(version string) trailers.AutoRule {
	var major, minor int
	if _, err := fmt.Sscanf(version, "%d.%d", &major, &minor); err == nil && (major < 2 || major == 2 && minor < 51) {
		return trailers.AutoCountsEveryLine
	}
	return trailers.AutoSkipsTrailingComments
}

// gitConfigBool reads a boolean config value as git does. A key with no value,
// which `git config --get-regexp` prints without a space, is true. An empty
// value is false, as are false, no, off and zero. Any other value is true:
// true, yes, on and other numbers, since git refuses the rest.
func gitConfigBool(value string, hasValue bool) bool {
	if !hasValue {
		return true
	}
	switch strings.ToLower(value) {
	case "", "false", "no", "off":
		return false
	}
	n, err := strconv.Atoi(value)
	return err != nil || n != 0
}

// prefixFor returns the prefix git cleans message with. For "auto" git picks
// it from the message (see trailers.AutoCommentPrefix), and writes its comment
// block with it only into a message it opens in the editor (edited) while
// commit.status is on.
func (cfg commentConfig) prefixFor(message string, edited bool) string {
	if cfg.prefix != autoCommentChar {
		return cfg.prefix
	}
	return trailers.AutoCommentPrefix(message, edited && cfg.status, cfg.autoRule)
}

// trailerPrefix returns the prefix git's trailer grammar skips in a recorded
// commit message. Only `git commit` resolves "auto"; `git interpret-trailers`
// reads it as git's default.
func (cfg commentConfig) trailerPrefix() string {
	if cfg.prefix == autoCommentChar {
		return trailers.DefaultCommentPrefix
	}
	return cfg.prefix
}

// cleanupStrip is the commit.cleanup mode that drops comment lines from every
// message, not only an edited one.
const cleanupStrip = "strip"

// dropsComments reports whether git's cleanup drops comment lines from a
// message: always under commit.cleanup=strip, never under whitespace, verbatim
// or scissors, and by default only from one it opened in the editor.
func (cfg commentConfig) dropsComments(edited bool) bool {
	switch cfg.cleanup {
	case cleanupStrip:
		return true
	case "whitespace", "verbatim", "scissors":
		return false
	}
	return edited
}

// gitOpensEditor reports whether git opens the commit message in the editor,
// which git tells its commit hooks: it runs them with GIT_EDITOR=: when it
// does not (githooks(5)), as for -m/-F without -e, --no-edit and -C. The
// source word cannot tell: -m with -e, and an amend with --no-edit or without.
func gitOpensEditor() bool {
	return os.Getenv("GIT_EDITOR") != ":"
}

// commentRules is how git treats comment lines in a message
// prepare-commit-msg was handed. prefix starts the comment lines git's cleanup
// sees. trailerPrefix starts the ones git's trailer grammar skips once the
// commit is recorded, as post-commit and attach read it; they differ only for
// core.commentChar=auto. edited means git opens the message in the editor,
// and has appended its comment block unless commit.status is off. stripped
// means git's cleanup drops every comment line; otherwise git keeps them.
type commentRules struct {
	prefix        string
	trailerPrefix string
	edited        bool
	stripped      bool
}

// messageCommentRules returns how git treats comment lines in the message
// prepare-commit-msg was handed.
func messageCommentRules(ctx context.Context, message string) commentRules {
	cfg := gitCommentConfig(ctx)
	edited := gitOpensEditor()
	return commentRules{
		prefix:        cfg.prefixFor(message, edited),
		trailerPrefix: cfg.trailerPrefix(),
		edited:        edited,
		stripped:      cfg.dropsComments(edited),
	}
}

// LinkedCheckpoints returns the checkpoints a saved commit's message links:
// the Entire-Checkpoint trailers of its final trailer block, read by git's
// trailer grammar, which skips comment lines (commit.cleanup modes other than
// strip keep them, git's comment block among them). Code that writes into a
// checkpoint resolves a commit with it; read-only views use CommitCheckpoints.
func LinkedCheckpoints(ctx context.Context, message string) []id.CheckpointID {
	return linkedCheckpointsReader(ctx)(message)
}

// linkedCheckpointsReader returns LinkedCheckpoints for resolving several
// commits, such as the ones a squash or redo inherits from: git config is read
// once, and only when a message needs its comment prefix.
func linkedCheckpointsReader(ctx context.Context) func(message string) []id.CheckpointID {
	var commentPrefix *string
	return func(message string) []id.CheckpointID {
		// A block read without the prefix holds no comment lines, so reading
		// with it finds the same IDs; only a message that still names a
		// checkpoint needs the git config read.
		if ids := trailers.ParseAllCheckpointsFromFinalTrailerBlock(message, ""); len(ids) > 0 || !strings.Contains(message, trailers.CheckpointTrailerKey) {
			return ids
		}
		if commentPrefix == nil {
			prefix := gitCommentConfig(ctx).trailerPrefix()
			commentPrefix = &prefix
		}
		return trailers.ParseAllCheckpointsFromFinalTrailerBlock(message, *commentPrefix)
	}
}

// CommitCheckpoints returns the checkpoints a read-only view shows for a saved
// commit, without reading git config: its final trailer block's IDs, read by
// git's trailer grammar skipping git's default comment lines, or, when that
// names none, the first ID anywhere in the message, since a squash commit can
// carry its squashed commits' trailers in the body. Only read-only views may
// use that fallback: a body mention can make them show a checkpoint, never
// write into one.
func CommitCheckpoints(message string) []id.CheckpointID {
	if ids := trailers.ParseAllCheckpointsFromFinalTrailerBlock(message, trailers.DefaultCommentPrefix); len(ids) > 0 {
		return ids
	}
	if cpID, ok := trailers.ParseCheckpoint(message); ok {
		return []id.CheckpointID{cpID}
	}
	return nil
}

// CommitCheckpoint returns the checkpoint a read-only view shows for a saved
// commit: the last of CommitCheckpoints, since a squash's inherited trailers
// come before the one prepare-commit-msg stamped. Attach and
// `explain --generate` resolve through LinkedCheckpoints instead.
func CommitCheckpoint(message string) (id.CheckpointID, bool) {
	ids := CommitCheckpoints(message)
	if len(ids) == 0 {
		return id.EmptyCheckpointID, false
	}
	return ids[len(ids)-1], true
}
