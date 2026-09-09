package strategy

import (
	"bytes"
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

func mergeLefthookLocalConfig(existing []byte) ([]byte, error) {
	document := &yaml.Node{}
	if len(bytes.TrimSpace(existing)) == 0 {
		document.Kind = yaml.DocumentNode
		document.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	} else if err := yaml.Unmarshal(existing, document); err != nil {
		return nil, fmt.Errorf("parse %s: %w", lefthookLocalConfigName, err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("parse %s: top level must be a mapping", lefthookLocalConfigName)
	}

	root := document.Content[0]
	if err := setOwnedLefthookSourceDir(root); err != nil {
		return nil, err
	}
	for _, hook := range gitHookNames {
		hookNode, err := ensureMappingValue(root, hook)
		if err != nil {
			return nil, err
		}
		scriptsNode, err := ensureMappingValue(hookNode, "scripts")
		if err != nil {
			return nil, err
		}
		if err := setOwnedLefthookScript(scriptsNode, hook); err != nil {
			return nil, fmt.Errorf("%s: %w", hook, err)
		}
	}

	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(document); err != nil {
		return nil, fmt.Errorf("encode %s: %w", lefthookLocalConfigName, err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("encode %s: %w", lefthookLocalConfigName, err)
	}
	return out.Bytes(), nil
}

func ensureMappingValue(parent *yaml.Node, key string) (*yaml.Node, error) {
	foundAt := -1
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value != key {
			continue
		}
		if foundAt >= 0 {
			return nil, fmt.Errorf("duplicate %s mapping", key)
		}
		foundAt = i
	}
	if foundAt >= 0 {
		value := parent.Content[foundAt+1]
		if value.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s must be a mapping", key)
		}
		return value, nil
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	valueNode := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	parent.Content = append(parent.Content, keyNode, valueNode)
	return valueNode, nil
}

func setOwnedLefthookSourceDir(root *yaml.Node) error {
	foundAt := -1
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "source_dir_local" {
			continue
		}
		if foundAt >= 0 {
			return errors.New("duplicate source_dir_local setting")
		}
		foundAt = i
	}
	if foundAt < 0 {
		key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "source_dir_local", LineComment: lefthookOwnedMarker}
		value := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: lefthookLocalDir}
		root.Content = append([]*yaml.Node{key, value}, root.Content...)
		return nil
	}
	key, value := root.Content[foundAt], root.Content[foundAt+1]
	owned := lefthookNodeOwned(key, value)
	if value.Kind != yaml.ScalarNode || value.Value != lefthookLocalDir {
		if !owned {
			return fmt.Errorf("source_dir_local: %w", ErrLefthookOwnedEntryConflict)
		}
		value.Kind = yaml.ScalarNode
		value.Tag = "!!str"
		value.Value = lefthookLocalDir
	}
	// A user-selected compatible source directory is sufficient for Entire's
	// participant config. Do not mark it: ownership would make uninstall remove
	// a setting the user created independently.
	return nil
}

func setOwnedLefthookScript(scripts *yaml.Node, hook string) error {
	foundAt := -1
	for i := 0; i+1 < len(scripts.Content); i += 2 {
		key := scripts.Content[i]
		if key.Value != lefthookScriptName {
			continue
		}
		if foundAt >= 0 {
			return ErrLefthookOwnedEntryConflict
		}
		foundAt = i
		value := scripts.Content[i+1]
		if !lefthookNodeOwned(key, value) {
			return ErrLefthookOwnedEntryConflict
		}
	}
	if foundAt >= 0 {
		key := scripts.Content[foundAt]
		scripts.Content[foundAt+1] = lefthookScriptValue(hook)
		key.HeadComment = ""
		key.LineComment = lefthookOwnedMarker
		key.FootComment = ""
		return nil
	}
	key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: lefthookScriptName, LineComment: lefthookOwnedMarker}
	scripts.Content = append(scripts.Content, key, lefthookScriptValue(hook))
	return nil
}

func lefthookNodeOwned(key, value *yaml.Node) bool {
	return exactLefthookYAMLMarker(key.HeadComment) ||
		exactLefthookYAMLMarker(key.LineComment) ||
		exactLefthookYAMLMarker(key.FootComment) ||
		exactLefthookYAMLMarker(value.HeadComment) ||
		exactLefthookYAMLMarker(value.LineComment) ||
		exactLefthookYAMLMarker(value.FootComment)
}

func exactLefthookYAMLMarker(comment string) bool {
	return comment == lefthookOwnedMarker || comment == "# "+lefthookOwnedMarker
}

func lefthookScriptValue(hook string) *yaml.Node {
	content := []*yaml.Node{
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: "runner"},
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: "bash"},
	}
	if hook == postRewriteHookName {
		content = append(content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "use_stdin"},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"},
		)
	}
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: content}
}

func removeOwnedLefthookConfig(data []byte) ([]byte, bool, error) {
	root, err := parseYAMLMapping(data, lefthookLocalConfigName)
	if err != nil {
		return nil, false, err
	}
	changed := removeOwnedMappingEntry(root, "source_dir_local")
	for _, hook := range gitHookNames {
		hookNode, ok := mappingValue(root, hook)
		if !ok || hookNode.Kind != yaml.MappingNode {
			continue
		}
		scripts, ok := mappingValue(hookNode, "scripts")
		if !ok || scripts.Kind != yaml.MappingNode {
			continue
		}
		if removeOwnedMappingEntry(scripts, lefthookScriptName) {
			changed = true
		}
	}
	if !changed {
		return data, false, nil
	}
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(root); err != nil {
		return nil, false, fmt.Errorf("encode %s: %w", lefthookLocalConfigName, err)
	}
	if err := encoder.Close(); err != nil {
		return nil, false, fmt.Errorf("encode %s: %w", lefthookLocalConfigName, err)
	}
	return out.Bytes(), true, nil
}

func removeOwnedMappingEntry(parent *yaml.Node, name string) bool {
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value == name && lefthookNodeOwned(parent.Content[i], parent.Content[i+1]) {
			parent.Content = append(parent.Content[:i], parent.Content[i+2:]...)
			return true
		}
	}
	return false
}
