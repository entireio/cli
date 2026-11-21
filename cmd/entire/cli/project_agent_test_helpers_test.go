package cli

import (
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
)

func writeAgentTestResponse(t *testing.T, w io.Writer, body string) {
	t.Helper()
	_, err := io.WriteString(w, body)
	assert.NoError(t, err)
}
