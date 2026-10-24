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
// git's own. Every commit must end with exactly the trailer Entire left, keep
// the user's lines in place, and abort when the user saves it untouched.
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
	template := filepath.Join(files, "template")
	require.NoError(t, os.WriteFile(template, []byte("Subject\n\n#42 is the issue\n"), 0o600))
	// Without a final newline git writes its comment block right below the
	// last line, with no blank line above it.
	templateNoNewline := filepath.Join(files, "template-no-newline")
	require.NoError(t, os.WriteFile(templateNoNewline, []byte("Subject\n\n#42 is the issue"), 0o600))
	templateHashAndSemicolon := filepath.Join(files, "template-hash-and-semicolon")
	require.NoError(t, os.WriteFile(templateHashAndSemicolon, []byte("#42 Subject\n\nBody\n; see the notes\n"), 0o600))
	ownTrailer := filepath.Join(files, "own-trailer")
	require.NoError(t, os.WriteFile(ownTrailer, []byte("Fix login\n\nEntire-Checkpoint: "+advForeignCheckpoint+"\n#42 is the issue\n"), 0o600))

	// commitLinked commits path with -m as an agent's work, and returns the
	// trailer post-commit condensed into.
	commitLinked := func(env *TestEnv, git func([]string, ...string) (string, error), path, message string) string {
		env.T.Helper()
		authorityAgentTurn(env, path, "work on "+path+"\n")
		_, err := git(nil, "add", path)
		require.NoError(env.T, err)
		out, err := git(nil, "commit", "--no-gpg-sign", "-m", message)
		require.NoError(env.T, err, out)
		ids := trailers.ParseAllCheckpoints(env.GetCommitMessage(env.GetHeadHash()))
		require.Len(env.T, ids, 1, "precondition: the commit is linked")
		return ids[0].String()
	}

	tests := []struct {
		name   string
		config map[string]string
		// setup makes the commits the one under test amends.
		setup  func(env *TestEnv, git func([]string, ...string) (string, error))
		commit []string
		// editor is the GIT_EDITOR for the commit, when git opens one.
		editor string
		// starts is how the recorded message begins: the user's own lines.
		starts string
		// prefix is what git's trailer grammar skips in the recorded message.
		prefix string
		// startsBefore251 is starts on git before 2.51, where it differs:
		// those gits count a template's trailing "#" paragraph when picking
		// auto's character, so they pick ";" and keep the paragraph.
		startsBefore251 string
		aborts          bool
		// links are checkpoints the user's own message names: the commit
		// mentions them, and post-commit must not condense into them.
		links []string
	}{
		{name: "-m subject starting with #", commit: []string{"-m", "#42 fix login"}, starts: "#42 fix login\n"},
		{name: "-m ending in a # paragraph", commit: []string{"-m", "Fix login\n\n#42 is the issue"}, starts: "Fix login\n\n#42 is the issue\n"},
		{name: "-m ending in Fixes: #123", commit: []string{"-m", "Fix login\n\nFixes:\n#123"}, starts: "Fix login\n\nFixes:\n#123\n"},
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
		{name: "editor", commit: nil, editor: typeSubject, starts: "Typed\n"},
		{name: "editor with ; comments", config: map[string]string{"core.commentChar": ";"}, editor: typeSubject, starts: "Typed\n", prefix: ";"},
		{name: "editor without git's comment block", config: map[string]string{"commit.status": "false"}, editor: typeSubject, starts: "Typed\n"},
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
		// These cleanup modes keep comment lines, so Entire's explanation and
		// the prompt it quotes would be recorded with the commit.
		{name: "editor with whitespace cleanup", config: map[string]string{"commit.cleanup": "whitespace"}, editor: typeSubject, starts: "Typed\n"},
		{name: "editor with scissors cleanup", config: map[string]string{"commit.cleanup": "scissors"}, editor: typeSubject, starts: "Typed\n"},
		{name: "editor with verbatim cleanup", config: map[string]string{"commit.cleanup": "verbatim"}, editor: typeSubject, starts: "Typed \n"},
		{name: "editor left untouched aborts", editor: keep, aborts: true},
		{
			name: "amend in the editor",
			setup: func(env *TestEnv, git func([]string, ...string) (string, error)) {
				commitLinked(env, git, "first.txt", "My change")
			},
			commit: []string{"--amend"},
			editor: keep,
			starts: "My change\n",
		},
		{
			name: "--amend --no-edit of a # subject",
			setup: func(env *TestEnv, git func([]string, ...string) (string, error)) {
				commitLinked(env, git, "first.txt", "#42 fix login")
			},
			commit: []string{"--amend", "--no-edit"},
			starts: "#42 fix login\n",
		},
		{
			// git picks ";" for the amend, since a line starts with "#", but its
			// trailer grammar, and post-commit's, read "#" lines as comments.
			name:   "amend under auto of a trailer followed by a # paragraph",
			config: map[string]string{"core.commentChar": "auto"},
			setup: func(env *TestEnv, git func([]string, ...string) (string, error)) {
				linked := commitLinked(env, git, "first.txt", "My change")
				out, err := git(nil, "commit", "--no-gpg-sign", "--amend", "-m", "My change\n\nEntire-Checkpoint: "+linked+"\n\n#42 is the issue")
				require.NoError(env.T, err, out)
			},
			commit: []string{"--amend"},
			editor: keep,
			starts: "My change\n",
		},
		{
			// git picks ";" since the template has a "#" line, and keeps its
			// comment block in the commit.
			name:   "template under auto with whitespace cleanup",
			config: map[string]string{"core.commentChar": "auto", "commit.cleanup": "whitespace"},
			commit: []string{"-t", template},
			editor: typeSubject,
			starts: "Typed Subject\n\n#42 is the issue\n",
		},
		{
			// git 2.51 and later skip the template's trailing "#" paragraph
			// when picking auto's character, so they pick "#" and strip the
			// paragraph. Entire must read "#" too, or its explanation,
			// written with ";", is recorded with the prompt.
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
			// git's block starts at its scissors line and is cut away.
			name:   "template under auto with scissors cleanup",
			config: map[string]string{"core.commentChar": "auto", "commit.cleanup": "scissors"},
			commit: []string{"-t", template},
			editor: typeSubject,
			starts: "Typed Subject\n\n#42 is the issue\n",
		},
		{
			name:   "template under auto with whitespace cleanup and -v",
			config: map[string]string{"core.commentChar": "auto", "commit.cleanup": "whitespace"},
			commit: []string{"-t", template, "-v"},
			editor: typeSubject,
			starts: "Typed Subject\n\n#42 is the issue\n",
		},
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
			if tt.setup != nil {
				tt.setup(env, git)
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
			mentioned := slices.DeleteFunc(trailers.ParseAllCheckpoints(recorded), func(cpID id.CheckpointID) bool {
				return slices.Contains(tt.links, cpID.String())
			})
			require.Len(t, mentioned, 1, "the commit carries one trailer of its own: %q", recorded)
			for _, link := range tt.links {
				require.False(t, advCheckpointStored(env, link), "post-commit does not condense into a checkpoint the user's message names: %q", recorded)
			}
			prefix := tt.prefix
			if prefix == "" {
				prefix = trailers.DefaultCommentPrefix
			}
			require.Equal(t, mentioned, trailers.ParseAllCheckpointsFromFinalTrailerBlock(recorded, prefix),
				"the trailer is the commit's final trailer block: %q", recorded)
			require.True(t, advCheckpointStored(env, mentioned[0].String()), "post-commit condenses into it: %q", recorded)
			require.NotContains(t, recorded, "Remove the Entire-Checkpoint trailer", "Entire's explanation never reaches the commit: %q", recorded)
			require.NotContains(t, recorded, "Last Prompt:", "the prompt never reaches the commit: %q", recorded)
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
