package settings

import "encoding/json"

// absoluteGitHookPathProjectDeprecation is shown when absolute_git_hook_path is
// being honored from the committed project file.
const absoluteGitHookPathProjectDeprecation = "absolute_git_hook_path is set in .entire/settings.json, which is committed. It describes one machine's installation, so a future release will honor it only from .entire/settings.local.json. Run `entire doctor` to copy it there now — nothing changes for you, and the committed value stops pinning your collaborators' hooks."

// localSetsAbsoluteGitHookPath reports whether the local override file explicitly
// sets absolute_git_hook_path.
//
// Presence is the question, not value: both layers may set the same bool, so
// comparing the effective value after the merge would misattribute provenance.
func localSetsAbsoluteGitHookPath(data []byte) bool {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return false
	}
	return rawHasKey(raw, "absolute_git_hook_path")
}

// recordAbsoluteGitHookPathDeprecation notes that absolute_git_hook_path is being
// honored from the committed project file, which a future release will stop doing.
//
// The setting replaces bare "entire" in every generated git hook with the absolute
// path of the binary that ran `entire enable`. That is the right trade for the GUI
// git clients it exists for, but it is a statement about one machine's filesystem,
// and honoring it from a version-controlled file lets a repository impose it on
// everyone who clones — pinning their hooks to a path chosen by whichever binary
// they happened to run, which is strictly more brittle than resolving through
// PATH.
//
// Staged rather than switched off here, because the only way to enable the feature
// used to write it exclusively to the project file: `entire configure
// --absolute-git-hook-path` reported "Settings updated (.entire/settings.json)"
// and created no local file at all. Everyone who ever used the feature therefore
// has it committed, and dropping it silently would unpin their hooks — in the GUI
// git client that cannot find `entire` on PATH, which is the whole reason they
// enabled it. The guard would take its else branch and capture nothing, with no
// error. This is a robustness fix, not a security one (the value only chooses
// between "entire" and os.Executable(), a binary the user already invoked), so
// there is no case for breaking anyone quickly.
//
// Provenance comes from the local file's raw keys rather than from call order, so
// this is safe to call at any point after the merge.
//
// Recorded rather than logged: the loader is reached from logging.Init, and a line
// in .entire/logs is not a signal anyone sees, so `entire status` and `entire
// doctor` report it instead.
func recordAbsoluteGitHookPathDeprecation(s *EntireSettings, localData []byte) {
	if s == nil || !s.AbsoluteGitHookPath {
		return
	}
	if localSetsAbsoluteGitHookPath(localData) {
		// Already migrated: the local file is the authority, and the committed
		// value is redundant rather than load-bearing.
		return
	}
	s.absoluteGitHookPathDeprecation = absoluteGitHookPathProjectDeprecation
}
