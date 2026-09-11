package strategy

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"gopkg.in/yaml.v3"
)

// errLefthookLocalConfigUnwritable reports a local config Entire will not
// edit. Lefthook reads exactly one local config, and lefthook-local.yml
// shadows lefthook-local.toml — so creating a .yml beside a user's .toml
// would silently disable their config. Rather than risk that, Entire declines
// and falls back to the native bridge; the caller tells the user what to add.
const lefthookExtendsKey = "extends"

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
		if root.Content[i].Value != lefthookExtendsKey {
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
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: lefthookExtendsKey}
	valueNode := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	root.Content = append(root.Content, keyNode, valueNode)
	return valueNode, nil
}

// renderEntireLefthookConfig builds Entire's own Lefthook config. Entire owns
// this file outright, so it is rendered as a whole rather than merged: there
// is no user content in it to preserve, which is the entire point of pointing
// the local config at it with one extends entry.
func renderEntireLefthookConfig() []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# %s\n", lefthookOwnedMarker)
	b.WriteString("# Managed by Entire. Edits are overwritten; remove the extends entry\n")
	b.WriteString("# in your Lefthook local config to detach.\n")
	fmt.Fprintf(&b, "source_dir_local: %s\n", lefthookLocalDir)
	for _, hook := range gitHookNames {
		fmt.Fprintf(&b, "%s:\n  scripts:\n    %q:\n      runner: bash\n", hook, lefthookScriptName)
	}
	return b.Bytes()
}

// entireLefthookConfigOwned reports whether a file at Entire's config path
// carries Entire's ownership marker. The path is one Entire chose, but the
// user's repository is theirs: a file there that Entire did not write is
// theirs to keep, exactly as an unowned script is.
func entireLefthookConfigOwned(data []byte) bool {
	return bytes.Contains(data, []byte(lefthookOwnedMarker))
}

// entireLefthookConfigCurrent reports whether Entire's config file is present
// and exactly what renderEntireLefthookConfig would write.
func entireLefthookConfigCurrent(root *os.Root) (bool, error) {
	data, _, err := readOptionalRegular(root, entireLefthookConfigName)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", entireLefthookConfigName, err)
	}
	return data != nil && bytes.Equal(data, renderEntireLefthookConfig()), nil
}

// lefthookExtendsEntryPresent reports whether the local Lefthook config
// extends Entire's config file.
func lefthookExtendsEntryPresent(root *os.Root) (bool, error) {
	name, data, err := findLefthookLocalConfig(root)
	if err != nil {
		// An unwritable local config is not an extends entry; the caller
		// reports it and falls back to the native bridge.
		if errors.Is(err, errLefthookLocalConfigUnwritable) {
			return false, nil
		}
		return false, err
	}
	if name == "" || data == nil {
		return false, nil
	}
	document := &yaml.Node{}
	if err := yaml.Unmarshal(data, document); err != nil {
		return false, fmt.Errorf("parse %s: %w", name, err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return false, nil
	}
	root0 := document.Content[0]
	for i := 0; i+1 < len(root0.Content); i += 2 {
		if root0.Content[i].Value != lefthookExtendsKey {
			continue
		}
		for _, item := range root0.Content[i+1].Content {
			if item.Value == entireLefthookConfigName {
				return true, nil
			}
		}
	}
	return false, nil
}

// removeLefthookExtendsEntry drops Entire's entry from the local config,
// removing the now-empty extends key with it. Anything else in the file is
// left alone.
func removeLefthookExtendsEntry(root *os.Root) (bool, error) {
	name, data, err := findLefthookLocalConfig(root)
	if err != nil {
		if errors.Is(err, errLefthookLocalConfigUnwritable) {
			return false, nil
		}
		return false, err
	}
	if name == "" || data == nil {
		return false, nil
	}
	document := &yaml.Node{}
	if err := yaml.Unmarshal(data, document); err != nil {
		return false, fmt.Errorf("parse %s: %w", name, err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return false, nil
	}
	docRoot := document.Content[0]
	changed := false
	for i := 0; i+1 < len(docRoot.Content); i += 2 {
		if docRoot.Content[i].Value != lefthookExtendsKey {
			continue
		}
		seq := docRoot.Content[i+1]
		kept := make([]*yaml.Node, 0, len(seq.Content))
		for _, item := range seq.Content {
			if item.Value == entireLefthookConfigName {
				changed = true
				continue
			}
			kept = append(kept, item)
		}
		seq.Content = kept
		if len(kept) == 0 {
			docRoot.Content = append(docRoot.Content[:i], docRoot.Content[i+2:]...)
		}
		break
	}
	if !changed {
		return false, nil
	}
	// A file that held nothing but our entry is ours to delete.
	if len(docRoot.Content) == 0 {
		if err := osroot.RemoveNoSymlinks(root, name); err != nil && !os.IsNotExist(err) {
			return false, fmt.Errorf("remove %s: %w", name, err)
		}
		return true, nil
	}
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(document); err != nil {
		return false, fmt.Errorf("encode %s: %w", name, err)
	}
	if err := encoder.Close(); err != nil {
		return false, fmt.Errorf("encode %s: %w", name, err)
	}
	if err := jsonutil.WriteFileAtomicIn(root, name, out.Bytes(), 0o644); err != nil {
		return false, fmt.Errorf("write %s: %w", name, err)
	}
	return true, nil
}

// lefthookManagerBackupSuffix is the name Lefthook moves a hook aside to when
// it reclaims one it did not install.
const lefthookManagerBackupSuffix = ".old"

// clearStaleNativeBackups removes hook backups left over from the era when
// Entire and Lefthook fought over .git/hooks/*.
//
// A repo that hit #1349 carries two of them per hook: <hook>.pre-entire, where
// Entire stashed Lefthook's launcher before overwriting it, and <hook>.old,
// where Lefthook then stashed Entire's wrapper when it took the file back.
// Both describe a conflict that no longer exists — Lefthook owns the hook file
// outright and Entire registers through its config.
//
// The .old one is not merely untidy. Lefthook refuses to move a hook aside
// when its backup already exists, so it reports
//
//	could not replace the hook: can't rename pre-push to pre-push.old - file already exists
//
// on every subsequent sync until the file is gone. That is #1349's step 4, and
// it outlives the fix unless the leftovers are cleared.
//
// Each file is removed only when its contents prove whose it was: a
// .pre-entire holding a Lefthook launcher, or a .old holding an Entire
// wrapper. Anything else is a real user hook and is left alone.
func clearStaleNativeBackups(hooks effectiveHooksRoot) (int, error) {
	removed := 0
	for _, hook := range gitHookNames {
		for _, candidate := range []struct {
			name  string
			stale func([]byte) bool
		}{
			{hook + backupSuffix, func(b []byte) bool { return looksLikeLefthookHook(b, hook) }},
			{hook + lefthookManagerBackupSuffix, func(b []byte) bool {
				return strings.Contains(string(b), entireHookMarker)
			}},
		} {
			name := hooks.name(candidate.name)
			data, _, err := readOptionalRegular(hooks.root, name)
			if err != nil {
				return removed, fmt.Errorf("read %s: %w", candidate.name, err)
			}
			if data == nil || !candidate.stale(data) {
				continue
			}
			if err := osroot.RemoveNoSymlinks(hooks.root, name); err != nil && !os.IsNotExist(err) {
				return removed, fmt.Errorf("remove stale backup %s: %w", candidate.name, err)
			}
			removed++
		}
	}
	return removed, nil
}
