package review

import (
	"reflect"
	"testing"
)

func TestWrapDisplayWidth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		in    string
		width int
		want  []string
	}{
		{
			name:  "empty input returns nil",
			in:    "",
			width: 80,
			want:  nil,
		},
		{
			name:  "zero width returns nil",
			in:    "anything",
			width: 0,
			want:  nil,
		},
		{
			name:  "negative width returns nil",
			in:    "anything",
			width: -10,
			want:  nil,
		},
		{
			name:  "short single line fits",
			in:    "hello",
			width: 80,
			want:  []string{"hello"},
		},
		{
			name:  "long line wraps to width",
			in:    "aaaa bbbb cccc",
			width: 5,
			want:  []string{"aaaa", "bbbb", "cccc"},
		},
		{
			name:  "embedded newline preserved as paragraph break",
			in:    "a\n\nb",
			width: 80,
			want:  []string{"a", "", "b"},
		},
		{
			name:  "trailing newline does not produce phantom blank line",
			in:    "text\n",
			width: 80,
			want:  []string{"text"},
		},
		{
			name:  "multiple trailing newlines collapsed",
			in:    "text\n\n\n",
			width: 80,
			want:  []string{"text"},
		},
		{
			name:  "ANSI escape stripped from output",
			in:    "\x1b[31mred\x1b[0m text",
			width: 80,
			want:  []string{"red text"},
		},
		{
			name:  "control chars stripped",
			in:    "a\x07b",
			width: 80,
			want:  []string{"ab"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := wrapDisplayWidth(tt.in, tt.width)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("wrapDisplayWidth(%q, %d) = %#v, want %#v", tt.in, tt.width, got, tt.want)
			}
		})
	}
}
