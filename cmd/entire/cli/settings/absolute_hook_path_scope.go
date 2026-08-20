package settings

// absoluteGitHookPathProjectReason explains why absolute_git_hook_path is
// ignored when it comes from the committed project file.
const absoluteGitHookPathProjectReason = "it was set in .entire/settings.json, which is committed — absolute_git_hook_path describes one machine's installation, so it is honored only from .entire/settings.local.json"

// recordAbsoluteGitHookPathScopeRejection notes that a committed
// absolute_git_hook_path was not applied.
//
// The setting replaces bare "entire" in every generated git hook with the absolute
// path of the binary that ran `entire enable`. That is the right trade for the GUI
// git clients it exists for, but it is a statement about one machine's filesystem,
// and honoring it from a version-controlled file lets a repository impose it on
// everyone who clones — pinning their hooks to a path chosen by whichever binary
// they happened to run, which is strictly more brittle than resolving through
// PATH. The loader therefore zeroes it before the local layer merges, and only the
// local file can turn it back on.
//
// Unlike the OPF command gate this does NOT re-verify the local file with the deep
// index-and-HEAD check, and the difference is deliberate: that setting becomes
// argv[0] of an exec, so an attacker-controlled value runs code. This one only
// chooses between "entire" and os.Executable() — a binary the user already invoked
// — so the worst outcome is a fragile hook, not execution of someone else's
// payload. The layer-level tracked-file rejection (localLayerTrackedReason)
// already covers the case of a committed local file.
//
// Called with the final merged value so the record reflects the outcome: if the
// local file enabled the setting anyway, the project value was redundant rather
// than overridden, and reporting "ignored" beside a hook that IS pinned reads as a
// contradiction. Recorded rather than logged — the loader is reached from
// logging.Init, and a line in .entire/logs is not a signal anyone sees, so
// `entire status` and `entire doctor` report it instead.
func recordAbsoluteGitHookPathScopeRejection(s *EntireSettings, projectRequested bool) {
	if s == nil || !projectRequested || s.AbsoluteGitHookPath {
		return
	}
	s.absoluteGitHookPathRejection = absoluteGitHookPathProjectReason
}
