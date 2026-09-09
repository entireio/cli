package strategy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"gopkg.in/yaml.v3"
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
	data, _, err := readOptionalRegular(root, lefthookLocalConfigName)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", lefthookLocalConfigName, err)
	}
	if data == nil {
		return false, nil
	}
	config, err := parseYAMLMapping(data, lefthookLocalConfigName)
	if err != nil {
		return false, err
	}
	if current, sourceErr := inspectOwnedLefthookSourceDir(config); sourceErr != nil || !current {
		return current, sourceErr
	}

	for _, spec := range buildHookSpecs(cmdPrefix) {
		entry, owned, found, entryErr := findLefthookScriptEntry(config, spec.name)
		if entryErr != nil {
			return false, entryErr
		}
		if !found {
			return false, nil
		}
		if !owned {
			return false, fmt.Errorf("%s: %w", spec.name, ErrLefthookOwnedEntryConflict)
		}
		if !lefthookEntryCurrent(entry, spec.name) {
			return false, nil
		}

		scriptName := lefthookScriptPath(spec.name)
		content, info, readErr := readOptionalRegular(root, scriptName)
		if readErr != nil {
			return false, fmt.Errorf("read %s: %w", scriptName, readErr)
		}
		if content == nil || string(content) != renderLefthookScript(spec) ||
			!lefthookScriptOwned(string(content)) || info.Mode().Perm()&0o111 == 0 {
			return false, nil
		}
	}
	return true, nil
}

func inspectOwnedLefthookSourceDir(root *yaml.Node) (bool, error) {
	value, found, duplicate := mappingValueCount(root, "source_dir_local")
	if duplicate {
		return false, errors.New("duplicate source_dir_local setting")
	}
	if !found {
		return false, nil
	}
	// A compatible unowned value may predate Entire. The owned hook nodes and
	// scripts prove Entire's participation without claiming this user setting.
	return value.Kind == yaml.ScalarNode && value.Value == lefthookLocalDir, nil
}

func hasOwnedLefthookConfigEntries(data []byte) (bool, error) {
	root, err := parseYAMLMapping(data, lefthookLocalConfigName)
	if err != nil {
		return false, err
	}
	if value, found, duplicate := mappingValueCount(root, "source_dir_local"); duplicate {
		return false, errors.New("duplicate source_dir_local setting")
	} else if found {
		key := mappingKey(root, "source_dir_local")
		if key != nil && lefthookNodeOwned(key, value) {
			return true, nil
		}
	}
	for _, hook := range gitHookNames {
		_, owned, found, findErr := findLefthookScriptEntry(root, hook)
		if findErr != nil {
			return false, findErr
		}
		if found && owned {
			return true, nil
		}
	}
	return false, nil
}

func findLefthookScriptEntry(root *yaml.Node, hook string) (*yaml.Node, bool, bool, error) {
	hookNode, found, duplicate := mappingValueCount(root, hook)
	if duplicate {
		return nil, false, false, fmt.Errorf("duplicate %s mapping", hook)
	}
	if !found || hookNode.Kind != yaml.MappingNode {
		return nil, false, false, nil
	}
	scripts, found, duplicate := mappingValueCount(hookNode, "scripts")
	if duplicate {
		return nil, false, false, fmt.Errorf("duplicate scripts mapping in %s", hook)
	}
	if !found || scripts.Kind != yaml.MappingNode {
		return nil, false, false, nil
	}
	var key, value *yaml.Node
	for i := 0; i+1 < len(scripts.Content); i += 2 {
		if scripts.Content[i].Value != lefthookScriptName {
			continue
		}
		if key != nil {
			return nil, false, false, fmt.Errorf("duplicate %s in %s scripts", lefthookScriptName, hook)
		}
		key, value = scripts.Content[i], scripts.Content[i+1]
	}
	if key == nil {
		return nil, false, false, nil
	}
	return value, lefthookNodeOwned(key, value), true, nil
}

func mappingValue(parent *yaml.Node, key string) (*yaml.Node, bool) {
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			return parent.Content[i+1], true
		}
	}
	return nil, false
}

func mappingKey(parent *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			return parent.Content[i]
		}
	}
	return nil
}

func lefthookRunnerIsBash(entry *yaml.Node) bool {
	if entry.Kind != yaml.MappingNode {
		return false
	}
	runner, ok := mappingValue(entry, "runner")
	return ok && runner.Kind == yaml.ScalarNode && runner.Value == "bash"
}

func lefthookEntryCurrent(entry *yaml.Node, hook string) bool {
	if !lefthookRunnerIsBash(entry) {
		return false
	}
	useStdin, found, duplicate := mappingValueCount(entry, "use_stdin")
	if duplicate {
		return false
	}
	if hook == postRewriteHookName {
		return found && useStdin.Kind == yaml.ScalarNode && useStdin.Tag == "!!bool" && useStdin.Value == "true"
	}
	return !found
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
