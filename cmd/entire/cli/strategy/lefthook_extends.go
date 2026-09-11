package strategy

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"gopkg.in/yaml.v3"
)

// errLefthookLocalConfigUnwritable reports a local config Entire will not
// edit. Lefthook reads exactly one local config, and lefthook-local.yml
// shadows lefthook-local.toml — so creating a .yml beside a user's .toml
// would silently disable their config. Rather than risk that, Entire declines
// and falls back to the native bridge; the caller tells the user what to add.
var errLefthookLocalConfigUnwritable = errors.New("lefthook local config is not YAML")

// ensureLefthookExtends makes the local Lefthook config extend Entire's own
// config file, creating the local config when there is none. It returns the
// name of the config it wrote.
//
// Entire adds exactly one key here and owns entire-lefthook.yml outright. The
// alternative — merging source_dir_local plus a nested scripts entry per hook
// into a file the user owns — is what required node-level surgery over their
// content, and it still refused TOML, JSONC, `extends` and `remotes` configs.
// Lefthook resolves extends at run time, so the moment Entire's file exists an
// already-installed launcher dispatches to it: no `lefthook install`, no user
// action, and the main config is never read at all.
func ensureLefthookExtends(root *os.Root) (string, error) {
	name, existing, err := findLefthookLocalConfig(root)
	if err != nil {
		return "", err
	}
	if name == "" {
		name, existing = lefthookLocalConfigName, nil
	}

	updated, changed, err := ensureExtendsEntry(existing, entireLefthookConfigName)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	if !changed {
		return name, nil
	}
	if err := jsonutil.WriteFileAtomicIn(root, name, updated, 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", name, err)
	}
	return name, nil
}

// findLefthookLocalConfig returns the local config Lefthook would actually
// read, probing the documented names in precedence order. Writing into a
// shadowed file would be a silent no-op, so the first match is the only
// correct target.
func findLefthookLocalConfig(root *os.Root) (string, []byte, error) {
	for _, candidate := range lefthookLocalConfigNames {
		data, _, err := readOptionalRegular(root, candidate)
		if err != nil {
			return "", nil, fmt.Errorf("read %s: %w", candidate, err)
		}
		if data == nil {
			continue
		}
		if !isYAMLConfigName(candidate) {
			return candidate, data, fmt.Errorf("%w: %s", errLefthookLocalConfigUnwritable, candidate)
		}
		return candidate, data, nil
	}
	return "", nil, nil
}

func isYAMLConfigName(name string) bool {
	return strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")
}

// ensureExtendsEntry adds entry to the document's `extends` sequence if it is
// not already there, preserving everything else including comments. It reports
// whether the document changed.
func ensureExtendsEntry(existing []byte, entry string) ([]byte, bool, error) {
	document := &yaml.Node{}
	if len(bytes.TrimSpace(existing)) == 0 {
		document.Kind = yaml.DocumentNode
		document.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	} else if err := yaml.Unmarshal(existing, document); err != nil {
		return nil, false, fmt.Errorf("parse: %w", err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, false, errors.New("parse: top level must be a mapping")
	}
	root := document.Content[0]

	extends, err := ensureExtendsSequence(root)
	if err != nil {
		return nil, false, err
	}
	for _, item := range extends.Content {
		if item.Value == entry {
			return existing, false, nil
		}
	}
	extends.Content = append(extends.Content, &yaml.Node{
		Kind: yaml.ScalarNode, Tag: "!!str", Value: entry,
		LineComment: lefthookOwnedMarker,
	})

	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(document); err != nil {
		return nil, false, fmt.Errorf("encode: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, false, fmt.Errorf("encode: %w", err)
	}
	return out.Bytes(), true, nil
}

// ensureExtendsSequence returns the document's `extends` sequence node,
// creating it when absent. Kept separate from ensureMappingValue, which
// creates a mapping: `extends` is a sequence.
func ensureExtendsSequence(root *yaml.Node) (*yaml.Node, error) {
	foundAt := -1
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "extends" {
			continue
		}
		if foundAt >= 0 {
			return nil, errors.New("duplicate extends key")
		}
		foundAt = i
	}
	if foundAt >= 0 {
		value := root.Content[foundAt+1]
		if value.Kind != yaml.SequenceNode {
			return nil, errors.New("extends must be a sequence")
		}
		return value, nil
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "extends"}
	valueNode := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	root.Content = append(root.Content, keyNode, valueNode)
	return valueNode, nil
}
