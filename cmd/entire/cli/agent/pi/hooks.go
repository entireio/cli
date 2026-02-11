package pi

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Ensure PiAgent implements HookSupport and HookHandler
var (
	_ agent.HookSupport = (*PiAgent)(nil)
	_ agent.HookHandler = (*PiAgent)(nil)
)

// Pi hook names - these become subcommands under `entire hooks pi`
const (
	HookNameSessionStart     = "session-start"
	HookNameSessionEnd       = "session-end"
	HookNameStop             = "stop"
	HookNameUserPromptSubmit = "user-prompt-submit"
	HookNameBeforeTool       = "before-tool"
	HookNameAfterTool        = "after-tool"
)

const (
	piExtensionDirName   = "extensions"
	managedExtensionDir  = "entire"
	managedExtensionFile = "index.ts"
	managedMarker        = "entire-managed: pi-extension-v1"
)

var requiredHookSignatures = []string{
	"hooks pi session-start",
	"hooks pi user-prompt-submit",
	"hooks pi before-tool",
	"hooks pi after-tool",
	"hooks pi stop",
	"hooks pi session-end",
}

// GetHookNames returns the hook verbs Pi supports.
// These become subcommands: entire hooks pi <verb>
func (p *PiAgent) GetHookNames() []string {
	return []string{
		HookNameSessionStart,
		HookNameSessionEnd,
		HookNameStop,
		HookNameUserPromptSubmit,
		HookNameBeforeTool,
		HookNameAfterTool,
	}
}

// InstallHooks installs (or updates) the managed Pi extension scaffold.
// Pi uses extensions for lifecycle integration, so Entire manages a local extension file.
func (p *PiAgent) InstallHooks(localDev bool, force bool) (int, error) {
	repoRoot, err := getRepoRootOrCWD()
	if err != nil {
		return 0, err
	}

	entryPath := managedExtensionPath(repoRoot)
	desiredContent := renderManagedExtension(repoRoot, localDev)

	existingData, err := os.ReadFile(entryPath) //nolint:gosec // path is derived from repo root + fixed location
	if err != nil {
		if !os.IsNotExist(err) {
			return 0, fmt.Errorf("failed to read existing extension file: %w", err)
		}

		for _, path := range discoverExtensionFiles(repoRoot) {
			data, readErr := os.ReadFile(path) //nolint:gosec // discovered from fixed extension directory patterns
			if readErr != nil {
				continue
			}
			if hasRequiredSignatures(string(data)) {
				// Entire hooks are already installed via a user-managed extension.
				return 0, nil
			}
		}

		if err := writeManagedExtension(entryPath, desiredContent); err != nil {
			return 0, err
		}
		return 1, nil
	}

	existingContent := string(existingData)
	isManaged := strings.Contains(existingContent, managedMarker)

	if !isManaged && !force {
		// Preserve user-owned extension by default.
		return 0, nil
	}

	if isManaged && existingContent == desiredContent {
		return 0, nil
	}

	if err := writeManagedExtension(entryPath, desiredContent); err != nil {
		return 0, err
	}

	return 1, nil
}

// UninstallHooks removes only the Entire-managed scaffold file.
func (p *PiAgent) UninstallHooks() error {
	repoRoot, err := getRepoRootOrCWD()
	if err != nil {
		return err
	}

	entryPath := managedExtensionPath(repoRoot)
	data, err := os.ReadFile(entryPath) //nolint:gosec // path is derived from repo root + fixed location
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read extension file: %w", err)
	}

	if !strings.Contains(string(data), managedMarker) {
		// User-owned extension file - never remove.
		return nil
	}

	if err := os.Remove(entryPath); err != nil {
		return fmt.Errorf("failed to remove managed extension file: %w", err)
	}

	if err := removeDirIfEmpty(filepath.Dir(entryPath)); err != nil {
		return err
	}
	if err := removeDirIfEmpty(filepath.Dir(filepath.Dir(entryPath))); err != nil {
		return err
	}

	return nil
}

// AreHooksInstalled returns true when either:
// - the Entire-managed extension scaffold exists, or
// - a user-owned extension contains explicit Entire Pi hook command signatures.
func (p *PiAgent) AreHooksInstalled() bool {
	repoRoot, err := getRepoRootOrCWD()
	if err != nil {
		return false
	}

	entryPath := managedExtensionPath(repoRoot)
	if data, readErr := os.ReadFile(entryPath); readErr == nil && strings.Contains(string(data), managedMarker) { //nolint:gosec // fixed path under repo
		return true
	}

	for _, path := range discoverExtensionFiles(repoRoot) {
		data, readErr := os.ReadFile(path) //nolint:gosec // discovered from fixed extension directory patterns
		if readErr != nil {
			continue
		}
		if hasRequiredSignatures(string(data)) {
			return true
		}
	}

	return false
}

// GetSupportedHooks returns the hook types Pi supports.
func (p *PiAgent) GetSupportedHooks() []agent.HookType {
	return []agent.HookType{
		agent.HookSessionStart,
		agent.HookSessionEnd,
		agent.HookUserPromptSubmit,
		agent.HookPreToolUse,
		agent.HookPostToolUse,
		agent.HookStop,
	}
}

func getRepoRootOrCWD() (string, error) {
	repoRoot, err := paths.RepoRoot()
	if err == nil {
		return repoRoot, nil
	}

	cwd, cwdErr := os.Getwd() //nolint:forbidigo // Intentional fallback when RepoRoot() fails (tests, non-git dirs)
	if cwdErr != nil {
		return "", fmt.Errorf("failed to determine repository path: %w", cwdErr)
	}
	return cwd, nil
}

func managedExtensionPath(repoRoot string) string {
	return filepath.Join(repoRoot, ".pi", piExtensionDirName, managedExtensionDir, managedExtensionFile)
}

func writeManagedExtension(path string, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("failed to create extension directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("failed to write extension file: %w", err)
	}
	return nil
}

func removeDirIfEmpty(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to inspect directory %q: %w", path, err)
	}
	if len(entries) > 0 {
		return nil
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to remove empty directory %q: %w", path, err)
	}
	return nil
}

func discoverExtensionFiles(repoRoot string) []string {
	patterns := []string{
		filepath.Join(repoRoot, ".pi", piExtensionDirName, "*.ts"),
		filepath.Join(repoRoot, ".pi", piExtensionDirName, "*", "index.ts"),
	}

	seen := make(map[string]struct{})
	var files []string
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, match := range matches {
			if _, ok := seen[match]; ok {
				continue
			}
			seen[match] = struct{}{}
			files = append(files, match)
		}
	}
	return files
}

func hasRequiredSignatures(content string) bool {
	for _, sig := range requiredHookSignatures {
		if !strings.Contains(content, sig) {
			return false
		}
	}
	return true
}

func renderManagedExtension(repoRoot string, localDev bool) string {
	goMainPath := filepath.Join(repoRoot, "cmd", "entire", "main.go")

	return fmt.Sprintf(`// Managed by Entire CLI
// %s
//
// Hook contract signatures (for detection):
// entire hooks pi session-start
// entire hooks pi user-prompt-submit
// entire hooks pi before-tool
// entire hooks pi after-tool
// entire hooks pi stop
// entire hooks pi session-end

import type { ExtensionAPI, ExtensionContext } from "@mariozechner/pi-coding-agent";
import type { SessionEntry } from "@mariozechner/pi-coding-agent";
import { spawn } from "node:child_process";

type HookVerb = "session-start" | "user-prompt-submit" | "before-tool" | "after-tool" | "stop" | "session-end";

type HookPayload = {
	session_id?: string;
	transcript_path?: string;
	prompt?: string;
	modified_files?: string[];
	tool_name?: string;
	tool_use_id?: string;
	tool_input?: unknown;
	tool_response?: unknown;
};

type HookResponse = {
	systemMessage?: string;
};

const LOCAL_DEV = %t;
const REPO_ROOT = %q;
const GO_MAIN_PATH = %q;

function getHookCommand(verb: HookVerb): { cmd: string; args: string[] } {
	if (LOCAL_DEV) {
		return {
			cmd: "go",
			args: ["run", GO_MAIN_PATH, "hooks", "pi", verb],
		};
	}
	return {
		cmd: "entire",
		args: ["hooks", "pi", verb],
	};
}

function extractModifiedFilesFromBranch(entries: SessionEntry[]): string[] {
	const files = new Set<string>();

	for (const entry of entries) {
		if (entry.type !== "message") {
			continue;
		}

		const message = entry.message as {
			role?: string;
			toolName?: string;
			details?: Record<string, unknown>;
			content?: unknown;
		};

		if (message.role === "toolResult" && (message.toolName === "write" || message.toolName === "edit")) {
			const details = message.details;
			const path = typeof details?.path === "string" ? details.path : undefined;
			if (path) {
				files.add(path);
			}
		}

		if (message.role === "assistant" && Array.isArray(message.content)) {
			for (const block of message.content) {
				const b = block as {
					type?: string;
					name?: string;
					arguments?: Record<string, unknown>;
				};
				if (b.type !== "toolCall") {
					continue;
				}
				if (b.name !== "write" && b.name !== "edit") {
					continue;
				}
				const path = typeof b.arguments?.path === "string" ? b.arguments.path : undefined;
				if (path) {
					files.add(path);
				}
			}
		}
	}

	return Array.from(files);
}

async function runHook(verb: HookVerb, payload: HookPayload, ctx: ExtensionContext): Promise<void> {
	const hook = getHookCommand(verb);

	await new Promise<void>((resolve) => {
		const child = spawn(hook.cmd, hook.args, {
			cwd: REPO_ROOT,
			stdio: ["pipe", "pipe", "pipe"],
			env: { ...process.env },
		});

		let stdout = "";
		let stderr = "";

		child.stdout?.on("data", (chunk) => {
			stdout += String(chunk);
		});
		child.stderr?.on("data", (chunk) => {
			stderr += String(chunk);
		});

		child.on("error", (err) => {
			console.warn("[entire] failed to execute hook " + verb + ": " + err.message);
			resolve();
		});

		child.on("close", (code) => {
			if (code !== 0) {
				const details = stderr.trim();
				if (details) {
					console.warn("[entire] hook " + verb + " failed: " + details);
				}
				resolve();
				return;
			}

			const trimmed = stdout.trim();
			if (!trimmed) {
				resolve();
				return;
			}

			try {
				const response = JSON.parse(trimmed) as HookResponse;
				if (response.systemMessage && ctx.hasUI) {
					ctx.ui.notify(response.systemMessage, "info");
				}
			} catch {
				// Ignore non-JSON output; hook execution succeeded.
			}
			resolve();
		});

		child.stdin?.end(JSON.stringify(payload));
	});
}

function readSessionState(ctx: ExtensionContext): { sessionId?: string; transcriptPath?: string } {
	return {
		sessionId: ctx.sessionManager.getSessionId() || undefined,
		transcriptPath: ctx.sessionManager.getSessionFile() || undefined,
	};
}

export default function register(pi: ExtensionAPI) {
	let activeSessionId: string | undefined;
	let activeTranscriptPath: string | undefined;

	pi.on("session_start", async (_event, ctx) => {
		const state = readSessionState(ctx);
		activeSessionId = state.sessionId;
		activeTranscriptPath = state.transcriptPath;

		await runHook("session-start", {
			session_id: activeSessionId,
			transcript_path: activeTranscriptPath,
		}, ctx);
	});

	pi.on("session_switch", async (event, ctx) => {
		const previousSessionId = activeSessionId;
		const previousTranscriptPath = event.previousSessionFile || activeTranscriptPath;

		if (previousSessionId || previousTranscriptPath) {
			await runHook("session-end", {
				session_id: previousSessionId,
				transcript_path: previousTranscriptPath,
			}, ctx);
		}

		const state = readSessionState(ctx);
		activeSessionId = state.sessionId;
		activeTranscriptPath = state.transcriptPath;

		await runHook("session-start", {
			session_id: activeSessionId,
			transcript_path: activeTranscriptPath,
		}, ctx);
	});

	pi.on("before_agent_start", async (event, ctx) => {
		const state = readSessionState(ctx);
		activeSessionId = state.sessionId;
		activeTranscriptPath = state.transcriptPath;

		await runHook("user-prompt-submit", {
			session_id: activeSessionId,
			transcript_path: activeTranscriptPath,
			prompt: event.prompt,
		}, ctx);
	});

	pi.on("tool_call", async (event, ctx) => {
		const state = readSessionState(ctx);
		activeSessionId = state.sessionId;
		activeTranscriptPath = state.transcriptPath;

		await runHook("before-tool", {
			session_id: activeSessionId,
			transcript_path: activeTranscriptPath,
			tool_name: event.toolName,
			tool_use_id: event.toolCallId,
			tool_input: event.input,
		}, ctx);
	});

	pi.on("tool_result", async (event, ctx) => {
		const state = readSessionState(ctx);
		activeSessionId = state.sessionId;
		activeTranscriptPath = state.transcriptPath;

		await runHook("after-tool", {
			session_id: activeSessionId,
			transcript_path: activeTranscriptPath,
			tool_name: event.toolName,
			tool_use_id: event.toolCallId,
			tool_input: event.input,
			tool_response: {
				content: event.content,
				is_error: event.isError,
				details: event.details,
			},
		}, ctx);
	});

	pi.on("agent_end", async (_event, ctx) => {
		const state = readSessionState(ctx);
		activeSessionId = state.sessionId;
		activeTranscriptPath = state.transcriptPath;

		const modifiedFiles = extractModifiedFilesFromBranch(ctx.sessionManager.getBranch());

		await runHook("stop", {
			session_id: activeSessionId,
			transcript_path: activeTranscriptPath,
			modified_files: modifiedFiles,
		}, ctx);
	});

	pi.on("session_shutdown", async (_event, ctx) => {
		const state = readSessionState(ctx);
		const sessionId = activeSessionId || state.sessionId;
		const transcriptPath = activeTranscriptPath || state.transcriptPath;

		await runHook("session-end", {
			session_id: sessionId,
			transcript_path: transcriptPath,
		}, ctx);
	});
}
`, managedMarker, localDev, repoRoot, goMainPath)
}
