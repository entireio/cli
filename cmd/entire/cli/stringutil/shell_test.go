package stringutil

import (
	"strings"
	"testing"
)

func TestSanitizeShellCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "plain command is unchanged",
			input: "go test ./... -run TestX",
			want:  "go test ./... -run TestX",
		},
		{
			name:  "empty command",
			input: "",
			want:  "",
		},
		{
			name:  "single-quoted span becomes one NUL",
			input: "echo 'a; b | c' d",
			want:  "echo \x00 d",
		},
		{
			name:  "double-quoted span becomes one NUL",
			input: `git commit -m "wip; entire search notes"`,
			want:  "git commit -m \x00",
		},
		{
			name:  "escaped double quote inside double quotes does not close the span",
			input: `echo "a\"b; c" d`,
			want:  "echo \x00 d",
		},
		{
			name:  "single quotes inside double quotes are text",
			input: `echo "it's; here" d`,
			want:  "echo \x00 d",
		},
		{
			name:  "unterminated single quote blanks to end",
			input: "echo 'a; b",
			want:  "echo \x00",
		},
		{
			name:  "unterminated double quote blanks to end",
			input: `echo "a; b`,
			want:  "echo \x00",
		},
		{
			name:  "backslash escape outside quotes is copied as a pair",
			input: `echo \; next`,
			want:  `echo \; next`,
		},
		{
			name:  "escaped quote outside quotes does not open a span",
			input: `echo \" a; b`,
			want:  `echo \" a; b`,
		},
		{
			name:  "trailing backslash is copied",
			input: `echo \`,
			want:  `echo \`,
		},
		{
			name:  "newline inside double quotes is text",
			input: "echo \"a\nb\" c",
			want:  "echo \x00 c",
		},
		{
			name:  "heredoc body is blanked and the command after it survives",
			input: "cat > notes.md <<DOC\nsome text\nmore; text | here\nDOC\necho done",
			want:  "cat > notes.md <<DOC\n\x00\necho done",
		},
		{
			name:  "quoted heredoc delimiter is copied through, body blanked",
			input: "cat <<'EOF'\nbody line\nEOF",
			want:  "cat <<'EOF'\n\x00\n",
		},
		{
			name:  "double-quoted heredoc delimiter",
			input: "cat <<\"EOF\"\nbody\nEOF\n",
			want:  "cat <<\"EOF\"\n\x00\n",
		},
		{
			name:  "dash heredoc tolerates tab-indented delimiter",
			input: "cat <<-EOF\n\tbody\n\tEOF\necho after",
			want:  "cat <<-EOF\n\x00\necho after",
		},
		{
			name:  "space between << and delimiter",
			input: "cat << EOF\nbody\nEOF\n",
			want:  "cat << EOF\n\x00\n",
		},
		{
			name:  "delimiter may contain digits after the first letter",
			input: "cat <<EOF2\nbody\nEOF2\nnext",
			want:  "cat <<EOF2\n\x00\nnext",
		},
		{
			name:  "unterminated heredoc blanks to end",
			input: "cat <<EOF\nbody\nstill body",
			want:  "cat <<EOF\n\x00\n",
		},
		{
			name:  "two heredocs on one line are blanked in order",
			input: "cat <<A <<B\na\nA\nb\nB\n",
			want:  "cat <<A <<B\n\x00\n\x00\n",
		},
		{
			name:  "arithmetic shift is not a heredoc",
			input: "echo $((1<<2))\nnext",
			want:  "echo $((1<<2))\nnext",
		},
		{
			name:  "here-string is not a heredoc",
			input: "cat <<< word\nnext",
			want:  "cat <<< word\nnext",
		},
		{
			name:  "input redirect is not a heredoc",
			input: "sort < in.txt\nnext",
			want:  "sort < in.txt\nnext",
		},
		{
			name:  "heredoc without a delimiter word is not a heredoc",
			input: "echo <<\nnext",
			want:  "echo <<\nnext",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := SanitizeShellCommand(tt.input)
			if got != tt.want {
				t.Errorf("SanitizeShellCommand(%q) = %q, want %q", tt.input, got, tt.want)
			}
			// The documented contract: strip the NULs and every remaining byte
			// appears in the input, in order.
			if !isSubsequence(strings.ReplaceAll(got, "\x00", ""), tt.input) {
				t.Errorf("SanitizeShellCommand(%q) = %q is not a NUL-padded subsequence of its input", tt.input, got)
			}
		})
	}
}

// isSubsequence reports whether every byte of sub appears in s in order.
func isSubsequence(sub, s string) bool {
	j := 0
	for i := 0; i < len(s) && j < len(sub); i++ {
		if s[i] == sub[j] {
			j++
		}
	}
	return j == len(sub)
}
