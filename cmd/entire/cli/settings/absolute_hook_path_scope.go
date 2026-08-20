package settings

// absoluteGitHookPathProjectReason explains why absolute_git_hook_path is
// ignored when it comes from the committed project file.
const absoluteGitHookPathProjectReason = "it was set in .entire/settings.json, which is committed — absolute_git_hook_path describes one machine's installation, so it is honored only from .entire/settings.local.json"

// enforceAbsoluteGitHookPathScope drops absolute_git_hook_path unless it comes
// from a scope that is local to this clone.
//
// The setting replaces bare "entire" in every generated git hook with the
// absolute path of the binary that ran `entire enable`. That is the right trade
// for the GUI git clients it exists for, but it is a statement about one
// machine's filesystem, and honoring it from a version-controlled file lets a
// repository impose it on everyone who clones — pinning their hooks to a path
// chosen by whichever binary they happened to run, which is strictly more
// brittle than resolving through PATH.
//
// Unlike the OPF command gate this does NOT re-verify the local file with the
// deep index-and-HEAD check, and the difference is deliberate: that setting
// becomes argv[0] of an exec, so an attacker-controlled value runs code. This
// one only chooses between "entire" and os.Executable() — a binary the user
// already invoked — so the worst outcome is a fragile hook, not execution of
// someone else's payload. The layer-level tracked-file rejection
// (localLayerTrackedReason) already covers the case of a committed local file.
//
// Called after the project file is loaded and before the local layer merges, so
// a value seen here can only have come from the project file. A rejection is
// recorded rather than logged: the loader is reached from logging.Init, and a
// line in .entire/logs is not a signal anyone sees — `entire status` and
// `entire doctor` report it instead.
func enforceAbsoluteGitHookPathScope(s *EntireSettings) {
	if s == nil || !s.AbsoluteGitHookPath {
		return
	}
	s.AbsoluteGitHookPath = false
	s.absoluteGitHookPathRejection = absoluteGitHookPathProjectReason
}
