//go:build integration

package integration

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// TestCheckpointRefsDiscovery_CrossMachineReadCandidates: git-refs only — a
// fresh clone discovers and hydrates refs-native checkpoints from both the
// elected sync remote and the legacy origin tier. This closes the default-
// setup gap scoped out in #1771 without letting discovery advertise a
// checkpoint that the read path cannot fetch.
func TestCheckpointRefsDiscovery_CrossMachineReadCandidates(t *testing.T) {
	t.Parallel()

	const (
		originRemote   = "origin"
		upstreamRemote = "upstream"
	)

	for _, checkpointRemote := range []string{upstreamRemote, originRemote} {
		t.Run(checkpointRemote, func(t *testing.T) {
			t.Parallel()

			env := NewFeatureBranchEnv(t)
			env.CheckpointStore = StoreGitRefs

			bareOrigin := env.SetupBareRemote()
			bareUpstream := env.SetupNamedBareRemote(upstreamRemote)
			if checkpointRemote == upstreamRemote {
				env.PatchSettings(map[string]any{
					"strategy_options": map[string]any{
						"checkpoint_push_remote": upstreamRemote,
					},
				})
			}

			checkpointID := createCheckpointedCommit(t, env, "Add machine module", "machine.go", "package machine", "Add machine module")
			env.GitPush(originRemote, "HEAD")
			env.RunPrePush(checkpointRemote)

			bareByRemote := map[string]string{originRemote: bareOrigin, upstreamRemote: bareUpstream}
			if !env.CheckpointExistsOnRemote(bareByRemote[checkpointRemote], checkpointID) {
				t.Fatalf("checkpoint %s should be on %s", checkpointID, checkpointRemote)
			}
			otherRemote := originRemote
			if checkpointRemote == originRemote {
				otherRemote = upstreamRemote
			}
			if env.CheckpointExistsOnRemote(bareByRemote[otherRemote], checkpointID) {
				t.Fatalf("fixture: checkpoint %s must exist only on %s", checkpointID, checkpointRemote)
			}

			// Machine B always elects upstream. The origin case therefore proves
			// that a miss on the elected remote advances to the legacy tier.
			cloneEnv := env.CloneFrom(bareOrigin)
			testutil.AddRemote(t, cloneEnv.RepoDir, upstreamRemote, bareUpstream)
			cloneEnv.setGitConfigBaseline()
			cloneEnv.PatchSettings(map[string]any{
				"strategy_options": map[string]any{
					"checkpoint_push_remote": upstreamRemote,
				},
			})
			if cloneEnv.CheckpointsPresentLocally() {
				t.Fatal("fixture: the clone must start with zero local checkpoint state")
			}

			listOutput := cloneEnv.RunCLI("checkpoint", "list")
			if !strings.Contains(listOutput, "Add machine module") && !strings.Contains(listOutput, checkpointID[:8]) {
				t.Errorf("cross-machine discovery should surface the remote checkpoint, got:\n%s", listOutput)
			}

			output := cloneEnv.RunCLI("checkpoint", "explain", "--checkpoint", checkpointID)
			if !strings.Contains(output, "Add machine module") {
				t.Errorf("the discovered checkpoint should hydrate on read, got:\n%s", output)
			}
		})
	}
}
