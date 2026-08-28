package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"golang.org/x/mod/semver"
	"gopkg.in/yaml.v3"
)

// Managed-install manifests. A plugin installed from a remote repository
// gets a per-plugin home under the managed dir:
//
//	<plugin-root>/pkg/<name>/entire-<name>[.exe]   the binary
//	<plugin-root>/pkg/<name>/manifest.yml          install provenance
//
// The bin/ dir remains the only dispatch surface — pkg/ binaries are
// linked into bin/ via the same symlink→hardlink→copy fallback as local
// installs, so the kubectl-style resolver in plugin.go never changes.
//
// Local-dev installs (`entire plugin install ./path`) bypass pkg/ entirely
// and have no manifest; ListPluginManifests simply doesn't see them.
const (
	pluginManagedPkgSubdir = "pkg"
	pluginManifestFileName = "manifest.yml"
	// windowsExeExt is the extension Windows executables need.
	windowsExeExt = ".exe"
	// maxPluginManifestSize bounds manifest.yml. We write it ourselves, but a
	// read with no ceiling is a ceiling of "however large the file on disk is",
	// and this one sits in a directory the user can edit.
	maxPluginManifestSize = 1 << 20
)

// readFileLimited reads at most limit bytes from path, erroring rather than
// silently truncating when the file is larger. os.ReadFile has no ceiling: it
// sizes its buffer from the file and reads to EOF, so every caller inherits
// whatever is on disk.
// pluginPkgName renders a plugin's pkg directory as a name inside pluginRoot.
// The name is validated first, as PluginPkgDir does, but the root is what makes
// that validation a defence rather than the only one.
func pluginPkgName(name string) (string, error) {
	if err := validatePluginName(name); err != nil {
		return "", err
	}
	return pluginManagedPkgSubdir + "/" + name, nil
}

// readFileLimitedIn is readFileLimited for a name inside root.
func readFileLimitedIn(root *os.Root, name string, limit int64) ([]byte, error) {
	f, err := root.Open(name)
	if err != nil {
		// Returned bare so callers can still test it with errors.Is against
		// os.ErrNotExist — LoadPluginManifest depends on that to report an
		// absent manifest as (nil, nil) rather than a failure.
		return nil, err //nolint:wrapcheck // see above
	}
	defer f.Close()
	return readWithinLimit(f, limit)
}

func readFileLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // paths here are inside the managed plugin tree or our cache
	if err != nil {
		// Returned bare so callers can still test it with errors.Is against
		// os.ErrNotExist — LoadPluginManifest depends on that to report an
		// absent manifest as (nil, nil) rather than a failure.
		return nil, err //nolint:wrapcheck // see above
	}
	defer f.Close()
	data, err := readWithinLimit(f, limit)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return data, nil
}

// pluginBinaryName returns the on-disk executable name for a bare plugin
// name: entire-<name>, plus the Windows extension where the host needs it.
func pluginBinaryName(name string) string {
	if runtime.GOOS == windowsGOOS {
		return pluginBinaryPrefix + name + windowsExeExt
	}
	return pluginBinaryPrefix + name
}

// PluginManifest records how a managed plugin was installed. Settings
// configure behavior; manifests record facts. The dependency list is
// copied from the plugin's metadata at install time so reverse-dependency
// checks (remove guard, doctor) work offline.
type PluginManifest struct {
	// Name is the bare plugin name ("run" for entire-run).
	Name string `yaml:"name"`
	// RepoURL is the full git URL the plugin was installed from.
	RepoURL string `yaml:"repo_url"`
	// Tag is the git tag that was installed (e.g. "v0.2.1").
	Tag string `yaml:"tag"`
	// Asset is the release asset filename the binary came from. Empty for
	// raw-binary downloads where the asset name equals the binary name.
	Asset string `yaml:"asset,omitempty"`
	// SHA256 is the hex digest of the downloaded asset.
	SHA256 string `yaml:"sha256,omitempty"`
	// BinarySHA256 is the hex digest of the installed binary under
	// pkg/<name>/. Distinct from SHA256, which covers the downloaded asset —
	// usually an archive, and discarded with the staging dir. Recording the
	// binary is what lets `plugin doctor` detect post-install tampering.
	BinarySHA256 string `yaml:"binary_sha256,omitempty"`
	// Unverified records that no checksum manifest authenticated the
	// download (installed with --allow-unverified). Surfaced by doctor.
	Unverified bool `yaml:"unverified,omitempty"`
	// Pinned marks installs done with --pin; upgrade skips them.
	Pinned bool `yaml:"pinned,omitempty"`
	// InstalledAt is when the install (or last upgrade) completed.
	InstalledAt time.Time `yaml:"installed_at,omitempty"`
	// Requires is the dependency list from the plugin's entire-plugin.yml
	// at the installed tag.
	Requires []PluginRequirement `yaml:"requires,omitempty"`
}

// PluginRequirement declares a dependency on another plugin. Shared between
// the author-side metadata file (entire-plugin.yml) and the install-side
// manifest so the two can never drift.
//
// There is deliberately no repo_url. A missing dependency resolves by name
// through the plugin index, so the URL a dependency install fetches from always
// comes from the curated catalog rather than from the requiring plugin's author.
// With an author-supplied URL, installing one plugin meant fetching and
// executing a binary from a URL its author chose — and dependency planning
// contacted that URL *before* the confirmation prompt. The capability is not
// gone, the authority moved: a user can still install an out-of-catalog
// dependency by URL themselves, with the usual untrusted-source prompt, after
// which the requirement is satisfied. Consent belongs to the user, not the
// author.
//
// A dependency must therefore be indexed before a dependent can ship, the same
// constraint krew, Homebrew and apt impose.
type PluginRequirement struct {
	// Name is the bare plugin name of the dependency, resolved through the
	// plugin index.
	Name string `yaml:"name"`
	// MinVersion is the minimum acceptable tag (e.g. "v0.2.0"). Minimum
	// only — there is deliberately no range syntax.
	MinVersion string `yaml:"min_version,omitempty"`
}

// PluginMetadata is the author-side declaration committed at the root of a
// plugin repository as entire-plugin.yml. Everything is optional — a repo
// without the file installs fine; the name then derives from the repo URL.
type PluginMetadata struct {
	// Name is the bare plugin name. When empty, derived from the repo URL
	// basename (entire-run → run).
	Name string `yaml:"name,omitempty"`
	// Description is a one-line summary shown by info/search.
	Description string `yaml:"description,omitempty"`
	// DownloadURL overrides the per-forge release-asset URL convention.
	// Template placeholders: {name} {tag} {version} {os} {arch} {asset}.
	// When {asset} is present, candidate asset filenames are substituted;
	// otherwise the expanded template is fetched as-is.
	DownloadURL string `yaml:"download_url,omitempty"`
	// Requires lists plugins this plugin needs at runtime.
	Requires []PluginRequirement `yaml:"requires,omitempty"`
}

// pluginMetadataFileName is the well-known path of the author-side metadata
// file at the root of a plugin repository.
const pluginMetadataFileName = "entire-plugin.yml"

// ParsePluginMetadata decodes entire-plugin.yml content.
//
// Decoding is lenient: unknown keys are ignored, matching the choice made for
// index.json. Both are artifacts read by every CLI version ever shipped, and
// refusing one an older binary doesn't fully understand breaks it permanently
// for everyone on that version — the plugin author cannot fix it for them.
// entire-plugin.yml has no version field to gate on either, so the first author
// to adopt any future field (min_cli_version, bin_name, …) would break installs
// on every older CLI.
//
// This deliberately gives up catching author typos here, which strict decoding
// did. The trade is asymmetric: a misspelled key costs the author one confused
// test run against their own plugin, while a forward-compatibility break is
// unfixable and fleet-wide. Author-side validation belongs in a lint command,
// not in the hot path every user's install runs through.
func ParsePluginMetadata(data []byte) (*PluginMetadata, error) {
	var meta PluginMetadata
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&meta); err != nil {
		// An empty or comment-only stream decodes to io.EOF. The file is
		// documented as optional and a *missing* one is fine, so a committed
		// placeholder must not be fatal at every tag. An explicit "---" already
		// behaved this way; align the empty cases with it.
		if !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("parsing %s: %w", pluginMetadataFileName, err)
		}
		meta = PluginMetadata{}
	}
	if meta.Name != "" {
		if err := validatePluginName(meta.Name); err != nil {
			return nil, fmt.Errorf("%s declares invalid name: %w", pluginMetadataFileName, err)
		}
	}
	for _, req := range meta.Requires {
		if err := validatePluginName(req.Name); err != nil {
			return nil, fmt.Errorf("%s declares invalid requirement: %w", pluginMetadataFileName, err)
		}
		// A malformed min_version silently removes the floor rather than
		// failing: x/mod/semver ranks an invalid string below every valid one,
		// so semver.Compare(installedTag, garbage) >= 0 is always true and
		// dependencySatisfied reports any version as acceptable. "vtypo",
		// "latest" and ">=1.0" all behave that way.
		//
		// Rejecting this is consistent with the lenient decoding above, not in
		// tension with it. Unknown *keys* stay lenient because they are a newer
		// CLI's fields and refusing them breaks older binaries permanently. A
		// malformed *value of a known field* is an author error that no CLI
		// version will ever accept — which is why req.Name is already validated
		// right here.
		if req.MinVersion != "" && !semver.IsValid(canonicalSemver(req.MinVersion)) {
			return nil, fmt.Errorf("%s requirement %q declares invalid min_version %q: want a semver tag such as v0.2.0 (a minimum only — ranges are not supported)",
				pluginMetadataFileName, req.Name, req.MinVersion)
		}
	}
	return &meta, nil
}

// PluginPkgDir returns the per-plugin package directory for the given bare
// name. Not created — callers use EnsurePluginPkgDir when writing.
func PluginPkgDir(name string) (string, error) {
	if err := validatePluginName(name); err != nil {
		return "", err
	}
	parent, err := pluginParentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, pluginManagedPkgSubdir, name), nil
}

// EnsurePluginPkgDir creates the package directory for name.
func EnsurePluginPkgDir(name string) (string, error) {
	dir, err := PluginPkgDir(name)
	if err != nil {
		return "", err
	}
	name, err = pluginPkgName(name)
	if err != nil {
		return "", err
	}
	root, err := pluginRoot(true)
	if err != nil {
		return "", err
	}
	if err := osroot.MkdirAllNoSymlink(root, name, 0o750); err != nil {
		return "", fmt.Errorf("create plugin pkg dir: %w", err)
	}
	return dir, nil
}

// LoadPluginManifest reads the manifest for name. Returns (nil, nil) when
// the plugin has no manifest — local-dev installs and raw-PATH plugins.
func LoadPluginManifest(name string) (*PluginManifest, error) {
	pkgName, err := pluginPkgName(name)
	if err != nil {
		return nil, err
	}
	root, err := pluginRoot(false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil //nolint:nilnil // no managed tree => no manifest
		}
		return nil, err
	}
	data, err := readFileLimitedIn(root, pkgName+"/"+pluginManifestFileName, maxPluginManifestSize)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil //nolint:nilnil // no-manifest signal
		}
		return nil, fmt.Errorf("read plugin manifest: %w", err)
	}
	var m PluginManifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse plugin manifest for %q: %w", name, err)
	}
	return &m, nil
}

// SavePluginManifest writes the manifest into the plugin's pkg dir.
func SavePluginManifest(m *PluginManifest) error {
	// Strip any credentials before persisting: manifest.yml is mode 0644 and a
	// token in a remote URL has no business on disk. Upgrades re-resolve the
	// remote through git's credential helpers, which is where auth belongs.
	m.RepoURL = redactURL(m.RepoURL)
	dir, err := EnsurePluginPkgDir(m.Name)
	if err != nil {
		return err
	}
	data, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal plugin manifest: %w", err)
	}
	// Atomic via the shared helper: a torn manifest would read as either a
	// missing install or a digest mismatch, and this write happens moments
	// after the binary it describes was swapped in.
	path := filepath.Join(dir, pluginManifestFileName)
	if err := jsonutil.WriteFileAtomic(path, data, 0o644); err != nil {
		return fmt.Errorf("write plugin manifest: %w", err)
	}
	return nil
}

// ListPluginManifests returns the manifests of every remote-installed
// plugin, sorted by name. Pkg entries without a readable manifest are
// skipped — a half-removed plugin shouldn't break listing.
func ListPluginManifests() ([]*PluginManifest, error) {
	parent, err := pluginParentDir()
	if err != nil {
		return nil, err
	}
	_ = parent // resolved above so a broken plugin dir still reports one error
	root, err := pluginRoot(false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	entries, err := osroot.ReadDir(root, pluginManagedPkgSubdir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read plugin pkg dir: %w", err)
	}
	var out []*PluginManifest
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m, err := LoadPluginManifest(e.Name())
		if err != nil || m == nil {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// RemovePluginPkg deletes the plugin's pkg dir (binary + manifest).
// Missing dir is not an error — local-dev installs never had one.
func RemovePluginPkg(name string) error {
	name, err := pluginPkgName(name)
	if err != nil {
		return err
	}
	root, err := pluginRoot(false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // nothing to remove
		}
		return err
	}
	if err := root.RemoveAll(name); err != nil {
		return fmt.Errorf("remove plugin pkg dir: %w", err)
	}
	return nil
}
