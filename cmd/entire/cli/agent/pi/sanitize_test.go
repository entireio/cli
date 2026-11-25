package pi

import (
	"bytes"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The instruction-file text Pi loads into its system message. None of it may
// reach the stored transcript.
const piProjectContext = "Client Acme Bank: staging db server is at 10.20.30.40"

// Lines in the shapes Pi writes them.
const (
	piSessionLine   = `{"type":"session","version":3,"id":"s1","cwd":"/repo"}`
	piSystemLine    = `{"type":"message","id":"m1","parentId":"m0","message":{"role":"system","content":"","sections":{"preamble":"You are <pi> & more","rules":"be brief","project_context":"` + piProjectContext + `","cwd":"/repo"},"timestamp":1}}`
	piUserLine      = `{"type":"message","id":"m2","parentId":"m1","message":{"role":"user","content":[{"type":"text","text":"add feature.txt"}],"timestamp":2}}`
	piAssistantLine = `{"type":"message","id":"m3","parentId":"m2","message":{"role":"assistant","content":[{"type":"text","text":"added feature.txt"}],"usage":{"input":10,"output":5,"cacheRead":3,"cacheWrite":2,"totalTokens":20},"timestamp":3}}`
	piCompactLine   = `{"type":"compaction","id":"c1","parentId":"m3","summary":"earlier work","systemMessage":{"role":"system","sections":{"rules":"be brief","project_context":"` + piProjectContext + `"}}}`
)

func piInjectedTranscript() []byte {
	return []byte(strings.Join([]string{
		piSessionLine, piSystemLine, piUserLine, piAssistantLine, piCompactLine,
	}, "\n") + "\n")
}

func TestSanitizeStoredTranscript_BlanksProjectContext(t *testing.T) {
	t.Parallel()

	got := string(SanitizeStoredTranscript(piInjectedTranscript()))

	assert.NotContains(t, got, piProjectContext, "project context survived")

	// The records stay, with their ids, their links and their other sections.
	for _, kept := range []string{
		`"project_context":""`, `"id":"m1"`, `"parentId":"m0"`, `"preamble":"You are <pi> & more"`,
		`"rules":"be brief"`, `"cwd":"/repo"`, `"type":"compaction"`, `"id":"c1"`, `"summary":"earlier work"`,
	} {
		assert.Contains(t, got, kept)
	}
}

func TestSanitizeStoredTranscript_LeavesOtherLinesByteIdentical(t *testing.T) {
	t.Parallel()

	input := piInjectedTranscript()
	rawLines := bytes.Split(input, []byte("\n"))
	gotLines := bytes.Split(SanitizeStoredTranscript(input), []byte("\n"))

	require.Len(t, gotLines, len(rawLines), "line count changed: offsets into the stored transcript would drift")
	for _, i := range []int{0, 2, 3, 5} { // session, user, assistant, trailing empty
		assert.Equal(t, string(rawLines[i]), string(gotLines[i]), "line %d was rewritten", i)
	}
}

func TestSanitizeStoredTranscript_Idempotent(t *testing.T) {
	t.Parallel()

	once := SanitizeStoredTranscript(piInjectedTranscript())
	twice := SanitizeStoredTranscript(once)

	assert.Equal(t, string(once), string(twice))
}

func TestSanitizeStoredTranscript_PassesThroughWhatItDoesNotRecognise(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"nothing injected":                     piUserLine,
		"malformed line":                       `{"type":"message","message":{"sections":{"project_context":"x`,
		"user prompt that names the field":     `{"type":"message","id":"m4","message":{"role":"user","content":[{"type":"text","text":"what is \"project_context\" in pi?"}]}}`,
		"sections that are not an object":      `{"type":"message","id":"m5","message":{"role":"system","sections":"project_context"}}`,
		"project context that is not a string": `{"type":"message","id":"m6","message":{"role":"system","sections":{"project_context":{"text":"kept"}}}}`,
		"not a JSON object":                    `["project_context"]`,
	}
	for name, line := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			input := []byte(piSessionLine + "\n" + line + "\n")

			assert.Equal(t, string(input), string(SanitizeStoredTranscript(input)))
		})
	}
}

func TestSanitizeStoredTranscript_ConversationReadersSeeNoDifference(t *testing.T) {
	t.Parallel()

	input := piInjectedTranscript()
	sanitized := SanitizeStoredTranscript(input)
	a := &PiAgent{}

	rawUsage, err := a.CalculateTokenUsage(input, 0)
	require.NoError(t, err)
	gotUsage, err := a.CalculateTokenUsage(sanitized, 0)
	require.NoError(t, err)
	assert.Equal(t, rawUsage, gotUsage)
	require.NotNil(t, gotUsage)
	assert.Equal(t, 5, gotUsage.OutputTokens, "the fixture's usage must be counted for this comparison to mean anything")
}

func TestPiAgent_SanitizesTranscriptForStorage(t *testing.T) {
	t.Parallel()

	got := agent.SanitizeTranscriptForStorage(&PiAgent{}, piInjectedTranscript())

	assert.NotContains(t, string(got), piProjectContext)
	assert.Contains(t, string(got), "added feature.txt")
}
