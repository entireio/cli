package checkpoint

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"slices"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// mergeReplayJSON merges independent fields without changing unknown values.
// Conflicting edits to the same field return ErrReplayConflict.
func mergeReplayJSON(base, local, remote map[string]json.RawMessage, name string) (map[string]json.RawMessage, error) {
	out := maps.Clone(remote)
	if out == nil {
		out = make(map[string]json.RawMessage)
	}
	keys := maps.Clone(base)
	if keys == nil {
		keys = make(map[string]json.RawMessage)
	}
	maps.Copy(keys, local)
	for _, key := range slices.Sorted(maps.Keys(keys)) {
		before, after, onto := base[key], local[key], remote[key]
		switch {
		case replayJSONEqual(after, before), replayJSONEqual(after, onto):
			continue
		case !replayJSONEqual(before, onto):
			return nil, fmt.Errorf("%w at %q field %q", ErrReplayConflict, name, key)
		}
		if after == nil {
			delete(out, key)
		} else {
			out[key] = after
		}
	}
	return out, nil
}

func replayJSONEqual(a, b json.RawMessage) bool {
	if bytes.Equal(a, b) {
		return true
	}
	canonical := func(raw json.RawMessage) ([]byte, error) {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode replay JSON: %w", err)
		}
		return json.Marshal(value)
	}
	left, leftErr := canonical(a)
	right, rightErr := canonical(b)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}

// A summary update and a transcript update can change different metadata fields.
//
//nolint:nilerr // Other JSON files use the normal file conflict check.
func mergeReplaySessionMetadata(repo *git.Repository, base, local, remote object.TreeEntry, name string) (object.TreeEntry, bool, error) {
	if path.Base(name) != paths.MetadataFileName || !local.Mode.IsRegular() || !remote.Mode.IsRegular() {
		return object.TreeEntry{}, false, nil
	}
	if local.Mode != remote.Mode || (base.Mode != 0 && base.Mode != local.Mode) {
		return object.TreeEntry{}, false, nil
	}
	left, leftErr := readJSONFromBlob[Metadata](repo, local.Hash)
	right, rightErr := readJSONFromBlob[Metadata](repo, remote.Hash)
	if leftErr != nil || rightErr != nil || left.SessionID == "" || left.SessionID != right.SessionID || left.CheckpointID != right.CheckpointID {
		return object.TreeEntry{}, false, nil
	}
	read := func(entry object.TreeEntry) (map[string]json.RawMessage, error) {
		if entry.Mode == 0 {
			return map[string]json.RawMessage{}, nil
		}
		fields, err := readJSONFromBlob[map[string]json.RawMessage](repo, entry.Hash)
		if err != nil {
			return nil, err
		}
		return *fields, nil
	}
	before, err := read(base)
	if err != nil {
		return object.TreeEntry{}, true, err
	}
	after, err := read(local)
	if err != nil {
		return object.TreeEntry{}, true, err
	}
	onto, err := read(remote)
	if err != nil {
		return object.TreeEntry{}, true, err
	}
	fields, err := mergeReplayJSON(before, after, onto, name)
	if err != nil {
		return object.TreeEntry{}, true, err
	}
	if replayJSONObjectEqual(fields, onto) {
		return remote, true, nil
	}
	data, err := jsonutil.MarshalIndentWithNewline(fields, "", "  ")
	if err != nil {
		return object.TreeEntry{}, true, fmt.Errorf("encode replay metadata: %w", err)
	}
	hash, err := CreateBlobFromContent(repo, data)
	if err != nil {
		return object.TreeEntry{}, true, err
	}
	return object.TreeEntry{Name: local.Name, Mode: local.Mode, Hash: hash}, true, nil
}

func replayJSONObjectEqual(a, b map[string]json.RawMessage) bool {
	return maps.EqualFunc(a, b, replayJSONEqual)
}
