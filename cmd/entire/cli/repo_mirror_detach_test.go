package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/internal/coreapi"
)

const testDetachMirrorULID = "01KS6KFJR2XS6PZ188MVYE07AM"

// fakeDetachCore stands in for the three core reads a detach makes — the
// project lookup, the mirror's placements, and POST /repos/{id}/detach — and
// records each detach body so a test can tell the dry run from the real call.
type fakeDetachCore struct {
	mu         sync.Mutex
	placements []string // mirror IDs the placement lookup returns
	plan       string   // dry-run answer, raw JSON
	result     string   // real-detach answer, raw JSON
	bodies     []coreapi.DetachRepoBody
	detachPath []string
	states     []string // GET /detach answers in order, raw JSON; the last repeats
	stateGets  int
	stateFails bool // GET /detach answers 503
	peopleGets int
	peopleFail bool // GET /people answers 503
	realFails  int  // the real (non-dry-run) POST answers this status
}

func (f *fakeDetachCore) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects":
			assert.Equal(t, "acme", r.URL.Query().Get("name"))
			writeJSONResponse(t, w, http.StatusOK, &coreapi.ListProjectsOutputBody{Project: coreapi.NewOptProject(coreapi.Project{
				ID: testProjectULID, Name: "acme", OwnerId: "01OWNER", OwnerType: coreapi.ProjectOwnerTypeOrg, Region: "us",
			})})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/mirrors/placements"):
			assert.Equal(t, "octocat", r.URL.Query().Get("owner"))
			assert.Equal(t, "hello-world", r.URL.Query().Get("repo"))
			resolved := make([]coreapi.ResolvedPlacement, 0, len(f.placements))
			for _, id := range f.placements {
				resolved = append(resolved, coreapi.ResolvedPlacement{ClusterHost: defaultClusterHost, MirrorId: id})
			}
			writeJSONResponse(t, w, http.StatusOK, &coreapi.ResolvePlacementsOutputBody{Placements: resolved})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/detach"):
			raw, err := io.ReadAll(r.Body)
			assert.NoError(t, err)
			var body coreapi.DetachRepoBody
			assert.NoError(t, json.Unmarshal(raw, &body), "decode detach body %s", raw)
			f.mu.Lock()
			f.bodies = append(f.bodies, body)
			f.detachPath = append(f.detachPath, r.URL.Path)
			f.mu.Unlock()
			if !body.DryRun && f.realFails != 0 {
				writeCoreProblem(t, w, f.realFails, "detach refused: retry later")
				return
			}
			answer := f.result
			if body.DryRun {
				answer = f.plan
			}
			writeJSONResponse(t, w, http.StatusOK, json.RawMessage(answer))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/"+testDetachMirrorULID+"/people":
			f.mu.Lock()
			f.peopleGets++
			fails := f.peopleFail
			f.mu.Unlock()
			if fails {
				writeCoreProblem(t, w, http.StatusServiceUnavailable, "people unavailable")
				return
			}
			// The shape production answers with for a mirror's GitHub collaborators.
			writeJSONResponse(t, w, http.StatusOK, json.RawMessage(`{"items":[
				{"accountId":"01ALICE","handle":"github:alice","provider":"github","displayName":"Alice Smith","role":"writer","directGrant":null,"sources":[{"source":"github","role":"writer"}]},
				{"accountId":"01BOB","handle":"github:bob","provider":"github","role":"reader","directGrant":null,"sources":[{"source":"github","role":"reader"}]}],
				"totalCount":2}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/"+testDetachMirrorULID+"/detach":
			f.mu.Lock()
			i := min(f.stateGets, len(f.states)-1)
			f.stateGets++
			fails := f.stateFails
			f.mu.Unlock()
			if fails {
				writeCoreProblem(t, w, http.StatusServiceUnavailable, "detach state unavailable")
				return
			}
			if i < 0 {
				t.Errorf("unexpected detach state read")
				w.WriteHeader(http.StatusNotFound)
				return
			}
			writeJSONResponse(t, w, http.StatusOK, json.RawMessage(f.states[i]))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

const (
	eligiblePlanJSON = `{"dryRun":true,"eligible":true,"requestedBy":"01ACCT","targetProject":"` + testProjectULID + `","name":"hello-world",
		"preconditions":[{"precondition":"single-placement","passed":true},{"precondition":"import-complete","passed":true}],
		"access":[{"subjectType":"account","subjectId":"01ALICE","role":"writer","source":"github","coveredByTargetProject":true},
		          {"subjectType":"team","subjectId":"01TEAM","role":"reader","source":"legacy-tuple","coveredByTargetProject":false}]}`
	ineligiblePlanJSON = `{"dryRun":true,"eligible":false,"requestedBy":"01ACCT","targetProject":"` + testProjectULID + `","name":"hello-world",
		"preconditions":[{"precondition":"single-placement","passed":false,"detail":"mirrored on 2 clusters"},{"precondition":"import-complete","passed":true}],
		"access":[]}`
	completeResultJSON = `{"dryRun":false,"eligible":true,"requestedBy":"01ACCT","targetProject":"` + testProjectULID + `","name":"hello-world",
		"preconditions":[],"access":[],"status":"complete","nativeName":"et/acme/hello-world","releasedAddresses":["gh/octocat/hello-world"],
		"lostAccess":[{"subjectType":"team","subjectId":"01TEAM","role":"reader","source":"legacy-tuple","coveredByTargetProject":false}],
		"notices":["The GitHub repository stays live."]}`
	inProgressResultJSON = `{"dryRun":false,"eligible":true,"requestedBy":"01ACCT","targetProject":"` + testProjectULID + `","name":"hello-world",
		"preconditions":[],"access":[],"status":"in_progress","nativeName":"et/acme/hello-world","releasedAddresses":["gh/octocat/hello-world"],
		"lostAccess":[],"statusUrl":"/api/v1/repos/` + testDetachMirrorULID + `/detach"}`
	stateInProgressJSON        = `{"status":"in_progress","step":6,"nativeName":"et/acme/hello-world","releasedAddresses":["gh/octocat/hello-world"],"resumable":false,"frozen":true}`
	stateStalledResumableJSON  = `{"status":"stalled","step":7,"nativeName":"et/acme/hello-world","releasedAddresses":["gh/octocat/hello-world"],"resumable":true,"frozen":true}`
	stateStalledNeedsAdminJSON = `{"status":"stalled","step":7,"nativeName":"et/acme/hello-world","releasedAddresses":["gh/octocat/hello-world"],"resumable":false,"frozen":true}`
	stateCompleteJSON          = `{"status":"complete","step":9,"nativeName":"et/acme/hello-world","releasedAddresses":["gh/octocat/hello-world"],"resumable":false,"frozen":false}`
)

// An in-progress answer is waited on — through a stall the server resumes on
// its own — and the final status is what both renderings report.
//
// Not parallel: swaps the package-level seams.
func TestRepoMirrorDetach_WaitsForCompletion(t *testing.T) {
	useFastMirrorPolling(t)
	fake, url := newDetachFixture(t, eligiblePlanJSON, inProgressResultJSON)
	fake.states = []string{stateInProgressJSON, stateStalledResumableJSON, stateCompleteJSON}

	stdout, stderr, err := execDetach(t, url, "--yes")
	require.NoError(t, err)
	assert.Equal(t, 3, fake.stateGets, "a resumable stall keeps the wait going")
	assert.Contains(t, stderr, "Detaching /gh/octocat/hello-world into /et/acme/hello-world. This can take a few minutes")
	assert.NotContains(t, stderr, "step", "core's internal steps mean nothing to a reader")
	assert.Contains(t, stdout, "✓ Detached /gh/octocat/hello-world into /et/acme/hello-world")

	fake.stateGets = 0
	stdout, _, err = execDetach(t, url, "--yes", "--json")
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &got), "stdout must be one JSON document: %s", stdout)
	assert.Equal(t, "complete", got["status"], "--json reports where the detach ended")
}

// A stall the server will not resume ends the wait and fails the command,
// naming who can resume it.
//
// Not parallel: swaps the package-level seams.
func TestRepoMirrorDetach_StallNeedingAnAdmin(t *testing.T) {
	useFastMirrorPolling(t)
	fake, url := newDetachFixture(t, eligiblePlanJSON, inProgressResultJSON)
	fake.states = []string{stateInProgressJSON, stateStalledNeedsAdminJSON}

	stdout, stderr, err := execDetach(t, url, "--yes")
	require.ErrorContains(t, err, "needs an admin of the target project")
	assert.Equal(t, 2, fake.stateGets)
	assert.Contains(t, stdout, "until an admin of the target project resumes the rewire")
	assert.Contains(t, stderr, "entire api -X POST /api/v1/repos/"+testDetachMirrorULID+"/detach -f targetProject="+testProjectULID+" -F dryRun=false",
		"the /gh/ ref answers moved now, so the hint is the only way back to the detach")
}

// --no-wait returns on the first answer without reading the state.
//
// Not parallel: swaps the package-level core-client seam.
func TestRepoMirrorDetach_NoWait(t *testing.T) {
	fake, url := newDetachFixture(t, eligiblePlanJSON, inProgressResultJSON)
	stdout, stderr, err := execDetach(t, url, "--yes", "--no-wait")
	require.NoError(t, err)
	assert.Zero(t, fake.stateGets)
	assert.Contains(t, stdout, "is in progress")
	assert.Contains(t, stderr, "Follow the detach with: entire api /api/v1/repos/"+testDetachMirrorULID+"/detach")

	fake.result = strings.Replace(inProgressResultJSON, `"in_progress"`, `"stalled"`, 1)
	stdout, _, err = execDetach(t, url, "--yes", "--no-wait")
	require.NoError(t, err, "a stall the caller chose not to wait on is not a failure")
	assert.Contains(t, stdout, "stalled")
}

// A wait that runs out reports the last state and fails, saying the detach
// carries on server-side.
//
// Not parallel: swaps the package-level seams.
func TestRepoMirrorDetach_WaitTimesOut(t *testing.T) {
	useFastMirrorPolling(t)
	fake, url := newDetachFixture(t, eligiblePlanJSON, inProgressResultJSON)
	fake.states = []string{stateInProgressJSON}

	stdout, _, err := execDetach(t, url, "--yes", "--timeout", "20ms")
	require.ErrorContains(t, err, "timed out waiting for the detach")
	require.ErrorContains(t, err, "carries on on the server")
	assert.Contains(t, stdout, "is in progress")
}

func newDetachFixture(t *testing.T, plan, result string) (*fakeDetachCore, string) {
	t.Helper()
	fake := &fakeDetachCore{placements: []string{testDetachMirrorULID}, plan: plan, result: result}
	srv := httptest.NewServer(fake.handler(t))
	t.Cleanup(srv.Close)
	return fake, srv.URL
}

// execDetach runs the command on /gh/octocat/hello-world into /et/acme/hello-world.
func execDetach(t *testing.T, srvURL string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	return runCoreCmd(t, newRepoMirrorDetachCmd, srvURL, append([]string{"/gh/octocat/hello-world", "--into", "/et/acme/hello-world"}, args...)...)
}

// stubDetachPrompt makes the confirmation reachable and answers it, recording
// whether it was asked.
func stubDetachPrompt(t *testing.T, answer bool) *bool {
	t.Helper()
	t.Setenv(interactive.EnvTestTTY, "1")
	asked := false
	prevConfirm := detachConfirmed
	detachConfirmed = func(*cobra.Command, mirrorRepoRef, string, *coreapi.DetachRepoResult, detachNames) (bool, error) {
		asked = true
		return answer, nil
	}
	t.Cleanup(func() { detachConfirmed = prevConfirm })
	return &asked
}

// A dry run sends exactly one request with dryRun set, addressed by the
// mirror's placement ID and the project's ULID, and prints both tables.
//
// Not parallel: swaps the package-level core-client seam.
func TestRepoMirrorDetach_DryRun(t *testing.T) {
	fake, url := newDetachFixture(t, eligiblePlanJSON, "")
	stdout, _, err := execDetach(t, url, "--dry-run")
	require.NoError(t, err)

	require.Len(t, fake.bodies, 1)
	assert.True(t, fake.bodies[0].DryRun)
	assert.Equal(t, testProjectULID, fake.bodies[0].TargetProject)
	assert.Equal(t, "hello-world", fake.bodies[0].Name.Or(""))
	assert.False(t, fake.bodies[0].RequestedBy.IsSet(), "requestedBy is never sent: the caller is the requester")
	assert.Equal(t, "/api/v1/repos/"+testDetachMirrorULID+"/detach", fake.detachPath[0])

	assert.Contains(t, stdout, "/gh/octocat/hello-world → /et/acme/hello-world")
	assert.Contains(t, stdout, "single-placement")
	assert.Equal(t, 1, fake.peopleGets)
	lost, kept, ok := strings.Cut(stdout, "Keeps access (1):")
	require.True(t, ok, "who keeps access is its own section: %s", stdout)
	assert.Contains(t, lost, "Loses access (1):")
	assert.Regexp(t, `01TEAM\s+-\s+reader\s+legacy-tuple\s+team`, lost, "a team has no handle: its ID, with the kind in TYPE")
	assert.Contains(t, kept, "github:alice")
	assert.Contains(t, kept, "Alice Smith")
	assert.NotContains(t, stdout, "01ALICE", "an account the people listing names is shown by handle")
	assert.Contains(t, stdout, "Eligible. 1 access source would be removed.")
}

// --json on a dry run is the wire plan, so a script reads `eligible` and the
// precondition slugs directly.
//
// Not parallel: swaps the package-level core-client seam.
func TestRepoMirrorDetach_DryRunJSON(t *testing.T) {
	_, url := newDetachFixture(t, ineligiblePlanJSON, "")
	stdout, _, err := execDetach(t, url, "--dry-run", "--json")
	require.NoError(t, err, "an ineligible plan is still a successful dry run")

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &got), "stdout must be one JSON document: %s", stdout)
	assert.Equal(t, false, got["eligible"])
}

// An ineligible plan stops a real detach before the write and names the failed
// preconditions.
//
// Not parallel: swaps the package-level core-client seam.
func TestRepoMirrorDetach_IneligibleRefusesBeforeWriting(t *testing.T) {
	fake, url := newDetachFixture(t, ineligiblePlanJSON, completeResultJSON)
	asked := stubDetachPrompt(t, true)
	stdout, _, err := execDetach(t, url)
	require.ErrorContains(t, err, "failed: single-placement")
	assert.Contains(t, stdout, "mirrored on 2 clusters")
	assert.False(t, *asked, "nothing to confirm on an ineligible plan")
	require.Len(t, fake.bodies, 1)
	assert.True(t, fake.bodies[0].DryRun, "only the dry run was sent")
}

// A confirmed detach sends the plan, then the real call, and reports the new
// address, the released one, and who lost access.
//
// Not parallel: swaps the package-level seams.
func TestRepoMirrorDetach_Confirmed(t *testing.T) {
	fake, url := newDetachFixture(t, eligiblePlanJSON, completeResultJSON)
	asked := stubDetachPrompt(t, true)
	stdout, _, err := execDetach(t, url)
	require.NoError(t, err)
	assert.True(t, *asked)

	require.Len(t, fake.bodies, 2)
	assert.True(t, fake.bodies[0].DryRun)
	assert.False(t, fake.bodies[1].DryRun)
	assert.Equal(t, fake.bodies[0].TargetProject, fake.bodies[1].TargetProject)

	assert.Contains(t, stdout, "✓ Detached /gh/octocat/hello-world into /et/acme/hello-world")
	assert.Contains(t, stdout, "Released: /gh/octocat/hello-world")
	assert.Contains(t, stdout, "Removed access (1):")
	assert.Contains(t, stdout, "Note: The GitHub repository stays live.")
}

// Declining the prompt sends nothing but the dry run and exits 0.
//
// Not parallel: swaps the package-level seams.
func TestRepoMirrorDetach_Declined(t *testing.T) {
	fake, url := newDetachFixture(t, eligiblePlanJSON, completeResultJSON)
	asked := stubDetachPrompt(t, false)
	_, _, err := execDetach(t, url)
	require.NoError(t, err)
	assert.True(t, *asked)
	require.Len(t, fake.bodies, 1)
	assert.True(t, fake.bodies[0].DryRun)
}

// --yes skips the prompt; --json then prints only the real result.
//
// Not parallel: swaps the package-level core-client seam.
func TestRepoMirrorDetach_YesJSON(t *testing.T) {
	fake, url := newDetachFixture(t, eligiblePlanJSON, completeResultJSON)
	stdout, _, err := execDetach(t, url, "--yes", "--json")
	require.NoError(t, err)
	require.Len(t, fake.bodies, 2)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &got), "stdout must be one JSON document: %s", stdout)
	assert.Equal(t, "complete", got["status"])
}

// Without a terminal and without --yes the command refuses before any
// request: an unanswerable prompt must not cost a lookup.
//
// Not parallel: swaps the package-level core-client seam.
func TestRepoMirrorDetach_NonInteractiveNeedsYes(t *testing.T) {
	fake, url := newDetachFixture(t, eligiblePlanJSON, completeResultJSON)
	t.Setenv(interactive.EnvTestTTY, "0")

	_, _, err := execDetach(t, url)
	require.ErrorContains(t, err, "pass --yes")
	assert.Empty(t, fake.bodies)
}

// The verb detaches a GitHub mirror into a native path, and refuses either
// the wrong way round, a missing --into, or an unmirrored repo with a reason
// rather than a server error — the grammar ones before any request.
//
// Not parallel: swaps the package-level core-client seam.
func TestRepoMirrorDetach_Refusals(t *testing.T) {
	fake, url := newDetachFixture(t, eligiblePlanJSON, "")
	run := func(args ...string) error {
		_, _, err := runCoreCmd(t, newRepoMirrorDetachCmd, url, args...)
		return err
	}

	require.ErrorContains(t, run("/et/acme/web", "--into", "/et/acme/web", "--dry-run"), "supports GitHub mirrors only")
	require.ErrorContains(t, run("/gh/octocat/hello-world", "--into", "/gh/acme/web", "--dry-run"), "invalid --into")
	require.ErrorContains(t, run("/gh/octocat/hello-world", "--into", "/et/acme", "--dry-run"), "invalid --into")
	require.ErrorContains(t, run("/gh/octocat/hello-world", "--dry-run"), `required flag(s) "into" not set`)
	assert.Empty(t, fake.bodies)

	fake.placements = nil
	_, _, err := execDetach(t, url, "--dry-run")
	require.ErrorContains(t, err, "not mirrored on any cluster you can read")
	assert.Empty(t, fake.bodies)
}

// A stalled detach leaves the repo frozen, and a resume carries no lostAccess:
// the output must say both without claiming nobody lost access.
func TestRenderDetachResult_Stalled(t *testing.T) {
	t.Parallel()
	var res coreapi.DetachRepoResult
	require.NoError(t, res.UnmarshalJSON([]byte(`{"dryRun":false,"eligible":true,"requestedBy":"01ACCT","targetProject":"`+testProjectULID+`","name":"web",
		"preconditions":[],"access":[],"status":"stalled","nativeName":"et/acme/web"}`)))
	var b strings.Builder
	ref := mirrorRepoRef{forge: mirrorCloneForge, owner: "o", repo: "web"}
	require.NoError(t, renderDetachResult(&b, ref, &res, nil, &coreapi.RepoDetachState{Status: "stalled", Resumable: true}))
	assert.Contains(t, b.String(), "until the server's sweep resumes the rewire")
	assert.Contains(t, b.String(), "/et/acme/web")
	assert.NotContains(t, b.String(), "No access was removed", "a resume does not know who lost access")

	// Without a state read (--no-wait) nothing says who resumes it.
	b.Reset()
	require.NoError(t, renderDetachResult(&b, ref, &res, nil, nil))
	assert.Contains(t, b.String(), "until the rewire is resumed.")
	assert.NotContains(t, b.String(), "sweep")
}

func TestDetachConfirmTitle(t *testing.T) {
	t.Parallel()
	ref := mirrorRepoRef{forge: mirrorCloneForge, owner: "o", repo: "r"}
	plan := &coreapi.DetachRepoResult{Access: []coreapi.DetachAccessEntry{
		{SubjectId: "a", CoveredByTargetProject: true},
		{SubjectId: "b"},
		{SubjectId: "c"},
	}}
	assert.Equal(t, "Detach /gh/o/r into /et/p/r? Writes freeze until the rewire finishes, and 2 access sources the project does not cover will be removed.",
		detachConfirmTitle(ref, "/et/p/r", plan))
	plan.Access = plan.Access[:1]
	assert.Equal(t, "Detach /gh/o/r into /et/p/r? Writes freeze until the rewire finishes.", detachConfirmTitle(ref, "/et/p/r", plan))
}

// A wait that keeps failing after the write must still say the detach ran:
// runCore's problem rendering would otherwise reduce it to the server's bare
// detail, reading as a detach that never happened.
//
// Not parallel: swaps the package-level seams.
func TestRepoMirrorDetach_PollFailureKeepsTheContext(t *testing.T) {
	useFastMirrorPolling(t)
	fake, url := newDetachFixture(t, eligiblePlanJSON, inProgressResultJSON)
	fake.states = []string{stateInProgressJSON}
	fake.stateFails = true

	_, stderr, err := execDetach(t, url, "--yes")
	require.ErrorContains(t, err, "detach state unavailable")
	require.ErrorContains(t, err, "carries on on the server")
	assert.Equal(t, maxConsecutivePollErrors, fake.stateGets)
	assert.Contains(t, stderr, "Follow the detach with:")
}

// A state of none contradicts a detach that just answered in_progress, and an
// answer this client has no word for is not a success either.
//
// Not parallel: swaps the package-level seams.
func TestRepoMirrorDetach_UnexpectedStatuses(t *testing.T) {
	useFastMirrorPolling(t)
	fake, url := newDetachFixture(t, eligiblePlanJSON, inProgressResultJSON)
	fake.states = []string{`{"status":"none","releasedAddresses":[],"resumable":false,"frozen":false}`}

	stdout, stderr, err := execDetach(t, url, "--yes")
	require.ErrorContains(t, err, `answered "in_progress", but the server reports no detach recorded`)
	assert.NotContains(t, err.Error(), "carries on", "the server just said there is nothing to carry on")
	assert.NotContains(t, stderr, "Follow the detach", "a read that repeats none is no follow-up")
	assert.Contains(t, stdout, "is in progress", "a none state is not merged over the detach's own answer")

	fake.result = strings.Replace(completeResultJSON, `"complete"`, `"queued"`, 1)
	_, _, err = execDetach(t, url, "--yes")
	require.ErrorContains(t, err, `unexpected status "queued"`)
}

// A refusal of the real call after the plan passed reports the server's reason,
// and only an outright refusal is taken to have changed nothing.
//
// Not parallel: swaps the package-level core-client seam.
func TestRepoMirrorDetach_RealCallRefused(t *testing.T) {
	fake, url := newDetachFixture(t, eligiblePlanJSON, completeResultJSON)
	fake.realFails = http.StatusConflict
	stdout, stderr, err := execDetach(t, url, "--yes")
	require.ErrorContains(t, err, "detach refused: retry later")
	assert.Empty(t, stdout)
	assert.NotContains(t, stderr, "may have started", "a 4xx changed nothing")
	require.Len(t, fake.bodies, 2)

	// A 5xx (or no answer at all) leaves it unknown whether the repo was
	// frozen and rewired, and the /gh/ ref may already answer moved.
	fake.realFails = http.StatusServiceUnavailable
	_, stderr, err = execDetach(t, url, "--yes")
	require.Error(t, err)
	assert.Contains(t, stderr, "The detach may have started. Follow the detach with: entire api /api/v1/repos/"+testDetachMirrorULID+"/detach")
}

// An ineligible real run under --json still hands a script one document, the
// plan that explains the refusal, and fails.
//
// Not parallel: swaps the package-level core-client seam.
func TestRepoMirrorDetach_IneligibleJSON(t *testing.T) {
	_, url := newDetachFixture(t, ineligiblePlanJSON, completeResultJSON)
	stdout, _, err := execDetach(t, url, "--yes", "--json")
	require.ErrorContains(t, err, "failed: single-placement")
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &got), "stdout must be one JSON document: %s", stdout)
	assert.Equal(t, false, got["eligible"])
}

// The real confirmation shows the plan on the prompt's writer, never on
// stdout, which carries the result (and --json).
//
// Not parallel: sets ACCESSIBLE and swaps the terminal opener.
func TestDetachConfirmed_PlanFollowsThePrompt(t *testing.T) {
	t.Setenv("ACCESSIBLE", "1")
	var terminal bytes.Buffer
	prev := openPromptTerminal
	openPromptTerminal = func() (promptTerminal, error) {
		return promptTerminal{in: strings.NewReader("y\n"), out: &terminal}, nil
	}
	t.Cleanup(func() { openPromptTerminal = prev })

	var plan coreapi.DetachRepoResult
	require.NoError(t, plan.UnmarshalJSON([]byte(eligiblePlanJSON)))
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr) // not a terminal, so the prompt falls back to the stub

	names := detachNames{"01ALICE": {AccountId: "01ALICE", Handle: coreapi.NewOptString("github:alice")}}
	proceed, err := detachConfirmed(cmd, mirrorRepoRef{forge: mirrorCloneForge, owner: "octocat", repo: "hello-world"}, "/et/acme/hello-world", &plan, names)
	require.NoError(t, err)
	assert.True(t, proceed)
	assert.Contains(t, terminal.String(), "01TEAM", "the plan is shown where the question is asked")
	assert.Contains(t, terminal.String(), "github:alice")
	assert.Contains(t, terminal.String(), "1 access source the project does not cover will be removed")
	assert.Empty(t, stdout.String())
}

// A context cancelled out from under the confirmation is an interruption, not
// a decline: main keys the quiet signal exit off context.Canceled.
func TestDetachConfirmed_ACancelledContextIsNotADecline(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	var render bytes.Buffer
	cmd.SetErr(&render)

	proceed, err := detachConfirmed(cmd, mirrorRepoRef{forge: mirrorCloneForge, owner: "o", repo: "r"}, "/et/p/r", &coreapi.DetachRepoResult{}, nil)
	require.False(t, proceed)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, render.String())
}

// Ctrl+C during the wait exits as an interruption after reporting where the
// detach got to.
func TestAwaitDetach_CancelIsAnInterruption(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := awaitDetach(ctx, detachStateFunc(func(context.Context, coreapi.GetRepoDetachParams) (*coreapi.RepoDetachState, error) {
		return nil, context.Canceled
	}), testDetachMirrorULID, 0)
	require.ErrorIs(t, err, context.Canceled)
	var silent *SilentError
	require.ErrorAs(t, err, &silent, "main re-raises the signal instead of printing")
}

type detachStateFunc func(context.Context, coreapi.GetRepoDetachParams) (*coreapi.RepoDetachState, error)

func (f detachStateFunc) GetRepoDetach(ctx context.Context, p coreapi.GetRepoDetachParams) (*coreapi.RepoDetachState, error) {
	return f(ctx, p)
}

// The confirmation is skipped with --yes/-y only: there is no --force, since
// nothing overrides an ineligible plan.
func TestRepoMirrorDetach_ConfirmFlags(t *testing.T) {
	t.Parallel()
	cmd := newRepoMirrorDetachCmd()
	yes := cmd.Flags().Lookup("yes")
	require.NotNil(t, yes)
	assert.Equal(t, "y", yes.Shorthand)
	assert.Nil(t, cmd.Flags().Lookup("force"))
	assert.Nil(t, cmd.Flags().ShorthandLookup("f"))
}

// Names are cosmetic: a failed people lookup leaves subjects shown by ID and
// the plan still renders.
//
// Not parallel: swaps the package-level core-client seam.
func TestRepoMirrorDetach_NamesAreBestEffort(t *testing.T) {
	fake, url := newDetachFixture(t, eligiblePlanJSON, "")
	fake.peopleFail = true
	stdout, _, err := execDetach(t, url, "--dry-run")
	require.NoError(t, err)
	assert.Regexp(t, `01ALICE\s+-\s+writer\s+github\s+account`, stdout)
}

// --json is the wire plan, which names subjects by ID, so it skips the lookup.
//
// Not parallel: swaps the package-level core-client seam.
func TestRepoMirrorDetach_JSONSkipsTheNameLookup(t *testing.T) {
	fake, url := newDetachFixture(t, eligiblePlanJSON, "")
	_, _, err := execDetach(t, url, "--dry-run", "--json")
	require.NoError(t, err)
	assert.Zero(t, fake.peopleGets)
}

// --into names both halves of the target: its project is resolved by name and
// its repo is the native name the detach is asked to use.
//
// Not parallel: swaps the package-level core-client seam.
func TestRepoMirrorDetach_IntoNamesTheTarget(t *testing.T) {
	fake, url := newDetachFixture(t, eligiblePlanJSON, "")
	_, _, err := runCoreCmd(t, newRepoMirrorDetachCmd, url, "/gh/octocat/hello-world", "--into", "/et/acme/capricciosa", "--dry-run")
	require.NoError(t, err)
	require.Len(t, fake.bodies, 1)
	assert.Equal(t, testProjectULID, fake.bodies[0].TargetProject)
	assert.Equal(t, "capricciosa", fake.bodies[0].Name.Or(""))
}
