//go:build integration

package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// The trailer tests above hand prepare-commit-msg the message git would write.
// These run real `git commit` with Entire's hooks and a scripted editor, so the
// message, GIT_EDITOR, the comment character git picks and its cleanup are
// git's own. Every commit must end with exactly the trailer Entire left, after
// the ones it inherits, keep the user's lines in place, and abort when the user
// saves it untouched; no checkpoint the message only names may be created.
//
//nolint:maintidx // table-driven: the score tracks the size of the case table (data), not branching logic; each row is one git configuration of the same commit
func TestTrailerAuthority_RealGitCommits(t *testing.T) {
	t.Parallel()
	editors := t.TempDir()
	writeScripts(t, editors, map[string]string{
		// Types at the start of the first line, where an editor opens.
		"type": "#!/bin/sh\n{ printf 'Typed '; cat \"$1\"; } > \"$1.typed\" && mv \"$1.typed\" \"$1\"\n",
	})
	keep := "GIT_EDITOR=true"
	typeSubject := "GIT_EDITOR=" + shellQuote(filepath.ToSlash(filepath.Join(editors, "type")))
	files := t.TempDir()
	message := filepath.Join(files, "message")
	require.NoError(t, os.WriteFile(message, []byte("Fix login\n\nBody.\n"), 0o600))
	foreignTrailer := filepath.Join(files, "foreign-trailer")
	require.NoError(t, os.WriteFile(foreignTrailer, []byte("Fix login\n\nEntire-Checkpoint: "+advForeignCheckpoint+"\n"), 0o600))
	template := filepath.Join(files, "template")
	require.NoError(t, os.WriteFile(template, []byte("Subject\n\n#42 is the issue\n"), 0o600))
	// Without a final newline git writes its comment block right below the
	// last line, with no blank line above it.
	templateNoNewline := filepath.Join(files, "template-no-newline")
	require.NoError(t, os.WriteFile(templateNoNewline, []byte("Subject\n\n#42 is the issue"), 0o600))
	templateHashAndSemicolon := filepath.Join(files, "template-hash-and-semicolon")
	require.NoError(t, os.WriteFile(templateHashAndSemicolon, []byte("#42 Subject\n\nBody\n; see the notes\n"), 0o600))
	hashSubject := filepath.Join(files, "hash-subject")
	require.NoError(t, os.WriteFile(hashSubject, []byte("#42 Subject\n\nBody\n"), 0o600))
	ownTrailer := filepath.Join(files, "own-trailer")
	require.NoError(t, os.WriteFile(ownTrailer, []byte("Fix login\n\nEntire-Checkpoint: "+advForeignCheckpoint+"\n#42 is the issue\n"), 0o600))

	type gitFunc = func([]string, ...string) (string, error)
	// commitLinked commits path with messageArgs (-m "My change" when none)
	// as an agent's work, and returns the checkpoint the commit links.
	commitLinked := func(env *TestEnv, git gitFunc, path string, messageArgs ...string) string {
		env.T.Helper()
		authorityAgentTurn(env, path, "work on "+path+"\n")
		_, err := git(nil, "add", path)
		require.NoError(env.T, err)
		if len(messageArgs) == 0 {
			messageArgs = []string{"-m", "My change"}
		}
		out, err := git(nil, append([]string{"commit", "--no-gpg-sign"}, messageArgs...)...)
		require.NoError(env.T, err, out)
		linked := trailers.ParseAllCheckpointsFromFinalTrailerBlock(env.GetCommitMessage(env.GetHeadHash()), trailers.DefaultCommentPrefix)
		require.Len(env.T, linked, 1, "precondition: the commit is linked")
		return linked[0].String()
	}
	runGit := func(env *TestEnv, git gitFunc, args ...string) {
		env.T.Helper()
		out, err := git(nil, args...)
		require.NoError(env.T, err, out)
	}
	// squashOf commits side.txt on a side branch with messageArgs, then
	// squash-merges it back, and returns the side commit's checkpoint.
	squashOf := func(env *TestEnv, git gitFunc, messageArgs ...string) string {
		env.T.Helper()
		runGit(env, git, "checkout", "-q", "-b", "side")
		sideID := commitLinked(env, git, "side.txt", messageArgs...)
		runGit(env, git, "checkout", "-q", "-")
		runGit(env, git, "merge", "--squash", "side")
		return sideID
	}
	// redoOf commits first.txt with messageArgs and resets it away softly, so
	// the commit under test redoes it, and returns its checkpoint.
	redoOf := func(env *TestEnv, git gitFunc, messageArgs ...string) string {
		env.T.Helper()
		firstID := commitLinked(env, git, "first.txt", messageArgs...)
		runGit(env, git, "reset", "-q", "--soft", "HEAD~1")
		return firstID
	}
	// amended commits first.txt with messageArgs for the commit under test to
	// amend; it inherits nothing.
	amended := func(messageArgs ...string) func(*TestEnv, gitFunc) ([]string, []string) {
		return func(env *TestEnv, git gitFunc) ([]string, []string) {
			commitLinked(env, git, "first.txt", messageArgs...)
			return nil, nil
		}
	}
	bodyMention := []string{"-m", "Side work", "-m", "Entire-Checkpoint: " + advForeignCheckpoint, "-m", "More body."}

	tests := []struct {
		name   string
		config map[string]string
		// setup prepares what the commit under test amends, squashes or
		// redoes, and returns the checkpoints the commit must also link and
		// the ones it must not.
		setup  func(env *TestEnv, git gitFunc) (inherits, unlinked []string)
		commit []string
		// editor is the GIT_EDITOR for the commit, when git opens one.
		editor string
		// starts is how the recorded message begins: the user's own lines.
		starts string
		// startsBefore251 is starts on git before 2.51, where it differs:
		// those gits count a template's trailing "#" paragraph when picking
		// auto's character, so they pick ";" and keep the paragraph.
		startsBefore251 string
		// prefix is what git's trailer grammar skips in the recorded message.
		prefix string
		aborts bool
		// links are checkpoints the user's own message names: the commit
		// mentions them, and post-commit must not condense into them.
		links []string
		// recordsExplanation marks a commit whose cleanup mode only git
		// knows, so Entire's explanation can be recorded with it; the prompt
		// never is.
		recordsExplanation bool
	}{
		{name: "-m", commit: []string{"-m", "Fix login"}, starts: "Fix login\n"},
		{name: "-m subject starting with #", commit: []string{"-m", "#42 fix login"}, starts: "#42 fix login\n"},
		{name: "-m ending in a # paragraph", commit: []string{"-m", "Fix login\n\n#42 is the issue"}, starts: "Fix login\n\n#42 is the issue\n"},
		{name: "-m ending in Fixes: #123", commit: []string{"-m", "Fix login\n\nFixes:\n#123"}, starts: "Fix login\n\nFixes:\n#123\n"},
		{name: "-F", commit: []string{"-F", message}, starts: "Fix login\n\nBody.\n"},
		{
			name:   "-F of a final trailer block naming a foreign checkpoint",
			commit: []string{"-F", foreignTrailer},
			starts: "Fix login\n\nEntire-Checkpoint: " + advForeignCheckpoint + "\n",
			links:  []string{advForeignCheckpoint},
		},
		{
			// git picks ";" since a line starts with "#", so "#42" stays, and
			// its trailer grammar skips it: the message ends with a trailer
			// block naming a checkpoint prepare did not stamp, so prepare
			// stamps its own below it.
			name:   "-F under auto of a trailer followed by a # line",
			config: map[string]string{"core.commentChar": "auto"},
			commit: []string{"-F", ownTrailer},
			starts: "Fix login\n\nEntire-Checkpoint: " + advForeignCheckpoint + "\n#42 is the issue\n",
			links:  []string{advForeignCheckpoint},
		},
		{name: "-m -e -v under auto", config: map[string]string{"core.commentChar": "auto"}, commit: []string{"-m", "Fix login", "-e", "-v"}, editor: keep, starts: "Fix login\n"},
		{name: "editor", editor: typeSubject, starts: "Typed\n"},
		{name: "editor with ; comments", config: map[string]string{"core.commentChar": ";"}, editor: typeSubject, starts: "Typed\n", prefix: ";"},
		{name: "editor without git's comment block", config: map[string]string{"commit.status": "false"}, editor: typeSubject, starts: "Typed\n"},
		{name: "editor under core.commentChar=auto", config: map[string]string{"core.commentChar": "auto"}, editor: typeSubject, starts: "Typed\n"},
		{
			// An empty commit.status is false to git: it writes no comment
			// block, and picks "@" since lines start with "#" and ";". Read as
			// true, Entire takes the ";" lines for git's block and writes its
			// explanation with ";", which git keeps.
			name:   "template under auto with an empty commit.status",
			config: map[string]string{"core.commentChar": "auto", "commit.status": ""},
			commit: []string{"-t", templateHashAndSemicolon},
			editor: typeSubject,
			starts: "Typed #42 Subject\n\nBody\n; see the notes\n",
		},
		{name: "editor with whitespace cleanup", config: map[string]string{"commit.cleanup": "whitespace"}, editor: typeSubject, starts: "Typed\n"},
		{name: "editor with scissors cleanup", config: map[string]string{"commit.cleanup": "scissors"}, editor: typeSubject, starts: "Typed\n"},
		{name: "editor with verbatim cleanup", config: map[string]string{"commit.cleanup": "verbatim"}, editor: typeSubject, starts: "Typed \n"},
		// The hooks cannot see --cleanup on the command line: prepare writes
		// the explanation for git's default cleanup, and whitespace keeps it.
		{name: "editor with --cleanup=whitespace", commit: []string{"--cleanup=whitespace"}, editor: typeSubject, starts: "Typed\n", recordsExplanation: true},
		{name: "editor left untouched aborts", editor: keep, aborts: true},
		{name: "amend in the editor", setup: amended(), commit: []string{"--amend"}, editor: keep, starts: "My change\n"},
		{name: "--amend --no-edit", setup: amended(), commit: []string{"--amend", "--no-edit"}, starts: "My change\n"},
		{name: "--amend --no-edit of a # subject", setup: amended("-m", "#42 fix login"), commit: []string{"--amend", "--no-edit"}, starts: "#42 fix login\n"},
		{
			// git picks ";" for the amend, since a line starts with "#", but its
			// trailer grammar, and post-commit's, read "#" lines as comments.
			name:   "amend under auto of a trailer followed by a # paragraph",
			config: map[string]string{"core.commentChar": "auto"},
			setup: func(env *TestEnv, git gitFunc) ([]string, []string) {
				linked := commitLinked(env, git, "first.txt")
				runGit(env, git, "commit", "--no-gpg-sign", "--amend", "-m", "My change\n\nEntire-Checkpoint: "+linked+"\n\n#42 is the issue")
				return nil, nil
			},
			commit: []string{"--amend"},
			editor: keep,
			starts: "My change\n",
		},
		{
			// git 2.51 and later skip the template's trailing "#" paragraph
			// when picking auto's character, so they pick "#" and strip the
			// paragraph.
			name:            "template under auto",
			config:          map[string]string{"core.commentChar": "auto"},
			commit:          []string{"-t", template},
			editor:          typeSubject,
			starts:          "Typed Subject\n",
			startsBefore251: "Typed Subject\n\n#42 is the issue\n",
		},
		{
			name:            "template without a final newline under auto",
			config:          map[string]string{"core.commentChar": "auto"},
			commit:          []string{"-t", templateNoNewline},
			editor:          typeSubject,
			starts:          "Typed Subject\n",
			startsBefore251: "Typed Subject\n\n#42 is the issue\n",
		},
		{
			// git picks ";" since the subject starts with "#", and keeps its
			// ";" lines, which git's trailer grammar reads as "#" does not
			// skip: an explanation written with ";" below the trailer would
			// leave the commit unlinked.
			name:   "template with a # subject under auto with whitespace cleanup",
			config: map[string]string{"core.commentChar": "auto", "commit.cleanup": "whitespace"},
			commit: []string{"-t", hashSubject},
			editor: keep,
			starts: "#42 Subject\n\nBody\n",
		},
		{
			name:   "template with a # subject under auto with scissors cleanup",
			config: map[string]string{"core.commentChar": "auto", "commit.cleanup": "scissors"},
			commit: []string{"-t", hashSubject},
			editor: keep,
			starts: "#42 Subject\n\nBody\n",
		},
		{name: "merge --squash then editor", setup: func(env *TestEnv, git gitFunc) ([]string, []string) {
			return []string{squashOf(env, git)}, nil
		}, editor: keep},
		{name: "merge --squash then -m", setup: func(env *TestEnv, git gitFunc) ([]string, []string) {
			return []string{squashOf(env, git)}, nil
		}, commit: []string{"-m", "Squash side"}, starts: "Squash side\n"},
		{name: "merge --squash of a commit whose body names a checkpoint", setup: func(env *TestEnv, git gitFunc) ([]string, []string) {
			return []string{squashOf(env, git, bodyMention...)}, []string{advForeignCheckpoint}
		}, editor: keep},
		{name: "merge --squash of a commit whose body names a commit", setup: func(env *TestEnv, git gitFunc) ([]string, []string) {
			namedID := commitLinked(env, git, "named.txt")
			sideID := squashOf(env, git, "-m", "Side work", "-m", "commit "+env.GetHeadHash())
			return []string{sideID}, []string{namedID}
		}, editor: keep},
		{name: "redo after reset --soft then editor", setup: func(env *TestEnv, git gitFunc) ([]string, []string) {
			return []string{redoOf(env, git)}, nil
		}, editor: typeSubject, starts: "Typed\n"},
		{name: "redo of a commit whose body names a checkpoint", setup: func(env *TestEnv, git gitFunc) ([]string, []string) {
			return []string{redoOf(env, git, bodyMention...)}, []string{advForeignCheckpoint}
		}, editor: typeSubject, starts: "Typed\n"},
	}
	skipsTrailingComments := gitAutoSkipsTrailingComments(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			starts := tt.starts
			if tt.startsBefore251 != "" && !skipsTrailingComments {
				starts = tt.startsBefore251
			}
			env := NewFeatureBranchEnv(t)
			git := gitWithEntireCommitHooksEnv(t, env)
			for key, value := range tt.config {
				setRepoGitConfig(env, key, value)
			}
			var inherits, unlinked []string
			if tt.setup != nil {
				inherits, unlinked = tt.setup(env, git)
			}
			authorityAgentTurn(env, "mine.txt", "my agent work\n")
			_, err := git(nil, "add", "mine.txt")
			require.NoError(t, err)
			before := env.GetHeadHash()
			editor := tt.editor
			if editor == "" {
				editor = keep // git opens no editor for these; it must not be the developer's
			}

			out, err := git([]string{editor}, append([]string{"commit", "--no-gpg-sign"}, tt.commit...)...)

			if tt.aborts {
				require.Error(t, err, "git must abort the commit: %s", out)
				require.Equal(t, before, env.GetHeadHash(), "no commit is recorded")
				return
			}
			require.NoError(t, err, out)
			recorded := env.GetCommitMessage(env.GetHeadHash())
			require.True(t, strings.HasPrefix(recorded, starts), "the user's lines stay first and in order: %q", recorded)
			prefix := tt.prefix
			if prefix == "" {
				prefix = trailers.DefaultCommentPrefix
			}
			final := trailers.ParseAllCheckpointsFromFinalTrailerBlock(recorded, prefix)
			for _, cpID := range inherits {
				require.Contains(t, final, id.MustCheckpointID(cpID), "the commit links the checkpoint it inherits: %q", recorded)
			}
			for _, cpID := range unlinked {
				require.NotContains(t, final, id.MustCheckpointID(cpID), "the commit does not link a checkpoint a squashed or redone message only names: %q", recorded)
			}
			notOwn := func(cpID id.CheckpointID) bool {
				return slices.Contains(inherits, cpID.String()) || slices.Contains(tt.links, cpID.String())
			}
			own := slices.DeleteFunc(slices.Clone(final), notOwn)
			require.Len(t, own, 1, "the final trailer block carries one trailer of its own: %q", recorded)
			if len(inherits) == 0 && len(unlinked) == 0 {
				require.Equal(t, own, slices.DeleteFunc(trailers.ParseAllCheckpoints(recorded), notOwn),
					"the commit names no other checkpoint: %q", recorded)
			}
			require.True(t, advCheckpointStored(env, own[0].String()), "post-commit condenses into it: %q", recorded)
			for _, link := range tt.links {
				require.False(t, advCheckpointStored(env, link), "post-commit does not condense into a checkpoint the user's message names: %q", recorded)
			}
			require.False(t, advCheckpointStored(env, advForeignCheckpoint), "no checkpoint the message only names is created: %q", recorded)
			if !tt.recordsExplanation {
				require.NotContains(t, recorded, "Remove the Entire-Checkpoint trailer", "Entire's explanation never reaches the commit: %q", recorded)
			}
			require.NotContains(t, recorded, "change mine.txt", "the prompt never reaches the commit: %q", recorded)
		})
	}
}

// gitAutoSkipsTrailingComments reports whether the installed git picks
// core.commentChar=auto's character the way git 2.51 and later do: without
// the empty and "#" lines that end the message.
func gitAutoSkipsTrailingComments(t *testing.T) bool {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "git", "--version").Output()
	require.NoError(t, err)
	var major, minor int
	_, err = fmt.Sscanf(string(out), "git version %d.%d", &major, &minor)
	require.NoError(t, err, "git --version: %q", out)
	return major > 2 || major == 2 && minor >= 51
}
