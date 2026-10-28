package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// TestResolveNativeRepoByPath_ConsistentBody pins the wire shape: the body
// carries `consistent: true` only under --consistent, and omits the key
// otherwise so the default request stays byte-identical to before.
func TestResolveNativeRepoByPath_ConsistentBody(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want any
	}{
		{name: "default omits the field", ctx: context.Background(), want: nil},
		{name: "--consistent sends true", ctx: withConsistentReads(context.Background()), want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var body map[string]any
			c, calls := resolveTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/repos/resolve") {
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusForbidden)
					return
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode resolve body: %v", err)
				}
				if err := printJSON(w, nativeResolution("widgets/web", ulidRepoWeb)); err != nil {
					t.Errorf("encode resolution: %v", err)
				}
			})
			got, err := resolveNativeRepoByPath(tc.ctx, c, "widgets", "web")
			require.NoError(t, err)
			require.Equal(t, ulidRepoWeb, got.ID)
			require.EqualValues(t, 1, calls.Load())
			consistent, present := body["consistent"]
			if tc.want == nil {
				require.False(t, present, "body = %v, want no consistent key", body)
				return
			}
			require.Equal(t, tc.want, consistent)
		})
	}
}

// TestRootConsistentFlag_ReachesCommandContext pins the root wiring: the
// persistent flag reaches a leaf's context, which every resolve takes.
func TestRootConsistentFlag_ReachesCommandContext(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{args: []string{"probe"}, want: false},
		{args: []string{"probe", "--consistent"}, want: true},
		{args: []string{"--consistent", "probe"}, want: true},
		{args: []string{"probe", "--consistent=false"}, want: false},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Parallel()
			var got bool
			root := NewRootCmd()
			// Hidden skips the telemetry post-run.
			root.AddCommand(exemptFromEntireDirCheck(&cobra.Command{
				Use:    "probe",
				Hidden: true,
				RunE: func(cmd *cobra.Command, _ []string) error {
					got = consistentReads(cmd.Context())
					return nil
				},
			}))
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			root.SetArgs(tc.args)
			require.NoError(t, root.ExecuteContext(context.Background()))
			require.Equal(t, tc.want, got)
		})
	}
}
