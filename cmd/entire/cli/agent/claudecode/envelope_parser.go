package claudecode

import (
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// classifyClaudeEnvelope inspects Claude CLI stdout for the structured JSON
// envelope and returns a typed *TextGenError when the envelope represents a
// failure, nil otherwise.
//
// Return contract:
//   - nil: no envelope to classify. Reached when stdout is empty (subprocess
//     produced no output — e.g. CLIMissing or SIGKILL before any writes), when
//     the envelope parsed cleanly with is_error:false (success path), or when
//     stdout was non-JSON AND runErr is non-nil (so the caller can classify
//     the run error — a ctx sentinel, or stderr, whichever applies).
//   - non-nil: structured failure — either an is_error:true envelope
//     classified by api_error_status / auth-phrase heuristic, or a malformed-
//     JSON case on an otherwise successful run (stdout non-empty but
//     unparseable while runErr is nil).
//
// runErr is consulted to suppress parse-failure classification whenever the
// subprocess also failed, because in that case the run error is strictly more
// informative than "the bytes on stdout were not JSON":
//   - ctx sentinel  → caller surfaces "canceled"/"timed out" unwrapped.
//   - any other err → caller classifies stderr, so a genuine "401
//     Unauthorized" on stderr is reported as an auth failure instead of being
//     masked by a JSON-parse complaint. (Claude can emit a node warning or a
//     progress line on stdout and still fail with a real error on stderr.)
//
// A complete is_error envelope still wins over a ctx sentinel — that is 963's
// intentional ordering: if Claude managed to emit a structured diagnostic,
// that is more actionable than "cancelled".
func classifyClaudeEnvelope(stdout []byte, runErr error) *agent.TextGenError {
	if len(stdout) == 0 {
		return nil
	}
	result, envelope, parseErr := parseGenerateTextResponse(stdout)
	if parseErr != nil {
		// The run itself failed: defer to the caller's runErr handling (ctx
		// sentinel, or stderr classification). A complete is_error envelope
		// below would still preempt ctx (that is the success of this
		// function, not its failure).
		if runErr != nil {
			return nil //nolint:nilerr // deliberate: parseErr is less informative than runErr, whose stderr the caller classifies
		}
		return &agent.TextGenError{
			Kind:     agent.TextGenErrorUnknown,
			Provider: agent.AgentNameClaudeCode,
			Message:  fmt.Sprintf("failed to parse claude CLI response: %v", parseErr),
		}
	}
	if envelope == nil || !envelope.IsError {
		return nil
	}
	apiStatus := 0
	if envelope.APIErrorStatus != nil {
		apiStatus = *envelope.APIErrorStatus
	}
	e := &agent.TextGenError{
		Provider:  agent.AgentNameClaudeCode,
		Message:   result,
		APIStatus: apiStatus,
	}
	switch {
	case apiStatus == 401, apiStatus == 403:
		e.Kind = agent.TextGenErrorAuth
	case apiStatus == 429:
		e.Kind = agent.TextGenErrorRateLimit
	case apiStatus >= 400 && apiStatus < 500:
		e.Kind = agent.TextGenErrorConfig
	case apiStatus == 0 && containsAuthPhrase(result):
		// Last-resort heuristic for envelopes that carry is_error:true
		// without a structured api_error_status. Small, evidence-based list
		// from 963.
		e.Kind = agent.TextGenErrorAuth
	default:
		e.Kind = agent.TextGenErrorUnknown
	}
	return e
}

var envelopeAuthPhrases = []string{"invalid api key", "not logged in"}

func containsAuthPhrase(s string) bool {
	lower := strings.ToLower(s)
	for _, p := range envelopeAuthPhrases {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}
