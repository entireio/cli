package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// Remote install orchestration: resolve tag → fetch metadata → download +
// verify + extract → place under pkg/<name>/ → link into bin/ → write
// manifest. The bin/ link goes through the same InstallPluginFromPath used
// by local-dev installs, so conflict handling, atomic replace, and the
// Windows symlink fallbacks are shared, not duplicated.

// maxTagFallbacks bounds how many tags we walk down when the newest tag has
// no published assets (tag pushed, release not finished).
const maxTagFallbacks = 3

// RemoteInstallOptions configures InstallPluginFromRepo. It holds only genuine
// options — the repository and the expected name are required arguments,
// because a caller that omits either is making a decision, not accepting a
// default.
type RemoteInstallOptions struct {
	// Pin, when non-empty, installs exactly this tag and marks the
	// manifest pinned so upgrade skips it.
	Pin string
	// Force replaces an existing managed entry with the same name.
	Force bool
	// AllowUnverified permits installing a release that publishes no
	// checksums.txt covering this platform. Off by default: downloading and
	// executing an unauthenticated binary is the supply-chain risk the
	// checksum path exists to remove, so it takes an explicit opt-in.
	AllowUnverified bool
}

// RemoteInstallResult is what a successful remote install produced.
type RemoteInstallResult struct {
	Installed *InstalledPlugin
	Manifest  *PluginManifest
	Metadata  *PluginMetadata
	// SkippedTags lists newer tags that were passed over for missing
	// assets, newest first. Callers surface these as warnings.
	SkippedTags []string
	// ReplacedFrom is the repository a --force install displaced, set only
	// when it differs from the one just installed. --force is *for* replacing,
	// so this is not an error — but the confirmation for a URL install names a
	// URL, never the plugin it is about to overwrite, and the remote picks that
	// name. Surfacing it is what turns an uninformed replacement into an
	// informed one, and gives the user the URL to put things back.
	ReplacedFrom string
}

// InstallPluginFromRepo installs a plugin from a git repository URL.
// Dependency resolution deliberately does not happen here — callers
// (the install command) plan and confirm dependency installs first.
// expectedName is the plugin name the caller has already committed to — the
// index entry the user asked for, the requirement being satisfied, or the
// plugin being upgraded. Pass "" only for a bare `install <url>`, where the
// repository legitimately names itself; installRepoAtTag explains why a
// mismatch is otherwise fatal.
//
// It is a required argument rather than an options field on purpose. As a
// field it could be silently omitted, and omitting it reopens a no-prompt
// name-substitution hole; as an argument, passing "" is a visible choice at
// the call site.
func InstallPluginFromRepo(ctx context.Context, repoURL, expectedName string, opts RemoteInstallOptions) (*RemoteInstallResult, error) {
	repoURL = strings.TrimRight(repoURL, "/")

	var tags []string
	if opts.Pin != "" {
		tags = []string{opts.Pin}
	} else {
		var err error
		tags, err = listRemoteSemverTags(ctx, repoURL)
		if err != nil {
			return nil, err
		}
		if len(tags) == 0 {
			return nil, fmt.Errorf("%s has no stable semver tags; prereleases are skipped, so pass --pin <tag> to install one (or a non-semver tag)", redactURL(repoURL))
		}
		if len(tags) > maxTagFallbacks {
			tags = tags[:maxTagFallbacks]
		}
	}

	var lastErr error
	for i, tag := range tags {
		res, err := installRepoAtTag(ctx, repoURL, expectedName, tag, opts)
		if err == nil {
			res.SkippedTags = tags[:i]
			return res, nil
		}
		lastErr = err
		if !errors.Is(err, errAssetNotFound) {
			return nil, err
		}
	}
	return nil, lastErr
}

func installRepoAtTag(ctx context.Context, repoURL, expectedName, tag string, opts RemoteInstallOptions) (*RemoteInstallResult, error) {
	meta, err := fetchPluginMetadataAtTag(ctx, repoURL, tag)
	if err != nil {
		return nil, err
	}
	// The installed name comes from the *remote* — entire-plugin.yml's name:,
	// else the repo basename.
	var name string
	if meta != nil && meta.Name != "" {
		name = meta.Name
	} else if name, err = pluginNameFromRepoURL(repoURL); err != nil {
		return nil, err
	}

	// Reconcile it with what the caller asked for. Unchecked, the remote chose
	// the name unilaterally, which broke three things:
	//
	//  - `install <index-name>` could install something else entirely: the
	//    index entry says "safe", the repo declares "hijack", and the user gets
	//    entire-hijack on PATH with no prompt (index installs never prompt).
	//  - --force escalated across plugins: the already-installed check below
	//    tests the *remote-declared* name, so reinstalling A let A's repo
	//    declare name: B and replace an unrelated installed B.
	//  - A dependency installed under a different name never satisfied the
	//    requirement, so dependencySatisfied/doctor reported it missing forever
	//    and every future parent install re-attempted it.
	//
	// Fatal rather than a warning: every caller that sets ExpectedName has
	// already made a trust decision about *that* name, and silently honoring a
	// different one voids it. A legitimate rename is a catalog or requirement
	// to fix, and the message says so.
	if expectedName != "" && name != expectedName {
		return nil, fmt.Errorf(
			"%s declares plugin name %q but %q was requested; refusing to install under a name that was not asked for (update the plugin index entry or the requirement if the plugin was renamed)",
			redactURL(repoURL), name, expectedName)
	}

	existing, err := FindInstalledPlugin(name)
	if err != nil {
		return nil, err
	}
	if existing != nil && !opts.Force {
		return nil, fmt.Errorf("plugin %q already installed at %s; use --force to replace", name, existing.Path)
	}
	// Note a --force replace that changes where the plugin comes from. A repo
	// move is legitimate (entire-sem → entire-graph), which is why this is not
	// refused — but the caller should be able to tell the user what was
	// displaced and from where.
	var replacedFrom string
	if existing != nil {
		if prev, prevErr := LoadPluginManifest(name); prevErr == nil && prev != nil &&
			normalizeRepoURL(prev.RepoURL) != normalizeRepoURL(repoURL) {
			replacedFrom = prev.RepoURL
		}
	}

	staging, err := os.MkdirTemp("", "entire-plugin-fetch-")
	if err != nil {
		return nil, fmt.Errorf("create staging dir: %w", err)
	}
	defer os.RemoveAll(staging)

	asset, err := downloadPluginAsset(ctx, meta, repoURL, name, tag, staging, opts.AllowUnverified)
	if err != nil {
		return nil, err
	}

	binBase := pluginBinaryName(name)
	stagedBin := filepath.Join(staging, "extracted-"+binBase)
	if err := extractPluginBinary(asset.Path, name, stagedBin); err != nil {
		return nil, err
	}

	pkgDir, err := EnsurePluginPkgDir(name)
	if err != nil {
		return nil, err
	}
	sweepOldBinaries(pkgDir)
	pkgBin := filepath.Join(pkgDir, binBase)
	if err := replaceBinary(stagedBin, pkgBin); err != nil {
		return nil, fmt.Errorf("place plugin binary: %w", err)
	}

	// Digest the binary we actually placed, not just the archive it came from.
	// asset.SHA256 covers the downloaded asset, which is discarded with the
	// staging dir — so it records provenance but can never detect later
	// tampering. binDigest is what `plugin doctor` re-checks.
	binDigest, err := fileSHA256(pkgBin)
	if err != nil {
		return nil, fmt.Errorf("hash installed plugin binary: %w", err)
	}

	manifest := &PluginManifest{
		Name:         name,
		RepoURL:      repoURL,
		Tag:          tag,
		Asset:        asset.Asset,
		SHA256:       asset.SHA256,
		BinarySHA256: binDigest,
		Unverified:   !asset.Verified,
		Pinned:       opts.Pin != "",
		InstalledAt:  time.Now().UTC(),
	}
	if meta != nil {
		manifest.Requires = meta.Requires
	}
	// The manifest is written before the bin/ link, immediately after the
	// binary swap. Ordering matters because replaceBinary has already mutated
	// pkg/<name>/: until the manifest catches up, it records the *previous*
	// tag and binary_sha256 while the new binary is on disk, and
	// checkManagedBinaryIntegrity reads that as tampering — a permanent false
	// alarm on a perfectly good install. Writing it here leaves only the local
	// re-hash above in that window.
	//
	// If the bin/ link then fails, the manifest is still accurate and doctor
	// reports the real problem ("has an install manifest but no entry in the
	// managed bin dir") with a fix that works, instead of accusing the user of
	// tampering.
	if err := SavePluginManifest(manifest); err != nil {
		return nil, err
	}

	installed, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: pkgBin, Force: true})
	if err != nil {
		return nil, err
	}
	return &RemoteInstallResult{Installed: installed, Manifest: manifest, Metadata: meta, ReplacedFrom: replacedFrom}, nil
}

// UpgradeOutcome describes what UpgradeInstalledPlugin did.
type UpgradeOutcome struct {
	Name string
	// Pinned: skipped because the manifest is pinned.
	Pinned bool
	// UpToDate: already at the newest tag.
	UpToDate bool
	// FromTag/ToTag are set when an upgrade actually happened.
	FromTag, ToTag string
}

// UpgradeInstalledPlugin re-resolves the newest tag for a remote-installed
// plugin and reinstalls when it differs from the manifest's tag. Plugins
// without a manifest (local-dev symlinks) are not upgradable.
func UpgradeInstalledPlugin(ctx context.Context, name string) (*UpgradeOutcome, error) {
	m, err := LoadPluginManifest(name)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("plugin %q has no install manifest (local-dev install?); reinstall it from its repository URL to make it upgradable", name)
	}
	if m.Pinned {
		return &UpgradeOutcome{Name: name, Pinned: true}, nil
	}
	tags, err := listRemoteSemverTags(ctx, m.RepoURL)
	if err != nil {
		return nil, err
	}
	if len(tags) == 0 {
		return nil, fmt.Errorf("%s has no stable semver tags; prereleases are skipped, so pass --pin <tag> to move to one", redactURL(m.RepoURL))
	}
	// Semver comparison, not string equality: "v0.2.0" and "0.2.0" are the
	// same version, and a remote that re-tagged with the other spelling
	// must not trigger a reinstall.
	if semver.Compare(canonicalSemver(tags[0]), canonicalSemver(m.Tag)) <= 0 {
		return &UpgradeOutcome{Name: name, UpToDate: true}, nil
	}
	// Inherit the install-time trust decision: a plugin the user knowingly
	// installed unverified shouldn't start failing on upgrade, and one
	// installed verified must not silently downgrade to unverified.
	res, err := InstallPluginFromRepo(ctx, m.RepoURL, name, RemoteInstallOptions{
		Force: true, AllowUnverified: m.Unverified,
	})
	if err != nil {
		return nil, err
	}
	// The install may have fallen back past an asset-less newest tag and
	// landed on the version already installed — that's up-to-date, not an
	// upgrade line claiming X → X.
	if semver.Compare(canonicalSemver(res.Manifest.Tag), canonicalSemver(m.Tag)) <= 0 {
		return &UpgradeOutcome{Name: name, UpToDate: true}, nil
	}
	return &UpgradeOutcome{Name: name, FromTag: m.Tag, ToTag: res.Manifest.Tag}, nil
}

// replaceBinary moves src over dest atomically. On Windows, a running
// executable can't be replaced (sharing violation) but it *can* be renamed:
// move the old binary aside to a .old-<rand> file and retry; leftovers are
// swept on the next install. os.Rename fails across filesystems (staging is
// in the system temp dir), so a copy fallback covers that case.
func replaceBinary(src, dest string) error {
	err := os.Rename(src, dest)
	if err == nil {
		return nil
	}
	if runtime.GOOS == windowsGOOS {
		if asideErr := os.Rename(dest, oldBinaryAsidePath(dest)); asideErr == nil {
			if err = os.Rename(src, dest); err == nil {
				return nil
			}
		}
	}
	// Cross-device rename: copy + fsync-free write, then remove src.
	in, openErr := os.Open(src) //nolint:gosec // staging file we created
	if openErr != nil {
		return errors.Join(err, openErr)
	}
	defer in.Close()
	if writeErr := writeExecutable(in, dest); writeErr != nil {
		return errors.Join(err, writeErr)
	}
	_ = os.Remove(src)
	return nil
}

// oldBinaryAsidePath returns a unique .old- sibling path for the
// rename-aside trick. Random suffix so concurrent upgrades can't collide.
func oldBinaryAsidePath(dest string) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return dest + ".old-fallback"
	}
	return dest + ".old-" + hex.EncodeToString(b[:])
}

// sweepOldBinaries best-effort removes .old-* leftovers from the Windows
// rename-aside fallback. Failures are ignored — the files are inert.
func sweepOldBinaries(pkgDir string) {
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".old-") {
			_ = os.Remove(filepath.Join(pkgDir, e.Name()))
		}
	}
}

// RemoveManagedPlugin removes a plugin's bin entries and its pkg dir.
// The dependency guard lives in the command layer so --force can bypass it.
func RemoveManagedPlugin(name string) error {
	if err := RemoveInstalledPlugin(name); err != nil {
		return err
	}
	return RemovePluginPkg(name)
}
