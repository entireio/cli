package redact

import (
	"strings"
	"testing"
	"time"
)

// nestedDefault returns n ${A:-…} defaults nested around inner.
func nestedDefault(n int, inner string) string {
	return strings.Repeat("${A:-", n) + inner + strings.Repeat("}", n)
}

// The placeholder rule reads a ${NAME:-word} default level by level. A value
// with more than maxPlaceholderExpansions expansions is not read as a
// placeholder, so it redacts.
func TestString_NestedPlaceholderDefaults(t *testing.T) {
	t.Parallel()
	threeLevels := "DB_PASSWORD=${DB_PASSWORD:-${PGPASSWORD:-$FALLBACK}}"
	eight := "DB_PASSWORD=" + nestedDefault(maxPlaceholderExpansions, "$FALLBACK")
	assertStringRedactionCases(t, []stringRedactionCase{
		{name: "nested references stay a placeholder", input: threeLevels, want: threeLevels},
		{name: "the most expansions a placeholder can hold", input: eight, want: eight},
		{
			name:  "one expansion past the cap redacts",
			input: "DB_PASSWORD=" + nestedDefault(maxPlaceholderExpansions+1, "$FALLBACK"),
			want:  "DB_PASSWORD=REDACTED",
		},
		{
			name:  "a braced innermost reference counts",
			input: "API_KEY=" + nestedDefault(maxPlaceholderExpansions, "${FALLBACK}"),
			want:  "API_KEY=REDACTED",
		},
		{
			name:  "a literal innermost default still redacts",
			input: "DB_PASSWORD=" + nestedDefault(maxPlaceholderExpansions, "hunter2"),
			want:  "DB_PASSWORD=REDACTED",
		},
		{
			name:  "double-quoted value past the cap redacts",
			input: `PGPASSWORD="` + nestedDefault(maxPlaceholderExpansions+1, "$FALLBACK") + `"`,
			want:  `PGPASSWORD="REDACTED"`,
		},
	})
}

// The cap rejects a deeply nested value without reading every level, so a
// value nested many thousands deep stays fast. The timing checks call
// isPlaceholderSecretValue directly; one String call pins the pipeline result.
func TestString_NestedPlaceholderDefaultsStayLinear(t *testing.T) {
	t.Parallel()
	const depth = 20000
	for _, shape := range []struct{ key, open string }{
		{"DB_PASSWORD=", "${a:-"},
		{"DB_PASSWORD=", "${a:-'"},
		{"DB_PASSWORD=", `${a:-"`},
		{"PGPASSWORD=", "${a:-"},
		{"API_KEY=", "${a:-"},
	} {
		value := strings.Repeat(shape.open, depth) + "hunter2" + strings.Repeat("}", depth)
		done := make(chan bool, 1)
		go func() { done <- isPlaceholderSecretValue(value) }()
		select {
		case got := <-done:
			if got {
				t.Fatalf("value nested %d deep in %q%q still read as a placeholder", depth, shape.key, shape.open)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("isPlaceholderSecretValue took over 30s on %q%q nested %d deep", shape.key, shape.open, depth)
		}
	}
	in := "DB_PASSWORD=" + strings.Repeat("${a:-", depth) + "hunter2" + strings.Repeat("}", depth)
	if out := String(in); strings.Contains(out, "hunter2") {
		t.Fatalf("String left the nested literal visible: %q", out)
	}
}
