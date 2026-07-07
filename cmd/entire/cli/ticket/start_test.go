package ticket

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestComposeStartPrompt(t *testing.T) {
	t.Parallel()

	prompt := composeStartPrompt(Link{Platform: "linear", ID: "ENG-142"}, nil)
	assert.Contains(t, prompt, "ENG-142")
	assert.Contains(t, prompt, "linear")
	assert.True(t, strings.HasPrefix(prompt, "Ticket ENG-142"))

	withTask := composeStartPrompt(
		Link{Platform: "linear", ID: "ENG-142"},
		&Task{
			Title:      "Add rate limiting",
			Intent:     "throttle /export",
			Acceptance: "429 over limit",
			URL:        "https://linear.app/x/ENG-142",
			Comments:   []Comment{{Author: "amy", Body: "cap at 100 rps"}},
		},
	)
	assert.Contains(t, withTask, "Add rate limiting")
	assert.Contains(t, withTask, "throttle /export")
	assert.Contains(t, withTask, "429 over limit")
	assert.Contains(t, withTask, "amy: cap at 100 rps")
	assert.Contains(t, withTask, "https://linear.app/x/ENG-142")
}

func TestNewStartCmd_Flags(t *testing.T) {
	t.Parallel()

	cmd := newStartCmd(Deps{})
	assert.Equal(t, "start <ticket-id>", cmd.Use)
	assert.NotNil(t, cmd.Flags().Lookup("agent"))
	assert.NotNil(t, cmd.Flags().Lookup("force"))
	assert.NotNil(t, cmd.Flags().Lookup("branch"))
	assert.NotNil(t, cmd.Flags().Lookup("no-branch"))
}
