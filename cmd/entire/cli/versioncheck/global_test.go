package versioncheck

import (
	"os"
	"testing"
)

// TestMain clears the install roots the probes read from the environment, so
// no test in this package inherits the developer's own mise or Scoop layout.
//
// It is a package-wide default rather than a per-test helper because forgetting
// it is silent and the failure blames the code under test: every probe root is
// a `strings.HasPrefix` match, and MISE_INSTALLS_DIR is the one variable used
// verbatim (miseRoots returns it unchanged, while MISE_DATA_DIR gains
// `installs` and Scoop's inputs gain `apps`), so a developer with it set to
// something broad like /opt makes a probe claim exec paths the test never
// meant it to. The rows that break are the ones asserting a *specific* probe
// or the fallback — and they break only on that developer's machine, never in
// CI, which sets none of these.
//
// XDG_CONFIG_HOME and USERPROFILE deliberately stay out of here: Scoop's
// config.json is read through them and several tests write one, so those need
// a per-test temp dir. isolateWindowsInstallEnv owns that.
//
// os.Setenv rather than t.Setenv because TestMain has no *testing.T; a test
// wanting a specific root still overrides this baseline with t.Setenv, which
// restores it afterwards.
func TestMain(m *testing.M) {
	for _, key := range []string{"MISE_INSTALLS_DIR", "MISE_DATA_DIR", "SCOOP", "SCOOP_GLOBAL"} {
		if err := os.Setenv(key, ""); err != nil {
			panic("versioncheck tests: clearing " + key + ": " + err.Error())
		}
	}
	os.Exit(m.Run())
}
