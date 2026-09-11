package testutil

import "testing"

// The shim's inputs are interpolated into a bash script that is made
// executable and placed first on PATH, so anything carrying a shell
// metacharacter must be refused rather than written out. A `case` pattern ends
// at the first unquoted `)` and a command at the first `;`, so these inputs are
// command execution, not cosmetics.
func TestValidateShimInputs_RefusesShellMetacharacters(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		subcommands []string
		rewrites    map[string]string
	}{
		{"pattern break in key", []string{"fetch"}, map[string]string{"origin) ;true;;#": "V"}},
		{"command substitution in key", []string{"fetch"}, map[string]string{"$(id)": "V"}},
		{"backtick in key", []string{"fetch"}, map[string]string{"a`id`b": "V"}},
		{"space in key", []string{"fetch"}, map[string]string{"a b": "V"}},
		{"newline in key", []string{"fetch"}, map[string]string{"a\nrm -rf /": "V"}},
		{"quote in key", []string{"fetch"}, map[string]string{"a\"b": "V"}},
		{"glob in key", []string{"fetch"}, map[string]string{"*": "V"}},
		{"expansion in value", []string{"fetch"}, map[string]string{"origin": "V; id"}},
		{"brace in value", []string{"fetch"}, map[string]string{"origin": "V}${IFS}"}},
		{"lowercase value", []string{"fetch"}, map[string]string{"origin": "v"}},
		{"pattern break in subcommand", []string{"fetch) ;true;;#"}, map[string]string{"origin": "V"}},
		{"substitution in subcommand", []string{"$(id)"}, map[string]string{"origin": "V"}},
		{"pipe in subcommand", []string{"fetch|id"}, map[string]string{"origin": "V"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := validateShimInputs(tc.subcommands, tc.rewrites); err == nil {
				t.Errorf("accepted shell-injectable input: subcommands=%q rewrites=%q",
					tc.subcommands, tc.rewrites)
			}
		})
	}
}

func TestValidateShimInputs_AcceptsOrdinaryRemotesAndURLs(t *testing.T) {
	t.Parallel()
	err := validateShimInputs(
		[]string{"ls-remote", "fetch", "fetch-pack", "push"},
		map[string]string{
			"fork":                                    "CHECKPOINT_TEST_FORK",
			"https://github.com/acme/app.git":         "CHECKPOINT_TEST_ORIGIN",
			"git@github.com:acme/app.git":             "CHECKPOINT_TEST_ORIGIN",
			"https://github.com/acme/checkpoints.git": "CHECKPOINT_TEST_DEDICATED",
		})
	if err != nil {
		t.Errorf("rejected ordinary remote names and URLs: %v", err)
	}
}
