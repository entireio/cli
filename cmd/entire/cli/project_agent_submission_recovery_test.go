package cli

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectAgentSubmissionValidationRecovery(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := osroot.Shared(dir)
	require.NoError(t, err)
	journal := &projectAgentJournal{root: root, name: "pending.json"}
	var accept atomic.Bool
	corrected := json.RawMessage(`{"result":{"findings":[]}}`)
	worker := testAgentWorker(t, func(out http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		if !accept.Load() {
			out.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		var body json.RawMessage
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.JSONEq(t, string(corrected), string(body))
		writeAgentTestResponse(t, out, `{}`)
	})
	worker.save = journal.save
	worker.pendingPath = journal.path()
	pending := projectAgentSubmission{RunID: agentServeClaim().RunID, Action: "complete", Body: json.RawMessage(`{"result":"private review content"}`)}
	require.NoError(t, journal.save(&pending))
	err = worker.submit(t.Context(), pending)
	require.ErrorContains(t, err, "correct the saved body before restarting serve")
	require.ErrorContains(t, err, filepath.Join(dir, "pending.json"))
	require.NotContains(t, err.Error(), "private review content")
	require.True(t, api.IsHTTPErrorStatus(err, http.StatusUnprocessableEntity), "path annotation must preserve the API error")
	retained, err := journal.load()
	require.NoError(t, err)
	require.Equal(t, &pending, retained)

	// An operator repairs the saved body; a new worker replays that submission.
	retained.Body = corrected
	require.NoError(t, journal.save(retained))
	accept.Store(true)
	restarted := projectAgentWorker{target: worker.target, out: worker.out, save: journal.save, wait: worker.wait, pendingPath: journal.path()}
	replay, err := journal.load()
	require.NoError(t, err)
	require.NotNil(t, replay)
	require.NoError(t, restarted.submit(t.Context(), *replay))
	remaining, err := journal.load()
	require.NoError(t, err)
	require.Nil(t, remaining)
}
