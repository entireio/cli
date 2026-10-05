package main

import "testing"

func TestGitPromptAction(t *testing.T) {
	t.Parallel()
	cases := map[string]struct{ action, host, want string }{
		"push": {"push", "us.entire.io", "git push to us.entire.io"},
		"pull": {"pull", "us.entire.io", "git fetch from us.entire.io"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := gitPromptAction(tc.action, tc.host); got != tc.want {
				t.Fatalf("gitPromptAction(%q,%q) = %q, want %q", tc.action, tc.host, got, tc.want)
			}
		})
	}
}
