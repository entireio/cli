package checkpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

type replaySession struct {
	tree   plumbing.Hash
	fields map[string]json.RawMessage
}

type replayCheckpoint struct {
	metadata object.TreeEntry
	summary  *CheckpointSummary
	fields   map[string]json.RawMessage
	sessions map[string]replaySession
	order    []string
	extra    map[string]object.TreeEntry
}

// Checkpoints can be at the tree root or nested under the v1 branch.
// Session IDs stay stable when numeric directories change.
func mergeCheckpointSessions(ctx context.Context, repo *git.Repository, base, local, remote map[string]object.TreeEntry, prefix string) (plumbing.Hash, bool, error) {
	left, err := loadReplayCheckpoint(ctx, repo, local, prefix)
	if err != nil || left == nil {
		return plumbing.ZeroHash, false, err
	}
	right, err := loadReplayCheckpoint(ctx, repo, remote, prefix)
	if err != nil || right == nil {
		return plumbing.ZeroHash, false, err
	}
	original, err := loadReplayCheckpoint(ctx, repo, base, prefix)
	if err != nil {
		return plumbing.ZeroHash, true, err
	}
	if original == nil {
		if _, exists := base[paths.MetadataFileName]; exists {
			return plumbing.ZeroHash, true, fmt.Errorf("%w: invalid checkpoint preimage at %q", ErrReplayConflict, prefix)
		}
		original = &replayCheckpoint{fields: map[string]json.RawMessage{}, extra: base, sessions: map[string]replaySession{}}
	}
	if left.summary.CheckpointID != right.summary.CheckpointID || (original.summary != nil && original.summary.CheckpointID != left.summary.CheckpointID) {
		return plumbing.ZeroHash, true, fmt.Errorf("%w: checkpoint identity changed at %q", ErrReplayConflict, prefix)
	}
	merged, err := mergeReplayCheckpoint(ctx, repo, original, left, right, prefix)
	return merged, true, err
}

//nolint:nilnil,nilerr // A nil result lets ordinary directories use the normal tree merge.
func loadReplayCheckpoint(ctx context.Context, repo *git.Repository, entries map[string]object.TreeEntry, prefix string) (*replayCheckpoint, error) {
	metadata, exists := entries[paths.MetadataFileName]
	if !exists || !metadata.Mode.IsFile() {
		return nil, nil
	}
	summary, err := readJSONFromBlob[CheckpointSummary](repo, metadata.Hash)
	if err != nil || summary.CheckpointID.IsEmpty() || summary.Sessions == nil {
		return nil, nil
	}
	fields, err := readJSONFromBlob[map[string]json.RawMessage](repo, metadata.Hash)
	if err != nil {
		return nil, err
	}
	var sessionFields []map[string]json.RawMessage
	if err := json.Unmarshal((*fields)["sessions"], &sessionFields); err != nil {
		return nil, fmt.Errorf("read replay session paths: %w", err)
	}
	cp := &replayCheckpoint{metadata: metadata, summary: summary, fields: *fields, sessions: make(map[string]replaySession), extra: maps.Clone(entries)}
	delete(cp.extra, paths.MetadataFileName)
	for index := range summary.Sessions {
		if err := ctx.Err(); err != nil {
			return nil, err //nolint:wrapcheck // preserve context cancellation
		}
		name := strconv.Itoa(index)
		entry := entries[name]
		if entry.Mode != filemode.Dir {
			return nil, fmt.Errorf("%w: missing session directory %q", ErrReplayConflict, path.Join(prefix, name))
		}
		sessionTree, treeErr := repo.TreeObject(entry.Hash)
		if treeErr != nil {
			return nil, fmt.Errorf("read replay session tree: %w", treeErr)
		}
		metaEntry, findErr := sessionTree.FindEntry(paths.MetadataFileName)
		if findErr != nil {
			return nil, fmt.Errorf("read replay session metadata: %w", findErr)
		}
		meta, readErr := readJSONFromBlob[Metadata](repo, metaEntry.Hash)
		if readErr != nil {
			return nil, readErr
		}
		if meta.SessionID == "" || meta.CheckpointID != summary.CheckpointID {
			return nil, fmt.Errorf("%w: invalid session identity in %q", ErrReplayConflict, path.Join(prefix, name))
		}
		if _, duplicate := cp.sessions[meta.SessionID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate session %q", ErrReplayConflict, meta.SessionID)
		}
		normalized, normalizeErr := replaySessionPaths(sessionFields[index], "/"+path.Join(prefix, name)+"/", "/")
		if normalizeErr != nil {
			return nil, normalizeErr
		}
		if string(normalized["metadata"]) != `"/metadata.json"` {
			return nil, fmt.Errorf("%w: invalid session metadata path", ErrReplayConflict)
		}
		cp.sessions[meta.SessionID] = replaySession{tree: entry.Hash, fields: normalized}
		cp.order = append(cp.order, meta.SessionID)
		delete(cp.extra, name)
	}
	for name := range cp.extra {
		if _, numeric := strconv.Atoi(name); numeric == nil {
			return nil, fmt.Errorf("%w: unlisted session slot %q", ErrReplayConflict, name)
		}
	}
	return cp, nil
}

func replaySessionPaths(fields map[string]json.RawMessage, from, to string) (map[string]json.RawMessage, error) {
	out := maps.Clone(fields)
	for key, raw := range fields {
		var value string
		if json.Unmarshal(raw, &value) != nil || !strings.HasPrefix(value, "/") {
			continue
		}
		if !strings.HasPrefix(value, from) {
			return nil, fmt.Errorf("%w: session path %q is outside its slot", ErrReplayConflict, value)
		}
		relative := strings.TrimPrefix(value, from)
		normalized, err := normalizeGitTreePath(relative)
		if err != nil || normalized != relative {
			return nil, fmt.Errorf("%w: invalid session path %q", ErrReplayConflict, value)
		}
		rewritten, err := json.Marshal(to + relative)
		if err != nil {
			return nil, fmt.Errorf("encode replay session path: %w", err)
		}
		out[key] = rewritten
	}
	return out, nil
}

func mergeReplayCheckpoint(ctx context.Context, repo *git.Repository, base, local, remote *replayCheckpoint, prefix string) (plumbing.Hash, error) {
	if local.metadata.Mode != remote.metadata.Mode && local.metadata != base.metadata && remote.metadata != base.metadata {
		return plumbing.ZeroHash, fmt.Errorf("%w at %q: metadata file mode changed", ErrReplayConflict, path.Join(prefix, paths.MetadataFileName))
	}
	remoteExtra, err := replayExtraTree(repo, remote.extra)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	extra, err := mergeReplayEntries(ctx, repo, base.extra, local.extra, remote.extra, remoteExtra, prefix)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	entries, err := replayTreeEntries(repo, extra)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	// Keep remote sessions in place and append local additions.
	order := slices.Concat(remote.order, local.order, base.order)
	seen := make(map[string]bool)
	sessions := make([]map[string]json.RawMessage, 0)
	stats := make(map[string]object.TreeEntry)
	for _, sid := range order {
		if err := ctx.Err(); err != nil {
			return plumbing.ZeroHash, err //nolint:wrapcheck // preserve context cancellation
		}
		if seen[sid] {
			continue
		}
		seen[sid] = true
		merged, mergeErr := mergeReplaySession(ctx, repo, base.sessions[sid], local.sessions[sid], remote.sessions[sid], path.Join(prefix, sid))
		if mergeErr != nil {
			return plumbing.ZeroHash, mergeErr
		}
		if merged.tree.IsZero() {
			continue
		}
		name := strconv.Itoa(len(sessions))
		fields, pathErr := replaySessionPaths(merged.fields, "/", "/"+path.Join(prefix, name)+"/")
		if pathErr != nil {
			return plumbing.ZeroHash, pathErr
		}
		sessions = append(sessions, fields)
		entries[name] = object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: merged.tree}
		sessionEntries, readErr := replayTreeEntries(repo, merged.tree)
		if readErr != nil {
			return plumbing.ZeroHash, readErr
		}
		for file, entry := range sessionEntries {
			stats[path.Join(name, file)] = entry
		}
	}
	root, err := mergeReplaySummary(repo, base, local, remote, sessions, stats)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	mode := remote.metadata.Mode
	if remote.metadata == base.metadata {
		mode = local.metadata.Mode
	}
	entries[paths.MetadataFileName] = object.TreeEntry{Name: paths.MetadataFileName, Mode: mode, Hash: root}
	return replayExtraTree(repo, entries)
}

func replayExtraTree(repo *git.Repository, entries map[string]object.TreeEntry) (plumbing.Hash, error) {
	list := slices.Collect(maps.Values(entries))
	sortTreeEntries(list)
	return storeTree(repo, list)
}

func mergeReplaySession(ctx context.Context, repo *git.Repository, base, local, remote replaySession, name string) (replaySession, error) {
	entry := func(session replaySession) object.TreeEntry {
		if session.tree.IsZero() {
			return object.TreeEntry{}
		}
		return object.TreeEntry{Name: "session", Mode: filemode.Dir, Hash: session.tree}
	}
	merged, err := mergeReplayEntry(ctx, repo, entry(base), entry(local), entry(remote), name)
	if err != nil || merged.Mode == 0 {
		return replaySession{}, err
	}
	fields, err := mergeReplayJSON(base.fields, local.fields, remote.fields, name)
	if err != nil {
		return replaySession{}, err
	}
	if err := validateReplaySessionPaths(repo, merged.Hash, fields); err != nil {
		return replaySession{}, err
	}
	return replaySession{tree: merged.Hash, fields: fields}, nil
}

func mergeReplaySummary(repo *git.Repository, base, local, remote *replayCheckpoint, sessions []map[string]json.RawMessage, entries map[string]object.TreeEntry) (plumbing.Hash, error) {
	filtered := func(fields map[string]json.RawMessage) map[string]json.RawMessage {
		out := maps.Clone(fields)
		for _, key := range []string{"sessions", "checkpoints_count", "files_touched", "token_usage", "redaction", "has_review", "has_investigation", "imported", "linked_commits", "cli_version", "branch"} {
			delete(out, key)
		}
		return out
	}
	fields, err := mergeReplayJSON(filtered(base.fields), filtered(local.fields), filtered(remote.fields), "checkpoint metadata")
	if err != nil {
		return plumbing.ZeroHash, err
	}
	writer := &treeWriter{repo: repo}
	count, files, tokens, redaction, err := writer.reaggregateFromEntries("", len(sessions), entries)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	values := map[string]any{
		"sessions": sessions, "checkpoints_count": count, "files_touched": files,
		"cli_version": versioninfo.Version,
	}
	// Keep the published branch label. Each session retains its own branch.
	if remote.summary.Branch != "" {
		values["branch"] = remote.summary.Branch
	}
	if tokens != nil {
		values["token_usage"] = tokens
	}
	if redaction != nil {
		values["redaction"] = redaction
	}
	for key, flag := range map[string]bool{
		"has_review":        local.summary.HasReview || remote.summary.HasReview,
		"has_investigation": local.summary.HasInvestigation || remote.summary.HasInvestigation,
		"imported":          local.summary.Imported || remote.summary.Imported,
	} {
		if flag {
			values[key] = true
		}
	}
	links, err := mergeReplayLinks(base, local, remote)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if len(links) > 0 {
		values["linked_commits"] = links
	}
	for key, value := range values {
		raw, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			return plumbing.ZeroHash, fmt.Errorf("encode replay summary %s: %w", key, marshalErr)
		}
		fields[key] = raw
	}
	if replayJSONObjectEqual(fields, remote.fields) {
		return remote.metadata.Hash, nil
	}
	data, err := jsonutil.MarshalIndentWithNewline(fields, "", "  ")
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("encode replay summary: %w", err)
	}
	return CreateBlobFromContent(repo, data)
}

func validateReplaySessionPaths(repo *git.Repository, hash plumbing.Hash, fields map[string]json.RawMessage) error {
	tree, err := repo.TreeObject(hash)
	if err != nil {
		return fmt.Errorf("read merged session: %w", err)
	}
	for _, raw := range fields {
		var name string
		if json.Unmarshal(raw, &name) != nil || !strings.HasPrefix(name, "/") {
			continue
		}
		if _, err := tree.FindEntry(strings.TrimPrefix(name, "/")); err != nil {
			return fmt.Errorf("merged session path %q: %w", name, err)
		}
	}
	return nil
}

func mergeReplayLinks(base, local, remote *replayCheckpoint) ([]map[string]json.RawMessage, error) {
	read := func(cp *replayCheckpoint) (map[string]map[string]json.RawMessage, error) {
		var list []map[string]json.RawMessage
		if raw := cp.fields["linked_commits"]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &list); err != nil {
				return nil, fmt.Errorf("read checkpoint links: %w", err)
			}
		}
		out := make(map[string]map[string]json.RawMessage)
		for _, link := range list {
			var sha string
			if err := json.Unmarshal(link["sha"], &sha); err != nil {
				return nil, fmt.Errorf("read checkpoint link SHA: %w", err)
			}
			out[sha] = link
		}
		return out, nil
	}
	original, err := read(base)
	if err != nil {
		return nil, err
	}
	left, err := read(local)
	if err != nil {
		return nil, err
	}
	right, err := read(remote)
	if err != nil {
		return nil, err
	}
	links := unionLinkedCommits(remote.summary.LinkedCommits, local.summary.LinkedCommits)
	out := make([]map[string]json.RawMessage, 0, len(links))
	for _, link := range links {
		if left[link.SHA] == nil {
			out = append(out, right[link.SHA])
			continue
		}
		if right[link.SHA] == nil {
			out = append(out, left[link.SHA])
			continue
		}
		fields, err := mergeReplayJSON(original[link.SHA], left[link.SHA], right[link.SHA], "checkpoint link "+link.SHA)
		if err != nil {
			return nil, err
		}
		out = append(out, fields)
	}
	return out, nil
}
