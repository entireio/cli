package antigravity

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

var (
	_ agent.SubagentSessionResolver = (*AntigravityAgent)(nil)
	_ agent.SubagentReplyCounter    = (*AntigravityAgent)(nil)
)

// agy runs a subagent as a conversation of its own, with its own hooks. The
// parent calls invoke_subagent, and agy then writes two steps that link them:
//
//   - in the parent's transcript, a GENERIC step after the call:
//     "Created the following subagent:\n{ "conversationId": "<child>", ... }"
//   - in the child's transcript, step 0, a SYSTEM_MESSAGE carrying the
//     parent's prompt: "[Message] timestamp=... sender=<parent> priority=..."
//
// Neither is a schema field: both are text agy formats. The resolver requires
// both, so a format change leaves the child a top-level session (fail closed)
// rather than attaching it to the wrong parent.
//
// The child's transcript does not exist during its first hooks (SessionStart
// and its first PreInvocation); it does by its first tool call, which is the
// first point its work can touch the worktree. Callers re-resolve on later
// hooks.

const (
	agyStepTypeSystemMessage = "SYSTEM_MESSAGE"
	agyStepTypeGeneric       = "GENERIC"
	agyStepTypeUserInput     = "USER_INPUT"
	agyToolInvokeSubagent    = "invoke_subagent"
	agyToolSendMessage       = "send_message"
	agyCreatedSubagentMarker = "Created the following subagent:"

	// subagentHeadScanLimit bounds how many leading steps of a child transcript
	// are examined for the parent's message. agy writes it as step 0.
	subagentHeadScanLimit = 4
	// subagentLabelMaxLen bounds a task description taken from a prompt.
	subagentLabelMaxLen = 120
	// subagentHeadBytes bounds the read of a transcript whose first steps are
	// all that is needed. The resolver runs on every tool call of every
	// session, top-level ones included.
	subagentHeadBytes = 256 << 10
)

var agySenderRe = regexp.MustCompile(`\[Message\] timestamp=\S+ sender=([A-Za-z0-9_-]+)\s`)

// agySystemMessageTypeRe matches a raw step that declares itself a
// SYSTEM_MESSAGE, for a step too long to decode.
var agySystemMessageTypeRe = regexp.MustCompile(`"type"\s*:\s*"` + agyStepTypeSystemMessage + `"`)

// ResolveSubagentSession implements agent.SubagentSessionResolver.
//
// The ToolUseID is the child's conversation ID: agy tool calls carry no IDs,
// the conversation ID is stable across every hook of the child, and a later
// message to the same child continues the same task.
func (a *AntigravityAgent) ResolveSubagentSession(sessionRef string) (agent.SubagentSessionLink, bool) {
	store, childName, err := a.transcriptStore(sessionRef)
	if err != nil {
		return agent.SubagentSessionLink{}, false
	}
	childData, err := store.ReadFileHead(childName, subagentHeadBytes)
	if err != nil {
		return agent.SubagentSessionLink{}, false
	}
	parentID := subagentSender(childData)
	if parentID == "" || validation.ValidateAgentSessionID(parentID) != nil {
		return agent.SubagentSessionLink{}, false
	}
	childID := conversationIDFromTranscriptName(childName)
	if childID == "" || childID == parentID || validation.ValidateToolUseID(childID) != nil {
		return agent.SubagentSessionLink{}, false
	}

	parentName, parentPath, err := store.SessionFile(parentID)
	if err != nil {
		return agent.SubagentSessionLink{}, false
	}
	parentData, err := store.ReadFile(parentName)
	if err != nil {
		return agent.SubagentSessionLink{}, false
	}
	subagentType, description, found := subagentInvocation(parentData, childID)
	if !found {
		return agent.SubagentSessionLink{}, false
	}
	return agent.SubagentSessionLink{
		ParentSessionID:      parentID,
		ToolUseID:            childID,
		ParentTranscriptPath: parentPath,
		SubagentType:         subagentType,
		TaskDescription:      description,
	}, true
}

// subagentSender returns the sender of the parent's message when the
// transcript opens with one, "" for a top-level conversation (which opens with
// the user's prompt).
func subagentSender(data []byte) string {
	for seen := 0; seen < subagentHeadScanLimit && len(data) > 0; {
		line, rest, _ := bytes.Cut(data, []byte("\n"))
		data = rest
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		seen++
		var step agyStep
		if json.Unmarshal(line, &step) != nil {
			// A step longer than the head read (a long prompt) is cut off and
			// does not decode; its sender is near the start of the message.
			if agySystemMessageTypeRe.Match(line) {
				if m := agySenderRe.FindSubmatch(line); m != nil {
					return string(m[1])
				}
			}
			continue
		}
		switch step.Type {
		case agyStepTypeUserInput:
			return "" // a user's prompt first: not a subagent
		case agyStepTypeSystemMessage:
			if m := agySenderRe.FindStringSubmatch(step.Content); m != nil {
				return m[1]
			}
		}
	}
	return ""
}

// subagentInvocation finds the parent step that created childID and returns
// the type and role of the invoke_subagent call before it. found is false when
// the parent never created this child.
func subagentInvocation(parentData []byte, childID string) (subagentType, description string, found bool) {
	var lastCall *agyStepToolCall
	forEachNonBlankLine(parentData, 0, func(raw []byte) {
		// Only the call and the step it creates matter, and agy writes the
		// second right after the first; skip decoding the rest of what can be a
		// long transcript.
		if found || (!bytes.Contains(raw, []byte(agyToolInvokeSubagent)) && !bytes.Contains(raw, []byte(agyCreatedSubagentMarker))) {
			return
		}
		var step agyStep
		if json.Unmarshal(raw, &step) != nil {
			return
		}
		switch step.Type {
		case agyStepTypePlannerResponse:
			lastCall = nil
			calls := 0
			for i := range step.ToolCalls {
				if step.ToolCalls[i].Name == agyToolInvokeSubagent {
					calls++
					lastCall = &step.ToolCalls[i]
				}
			}
			if calls > 1 {
				lastCall = nil // several in one step: which one made this child is unknown
			}
		case agyStepTypeGeneric:
			if createdSubagentID(step.Content) == childID {
				found = true
			}
		}
	})
	if !found || lastCall == nil {
		return "", "", found
	}
	subagentType = decodeAgyString(lastCall.Args["TypeName"])
	description = decodeAgyString(lastCall.Args["Role"])
	if description == "" {
		description = subagentLabel(decodeAgyString(lastCall.Args["Prompt"]), subagentLabelMaxLen)
	}
	return subagentType, description, true
}

// createdSubagentID returns the conversationId from a "Created the following
// subagent" step, "" for any other content.
func createdSubagentID(content string) string {
	_, rest, ok := strings.Cut(content, agyCreatedSubagentMarker)
	if !ok {
		return ""
	}
	var created struct {
		ConversationID string `json:"conversationId"`
	}
	if json.NewDecoder(strings.NewReader(rest)).Decode(&created) != nil {
		return ""
	}
	return created.ConversationID
}

// conversationIDFromTranscriptName returns <id> from the store name
// "<id>/.system_generated/logs/transcript_full.jsonl" (ResolveSessionFile's
// layout), "" for any other shape.
func conversationIDFromTranscriptName(name string) string {
	id, rest, ok := strings.Cut(name, "/")
	if !ok || rest != ".system_generated/logs/transcript_full.jsonl" {
		return ""
	}
	return id
}

// lastStepIsUserInput reports whether the transcript's last step is the user's
// prompt. agy writes a prompt's USER_INPUT step before the prompt's first
// model call, but a parent resumed by a subagent's message gets that message
// only after its PreInvocation fires; so at PreInvocation a new prompt ends
// the transcript with USER_INPUT and a wake-up does not.
func (a *AntigravityAgent) lastStepIsUserInput(transcriptRef string) (isPrompt, known bool) {
	store, name, err := a.transcriptStore(transcriptRef)
	if err != nil {
		return false, false
	}
	data, err := store.ReadFile(name)
	if err != nil {
		return false, false
	}
	last := lastNonBlankLine(data)
	if last == nil {
		return false, false
	}
	var step agyStep
	if json.Unmarshal(bytes.TrimSpace(last), &step) != nil {
		return false, false
	}
	return step.Type == agyStepTypeUserInput, true
}

func subagentLabel(s string, maxLen int) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if r := []rune(s); len(r) > maxLen {
		s = strings.TrimSpace(string(r[:maxLen])) + "…"
	}
	return s
}

// lastNonBlankLine returns the last non-blank line of data, nil if none,
// scanning back from the end rather than splitting the whole transcript.
func lastNonBlankLine(data []byte) []byte {
	for len(data) > 0 {
		i := bytes.LastIndexByte(data, '\n')
		line := data[i+1:]
		if len(bytes.TrimSpace(line)) > 0 {
			return line
		}
		if i < 0 {
			return nil
		}
		data = data[:i]
	}
	return nil
}

// TurnSubagents implements agent.SubagentReplyCounter: the subagents whose IDs
// appear in the parent's steps after fromOffset — created by invoke_subagent,
// messaged by send_message, or heard from in a SYSTEM_MESSAGE — each with how
// many messages from it the whole transcript holds and how many the parent
// sent it (its launch prompt and its send_message calls). Only conversations
// this parent created count (its "Created the following subagent" steps, the
// same link ResolveSubagentSession requires), from any turn: a later turn can
// message a child created earlier, while a conversation it never created is no
// subagent of its, and must not hold its turn open.
func (a *AntigravityAgent) TurnSubagents(parentRef string, fromOffset int) (map[string]agent.SubagentExchange, error) {
	data, err := a.readStoreTranscript(parentRef)
	if err != nil {
		return nil, err
	}
	scan := turnSubagentScan{
		turn:     make(map[string]struct{}),
		created:  make(map[string]struct{}),
		exchange: make(map[string]agent.SubagentExchange),
	}
	line := 0
	forEachNonBlankLine(data, 0, func(raw []byte) {
		line++
		var step agyStep
		if json.Unmarshal(raw, &step) != nil {
			return
		}
		scan.record(step, line > fromOffset)
	})
	out := make(map[string]agent.SubagentExchange, len(scan.turn))
	for id := range scan.turn {
		if _, ok := scan.created[id]; ok && validation.ValidateAgentSessionID(id) == nil {
			out[id] = scan.exchange[id]
		}
	}
	return out, nil
}

// turnSubagentScan collects, across a parent's steps, the subagent IDs its
// turn involves, the subagents it created, and the exchange with each.
type turnSubagentScan struct {
	turn     map[string]struct{}
	created  map[string]struct{}
	exchange map[string]agent.SubagentExchange
}

// record adds one parent step; inTurn reports whether it is past the turn's
// start offset.
func (s *turnSubagentScan) record(step agyStep, inTurn bool) {
	switch step.Type {
	case agyStepTypeSystemMessage:
		if m := agySenderRe.FindStringSubmatch(step.Content); m != nil {
			s.add(m[1], inTurn, agent.SubagentExchange{Replies: 1})
		}
	case agyStepTypeGeneric:
		if id := createdSubagentID(step.Content); id != "" {
			s.created[id] = struct{}{}
			s.add(id, inTurn, agent.SubagentExchange{Messages: 1})
		}
	case agyStepTypePlannerResponse:
		for _, call := range step.ToolCalls {
			if call.Name != agyToolSendMessage {
				continue
			}
			if id := decodeAgyString(call.Args["Recipient"]); id != "" {
				s.add(id, inTurn, agent.SubagentExchange{Messages: 1})
			}
		}
	}
}

// add counts one message with id, and marks id as involved in the turn when
// the step is past the turn's start.
func (s *turnSubagentScan) add(id string, inTurn bool, delta agent.SubagentExchange) {
	ex := s.exchange[id]
	ex.Replies += delta.Replies
	ex.Messages += delta.Messages
	s.exchange[id] = ex
	if inTurn {
		s.turn[id] = struct{}{}
	}
}

// ExchangeWithParent implements agent.SubagentReplyCounter: the subagent's
// send_message calls addressed to parentID, and the messages from parentID
// (its launch prompt first) its transcript holds.
func (a *AntigravityAgent) ExchangeWithParent(subagentRef, parentID string) (agent.SubagentExchange, error) {
	data, err := a.readStoreTranscript(subagentRef)
	if err != nil {
		return agent.SubagentExchange{}, err
	}
	var ex agent.SubagentExchange
	forEachNonBlankLine(data, 0, func(raw []byte) {
		if !bytes.Contains(raw, []byte(agyToolSendMessage)) && !bytes.Contains(raw, []byte("sender=")) {
			return
		}
		var step agyStep
		if json.Unmarshal(raw, &step) != nil {
			return
		}
		switch step.Type {
		case agyStepTypeSystemMessage:
			if m := agySenderRe.FindStringSubmatch(step.Content); m != nil && m[1] == parentID {
				ex.Messages++
			}
		case agyStepTypePlannerResponse:
			for _, call := range step.ToolCalls {
				if call.Name == agyToolSendMessage && decodeAgyString(call.Args["Recipient"]) == parentID {
					ex.Replies++
				}
			}
		}
	})
	return ex, nil
}

// readStoreTranscript reads a transcript inside agy's brain directory. A
// missing file is an empty transcript.
func (a *AntigravityAgent) readStoreTranscript(ref string) ([]byte, error) {
	store, name, err := a.transcriptStore(ref)
	if err != nil {
		return nil, err
	}
	data, err := store.ReadFile(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("antigravity: read transcript: %w", err)
	}
	return data, nil
}
