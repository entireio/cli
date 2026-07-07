package ticket

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrintStatus_NotConfigured(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	printStatus(&buf, statusReport{Configured: false})
	assert.Contains(t, buf.String(), "not configured")
}

func TestPrintStatus_ConfiguredWithLink(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	printStatus(&buf, statusReport{
		Configured:  true,
		Platform:    "linear",
		Team:        "ENG",
		Credential:  true,
		Branch:      "amy/eng-142",
		TicketID:    "ENG-142",
		TicketTitle: "Add rate limiting",
		TicketState: "in_progress",
		TicketURL:   "https://linear.app/x/ENG-142",
	})
	out := buf.String()
	assert.Contains(t, out, "linear (team ENG)")
	assert.Contains(t, out, "present")
	assert.Contains(t, out, "ENG-142")
	assert.Contains(t, out, "Add rate limiting")
	assert.Contains(t, out, "in_progress")
}

func TestPrintStatus_ConfiguredNoLink(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	printStatus(&buf, statusReport{Configured: true, Platform: "linear", Team: "ENG", Credential: true, Branch: "main"})
	assert.Contains(t, buf.String(), "(none")
}

func TestNewStatusCmd_JSONFlag(t *testing.T) {
	t.Parallel()

	cmd := newStatusCmd()
	require.Equal(t, "status", cmd.Use)
	assert.NotNil(t, cmd.Flags().Lookup("json"))
}
