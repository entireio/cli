package agent_test

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestLooksLikeUnrecognizedFlag(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		stderr   string
		keywords []string
		want     bool
	}{
		{"unknown_flag_with_matching_keyword", "error: unknown flag: --stream-json", []string{"stream-json"}, true},
		{"unrecognized_option_with_matching_keyword", "unrecognized option '--json'", []string{"json"}, true},
		{"invalid_option_with_matching_keyword", "invalid option: --output-format", []string{"output-format"}, true},
		{"unknown_option_with_matching_keyword", "unknown option: --stream", []string{"stream"}, true},

		{"case_insensitive_stderr", "ERROR: UNKNOWN FLAG: --JSON", []string{"json"}, true},
		{"case_insensitive_keyword", "unknown flag: --json", []string{"JSON"}, true},

		{"rejection_present_but_no_matching_keyword", "unknown flag: --foo", []string{"json", "stream"}, false},
		{"matching_keyword_but_no_rejection", "json: parse error", []string{"json"}, false},

		{"empty_stderr", "", []string{"json"}, false},
		{"empty_keywords", "unknown flag: --json", nil, false},

		{"weak_rejection_words_alone_dont_match", "command unknown", []string{"unknown"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := agent.LooksLikeUnrecognizedFlag(tc.stderr, tc.keywords...)
			if got != tc.want {
				t.Errorf("LooksLikeUnrecognizedFlag(%q, %v) = %v, want %v", tc.stderr, tc.keywords, got, tc.want)
			}
		})
	}
}
