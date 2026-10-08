//go:build !linux && !darwin

package proclive

// On platforms without process introspection (e.g. Windows), the seam reports
// "unsupported". Check then yields LivenessUnknown and ResolveOwner yields no
// owner, so session liveness degrades cleanly to the inactivity-timeout
// fallback instead of producing wrong answers.

func procStat(pid int) (ppid int, name, start string, err error) {
	return 0, "", "", errUnsupported
}

func bootID() (string, error) {
	return "", errUnsupported
}
