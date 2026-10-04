package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// Per-push unlock.
//
// A user's `git push` to an entire:// remote runs one helper process for the
// whole push, and the pre-push hook spawns further git processes that each
// need the same login. Each would otherwise show its own dialog. Instead,
// the helper that passed the dialog serves its unsealed bundles over Unix
// sockets, and only to processes that descend from the same git process.
//
// Git runs a remote helper through a `git remote-<name>` wrapper, so the
// helper's parent is that wrapper and the user's `git push` is further up.
// The server therefore anchors one socket on every ancestor that is a git
// process, and a client walks its own ancestry looking for a socket named
// after each. The user's push is the ancestor they share.
//
// The plaintext never touches disk. A socket carries no secret on its own:
// a client must descend from the socket's anchor, checked through the
// kernel-reported peer pid and the process table, and the anchor must still
// be the same process it was when serving began. Hardened-runtime release
// builds keep other processes out of the helper's memory; see
// docs/architecture/token-protection.md.

const (
	unlockDirName  = "unlock"
	unlockMaxDepth = 16
	unlockTimeout  = 2 * time.Second
	unlockMaxBytes = 1 << 20
	// Unix socket paths are limited to 104 bytes on macOS.
	unlockMaxPath = 100
	// Anchors are ancestors running git itself, not shells or agents.
	unlockAnchorComm = "git"
)

// ErrUnlockUnsupported: this platform cannot identify socket peers.
var ErrUnlockUnsupported = errors.New("per-push unlock unsupported on this platform")

// procInfo is what the ancestry check needs from the process table.
type procInfo struct {
	ppid  int
	start int64 // process start, microseconds since the epoch
	comm  string
}

// anchor is a git ancestor a socket is named after.
type anchor struct {
	pid   int
	start int64
}

// Platform hooks; tests substitute fakes. serveBundles is what a server
// hands out, swapped in tests because server and client share one cache.
var (
	procLookup   = lookupProc
	peerLookup   = lookupPeer
	currentPPID  = os.Getppid // where the server's own ancestry begins
	walkStartPID = os.Getppid // where a client's ancestry walk begins
	serveBundles = snapshotBundles
)

// prompted records that this process passed the dialog itself, which is
// what qualifies it to serve others.
var prompted atomic.Bool

// unlockPayload is the wire format, one JSON document then EOF.
type unlockPayload struct {
	Version int                    `json:"v"`
	Bundles map[string]tokenBundle `json:"bundles"`
}

func unlockSocketName(pid int) string {
	return fmt.Sprintf("git-%d.sock", pid)
}

// gitAncestors lists the git processes above pid, nearest first.
func gitAncestors(pid int) ([]anchor, error) {
	var out []anchor
	for depth := 0; depth < unlockMaxDepth && pid > 1; depth++ {
		info, err := procLookup(pid)
		if err != nil {
			return out, err
		}
		if info.comm == unlockAnchorComm {
			out = append(out, anchor{pid: pid, start: info.start})
		}
		pid = info.ppid
	}
	return out, nil
}

// UnlockServer serves this process's unsealed bundles to descendants of
// its git ancestors.
type UnlockServer struct {
	root      *os.Root
	uid       int
	listeners []*anchorListener
	wg        sync.WaitGroup
}

type anchorListener struct {
	ln     *net.UnixListener
	name   string
	anchor anchor
}

// StartUnlockServer starts serving when this process unsealed a bundle
// through the dialog. It returns a no-op stop when there is nothing to
// serve or the platform cannot vouch for peers.
func StartUnlockServer(ctx context.Context) (func(), error) {
	noop := func() {}
	if !prompted.Load() {
		return noop, nil
	}
	anchors, err := gitAncestors(currentPPID())
	if err != nil && errors.Is(err, ErrUnlockUnsupported) {
		return noop, nil
	}
	if len(anchors) == 0 {
		return noop, nil
	}
	return startUnlockServer(ctx, anchors)
}

// startUnlockServer listens on one socket per anchor.
func startUnlockServer(ctx context.Context, anchors []anchor) (func(), error) {
	noop := func() {}
	root, err := userdirs.CacheRoot()
	if err != nil {
		return noop, fmt.Errorf("open cache dir: %w", err)
	}
	if err := osroot.MkdirAllNoSymlink(root, unlockDirName, 0o700); err != nil {
		return noop, fmt.Errorf("create unlock dir: %w", err)
	}
	dir, err := userdirs.CacheDirChecked()
	if err != nil {
		return noop, fmt.Errorf("resolve cache dir: %w", err)
	}
	s := &UnlockServer{root: root, uid: os.Getuid()}
	for _, a := range anchors {
		name := filepath.Join(unlockDirName, unlockSocketName(a.pid))
		path := filepath.Join(dir, name)
		if len(path) > unlockMaxPath {
			s.stop()
			return noop, fmt.Errorf("unlock socket path too long: %d bytes", len(path))
		}
		// A stale socket from a dead helper under the same anchor pid.
		if err := osroot.RemoveNoSymlinks(root, name); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.stop()
			return noop, fmt.Errorf("remove stale unlock socket: %w", err)
		}
		ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			s.stop()
			return noop, fmt.Errorf("listen on unlock socket: %w", err)
		}
		// Belt and braces with the 0700 directory: the check is the peer test.
		if err := os.Chmod(path, 0o600); err != nil {
			_ = ln.Close()
			s.stop()
			return noop, fmt.Errorf("chmod unlock socket: %w", err)
		}
		al := &anchorListener{ln: ln, name: name, anchor: a}
		s.listeners = append(s.listeners, al)
		s.wg.Add(1)
		go s.acceptLoop(ctx, al)
	}
	return s.stop, nil
}

func (s *UnlockServer) stop() {
	for _, al := range s.listeners {
		_ = al.ln.Close()
		_ = osroot.RemoveNoSymlinks(s.root, al.name) //nolint:errcheck // best-effort cleanup; a stale socket is removed on the next start
	}
	s.wg.Wait()
}

func (s *UnlockServer) acceptLoop(ctx context.Context, al *anchorListener) {
	defer s.wg.Done()
	for {
		conn, err := al.ln.AcceptUnix()
		if err != nil {
			return
		}
		if ctx.Err() != nil {
			_ = conn.Close()
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(conn, al.anchor)
		}()
	}
}

func (s *UnlockServer) handle(conn *net.UnixConn, a anchor) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(unlockTimeout)) //nolint:errcheck // a failed deadline only loses the timeout; the peer check still runs
	pid, uid, err := peerLookup(conn)
	if err != nil || uid != s.uid || !authorized(pid, a) {
		return
	}
	payload := unlockPayload{Version: bundleVersion, Bundles: serveBundles()}
	_ = json.NewEncoder(conn).Encode(payload) //nolint:errcheck,errchkjson // the client falls back to its own dialog on a short read
}

// authorized reports whether peerPID descends from anchor a, which must
// still be the same running process and still be above this server.
func authorized(peerPID int, a anchor) bool {
	if !descendsFrom(currentPPID(), a) {
		return false // our git ancestor is gone; we were reparented
	}
	return descendsFrom(peerPID, a)
}

// descendsFrom walks up from pid looking for a, refusing on pid reuse or a
// process older than its supposed ancestor.
func descendsFrom(pid int, a anchor) bool {
	for depth := 0; depth < unlockMaxDepth && pid > 1; depth++ {
		info, err := procLookup(pid)
		if err != nil || info.start < a.start {
			return false // a descendant cannot predate its ancestor
		}
		if pid == a.pid {
			return info.start == a.start
		}
		pid = info.ppid
	}
	return false
}

func snapshotBundles() map[string]tokenBundle {
	unsealedCache.mu.Lock()
	defer unsealedCache.mu.Unlock()
	out := make(map[string]tokenBundle, len(unsealedCache.m))
	for k, v := range unsealedCache.m {
		out[k] = v
	}
	return out
}

// fetchUnlockedBundles asks an ancestor's helper for its bundles. It walks
// this process's parents looking for a socket named after each, so only a
// process inside a running push finds one. Nil means no unlock available.
func fetchUnlockedBundles() map[string]tokenBundle {
	root, err := userdirs.CacheRootForRead()
	if err != nil {
		return nil
	}
	dir, err := userdirs.CacheDirChecked()
	if err != nil {
		return nil
	}
	pid := walkStartPID()
	for depth := 0; depth < unlockMaxDepth && pid > 1; depth++ {
		name := filepath.Join(unlockDirName, unlockSocketName(pid))
		if _, err := osroot.LstatNoSymlinks(root, name); err == nil {
			if m := dialUnlock(filepath.Join(dir, name)); m != nil {
				return m
			}
		}
		info, err := procLookup(pid)
		if err != nil {
			return nil
		}
		pid = info.ppid
	}
	return nil
}

func dialUnlock(path string) map[string]tokenBundle {
	ctx, cancel := context.WithTimeout(context.Background(), unlockTimeout)
	defer cancel()
	dialer := net.Dialer{Timeout: unlockTimeout}
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return nil
	}
	defer conn.Close()
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return nil
	}
	// Only accept bundles from a process of our own user.
	if _, uid, err := peerLookup(uc); err != nil || uid != os.Getuid() {
		return nil
	}
	_ = conn.SetDeadline(time.Now().Add(unlockTimeout)) //nolint:errcheck // a failed deadline only loses the timeout on a local socket
	raw, err := io.ReadAll(io.LimitReader(conn, unlockMaxBytes))
	if err != nil || len(raw) == 0 {
		return nil
	}
	var payload unlockPayload
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Version != bundleVersion {
		return nil
	}
	return payload.Bundles
}
