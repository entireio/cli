// vogon is a deterministic agent binary for E2E canary tests.
// It parses simple prompt instructions, performs file operations, fires
// lifecycle hooks via `entire hooks vogon <verb>`, and writes a minimal
// JSONL transcript. No API calls are made.
//
// Named after the Vogons from The Hitchhiker's Guide to the Galaxy —
// bureaucratic, procedural, and deterministic to a fault.
//
// Usage:
//
//	vogon -p <prompt>   # Headless (single-turn) mode
//	vogon               # Interactive (multi-turn) mode
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/google/uuid"
)

func main() {
	cfg := parseArgs(os.Args[1:])

	if cfg.writeSessionOnly {
		transcriptPath := setupTranscript(cfg.sessionID)
		appendTranscript(transcriptPath, "user", cfg.userPrompt)
		appendTranscript(transcriptPath, "assistant", cfg.assistantMessage)
		fmt.Fprintln(os.Stdout, cfg.sessionID)
		return
	}

	dir, err := os.Getwd() //nolint:forbidigo // Standalone binary, not part of CLI — CWD is the repo dir
	if err != nil {
		fatal("getwd: %v", err)
	}

	transcriptPath := setupTranscript(cfg.sessionID)

	fireHook(dir, "session-start", map[string]string{
		"session_id":      cfg.sessionID,
		"transcript_path": transcriptPath,
		"model":           "vogon-llm-42",
	})

	if cfg.prompt != "" {
		// Headless mode: single prompt
		runTurn(dir, cfg.sessionID, transcriptPath, cfg.prompt)
	} else {
		// Interactive mode: read prompts from stdin
		fmt.Fprint(os.Stdout, "> ")
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || line == "exit" || line == "quit" {
				break
			}
			// Give tmux's Send() time to capture the echoed-input state
			// before we produce any output. Send polls every 200ms; without
			// this pause, vogon output appears in the same capture window
			// as the echo, causing stableAtSend to include final output and
			// preventing WaitFor's contentChanged detection.
			time.Sleep(700 * time.Millisecond)
			fmt.Fprintln(os.Stdout, "Working...")
			runTurn(dir, cfg.sessionID, transcriptPath, line)
			fmt.Fprint(os.Stdout, "> ")
		}
	}

	fireHook(dir, "session-end", map[string]string{
		"session_id":      cfg.sessionID,
		"transcript_path": transcriptPath,
		"model":           "vogon-llm-42",
	})
}

type config struct {
	prompt           string
	sessionID        string
	writeSessionOnly bool
	userPrompt       string
	assistantMessage string
}

func parseArgs(args []string) config {
	cfg := config{
		sessionID: uuid.New().String(),
	}

	fs := flag.NewFlagSet("vogon", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&cfg.prompt, "p", "", "single-turn prompt")
	fs.StringVar(&cfg.sessionID, "session-id", cfg.sessionID, "session identifier")
	fs.BoolVar(&cfg.writeSessionOnly, "write-session", false, "write a session transcript without firing hooks")
	fs.StringVar(&cfg.userPrompt, "user-prompt", "", "user prompt to record in write-session mode")
	fs.StringVar(&cfg.assistantMessage, "assistant-message", "", "assistant message to record in write-session mode")
	if err := fs.Parse(args); err != nil {
		fatal("parse args: %v", err)
	}
	if cfg.writeSessionOnly {
		if cfg.userPrompt == "" {
			fatal("--user-prompt is required with --write-session")
		}
		if cfg.assistantMessage == "" {
			fatal("--assistant-message is required with --write-session")
		}
	}
	if err := validateSessionID(cfg.sessionID); err != nil {
		fatal("invalid --session-id: %v", err)
	}

	return cfg
}

func validateSessionID(sessionID string) error {
	if sessionID == "" {
		return errors.New("session ID cannot be empty")
	}
	if filepath.Base(sessionID) != sessionID {
		return errors.New("must not contain path separators")
	}
	if sessionID == "." || sessionID == ".." || strings.Contains(sessionID, "..") {
		return errors.New("must not contain dot path elements")
	}
	return nil
}

func runTurn(dir, sessionID, transcriptPath, prompt string) {
	fireHook(dir, "user-prompt-submit", map[string]any{
		"session_id":      sessionID,
		"transcript_path": transcriptPath,
		"prompt":          prompt,
		"model":           "vogon-llm-42",
	})

	appendTranscript(transcriptPath, "user", prompt)

	actions := parsePrompt(prompt)
	executeActions(dir, actions)

	appendTranscript(transcriptPath, "assistant", fmt.Sprintf("Done. Executed %d actions.", len(actions)))
	fmt.Fprintf(os.Stdout, "Done. Executed %d actions.\n", len(actions))

	fireHook(dir, "stop", map[string]string{
		"session_id":      sessionID,
		"transcript_path": transcriptPath,
		"model":           "vogon-llm-42",
	})
}

// --- Prompt Parsing ---

type action struct {
	kind    string // "create", "modify", "delete", "commit"
	path    string
	content string
}

// fileExt is the shared file extension pattern used across all prompt-parsing regexes.
const fileExt = `go|md|txt|js|ts|py|rb|rs|toml|yaml|json`

var (
	// "create a [single|new|...] [markdown|text] file [exactly at|at|called] <path>"
	createFileRe = regexp.MustCompile(`(?i)create\s+(?:a\s+)?(?:\w+\s+)*?(?:markdown\s+|text\s+)?file\s+(?:exactly\s+at\s+|at\s+|called\s+)?([^\s,]+\.(?:` + fileExt + `))`)
	// "create N files: <path> about <topic>, <path> about <topic>"
	createMultiRe = regexp.MustCompile(`(?i)create\s+(?:\w+\s+)*?(?:four|three|two|\d+)\s+(?:\w+\s+)*?(?:markdown\s+)?files?:?\s+(.+)`)
	// "modify <path>"
	modifyFileRe = regexp.MustCompile(`(?i)modify\s+(?:the\s+)?(?:file\s+)?(?:at\s+)?([^\s,]+\.(?:` + fileExt + `))`)
	// "modify [these] N [existing] files[:]"
	modifyMultiRe = regexp.MustCompile(`(?i)modify\s+(?:these\s+)?(?:three|four|two|\d+)\s+(?:\w+\s+)*?files?[.:]\s*(.+)`)
	// "add ... to <path>" (e.g. "add another stanza to poem.txt")
	addToFileRe = regexp.MustCompile(`(?i)add\s+(?:another\s+|a\s+|some\s+)?\w+\s+to\s+([^\s,]+\.(?:` + fileExt + `))`)
	// "delete <path>"
	deleteFileRe = regexp.MustCompile(`(?i)delete\s+(?:the\s+)?(?:file\s+)?(?:at\s+)?([^\s,]+\.(?:` + fileExt + `))`)
	// "commit" anywhere in the prompt, but not "do not commit" / "don't commit"
	commitRe = regexp.MustCompile(`(?i)(?:then\s+|now\s+)?(?:git\s+)?commit\b`)
	// Negative: "do not commit" / "don't commit" with optional object, at sentence boundary
	// Matches: "Do not commit.", "Do not commit the file.", "Do not commit them,"
	// Does NOT match: "Do not commit any other files" (different intent)
	noCommitRe = regexp.MustCompile(`(?i)(?:do\s+not|don'?t)\s+commit(?:\s+(?:it|them|the\s+\w+))?(?:\.|,|$)`)
	// "commit each file separately" — interleave create+commit
	commitSeparatelyRe = regexp.MustCompile(`(?i)commit\s+each\s+file\s+separately`)
	// Individual file in multi-create: "docs/a.md about apples"
	// Topic capture stops at comma, "and", period followed by space+uppercase, or end
	multiFileItemRe = regexp.MustCompile(`([^\s,]+\.(?:` + fileExt + `))\s+(?:about\s+|with\s+|containing\s+)?(.+?)(?:,\s*|\s+and\s+|\.\s+[A-Z]|\.\s*$)`)
	// Individual file in multi-modify: "src/model.go should define ..."
	modifyItemRe = regexp.MustCompile(`([^\s,]+\.(?:` + fileExt + `))\s+should\s+(.+?)(?:,|$)`)
	// "In src/a.go, add ..." pattern for multi-modify
	inFileRe = regexp.MustCompile(`[Ii]n\s+([^\s,]+\.(?:` + fileExt + `))`)
	// Inline content: "with content 'package main; func Foo() {}'"
	inlineContentRe = regexp.MustCompile(`(?i)with\s+content\s+['"]([^'"]+)['"]`)
	// Numbered steps: "(1) ... (2) ... (3) ..."
	numberedStepRe = regexp.MustCompile(`\(\d+\)\s+`)
	// Explicit git command: "Run: git add ... && git commit -m '...'"
	explicitGitRe = regexp.MustCompile(`(?i)run:?\s+git\s+add\s+.+(?:&&\s*git\s+commit|;\s*git\s+commit)`)
	// "about the colour red" → "the colour red"
	topicRe = regexp.MustCompile(`(?i)about\s+(.+?)(?:\.|,|\s+Do\s|\s+do\s|\s+then\s|$)`)
	// Fallback: extract any file path from text
	anyFileRe = regexp.MustCompile(`([^\s,]+\.(?:` + fileExt + `))`)
)

func parsePrompt(prompt string) []action {
	// Check for numbered-step prompts — parse each step independently to
	// preserve ordering (e.g., create, commit, then create more).
	if numberedStepRe.MatchString(prompt) {
		return parseNumberedSteps(prompt)
	}
	var actions []action

	// Multi-file modify: "modify these three files: src/model.go should ..., src/view.go should ..."
	// Also matches: "Modify two existing files. In src/a.go, add ..."
	if m := modifyMultiRe.FindStringSubmatch(prompt); m != nil {
		// Try "should" pattern first
		items := modifyItemRe.FindAllStringSubmatch(m[1], -1)
		for _, item := range items {
			actions = append(actions, action{
				kind:    "modify",
				path:    item[1],
				content: "// " + strings.TrimSpace(item[2]) + "\n",
			})
		}
		// Try "In <path>" pattern as fallback
		if len(actions) == 0 {
			for _, item := range inFileRe.FindAllStringSubmatch(m[1], -1) {
				actions = append(actions, action{
					kind:    "modify",
					path:    item[1],
					content: "// Modified by vogon agent\n",
				})
			}
		}
		// Last resort: extract any file paths
		if len(actions) == 0 {
			for _, fm := range anyFileRe.FindAllStringSubmatch(m[1], -1) {
				actions = append(actions, action{
					kind:    "modify",
					path:    fm[1],
					content: "// Modified by vogon agent\n",
				})
			}
		}
	}

	// Multi-file create: "create four markdown files: docs/a.md about apples, ..."
	if len(actions) == 0 || hasModifyOnly(actions) {
		if m := createMultiRe.FindStringSubmatch(prompt); m != nil {
			items := multiFileItemRe.FindAllStringSubmatch(m[1], -1)
			for _, item := range items {
				actions = append(actions, action{
					kind:    "create",
					path:    item[1],
					content: fmt.Sprintf("# %s\n\nA paragraph about %s.\n", strings.TrimSpace(item[2]), strings.TrimSpace(item[2])),
				})
			}
		}
	}

	// Single file create — also runs if we only have modifies (combined prompts)
	if !hasCreate(actions) {
		if m := createFileRe.FindStringSubmatch(prompt); m != nil {
			topic := extractTopic(prompt)
			content := fmt.Sprintf("# %s\n\nA paragraph about %s.\n", topic, topic)
			if ic := inlineContentRe.FindStringSubmatch(prompt); ic != nil {
				content = ic[1] + "\n"
			}
			actions = append(actions, action{
				kind:    "create",
				path:    m[1],
				content: content,
			})
		}
	}

	// Single file modify — also runs if we only have creates (combined prompts)
	if !hasModify(actions) {
		if m := modifyFileRe.FindStringSubmatch(prompt); m != nil {
			actions = append(actions, action{
				kind:    "modify",
				path:    m[1],
				content: "// Modified by vogon agent\n",
			})
		}
	}

	// "add ... to <path>" as modify (e.g. "add another stanza to poem.txt")
	if !hasModify(actions) {
		if m := addToFileRe.FindStringSubmatch(prompt); m != nil {
			actions = append(actions, action{
				kind:    "modify",
				path:    m[1],
				content: "// Modified by vogon agent\n",
			})
		}
	}

	// Delete
	if m := deleteFileRe.FindStringSubmatch(prompt); m != nil {
		actions = append(actions, action{kind: "delete", path: m[1]})
	}

	// Commit: check for "commit each file separately" (interleaved) vs normal.
	// Skip if prompt contains "do not commit" / "don't commit".
	if !noCommitRe.MatchString(prompt) {
		if commitSeparatelyRe.MatchString(prompt) {
			actions = interleaveCommits(actions)
		} else if commitRe.MatchString(prompt) {
			actions = append(actions, action{kind: "commit"})
		}
	}

	return actions
}

// parseNumberedSteps splits "(1) ... (2) ... (3) ..." prompts into individual
// steps and parses each one, preserving ordering. This handles prompts like:
// "(1) Create file A. (2) Create file B. (3) git commit. (4) Create file C."
func parseNumberedSteps(prompt string) []action {
	// Split on numbered markers: "(1) ", "(2) ", etc.
	indices := numberedStepRe.FindAllStringIndex(prompt, -1)
	if len(indices) == 0 {
		return nil
	}

	var steps []string
	for i, idx := range indices {
		start := idx[1] // after the "(N) " marker
		var end int
		if i+1 < len(indices) {
			end = indices[i+1][0]
		} else {
			end = len(prompt)
		}
		step := strings.TrimSpace(prompt[start:end])
		if step != "" {
			steps = append(steps, step)
		}
	}

	var actions []action
	for _, step := range steps {
		hasExplicitGit := explicitGitRe.MatchString(step)

		// Create file with optional inline content.
		// Try the full "create ... file ... <path>" regex first, then fall back
		// to "Create <path>" (any file path after a Create verb).
		if m := createFileRe.FindStringSubmatch(step); m != nil {
			content := fmt.Sprintf("# %s\n\nGenerated content.\n", m[1])
			if ic := inlineContentRe.FindStringSubmatch(step); ic != nil {
				content = ic[1] + "\n"
			}
			actions = append(actions, action{kind: "create", path: m[1], content: content})
			if hasExplicitGit {
				actions = append(actions, action{kind: "commit"})
			}
			continue
		}
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(step)), "create") {
			if m := anyFileRe.FindStringSubmatch(step); m != nil {
				topic := extractTopic(step)
				content := fmt.Sprintf("# %s\n\nA paragraph about %s.\n", topic, topic)
				if ic := inlineContentRe.FindStringSubmatch(step); ic != nil {
					content = ic[1] + "\n"
				}
				actions = append(actions, action{kind: "create", path: m[1], content: content})
				if hasExplicitGit {
					actions = append(actions, action{kind: "commit"})
				}
				continue
			}
		}

		// Modify file
		if m := modifyFileRe.FindStringSubmatch(step); m != nil {
			actions = append(actions, action{kind: "modify", path: m[1], content: "// Modified by vogon agent\n"})
			if hasExplicitGit {
				actions = append(actions, action{kind: "commit"})
			}
			continue
		}

		// Delete file
		if m := deleteFileRe.FindStringSubmatch(step); m != nil {
			actions = append(actions, action{kind: "delete", path: m[1]})
			if hasExplicitGit {
				actions = append(actions, action{kind: "commit"})
			}
			continue
		}

		// Explicit git command without a file operation in this step
		if hasExplicitGit {
			actions = append(actions, action{kind: "commit"})
			continue
		}

		// Bare commit instruction
		if commitRe.MatchString(step) && !noCommitRe.MatchString(step) {
			actions = append(actions, action{kind: "commit"})
		}
	}
	return actions
}

func hasCreate(actions []action) bool {
	for _, a := range actions {
		if a.kind == "create" {
			return true
		}
	}
	return false
}

func hasModify(actions []action) bool {
	for _, a := range actions {
		if a.kind == "modify" {
			return true
		}
	}
	return false
}

func hasModifyOnly(actions []action) bool {
	if len(actions) == 0 {
		return false
	}
	for _, a := range actions {
		if a.kind != "modify" {
			return false
		}
	}
	return true
}

// interleaveCommits inserts a commit after each create/modify action.
func interleaveCommits(actions []action) []action {
	var result []action
	for _, a := range actions {
		result = append(result, a)
		if a.kind == "create" || a.kind == "modify" {
			result = append(result, action{kind: "commit"})
		}
	}
	return result
}

func extractTopic(prompt string) string {
	if m := topicRe.FindStringSubmatch(prompt); m != nil {
		return strings.TrimSpace(m[1])
	}
	return "the topic"
}

func executeActions(dir string, actions []action) {
	var pendingFiles []string
	for _, a := range actions {
		switch a.kind {
		case "create":
			createFile(dir, a.path, a.content)
			pendingFiles = append(pendingFiles, a.path)
		case "modify":
			modifyFile(dir, a.path, a.content)
			pendingFiles = append(pendingFiles, a.path)
		case "delete":
			deleteFile(dir, a.path)
			pendingFiles = append(pendingFiles, a.path)
		case "commit":
			gitCommit(dir, pendingFiles)
			pendingFiles = nil
		}
	}
}

func createFile(dir, path, content string) {
	abs := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir %s: %v\n", filepath.Dir(abs), err)
		return
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", abs, err)
	}
}

func modifyFile(dir, path, appendContent string) {
	abs := filepath.Join(dir, path)
	existing, _ := os.ReadFile(abs)
	content := string(existing) + "\n" + appendContent
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", abs, err)
	}
}

func deleteFile(dir, path string) {
	abs := filepath.Join(dir, path)
	if err := os.Remove(abs); err != nil {
		fmt.Fprintf(os.Stderr, "remove %s: %v\n", abs, err)
	}
}

func gitCommit(dir string, files []string) {
	if len(files) == 0 {
		gitRun(dir, "add", ".")
	} else {
		gitRun(dir, append([]string{"add", "--"}, files...)...)
	}
	gitRun(dir, "commit", "-m", "Vogon agent commit")
}

func gitRun(dir string, args ...string) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Signal that this is a non-interactive agent — git hooks should not
	// attempt TTY prompts. GIT_TERMINAL_PROMPT is the standard git env var
	// for this (same approach used by Factory AI Droid).
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "git %s: %v\n", strings.Join(args, " "), err)
	}
}

// --- Hook Firing ---

func fireHook(dir, hookName string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal hook payload: %v\n", err)
		return
	}

	cmd := execx.NonInteractive(context.Background(), "entire", "hooks", "vogon", hookName)
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(data)
	cmd.Env = os.Environ()
	// Capture output but don't show it — hooks may output banners
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hook %s failed: %v\n%s\n", hookName, err, out)
	}
}

// --- Transcript ---

func setupTranscript(sessionID string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		fatal("home dir: %v", err)
	}
	transcriptDir := filepath.Join(home, ".vogon", "sessions")
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		fatal("mkdir transcript dir: %v", err)
	}
	path := filepath.Join(transcriptDir, sessionID+".jsonl")
	// Create empty file
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		fatal("create transcript: %v", err)
	}
	return path
}

type transcriptEntry struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Message   string `json:"message"`
}

func appendTranscript(path, role, content string) {
	entry := transcriptEntry{
		Type:      role,
		Timestamp: time.Now().Format(time.RFC3339),
		Message:   content,
	}
	data, _ := json.Marshal(entry) //nolint:errchkjson // transcriptEntry has no unsafe types
	data = append(data, '\n')

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(data)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "vogon: "+format+"\n", args...)
	os.Exit(1)
}
