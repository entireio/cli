package claudecode

import (
	"bytes"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	transcriptlib "github.com/entireio/cli/transcript"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The private text a developer's instruction files and memory notes carry. None
// of it may reach the stored transcript.
const (
	memoryNote     = "Client Acme Bank: staging db server is at 10.20.30.40"
	globalRule     = "Never mention the Globex Corp migration"
	systemPromptTx = "You are Claude Code. # Memory: clients.md lists Acme Bank"
	toolDescTx     = "Runs the internal Initech deploy tool"
)

// Lines in the shapes Claude Code writes them. The conversation lines carry
// their own text, which must survive untouched.
const (
	userLine      = `{"parentUuid":null,"type":"user","uuid":"u1","message":{"role":"user","content":"add feature.txt <tag> & more"},"sessionId":"s1"}`
	assistantLine = `{"parentUuid":"u1","type":"assistant","uuid":"a1","message":{"id":"msg_1","role":"assistant","model":"claude-fable-5-2","content":[{"type":"text","text":"added feature.txt"}],"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":3,"cache_creation_input_tokens":2}},"sessionId":"s1"}`

	instructionsLine = `{"parentUuid":"u1","isSidechain":false,"attachment":{"type":"instructions","files":[{"path":"/home/dev/.claude/memory/clients.md","type":"User","content":"` + memoryNote + `"},{"path":"/home/dev/.claude/CLAUDE.md","type":"User","content":"` + globalRule + `"}]},"type":"attachment","uuid":"i1","rendered":[{"content":"<system-reminder>` + memoryNote + ` ` + globalRule + `</system-reminder>"}],"renderedRole":"user","sessionId":"s1"}`
	snapshotLine     = `{"parentUuid":"i1","isSidechain":false,"attachment":{"type":"prompt_snapshot","systemPrompt":["` + systemPromptTx + `","second part"],"tools":[{"name":"Deploy","description":"` + toolDescTx + `"}],"reminderFold":true},"type":"attachment","uuid":"p1","sessionId":"s1"}`
	shapeLine        = `{"type":"api-request-shape","sessionId":"s1","shapeHash":"shape-hash-1","shape":{"system":[{"type":"text","text":"` + systemPromptTx + `"}],"tools":[{"name":"Deploy","description":"` + toolDescTx + `","input_schema":{"type":"object"}}]}}`
	blobLine         = `{"type":"api-request-blob","sessionId":"s1","hash":"blob-hash-1","message":{"role":"user","content":[{"type":"text","text":"<system-reminder>` + memoryNote + `</system-reminder>"},{"type":"text","text":"add feature.txt"}]}}`
	requestLine      = `{"type":"api-request","sessionId":"s1","id":"r1","shapeHash":"shape-hash-1","params":{"model":"claude-fable-5-2","max_tokens":64000},"tail":["blob-hash-1"]}`
)

func injectedTranscript() []byte {
	return []byte(strings.Join([]string{
		userLine, instructionsLine, snapshotLine, shapeLine, blobLine, requestLine, assistantLine,
	}, "\n") + "\n")
}

func TestSanitizeStoredTranscript_BlanksInjectedRequestText(t *testing.T) {
	t.Parallel()

	got := string(SanitizeStoredTranscript(injectedTranscript()))

	for _, private := range []string{memoryNote, globalRule, systemPromptTx, toolDescTx, "second part"} {
		assert.NotContains(t, got, private, "injected request text survived")
	}
}

func TestSanitizeStoredTranscript_KeepsRecordsAndTheirIdentity(t *testing.T) {
	t.Parallel()

	got := string(SanitizeStoredTranscript(injectedTranscript()))

	// Every record stays, with the fields that identify and link it: a blanked
	// record must still be the same record to anything that follows parentUuid,
	// a blob hash, or a file path.
	for _, kept := range []string{
		`"type":"instructions"`, `"uuid":"i1"`, `"parentUuid":"u1"`,
		`"path":"/home/dev/.claude/memory/clients.md"`, `"path":"/home/dev/.claude/CLAUDE.md"`,
		`"type":"prompt_snapshot"`, `"uuid":"p1"`, `"reminderFold":true`,
		`"type":"api-request-shape"`, `"shapeHash":"shape-hash-1"`,
		`"type":"api-request-blob"`, `"hash":"blob-hash-1"`, `"role":"user"`,
	} {
		assert.Contains(t, got, kept)
	}
}

func TestSanitizeStoredTranscript_LeavesOtherLinesByteIdentical(t *testing.T) {
	t.Parallel()

	input := injectedTranscript()
	rawLines := bytes.Split(input, []byte("\n"))
	gotLines := bytes.Split(SanitizeStoredTranscript(input), []byte("\n"))

	require.Len(t, gotLines, len(rawLines), "line count changed: offsets into the stored transcript would drift")
	for _, i := range []int{0, 5, 6, 7} { // user, api-request, assistant, trailing empty
		assert.Equal(t, string(rawLines[i]), string(gotLines[i]), "line %d was rewritten", i)
	}
}

func TestSanitizeStoredTranscript_PreservesBlankLinesAndMissingFinalNewline(t *testing.T) {
	t.Parallel()

	input := []byte(userLine + "\n\n" + blobLine + "\n\n" + assistantLine)
	got := SanitizeStoredTranscript(input)

	gotLines := bytes.Split(got, []byte("\n"))
	require.Len(t, gotLines, 5)
	assert.Empty(t, gotLines[1])
	assert.Empty(t, gotLines[3])
	if string(gotLines[4]) != assistantLine {
		t.Errorf("an untouched line must keep its exact bytes, got %s", gotLines[4])
	}
	assert.NotContains(t, string(got), memoryNote)
}

func TestSanitizeStoredTranscript_Idempotent(t *testing.T) {
	t.Parallel()

	once := SanitizeStoredTranscript(injectedTranscript())
	twice := SanitizeStoredTranscript(once)

	assert.Equal(t, string(once), string(twice))
}

func TestSanitizeStoredTranscript_NothingInjectedReturnsInput(t *testing.T) {
	t.Parallel()

	input := []byte(userLine + "\n" + requestLine + "\n" + assistantLine + "\n")

	assert.Equal(t, string(input), string(SanitizeStoredTranscript(input)))
}

func TestSanitizeStoredTranscript_PassesThroughWhatItDoesNotRecognise(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"malformed line naming a record kind":  `{"type":"api-request-blob","message":{"content":[`,
		"user prompt that mentions the kinds":  `{"type":"user","uuid":"u2","message":{"role":"user","content":"what are api-request-blob, prompt_snapshot and \"instructions\" lines?"}}`,
		"tool result quoting an injected line": `{"type":"user","uuid":"u3","toolUseResult":{"stdout":"{\"type\":\"api-request-blob\",\"message\":{\"content\":[{\"text\":\"kept\"}]}}"}}`,
		"unknown attachment":                   `{"type":"attachment","uuid":"x1","attachment":{"type":"skill_listing","content":"kept"}}`,
		"record kind with an unexpected shape": `{"type":"api-request-blob","hash":"h","message":"not an object"}`,
		"not a JSON object":                    `["api-request-blob"]`,
	}
	for name, line := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			input := []byte(userLine + "\n" + line + "\n")

			assert.Equal(t, string(input), string(SanitizeStoredTranscript(input)))
		})
	}
}

func TestSanitizeStoredTranscript_DoesNotEscapeKeptText(t *testing.T) {
	t.Parallel()

	// A blanked record is re-encoded; text it keeps must come back as written,
	// not HTML-escaped.
	line := `{"type":"api-request-blob","sessionId":"<s&1>","hash":"h","message":{"role":"user","content":[{"type":"text","text":"x"}]}}`

	got := string(SanitizeStoredTranscript([]byte(line + "\n")))

	assert.Contains(t, got, `"sessionId":"<s&1>"`)
}

func TestSanitizeStoredTranscript_ConversationReadersSeeNoDifference(t *testing.T) {
	t.Parallel()

	input := injectedTranscript()
	sanitized := SanitizeStoredTranscript(input)
	c := &ClaudeCodeAgent{}

	rawUsage, err := c.CalculateTokenUsage(input, 0)
	require.NoError(t, err)
	gotUsage, err := c.CalculateTokenUsage(sanitized, 0)
	require.NoError(t, err)
	assert.Equal(t, rawUsage, gotUsage)
	require.NotNil(t, gotUsage)
	assert.Equal(t, 5, gotUsage.OutputTokens, "the fixture's usage must be counted for this comparison to mean anything")

	opts := transcriptlib.Options{Agent: string(agent.AgentNameClaudeCode), CLIVersion: "test"}
	rawCompact, err := transcriptlib.Convert(input, opts)
	require.NoError(t, err)
	gotCompact, err := transcriptlib.Convert(sanitized, opts)
	require.NoError(t, err)
	assert.Equal(t, string(rawCompact), string(gotCompact))
	assert.Contains(t, string(gotCompact), "added feature.txt")

	rawPrompts, err := c.ExtractPromptsFromTranscript(input, 0)
	require.NoError(t, err)
	gotPrompts, err := c.ExtractPromptsFromTranscript(sanitized, 0)
	require.NoError(t, err)
	assert.Equal(t, rawPrompts, gotPrompts)
}

func TestClaudeCodeAgent_SanitizesTranscriptForStorage(t *testing.T) {
	t.Parallel()

	got := agent.SanitizeTranscriptForStorage(&ClaudeCodeAgent{}, injectedTranscript())

	assert.NotContains(t, string(got), memoryNote)
	assert.Contains(t, string(got), "added feature.txt")
}
