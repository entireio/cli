package strategy

import (
	"fmt"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"os"
	"path/filepath"
	"strings"
)

func renderLefthookScript(spec hookSpec) string {
	nativeMarker := "# " + entireHookMarker + "\n"
	lefthookMarker := "# " + lefthookOwnedMarker + "\n"
	content := strings.Replace(spec.content, nativeMarker, lefthookMarker, 1)
	commandNeedle := " hooks git " + spec.name
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if !strings.Contains(line, commandNeedle) {
			continue
		}
		elseAt := strings.LastIndex(line, "; else ")
		if elseAt < 0 || !strings.HasSuffix(line, "; fi") {
			continue
		}
		warning := "[entire] Entire CLI is unavailable; skipping " + spec.name + " hook."
		lines[i] = line[:elseAt] + "; else printf '%s\\n' " + shellQuote(warning) + " >&2 || :; fi"
		break
	}
	return strings.Join(lines, "\n")
}

func lefthookScriptOwned(content string) bool {
	want := "# " + lefthookOwnedMarker
	for line := range strings.SplitSeq(content, "\n") {
		if line == want {
			return true
		}
	}
	return false
}

func inspectLefthookArtifacts(root *os.Root, cmdPrefix string) (bool, error) {
	// Three facts, in the order they become true during an install: Entire's
	// own config is present and current, the user's local config extends it,
	// and every owned script is ours and current. The old check instead walked
	// the user's local config for source_dir_local plus a nested scripts entry
	// per hook, which is what required node-level surgery over their content.
	configCurrent, err := entireLefthookConfigCurrent(root)
	if err != nil || !configCurrent {
		return false, err
	}
	extendsPresent, err := lefthookExtendsEntryPresent(root)
	if err != nil || !extendsPresent {
		return false, err
	}
	for _, spec := range buildHookSpecs(cmdPrefix) {
		scriptName := lefthookScriptPath(spec.name)
		content, info, readErr := readOptionalRegular(root, scriptName)
		if readErr != nil {
			return false, fmt.Errorf("read %s: %w", scriptName, readErr)
		}
		// Any mismatch is "ours but outdated", including a script that lost
		// its ownership marker: by content alone a stale Entire script and a
		// foreign one are indistinguishable, and treating the stale case as a
		// conflict would report a repo Entire itself installed as someone
		// else's. A genuinely foreign script is refused at install time, where
		// installLefthookFilesAt checks ownership before writing.
		if content == nil || info.Mode().Perm()&0o111 == 0 ||
			!lefthookScriptOwned(string(content)) ||
			string(content) != renderLefthookScript(spec) {
			return false, nil
		}
	}
	return true, nil
}

func mergeLefthookInfoExclude(existing []byte) []byte {
	content := string(existing)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	block := lefthookExcludeBlock()
	if strings.Contains(content, block) {
		return []byte(content)
	}
	return []byte(content + block)
}

func lefthookExcludeBlock() string {
	const begin = "# entire-cli-owned:lefthook:v1 begin\n"
	const end = "# entire-cli-owned:lefthook:v1 end\n"
	entries := []string{"/" + lefthookLocalConfigName}
	for _, hook := range gitHookNames {
		entries = append(entries, "/"+filepath.ToSlash(filepath.Join(lefthookLocalDir, hook, lefthookScriptName)))
	}
	var block strings.Builder
	block.WriteString(begin)
	for _, entry := range entries {
		block.WriteString(entry)
		block.WriteByte('\n')
	}
	block.WriteString(end)
	return block.String()
}

func lefthookScriptPath(hook string) string {
	return filepath.ToSlash(filepath.Join(lefthookLocalDir, hook, lefthookScriptName))
}

func readOptionalRegular(root *os.Root, name string) ([]byte, os.FileInfo, error) {
	info, err := osroot.LstatNoSymlinks(root, name)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("lstat %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s is not a regular file", name)
	}
	data, err := osroot.ReadFileNoFollow(root, name)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s without following links: %w", name, err)
	}
	return data, info, nil
}
