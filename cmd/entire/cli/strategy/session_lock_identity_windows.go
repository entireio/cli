package strategy

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func sessionLockDirectoryIdentity(path string, info os.FileInfo) (sessionLockIdentity, error) {
	file, err := os.Open(path)
	if err != nil {
		return sessionLockIdentity{}, fmt.Errorf("open directory: %w", err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return sessionLockIdentity{}, fmt.Errorf("stat directory handle: %w", err)
	}
	if !os.SameFile(info, openedInfo) {
		return sessionLockIdentity{}, errors.New("directory changed while resolving identity")
	}
	var identity windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &identity); err != nil {
		return sessionLockIdentity{}, fmt.Errorf("read directory identity: %w", err)
	}
	return sessionLockIdentity{
		uint64(identity.VolumeSerialNumber),
		uint64(identity.FileIndexHigh)<<32 | uint64(identity.FileIndexLow),
	}, nil
}
