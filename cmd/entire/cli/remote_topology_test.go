package cli

import (
	"strings"
	"testing"
)

// describeCheckpointDestination writes to an io.Writer from a plain struct, so
// the disabled-pushing caveat needs no repo, no remotes and no CLI run.
//
// What it must not do is present the URLs it lists as the read source: they
// are PUSH urls, and a fan-out remote's reads use its fetch url. It points at
// `entire status` for that instead, and does not promise status will always
// have an answer.
func TestDescribeCheckpointDestination_PushDisabledCaveat(t *testing.T) {
	t.Parallel()
	topology := remoteTopology{
		destinations: []remoteDestination{
			{name: "backup", pushURLs: []string{"https://github.com/org/backup.git"}},
			{name: "origin", pushURLs: []string{"https://github.com/org/repo.git"}},
		},
	}
	for _, tc := range []struct {
		name           string
		pushDisabled   bool
		want, unwanted []string
	}{
		{
			name:     "pushing enabled",
			want:     []string{"2 remotes", "single elected remote"},
			unwanted: []string{"push_sessions=false"},
		},
		{
			name:         "pushing disabled",
			pushDisabled: true,
			want: []string{
				// The note still renders its body: the ambiguity is what
				// re-enabling pushing would run into.
				"single elected remote",
				"Automatic checkpoint pushing is disabled (push_sessions=false)",
				"they would go if you re-enabled it",
			},
			// "waiting for it" implied a pending push that cannot happen.
			unwanted: []string{"checkpoints are waiting for it"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			topology := topology
			topology.pushDisabled = tc.pushDisabled
			var b strings.Builder
			topology.describeCheckpointDestination(&b, "Checkpoint destination: REVIEW")
			for _, want := range tc.want {
				if !strings.Contains(b.String(), want) {
					t.Errorf("missing %q:\n%s", want, b.String())
				}
			}
			for _, unwanted := range tc.unwanted {
				if strings.Contains(b.String(), unwanted) {
					t.Errorf("must not mention %q:\n%s", unwanted, b.String())
				}
			}
		})
	}
}
