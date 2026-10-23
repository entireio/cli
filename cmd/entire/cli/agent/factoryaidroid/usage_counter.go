package factoryaidroid

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

var _ agent.UsageCounterSource = (*FactoryAIDroidAgent)(nil)

// recentMessageWindow is how many recent messages stay open to later lines.
const recentMessageWindow = 64

const settingsSuffix = ".settings.json"

// NewUsageCounter prefers the session-cumulative tokenUsage Droid keeps in the
// settings.json beside each transcript (verified on captured Droid 0.243.0
// sessions, BYOK included), with one call per distinct assistant message in
// the transcript. Droid writes settings.json on a debounce, so it can lag the
// transcript; the per-field max with the transcript's own usage (by message
// ID, most output winning) keeps the total from going down when it does, and
// covers versions that write usage there. A session's tokenUsage counts only
// its own calls, so the total excludes its Workers. Children are the Worker
// sessions it launched, keyed by Worker session ID and read the same way.
func (f *FactoryAIDroidAgent) NewUsageCounter() agent.UsageCounter {
	return &usageCounter{
		Calls:    agent.KeyedUsage{Policy: agent.KeepMostOutput, Window: recentMessageWindow},
		Messages: agent.KeyedUsage{Policy: agent.KeepFirst, Remember: true},
	}
}

type usageCounter struct {
	// Calls holds the transcripts' usage by message ID and Messages one
	// call per assistant message; the child is "" for the session itself and
	// the Worker's session ID for a Worker.
	Calls    agent.KeyedUsage `json:"calls"`
	Messages agent.KeyedUsage `json:"messages"`
	// Settings is each session's latest settings.json total.
	Settings map[string]*agent.TokenUsage `json:"settings,omitempty"`
	// sessions maps each file Files returned to its session ("" = this one),
	// rebuilt each reading; workers caches the reading's Worker scan.
	sessions map[string]string
	workers  map[string]string
}

// Files are the session's transcript and settings.json, and the same pair for
// each Worker. Workers live beside the parent or in the sessions root one
// level up; finding them opens the session_start line of every transcript
// there on each reading.
func (u *usageCounter) Files(in agent.CumulativeTokenInput) []agent.UsageFile {
	sessionID := in.SessionID
	if sessionID == "" {
		sessionID = strings.TrimSuffix(filepath.Base(in.TranscriptPath), ".jsonl")
	}
	u.sessions = map[string]string{}
	// The session's own transcript is not the only source of its total, so a
	// rewrite of it is not reported (see UsageFile.NoRestart).
	files := []agent.UsageFile{{Path: in.TranscriptPath, NoRestart: true}}
	add := func(child, transcriptPath string) {
		settingsPath := strings.TrimSuffix(transcriptPath, ".jsonl") + settingsSuffix
		u.sessions[transcriptPath], u.sessions[settingsPath] = child, child
		if child != "" {
			files = append(files, agent.UsageFile{Path: transcriptPath, ID: "worker:" + child})
		}
		files = append(files, agent.UsageFile{Path: settingsPath, ID: "settings:" + child, Whole: true})
	}
	add("", in.TranscriptPath)
	if u.workers == nil {
		dir := filepath.Dir(in.TranscriptPath)
		u.workers = findWorkerSessions([]string{dir, filepath.Dir(dir)}, sessionID)
	}
	for _, workerID := range slices.Sorted(maps.Keys(u.workers)) {
		add(workerID, u.workers[workerID])
	}
	return files
}

func (u *usageCounter) Add(path string, data []byte, firstLine int) {
	child := u.sessions[path]
	if strings.HasSuffix(path, settingsSuffix) {
		var settings droidSettings
		if json.Unmarshal(data, &settings) != nil || settings.TokenUsage == nil {
			return
		}
		t := settings.TokenUsage
		if u.Settings == nil {
			u.Settings = make(map[string]*agent.TokenUsage)
		}
		u.Settings[child] = &agent.TokenUsage{
			InputTokens: t.InputTokens, OutputTokens: t.OutputTokens, CacheCreationTokens: t.CacheCreationTokens, CacheReadTokens: t.CacheReadTokens,
		}
		return
	}
	lines, _, err := ParseDroidTranscriptFromBytes(data, 0)
	if err != nil {
		return
	}
	for i, line := range lines {
		if line.Type != roleAssistant {
			continue
		}
		key := line.UUID
		if key == "" {
			key = "entry:" + strconv.Itoa(firstLine+i)
		}
		u.Messages.Add(child, key, agent.TokenUsage{APICallCount: 1})
		var msg messageWithUsage
		if json.Unmarshal(line.Message, &msg) != nil || msg.ID == "" {
			continue
		}
		u.Calls.Add(child, msg.ID, agent.TokenUsage{
			InputTokens:         msg.Usage.InputTokens,
			CacheCreationTokens: msg.Usage.CacheCreationInputTokens,
			CacheReadTokens:     msg.Usage.CacheReadInputTokens,
			OutputTokens:        msg.Usage.OutputTokens,
			APICallCount:        1,
		})
	}
}

func (u *usageCounter) Reset(path string) {
	child := u.sessions[path]
	if strings.HasSuffix(path, settingsSuffix) {
		delete(u.Settings, child)
		return
	}
	u.Calls.ResetChild(child)
	u.Messages.ResetChild(child)
}

func (u *usageCounter) Totals(context.Context, agent.CumulativeTokenInput) (*agent.TokenUsage, map[string]*agent.TokenUsage) {
	var children map[string]*agent.TokenUsage
	for _, child := range slices.Sorted(maps.Values(u.sessions)) {
		if child == "" || children[child] != nil {
			continue
		}
		if usage := u.session(child); usage != nil {
			if children == nil {
				children = make(map[string]*agent.TokenUsage)
			}
			children[child] = usage
		}
	}
	return u.session(""), children
}

// session returns one session's usage: the per-field max of its settings.json
// total (an all-zero placeholder is no data) and its transcript's usage.
func (u *usageCounter) session(child string) *agent.TokenUsage {
	settings := agent.UsageOrNil(u.Settings[child])
	if settings != nil {
		c := *settings
		if messages := u.Messages.Sum(child); messages != nil {
			c.APICallCount = messages.APICallCount
		}
		settings = &c
	}
	transcript := u.Calls.Sum(child)
	if transcript != nil && transcript.APICallCount == 0 {
		transcript = nil
	}
	return agent.MaxTokenUsage(settings, transcript)
}

// findWorkerSessions returns the Worker sessions in dirs whose session_start
// line names parentSessionID as the calling session, mapped from Worker
// session ID to transcript path.
func findWorkerSessions(dirs []string, parentSessionID string) map[string]string {
	workers := make(map[string]string)
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			workerID, ok := strings.CutSuffix(entry.Name(), ".jsonl")
			// Regular files only: no directories, symlinks out of the session
			// store, or FIFOs that would block the open.
			if !ok || !entry.Type().IsRegular() || workerID == parentSessionID || validation.ValidateAgentSessionID(workerID) != nil {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			if start, ok := readDroidSessionStart(path); ok && parentSessionID != "" && start.CallingSessionID == parentSessionID {
				workers[workerID] = path
			}
		}
	}
	return workers
}
