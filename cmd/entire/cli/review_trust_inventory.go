package cli

// review_trust_inventory.go lists what a checkout would run during `entire
// review`. It lives here because the agent packages it reads import review.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/agent/pi"
	"github.com/entireio/cli/cmd/entire/cli/gitexec"
	cliReview "github.com/entireio/cli/cmd/entire/cli/review"
	"github.com/entireio/cli/cmd/entire/cli/worktreedir"
	"github.com/entireio/cli/redact"
)

// trustPathKind classifies a repo-relative path in the inspected checkout.
type trustPathKind int

const (
	trustPathAbsent trustPathKind = iota
	trustPathFile
	trustPathDir
	// trustPathOpaque is a symlink, submodule, or anything not followed;
	// reported as unknown, never as absent.
	trustPathOpaque
)

// trustFiles reads a commit's tree or files on disk.
type trustFiles interface {
	kind(rel string) (trustPathKind, error)
	read(rel string) ([]byte, error)
	// files lists the regular files under dir and any opaque entries.
	files(dir string) (regular, opaque []string, err error)
}

const (
	trustClaudeMD = "CLAUDE.md"
	trustAgentsMD = "AGENTS.md"
)

const (
	trustCodexConfig = ".codex/config.toml"
	trustHooksKey    = "hooks"
)

// Inspection reads branch data before approval, so it is bounded: larger files
// are unknown, directories list a limited number of items, and a larger tree
// fails closed.
const (
	trustMaxFileBytes   = 1 << 20
	trustMaxItems       = 100
	trustMaxTreeEntries = 20000
)

var errTrustTooLarge = errors.New("too large to inspect")

// trustRoots are the only paths the inventory reads.
var trustRoots = []string{".claude", ".codex", ".pi", ".agents", ".mcp.json", trustClaudeMD, "CLAUDE.local.md", trustAgentsMD, "AGENTS.override.md"}

// inspectReviewTrust implements review.Deps.InspectTrust.
func inspectReviewTrust(ctx context.Context, source cliReview.TrustSource, agents []string) (cliReview.TrustInventory, error) {
	var files trustFiles
	if source.Commit != "" {
		tree, err := loadGitTrustTree(ctx, source.RepoRoot, source.Commit)
		if err != nil {
			return cliReview.TrustInventory{}, err
		}
		files = tree
	} else {
		root, err := worktreedir.OpenAt(source.WorktreeRoot)
		if err != nil {
			return cliReview.TrustInventory{}, fmt.Errorf("open worktree: %w", err)
		}
		files = diskTrustFiles{root: root}
	}
	return buildTrustInventory(files, agents)
}

func buildTrustInventory(files trustFiles, agents []string) (cliReview.TrustInventory, error) {
	var inv cliReview.TrustInventory
	for _, name := range agents {
		var (
			entries []cliReview.TrustEntry
			err     error
		)
		switch name {
		case string(agent.AgentNameClaudeCode):
			entries, err = claudeTrustEntries(files)
		case string(agent.AgentNameCodex):
			entries, err = codexTrustEntries(files)
		case string(agent.AgentNamePi):
			entries, err = piTrustEntries(files)
		default:
			// Other agents have no reviewer runner and run nothing.
			continue
		}
		if err != nil {
			return cliReview.TrustInventory{}, err
		}
		// Name-based hiding misses secrets in positional arguments, hook
		// commands, and innocuously named keys, so everything displayed also
		// goes through content-based redaction, which replaces only the
		// secret span.
		for i := range entries {
			entries[i].Name = redact.String(entries[i].Name)
			entries[i].Command = redact.String(entries[i].Command)
		}
		inv.Entries = append(inv.Entries, entries...)
	}
	for _, rel := range []string{trustClaudeMD, "CLAUDE.local.md", ".claude/" + trustClaudeMD, trustAgentsMD, "AGENTS.override.md"} {
		k, err := files.kind(rel)
		if err != nil {
			return cliReview.TrustInventory{}, err
		}
		if k != trustPathAbsent {
			inv.Instructions = append(inv.Instructions, rel)
		}
	}
	return inv, nil
}

func unknownTrustEntry(agentName, source, reason string) cliReview.TrustEntry {
	return cliReview.TrustEntry{Agent: agentName, Kind: cliReview.TrustKindUnknown, Name: source, Command: reason, Source: source}
}

// readTrustFile reads rel; anything uninspectable becomes an unknown entry.
func readTrustFile(files trustFiles, agentName, rel string) (data []byte, entries []cliReview.TrustEntry, ok bool, err error) {
	k, err := files.kind(rel)
	if err != nil {
		return nil, nil, false, err
	}
	switch k {
	case trustPathAbsent:
		return nil, nil, false, nil
	case trustPathFile:
		data, err := files.read(rel)
		if errors.Is(err, errTrustTooLarge) {
			return nil, []cliReview.TrustEntry{unknownTrustEntry(agentName, rel, errTrustTooLarge.Error())}, false, nil
		}
		if err != nil {
			return nil, nil, false, err
		}
		return data, nil, true, nil
	case trustPathDir, trustPathOpaque:
		return nil, []cliReview.TrustEntry{unknownTrustEntry(agentName, rel, "symlink or non-file; not inspected")}, false, nil
	}
	return nil, nil, false, nil
}

// --- Claude Code ---

// claudeIgnoredSettings run nothing and grant nothing. Every other key is
// listed, so new command-bearing settings are covered by default.
var claudeIgnoredSettings = []string{
	"$schema", "model", "cleanupPeriodDays", "includeCoAuthoredBy", "includeGitInstructions",
	"outputStyle", "language", "alwaysThinkingEnabled", "spinnerTipsEnabled", "respectGitignore",
	"attribution", "companyAnnouncements", "disableAllHooks",
}

// claudeRiskyPermissionTools let the model run or change things unasked.
var claudeRiskyPermissionTools = []string{"Bash", "Write", "Edit", "MultiEdit", "NotebookEdit", "WebFetch", "mcp__"}

func claudeTrustEntries(files trustFiles) ([]cliReview.TrustEntry, error) {
	const agentName = string(agent.AgentNameClaudeCode)
	canonical := claudecode.EntireHookCommands()
	var out []cliReview.TrustEntry
	for _, rel := range []string{".claude/settings.json", ".claude/settings.local.json"} {
		data, unknown, ok, err := readTrustFile(files, agentName, rel)
		if err != nil {
			return nil, err
		}
		out = append(out, unknown...)
		if !ok {
			continue
		}
		var settings map[string]json.RawMessage
		if err := json.Unmarshal(data, &settings); err != nil {
			out = append(out, unknownTrustEntry(agentName, rel, "could not parse"))
			continue
		}
		for _, key := range trustSortedKeys(settings) {
			raw := settings[key]
			switch {
			case key == trustHooksKey:
				hooks, ok := hookTrustEntries(agentName, rel, raw, canonical)
				if !ok {
					out = append(out, unknownTrustEntry(agentName, rel, "could not parse hooks"))
				}
				out = append(out, hooks...)
			case key == "permissions":
				perms, ok := claudePermissionEntries(rel, raw)
				if !ok {
					out = append(out, unknownTrustEntry(agentName, rel, "could not parse permissions"))
				}
				out = append(out, perms...)
			case key == "env":
				var env map[string]json.RawMessage
				if err := json.Unmarshal(raw, &env); err != nil {
					out = append(out, unknownTrustEntry(agentName, rel, "could not parse env"))
					continue
				}
				for _, name := range trustSortedKeys(env) {
					out = append(out, cliReview.TrustEntry{Agent: agentName, Kind: cliReview.TrustKindSetting, Name: "env " + name, Command: trustHiddenValue, Source: rel})
				}
			case slices.Contains(claudeIgnoredSettings, key):
			default:
				out = append(out, cliReview.TrustEntry{Agent: agentName, Kind: cliReview.TrustKindSetting, Name: key, Command: trustSettingValue(key, raw), Source: rel})
			}
		}
	}

	data, unknown, ok, err := readTrustFile(files, agentName, ".mcp.json")
	if err != nil {
		return nil, err
	}
	out = append(out, unknown...)
	if ok {
		doc, docOK := jsonObject(data)
		servers, serversOK := jsonObject(doc["mcpServers"])
		switch {
		case !docOK || (doc["mcpServers"] != nil && !serversOK):
			out = append(out, unknownTrustEntry(agentName, ".mcp.json", "could not parse"))
		default:
			for _, name := range trustSortedKeys(servers) {
				out = append(out, cliReview.TrustEntry{Agent: agentName, Kind: cliReview.TrustKindMCP, Name: name, Command: mcpServerCommand(servers[name]), Source: ".mcp.json"})
			}
		}
	}
	return append(out, instructionDirEntries(files, agentName, claudeInstructionDirs)...), nil
}

// claudeInstructionDirs hold skills, commands, subagents, and local plugins.
var claudeInstructionDirs = []string{".claude/commands", ".claude/skills", ".claude/agents", ".claude/plugins"}

func claudePermissionEntries(source string, raw json.RawMessage) ([]cliReview.TrustEntry, bool) {
	perms, ok := jsonObject(raw)
	if !ok {
		return nil, false
	}
	allow, allowOK := jsonStrings(perms["allow"])
	defaultMode, modeOK := jsonString(perms["defaultMode"])
	if !allowOK || !modeOK {
		return nil, false
	}
	const agentName = string(agent.AgentNameClaudeCode)
	var out []cliReview.TrustEntry
	for _, rule := range allow {
		for _, tool := range claudeRiskyPermissionTools {
			if strings.HasPrefix(rule, tool) {
				out = append(out, cliReview.TrustEntry{Agent: agentName, Kind: cliReview.TrustKindSetting, Name: "permissions.allow", Command: rule, Source: source})
				break
			}
		}
	}
	if defaultMode != "" && defaultMode != "default" {
		out = append(out, cliReview.TrustEntry{Agent: agentName, Kind: cliReview.TrustKindSetting, Name: "permissions.defaultMode", Command: defaultMode, Source: source})
	}
	if dirs, ok := jsonStrings(perms["additionalDirectories"]); !ok || len(dirs) > 0 {
		out = append(out, cliReview.TrustEntry{Agent: agentName, Kind: cliReview.TrustKindSetting, Name: "permissions.additionalDirectories", Command: compactJSON(perms["additionalDirectories"]), Source: source})
	}
	return out, true
}

// hookTrustEntries reads the hooks object shared by Claude Code and Codex. A
// hook is Entire's only when its whole command equals one Entire installs.
func hookTrustEntries(agentName, source string, raw json.RawMessage, canonical map[string][]string) ([]cliReview.TrustEntry, bool) {
	// Exact-key maps, as the agents read them: Go structs match keys
	// case-insensitively, so a "Hooks": [] decoy could hide "hooks".
	events, ok := jsonObject(raw)
	if !ok {
		return nil, false
	}
	var out []cliReview.TrustEntry
	for _, event := range trustSortedKeys(events) {
		groups, ok := jsonObjects(events[event])
		if !ok {
			return out, false
		}
		for _, group := range groups {
			hooks, ok := jsonObjects(group["hooks"])
			if !ok {
				return out, false
			}
			for _, hook := range hooks {
				hookType, typeOK := jsonString(hook["type"])
				command, commandOK := jsonString(hook["command"])
				shown := command
				if shown == "" || !commandOK {
					shown = trustSettingValue("", mustMarshal(hook))
				}
				entire := typeOK && commandOK && (hookType == "" || hookType == "command") &&
					command != "" && slices.Contains(canonical[event], command)
				out = append(out, cliReview.TrustEntry{Agent: agentName, Kind: cliReview.TrustKindHook, Name: event, Command: shown, Source: source, Entire: entire})
			}
		}
	}
	return out, true
}

func mcpServerCommand(raw json.RawMessage) string {
	server, ok := jsonObject(raw)
	if !ok {
		return compactJSON(raw)
	}
	command, _ := jsonString(server["command"])
	args, argsOK := jsonStrings(server["args"])
	serverURL, _ := jsonString(server["url"])
	switch {
	case command != "" && argsOK:
		// The server's env can change what the command does (NODE_OPTIONS,
		// LD_PRELOAD), so its names are shown; values stay hidden like any env.
		parts := mcpServerEnvNames(server["env"])
		parts = append(parts, command)
		return strings.Join(append(parts, hideTrustSecretArgs(args)...), " ")
	case command == "" && serverURL != "":
		return hideTrustURLSecrets(serverURL)
	default:
		return trustSettingValue("", raw)
	}
}

// mcpServerEnvNames renders an MCP server's env as NAME=hidden words, sorted.
func mcpServerEnvNames(raw json.RawMessage) []string {
	env, ok := jsonObject(raw)
	if !ok {
		return nil
	}
	names := slices.Sorted(maps.Keys(env))
	for i, name := range names {
		names[i] = name + "=" + trustHiddenValue
	}
	return names
}

// instructionDirEntries lists each item directly under dirs. These files can
// carry their own hooks or shell expansions, so they are listed, not parsed.
func instructionDirEntries(files trustFiles, agentName string, dirs []string) []cliReview.TrustEntry {
	var out []cliReview.TrustEntry
	for _, dir := range dirs {
		regular, opaque, err := files.files(dir)
		if err != nil {
			out = append(out, unknownTrustEntry(agentName, dir, "could not list"))
			continue
		}
		for _, rel := range opaque {
			out = append(out, unknownTrustEntry(agentName, rel, "symlink or non-file; not inspected"))
		}
		seen := map[string]bool{}
		for _, rel := range regular {
			item, _, _ := strings.Cut(strings.TrimPrefix(rel, dir+"/"), "/")
			if seen[item] {
				continue
			}
			seen[item] = true
			if len(seen) > trustMaxItems {
				out = append(out, unknownTrustEntry(agentName, dir, fmt.Sprintf("more than %d items; not all listed", trustMaxItems)))
				break
			}
			out = append(out, cliReview.TrustEntry{Agent: agentName, Kind: cliReview.TrustKindSkill, Name: item, Command: dir + "/" + item, Source: dir})
		}
	}
	return out
}

// --- Codex ---

func codexTrustEntries(files trustFiles) ([]cliReview.TrustEntry, error) {
	const agentName = string(agent.AgentNameCodex)
	var out []cliReview.TrustEntry
	data, unknown, ok, err := readTrustFile(files, agentName, ".codex/hooks.json")
	if err != nil {
		return nil, err
	}
	out = append(out, unknown...)
	if ok {
		doc, docOK := jsonObject(data)
		if !docOK {
			out = append(out, unknownTrustEntry(agentName, ".codex/hooks.json", "could not parse"))
		} else if raw := doc[trustHooksKey]; len(raw) > 0 && !isJSONNull(raw) {
			hooks, ok := hookTrustEntries(agentName, ".codex/hooks.json", raw, codex.EntireHookCommands())
			if !ok {
				out = append(out, unknownTrustEntry(agentName, ".codex/hooks.json", "could not parse hooks"))
			}
			out = append(out, hooks...)
		}
	}

	// Entire writes nothing here, so every key is the branch's.
	data, unknown, ok, err = readTrustFile(files, agentName, trustCodexConfig)
	if err != nil {
		return nil, err
	}
	out = append(out, unknown...)
	if ok {
		config, parsed := decodeTrustDocument[map[string]any](data, toml.Unmarshal)
		if !parsed {
			out = append(out, unknownTrustEntry(agentName, trustCodexConfig, "could not parse"))
			return append(out, instructionDirEntries(files, agentName, codexInstructionDirs)...), nil
		}
		for _, key := range trustSortedKeys(config) {
			if key == "features" {
				out = append(out, codexFeatureEntries(config[key])...)
				continue
			}
			if key == "mcp_servers" {
				servers, isMap := config[key].(map[string]any)
				if isMap {
					for _, name := range trustSortedKeys(servers) {
						out = append(out, cliReview.TrustEntry{Agent: agentName, Kind: cliReview.TrustKindMCP, Name: name, Command: mcpServerCommand(mustMarshal(servers[name])), Source: trustCodexConfig})
					}
					continue
				}
			}
			out = append(out, cliReview.TrustEntry{Agent: agentName, Kind: cliReview.TrustKindSetting, Name: key, Command: trustSettingValue(key, mustMarshal(config[key])), Source: trustCodexConfig})
		}
	}
	return append(out, instructionDirEntries(files, agentName, codexInstructionDirs)...), nil
}

// codexInstructionDirs hold Codex skills, prompts, and subagents.
var codexInstructionDirs = []string{".codex/skills", ".codex/prompts", ".codex/agents", ".agents/skills"}

// codexHookFeatureFlags only switch on hooks, which hooks.json already lists.
var codexHookFeatureFlags = []string{"hooks", "codex_hooks"}

func codexFeatureEntries(value any) []cliReview.TrustEntry {
	const agentName = string(agent.AgentNameCodex)
	features, ok := value.(map[string]any)
	if !ok {
		return []cliReview.TrustEntry{{Agent: agentName, Kind: cliReview.TrustKindSetting, Name: "features", Command: compactJSON(mustMarshal(value)), Source: trustCodexConfig}}
	}
	var out []cliReview.TrustEntry
	for _, name := range trustSortedKeys(features) {
		if enabled, isBool := features[name].(bool); isBool && enabled && slices.Contains(codexHookFeatureFlags, name) {
			continue
		}
		out = append(out, cliReview.TrustEntry{Agent: agentName, Kind: cliReview.TrustKindSetting, Name: "features." + name, Command: compactJSON(mustMarshal(features[name])), Source: trustCodexConfig})
	}
	return out
}

// --- Pi ---

func piTrustEntries(files trustFiles) ([]cliReview.TrustEntry, error) {
	const agentName = string(agent.AgentNamePi)
	var out []cliReview.TrustEntry
	k, err := files.kind(".pi")
	if err != nil {
		return nil, err
	}
	switch k {
	case trustPathAbsent:
		return nil, nil
	case trustPathFile, trustPathOpaque:
		return []cliReview.TrustEntry{unknownTrustEntry(agentName, ".pi", "symlink or non-directory; not inspected")}, nil
	case trustPathDir:
	}
	regular, opaque, err := files.files(".pi/extensions")
	if err != nil {
		return nil, err
	}
	for _, rel := range opaque {
		out = append(out, unknownTrustEntry(agentName, rel, "symlink or non-file; not inspected"))
	}
	for _, rel := range regular {
		entire := false
		if rel == pi.ExtensionRelPath {
			data, err := files.read(rel)
			if err != nil && !errors.Is(err, errTrustTooLarge) {
				return nil, err
			}
			entire = err == nil && pi.IsEntireExtension(data)
		}
		out = append(out, cliReview.TrustEntry{Agent: agentName, Kind: cliReview.TrustKindExtension, Name: path.Base(path.Dir(rel)), Command: rel, Source: rel, Entire: entire})
	}

	// Entire writes nothing here, so every key is listed.
	data, unknown, ok, err := readTrustFile(files, agentName, ".pi/settings.json")
	if err != nil {
		return nil, err
	}
	out = append(out, unknown...)
	if ok {
		settings, parsed := decodeTrustDocument[map[string]json.RawMessage](data, json.Unmarshal)
		if !parsed {
			out = append(out, unknownTrustEntry(agentName, ".pi/settings.json", "could not parse"))
			return append(out, instructionDirEntries(files, agentName, piInstructionDirs)...), nil
		}
		for _, key := range trustSortedKeys(settings) {
			out = append(out, cliReview.TrustEntry{Agent: agentName, Kind: cliReview.TrustKindSetting, Name: key, Command: trustSettingValue(key, settings[key]), Source: ".pi/settings.json"})
		}
	}
	return append(out, instructionDirEntries(files, agentName, piInstructionDirs)...), nil
}

// piInstructionDirs hold Pi skills and prompt templates.
var piInstructionDirs = []string{".pi/skills", ".pi/prompts"}

// --- sources ---

// gitTrustTree is a commit's tree under trustRoots, matched case-insensitively
// as a macOS or Windows checkout loads it; names that collide are opaque.
type gitTrustTree struct {
	ctx      context.Context
	repoRoot string
	// entries is keyed by lower-cased path.
	entries map[string]gitTrustTreeEntry
}

type gitTrustTreeEntry struct {
	mode    string
	oid     string
	size    int64
	collide bool
}

const (
	gitModeTree    = "040000"
	gitModeSymlink = "120000"
	gitModeGitlink = "160000"
)

func loadGitTrustTree(ctx context.Context, repoRoot, commit string) (*gitTrustTree, error) {
	tree := &gitTrustTree{ctx: ctx, repoRoot: repoRoot, entries: map[string]gitTrustTreeEntry{}}
	rootEntries, err := lsTreeRecords(ctx, repoRoot, "ls-tree", "-z", "--full-tree", "--end-of-options", commit)
	if err != nil {
		return nil, fmt.Errorf("list configuration at %s: %w", commit, err)
	}
	var roots []string
	for _, rec := range rootEntries {
		for _, want := range trustRoots {
			if strings.EqualFold(rec.name, want) {
				roots = append(roots, rec.name)
			}
		}
	}
	if len(roots) == 0 {
		return tree, nil
	}
	args := append([]string{"ls-tree", "-r", "-t", "-l", "-z", "--full-tree", "--end-of-options", commit, "--"}, roots...)
	records, err := lsTreeRecords(ctx, repoRoot, args...)
	if err != nil {
		return nil, fmt.Errorf("list configuration at %s: %w", commit, err)
	}
	if len(records) > trustMaxTreeEntries {
		return nil, fmt.Errorf("more than %d configuration entries at %s", trustMaxTreeEntries, commit)
	}
	for _, rec := range records {
		key := strings.ToLower(rec.name)
		if _, dup := tree.entries[key]; dup {
			tree.entries[key] = gitTrustTreeEntry{mode: gitModeSymlink, collide: true}
			continue
		}
		tree.entries[key] = gitTrustTreeEntry{mode: rec.mode, oid: rec.oid, size: rec.size}
	}
	return tree, nil
}

type lsTreeRecord struct {
	mode, oid, name string
	// size is from `ls-tree -l`; -1 for trees.
	size int64
}

func lsTreeRecords(ctx context.Context, repoRoot string, args ...string) ([]lsTreeRecord, error) {
	out, err := gitexec.Run(ctx, repoRoot, args...)
	if err != nil {
		return nil, err //nolint:wrapcheck // callers name the commit
	}
	var records []lsTreeRecord
	for _, record := range strings.Split(out, "\x00") {
		if record == "" {
			continue
		}
		meta, name, found := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if !found || (len(fields) != 3 && len(fields) != 4) {
			return nil, fmt.Errorf("unexpected git ls-tree output %q", record)
		}
		size := int64(-1)
		if len(fields) == 4 && fields[3] != "-" {
			n, err := strconv.ParseInt(fields[3], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("unexpected git ls-tree size %q", record)
			}
			size = n
		}
		records = append(records, lsTreeRecord{mode: fields[0], oid: fields[2], name: name, size: size})
	}
	return records, nil
}

func (t *gitTrustTree) kind(rel string) (trustPathKind, error) {
	parts := strings.Split(strings.ToLower(rel), "/")
	for i := 1; i <= len(parts); i++ {
		entry, ok := t.entries[strings.Join(parts[:i], "/")]
		if !ok {
			return trustPathAbsent, nil
		}
		switch {
		case entry.mode == gitModeSymlink || entry.mode == gitModeGitlink:
			return trustPathOpaque, nil
		case i < len(parts) && entry.mode != gitModeTree:
			return trustPathOpaque, nil
		case i == len(parts) && entry.mode == gitModeTree:
			return trustPathDir, nil
		}
	}
	return trustPathFile, nil
}

func (t *gitTrustTree) read(rel string) ([]byte, error) {
	entry, ok := t.entries[strings.ToLower(rel)]
	if !ok || entry.collide {
		return nil, fmt.Errorf("%s is not readable in the tree", rel)
	}
	if entry.size > trustMaxFileBytes {
		return nil, errTrustTooLarge
	}
	out, err := gitexec.Run(t.ctx, t.repoRoot, "cat-file", "blob", entry.oid)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rel, err)
	}
	return []byte(out), nil
}

func (t *gitTrustTree) files(dir string) (regular, opaque []string, err error) {
	k, err := t.kind(dir)
	if err != nil || k == trustPathAbsent {
		return nil, nil, err
	}
	if k != trustPathDir {
		return nil, []string{dir}, nil
	}
	prefix := strings.ToLower(dir) + "/"
	for name, entry := range t.entries {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rel := dir + "/" + strings.TrimPrefix(name, prefix)
		switch entry.mode {
		case gitModeTree:
		case gitModeSymlink, gitModeGitlink:
			opaque = append(opaque, rel)
		default:
			regular = append(regular, rel)
		}
	}
	sort.Strings(regular)
	sort.Strings(opaque)
	return regular, opaque, nil
}

// diskTrustFiles reads a worktree through its root, never following links.
type diskTrustFiles struct {
	root *os.Root
}

func (d diskTrustFiles) kind(rel string) (trustPathKind, error) {
	parts := strings.Split(rel, "/")
	for i := 1; i <= len(parts); i++ {
		info, err := d.root.Lstat(strings.Join(parts[:i], "/"))
		if errors.Is(err, fs.ErrNotExist) {
			return trustPathAbsent, nil
		}
		if err != nil {
			return trustPathAbsent, fmt.Errorf("inspect %s: %w", rel, err)
		}
		mode := info.Mode()
		switch {
		case mode&fs.ModeSymlink != 0:
			return trustPathOpaque, nil
		case i < len(parts) && !mode.IsDir():
			return trustPathOpaque, nil
		case i == len(parts) && mode.IsDir():
			return trustPathDir, nil
		case i == len(parts) && !mode.IsRegular():
			return trustPathOpaque, nil
		}
	}
	return trustPathFile, nil
}

func (d diskTrustFiles) read(rel string) ([]byte, error) {
	if info, err := d.root.Lstat(rel); err == nil && info.Size() > trustMaxFileBytes {
		return nil, errTrustTooLarge
	}
	data, err := d.root.ReadFile(rel)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rel, err)
	}
	return data, nil
}

func (d diskTrustFiles) files(dir string) (regular, opaque []string, err error) {
	k, err := d.kind(dir)
	if err != nil || k == trustPathAbsent {
		return nil, nil, err
	}
	if k != trustPathDir {
		return nil, []string{dir}, nil
	}
	err = fs.WalkDir(d.root.FS(), dir, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		switch {
		case entry.IsDir():
		case entry.Type().IsRegular():
			regular = append(regular, name)
		default:
			opaque = append(opaque, name)
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("list %s: %w", dir, err)
	}
	return regular, opaque, nil
}

// --- helpers ---

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// jsonObject decodes an object with exact keys; absent input is empty.
func jsonObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if len(raw) == 0 || isJSONNull(raw) {
		return map[string]json.RawMessage{}, true
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, false
	}
	return m, true
}

func jsonObjects(raw json.RawMessage) ([]map[string]json.RawMessage, bool) {
	if len(raw) == 0 || isJSONNull(raw) {
		return nil, true
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, false
	}
	return items, true
}

func jsonString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || isJSONNull(raw) {
		return "", true
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", false
	}
	return v, true
}

func jsonStrings(raw json.RawMessage) ([]string, bool) {
	if len(raw) == 0 || isJSONNull(raw) {
		return nil, true
	}
	var v []string
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false
	}
	return v, true
}

// decodeTrustDocument reports a parse failure as false, so the file is listed
// as unknown instead of failing the inspection.
func decodeTrustDocument[T any](data []byte, unmarshal func([]byte, any) error) (T, bool) {
	var v T
	if err := unmarshal(data, &v); err != nil {
		return v, false
	}
	return v, true
}

func trustSortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func compactJSON(raw []byte) string {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return string(raw)
	}
	return b.String()
}

// trustHiddenValue stands in for a value the inventory must not print. The
// inventory reaches --show-config, the confirm dialog, and agents that are told
// they may relay it, and a file read from disk can be the reviewer's own (e.g.
// the gitignored .claude/settings.local.json holding their API key). So
// credentials are named, never echoed, whichever file they came from.
const trustHiddenValue = "(value hidden)"

// trustSecretMapKeys hold name-to-value maps whose values can be credentials:
// environment variables, HTTP headers, Codex's shell_environment_policy.set.
var trustSecretMapKeys = []string{"env", "headers", "http_headers", "set"}

// trustSecretKeyParts mark a key whose value is a credential.
var trustSecretKeyParts = []string{"token", "secret", "password"}

// trustSecretFlagParts mark a command-line flag or URL query parameter whose
// value is a credential. Broader than trustSecretKeyParts: a flag value is
// never a command we need to show, while a setting like apiKeyHelper is.
var trustSecretFlagParts = append(slices.Clone(trustSecretKeyParts), "key", "auth")

func isTrustSecretFlag(name string) bool {
	lower := strings.ToLower(name)
	return slices.ContainsFunc(trustSecretFlagParts, func(part string) bool { return strings.Contains(lower, part) })
}

// hideTrustSecretArgs hides the values of credential flags, given as
// "--token X" or "--token=X". A secret in a positional argument is not caught.
func hideTrustSecretArgs(args []string) []string {
	out := slices.Clone(args)
	for i := 0; i < len(out); i++ {
		if !strings.HasPrefix(out[i], "-") {
			continue
		}
		flag, _, hasValue := strings.Cut(out[i], "=")
		if !isTrustSecretFlag(strings.TrimLeft(flag, "-")) {
			continue
		}
		if hasValue {
			out[i] = flag + "=" + trustHiddenValue
		} else if i+1 < len(out) && !strings.HasPrefix(out[i+1], "-") {
			i++
			out[i] = trustHiddenValue
		}
	}
	return out
}

// hideTrustURLSecrets drops a URL's password and hides credential query
// parameters.
// A URL that will not parse is hidden whole rather than guessed at.
func hideTrustURLSecrets(raw string) string {
	const hidden = "hidden" // URL-safe, unlike trustHiddenValue
	u, err := url.Parse(raw)
	if err != nil {
		return trustHiddenValue
	}
	changed := false
	if _, hasPassword := u.User.Password(); hasPassword {
		// Dropped, not replaced: content redaction would hide a whole URL that
		// still carries any password.
		u.User = url.User(u.User.Username())
		changed = true
	}
	query := u.Query()
	for name := range query {
		if isTrustSecretFlag(name) {
			query.Set(name, hidden)
			changed = true
		}
	}
	if !changed {
		return raw
	}
	u.RawQuery = query.Encode()
	return u.String()
}

func isTrustSecretKey(key string) bool {
	lower := strings.ToLower(key)
	return slices.ContainsFunc(trustSecretKeyParts, func(part string) bool { return strings.Contains(lower, part) })
}

// trustSettingValue renders a setting's value with credentials hidden; key is
// the setting's own name, or "" when raw is a whole entry.
func trustSettingValue(key string, raw json.RawMessage) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return compactJSON(raw)
	}
	// Wrapped so the setting's own name is judged like a nested key.
	wrapped := map[string]any{key: v}
	if !hideTrustSecrets(wrapped) {
		return compactJSON(raw)
	}
	return compactJSON(mustMarshal(wrapped[key]))
}

// hideTrustSecrets replaces credential values in v in place and reports
// whether it replaced any.
func hideTrustSecrets(v any) bool {
	hid := false
	switch v := v.(type) {
	case map[string]any:
		for key, child := range v {
			switch {
			case slices.Contains(trustSecretMapKeys, strings.ToLower(key)):
				values, isMap := child.(map[string]any)
				if !isMap {
					v[key] = trustHiddenValue
					hid = true
					continue
				}
				for name := range values {
					values[name] = trustHiddenValue
					hid = true
				}
			case isTrustSecretKey(key):
				v[key] = trustHiddenValue
				hid = true
			default:
				hid = hideTrustSecrets(child) || hid
			}
		}
	case []any:
		for _, child := range v {
			hid = hideTrustSecrets(child) || hid
		}
	}
	return hid
}

func mustMarshal(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(fmt.Sprintf("%q", fmt.Sprint(v)))
	}
	return data
}
