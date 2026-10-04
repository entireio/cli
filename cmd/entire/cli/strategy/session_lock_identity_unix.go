//go:build unix

package strategy

import (
	"fmt"
	"os"
	"syscall"
)

func sessionLockDirectoryIdentity(_ string, info os.FileInfo) (sessionLockIdentity, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return sessionLockIdentity{}, fmt.Errorf("unexpected directory stat type %T", info.Sys())
	}
	return sessionLockIdentity{uint64(stat.Dev), uint64(stat.Ino)}, nil //nolint:gosec,unconvert // Stat field types vary by OS; preserve their bits as unsigned ordering keys.
}
