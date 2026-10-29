package jsonutil

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"runtime"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// WriteFileDurableIn is WriteFileAtomicIn for the few files that cannot be
// rebuilt if a power loss eats them (a private key, the record of what a key
// still has to do on a server). It adds what WriteFileAtomicIn deliberately
// leaves out: the temp file is fsynced before the rename, and the parent
// directory after it, so a crash leaves either the old file or the complete
// new one, never a zero-length replacement. Do not use it for caches or state
// that is written often; see WriteFileAtomic for the cost.
func WriteFileDurableIn(root *os.Root, name string, data []byte, perm fs.FileMode) error {
	return writeDurable(root, name, data, perm, false)
}

// CreateFileDurableIn writes name only if it does not exist, durably: the
// content goes to an fsynced temp file that is hard-linked into place (which
// fails with fs.ErrExist rather than replacing) and the parent directory is
// fsynced. A reader never sees a partial file.
func CreateFileDurableIn(root *os.Root, name string, data []byte, perm fs.FileMode) error {
	return writeDurable(root, name, data, perm, true)
}

func writeDurable(root *os.Root, name string, data []byte, perm fs.FileMode, exclusive bool) (retErr error) {
	parent, leaf, closeParent, err := osroot.OpenParentNoSymlinks(root, name)
	if err != nil {
		return fmt.Errorf("open parent for %s: %w", name, err)
	}
	defer closeParent()

	tmp, tmpName, err := CreateTempIn(parent, leaf)
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", name, err)
	}
	removeTmp := true
	defer func() {
		if removeTmp {
			if rerr := parent.Remove(tmpName); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) && retErr == nil {
				retErr = fmt.Errorf("remove temp for %s: %w", name, rerr)
			}
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp for %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp for %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp for %s: %w", name, err)
	}
	if err := parent.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("chmod temp for %s: %w", name, err)
	}
	if exclusive {
		// The link leaves the temp name behind; the deferred remove drops it.
		if err := parent.Link(tmpName, leaf); err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
	} else {
		if err := parent.Rename(tmpName, leaf); err != nil {
			return fmt.Errorf("rename temp to %s: %w", name, err)
		}
		removeTmp = false
	}
	return syncParent(parent, name)
}

// syncParent flushes the directory entry the rename or link created. Windows
// cannot open a directory for fsync (NTFS journals the metadata), so it is
// skipped there; everywhere else a failure is returned, since durability is
// the point of these functions.
func syncParent(parent *os.Root, name string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := parent.Open(".")
	if err != nil {
		return fmt.Errorf("open directory of %s: %w", path.Base(name), err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync directory of %s: %w", path.Base(name), err)
	}
	return nil
}
