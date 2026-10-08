package compact

import (
	"testing"

	"github.com/entireio/cli/redact"
	"github.com/entireio/cli/transcript"
	"github.com/stretchr/testify/require"
)

func TestCompact_RedactedWrapper(t *testing.T) {
	t.Parallel()

	raw := []byte("{\"type\":\"user\",\"message\":{\"content\":\"first\"}}\n" +
		"{\"type\":\"user\",\"message\":{\"content\":\"second\"}}\n")
	opts := MetadataFields{Agent: "claude-code", CLIVersion: "0.5.1", StartLine: 1}
	expected, err := transcript.Convert(raw, opts)
	require.NoError(t, err)

	actual, err := Compact(redact.AlreadyRedacted(raw), opts)
	require.NoError(t, err)
	require.Equal(t, expected, actual)
	require.JSONEq(t, `{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"user","content":[{"text":"second"}]}`, string(actual))
}

func TestFullWithBoundary_RedactedWrapper(t *testing.T) {
	t.Parallel()

	raw := []byte("{\"type\":\"user\",\"message\":{\"content\":\"first\"}}\n" +
		"{\"type\":\"user\",\"message\":{\"content\":\"second\"}}\n")
	opts := MetadataFields{Agent: "claude-code", CLIVersion: "0.5.1", StartLine: 1}
	expected, expectedBoundary, err := transcript.ConvertWithBoundary(raw, opts)
	require.NoError(t, err)

	actual, boundary, err := FullWithBoundary(redact.AlreadyRedacted(raw), opts)
	require.NoError(t, err)
	require.Equal(t, expected, actual)
	require.Equal(t, expectedBoundary, boundary)
	require.Equal(t, 1, boundary)
}
