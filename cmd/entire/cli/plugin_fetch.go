package cli

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// Release-asset download. The one irreducibly forge-specific piece of the
// plugin system is where release binaries live; it's contained here as a
// small per-host URL convention table with a declarative escape hatch
// (download_url in entire-plugin.yml). Everything else — version listing,
// metadata, the index — runs on the git protocol.

// errAssetNotFound distinguishes "this tag has no matching asset" (worth
// trying the next-highest tag — a pushed tag whose release isn't published
// yet) from transport errors (not worth retrying on another tag).
var errAssetNotFound = errors.New("no matching release asset")

// errUnverifiedAsset means an asset was reachable but nothing authenticated
// it — no checksums.txt covering this platform. Distinct from
// errAssetNotFound so it never triggers the next-highest-tag walk: falling
// back to an older tag wouldn't make the download any more trustworthy.
var errUnverifiedAsset = errors.New("release asset cannot be verified")

// requireSecureAssetURL gates every plugin HTTP fetch on a transport that a
// network attacker can't rewrite.
//
// Plaintext HTTP is not merely "less good" here: the asset and the
// checksums.txt that authenticates it come from the same origin, so an
// attacker who can rewrite one can rewrite both, and checksum verification
// becomes theatre. HTTPS is therefore required for anything off-machine.
//
// Loopback is exempt so httptest-backed tests and local forge experiments
// keep working — traffic that never leaves the machine has no network
// attacker to defend against.
func requireSecureAssetURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse download URL %q: %w", redactURL(rawURL), err)
	}
	switch strings.ToLower(u.Scheme) {
	case schemeHTTPS:
		return nil
	case schemeHTTP:
		if isLoopbackHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("refusing to download %s over plaintext HTTP: the asset and its %s would share an origin, so a network attacker could replace both and checksum verification would prove nothing; serve the release over HTTPS or declare an https download_url in %s",
			redactURL(rawURL), checksumsFileName, pluginMetadataFileName)
	default:
		return fmt.Errorf("plugin downloads must use https; %q has scheme %q", redactURL(rawURL), u.Scheme)
	}
}

// isLoopbackHost reports whether host addresses this machine.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// maxAssetRedirects bounds the redirect chain for an asset fetch. Release
// hosts routinely redirect to a CDN, so some hops are expected.
const maxAssetRedirects = 10

// pluginHTTPClient bounds the total time for a single asset download.
// Plugins can be tens of MB; 5 minutes is generous even on slow links.
//
// CheckRedirect re-runs the transport check on every hop. Without it the
// https-only rule is trivially bypassed: Go's default policy follows up to 10
// redirects *across schemes* and only the initial URL was ever validated, so an
// https:// entry point could deliver both the asset and its checksums.txt over
// plaintext — exactly the outcome requireSecureAssetURL exists to prevent.
var pluginHTTPClient = &http.Client{
	Timeout: 5 * time.Minute,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxAssetRedirects {
			return fmt.Errorf("stopped after %d redirects", maxAssetRedirects)
		}
		return requireSecureAssetURL(req.URL.String())
	},
}

// maxPluginAssetSize caps a downloaded asset. Real plugin binaries are tens
// of MB; the cap exists so a misconfigured URL serving an endless stream
// can't fill the disk.
const maxPluginAssetSize = 512 << 20 // 512 MiB

// maxChecksumsSize caps a checksums.txt fetch. One line per released artifact,
// so 1 MiB covers thousands.
const maxChecksumsSize = 1 << 20

// releaseAssetBaseURL returns the URL prefix release assets live under for
// the repo's host. GitHub and the Gitea family share a convention; GitLab
// has its own. Unknown hosts default to the GitHub-style convention (most
// self-hosted forges mirror it) — authors on hosts that don't can declare
// download_url in entire-plugin.yml.
func releaseAssetBaseURL(repoURL, tag string) (string, error) {
	u, err := url.Parse(strings.TrimSuffix(repoURL, ".git"))
	if err != nil {
		return "", fmt.Errorf("parse repo URL: %w", err)
	}
	if u.Scheme != schemeHTTPS && u.Scheme != schemeHTTP {
		return "", fmt.Errorf("release downloads need an http(s) repo URL, got %q (declare download_url in %s for non-HTTP remotes)", redactURL(repoURL), pluginMetadataFileName)
	}
	base := strings.TrimSuffix(u.String(), "/")
	host := strings.ToLower(u.Hostname())
	if host == "gitlab.com" || strings.HasPrefix(host, "gitlab.") {
		return base + "/-/releases/" + url.PathEscape(tag) + "/downloads/", nil
	}
	return base + "/releases/download/" + url.PathEscape(tag) + "/", nil
}

// expandDownloadTemplate expands the author-declared download_url template.
// Placeholders: {name} {tag} {version} {os} {arch} {asset}.
func expandDownloadTemplate(tmpl, name, tag, asset string) string {
	r := strings.NewReplacer(
		"{name}", name,
		"{tag}", tag,
		"{version}", strings.TrimPrefix(tag, "v"),
		"{os}", runtime.GOOS,
		"{arch}", runtime.GOARCH,
		"{asset}", asset,
	)
	return r.Replace(tmpl)
}

// archAliases maps a GOARCH to the spellings seen in release asset names.
func archAliases(goarch string) []string {
	switch goarch {
	case "amd64":
		return []string{"amd64", "x86_64"}
	case "arm64":
		return []string{"arm64", "aarch64"}
	default:
		return []string{goarch}
	}
}

// osAliases maps a GOOS to asset-name spellings, most specific first.
func osAliases(goos string) []string {
	if goos == darwinGOOS {
		return []string{"darwin", "macos"}
	}
	return []string{goos}
}

// platformArchAliases returns the arch spellings to try for goos. On darwin it
// appends "all", which is where goreleaser puts the universal-binary marker:
// the name is entire-<x>_<ver>_darwin_all, so "all" belongs in the arch slot.
// It used to sit in the OS slot, generating _all_arm64 — a spelling no forge
// produces — so the darwin_all fallback the docs promise never actually
// matched and universal-only releases could not be installed at all.
func platformArchAliases(goos, goarch string) []string {
	aliases := archAliases(goarch)
	if goos == darwinGOOS {
		aliases = append(aliases, "all")
	}
	return aliases
}

// assetCandidates returns release asset filenames to try for this platform,
// in preference order: archives before raw binaries, version-in-name
// (goreleaser's default template) before version-less.
func assetCandidates(name, tag string) []string {
	binName := pluginBinaryPrefix + name
	version := strings.TrimPrefix(tag, "v")
	stems := make([]string, 0, 16)
	for _, osName := range osAliases(runtime.GOOS) {
		for _, arch := range platformArchAliases(runtime.GOOS, runtime.GOARCH) {
			stems = append(stems,
				fmt.Sprintf("%s_%s_%s_%s", binName, version, osName, arch),
				fmt.Sprintf("%s_%s_%s", binName, osName, arch),
			)
		}
	}
	exts := []string{".tar.gz", ".zip", ""}
	if runtime.GOOS == windowsGOOS {
		exts = []string{".zip", ".tar.gz", windowsExeExt}
	}
	var out []string
	for _, ext := range exts {
		for _, stem := range stems {
			out = append(out, stem+ext)
		}
	}
	return out
}

// checksumsFileName is the conventional name of a release's checksum
// manifest (goreleaser's default).
const checksumsFileName = "checksums.txt"

// checksumCandidates returns filenames the checksum manifest may go by.
func checksumCandidates(name, tag string) []string {
	binName := pluginBinaryPrefix + name
	version := strings.TrimPrefix(tag, "v")
	return []string{
		checksumsFileName,
		fmt.Sprintf("%s_%s_checksums.txt", binName, version),
		binName + "_checksums.txt",
	}
}

// parseChecksums parses "hex  filename" lines (sha256sum format) into a
// filename → digest map.
func parseChecksums(data []byte) map[string]string {
	out := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		// sha256sum marks binary-mode files with a leading *.
		out[strings.TrimPrefix(fields[1], "*")] = strings.ToLower(fields[0])
	}
	return out
}

// selectAssetFromChecksums picks the preferred candidate present in the
// checksum manifest. The manifest lists what was actually published, so
// this avoids probing candidate URLs one by one.
func selectAssetFromChecksums(sums map[string]string, name, tag string) (asset, digest string, ok bool) {
	for _, c := range assetCandidates(name, tag) {
		if d, present := sums[c]; present {
			return c, d, true
		}
	}
	return "", "", false
}

// fetchedAsset is the result of downloadPluginAsset: a verified asset on
// disk in a caller-owned staging dir.
type fetchedAsset struct {
	// Path is the downloaded asset file inside the staging dir.
	Path string
	// Asset is the asset filename that matched.
	Asset string
	// SHA256 is the hex digest of the downloaded bytes.
	SHA256 string
	// Verified is true when SHA256 was checked against a digest published in
	// the release's checksum manifest, rather than merely computed from
	// whatever arrived. Recorded in the install manifest so `plugin doctor`
	// can surface unverified installs.
	Verified bool
}

// downloadPluginAsset locates and downloads the release asset for name@tag
// into stagingDir, verifying against the release's checksum manifest when
// one is published. Returns errAssetNotFound (possibly wrapped) when the
// tag has no asset for this platform.
func downloadPluginAsset(ctx context.Context, meta *PluginMetadata, repoURL, name, tag, stagingDir string, allowUnverified bool) (*fetchedAsset, error) {
	// Resolve the prefix once. It does not depend on the asset name, so
	// deriving it per candidate meant re-parsing the repo URL ~36 times in the
	// probe loop and carrying an error return through three call sites for a
	// failure that can only happen here.
	var assetURL func(asset string) string
	if meta != nil && meta.DownloadURL != "" {
		assetURL = func(asset string) string {
			return expandDownloadTemplate(meta.DownloadURL, name, tag, asset)
		}
	} else {
		base, err := releaseAssetBaseURL(repoURL, tag)
		if err != nil {
			return nil, err
		}
		assetURL = func(asset string) string { return base + asset }
	}

	// A download_url template without {asset} is a single fully-specified
	// URL; there is nothing to select — and no way to locate a checksum
	// manifest, since every candidate name would expand to the same URL.
	if meta != nil && meta.DownloadURL != "" && !strings.Contains(meta.DownloadURL, "{asset}") {
		if !allowUnverified {
			return nil, fmt.Errorf("%w: %s declares a fixed download_url with no {asset} placeholder, so no %s can be located; add {asset} to the template or install with --allow-unverified",
				errUnverifiedAsset, pluginMetadataFileName, checksumsFileName)
		}
		u := expandDownloadTemplate(meta.DownloadURL, name, tag, "")
		return fetchAndVerify(ctx, u, assetNameFromURL(u), "", stagingDir)
	}

	// Preferred path: fetch the checksum manifest and pick from what was
	// actually published.
	for _, cs := range checksumCandidates(name, tag) {
		data, err := httpGetSmall(ctx, assetURL(cs))
		if err != nil {
			continue
		}
		asset, digest, ok := selectAssetFromChecksums(parseChecksums(data), name, tag)
		if !ok {
			// This manifest lists nothing for the platform. Keep going: a
			// stale or hand-written root checksums.txt must not mask a
			// goreleaser manifest under another candidate name, or a
			// published asset still reachable by the probe fallback.
			// Falling through doesn't weaken verification — an attacker
			// who controls the manifest could list a malicious digest
			// directly.
			continue
		}
		return fetchAndVerify(ctx, assetURL(asset), asset, digest, stagingDir)
	}

	// Fallback: probe candidates directly (a release published without a
	// checksum manifest). Nothing authenticates these bytes, so by default an
	// asset found this way is refused rather than installed.
	//
	// The probe still runs when verification is required: it distinguishes
	// "this tag has no release yet" (errAssetNotFound, worth walking down to
	// the next tag) from "the release exists but publishes no checksums"
	// (errUnverifiedAsset, which an older tag wouldn't fix). Getting that
	// wrong would report a missing release for a plugin that simply doesn't
	// ship checksums.
	for _, asset := range assetCandidates(name, tag) {
		fa, err := fetchAndVerify(ctx, assetURL(asset), asset, "", stagingDir)
		switch {
		case errors.Is(err, errAssetNotFound):
			continue
		case err != nil:
			return nil, err
		case allowUnverified:
			return fa, nil
		}
		_ = os.Remove(fa.Path)
		return nil, fmt.Errorf("%w: %s %s publishes %s but no %s to authenticate it; ask the author to publish checksums, or install with --allow-unverified",
			errUnverifiedAsset, name, tag, asset, checksumsFileName)
	}
	return nil, fmt.Errorf("%w for %s/%s at %s %s", errAssetNotFound, runtime.GOOS, runtime.GOARCH, redactURL(repoURL), tag)
}

// assetNameFromURL derives the staging filename for a fully-specified
// download_url. Only the URL path contributes: path.Base on the whole URL
// folds a query string into the name ("entire-run.tar.gz?token=abc"), which
// then misses extractPluginBinary's extension sniff and gets written out as
// a raw binary — a silently broken install (and an invalid filename on
// Windows).
func assetNameFromURL(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Path != "" {
		return path.Base(u.Path)
	}
	trimmed := rawURL
	if i := strings.IndexAny(trimmed, "?#"); i >= 0 {
		trimmed = trimmed[:i]
	}
	return path.Base(trimmed)
}

// httpGetSmall fetches a small text resource (checksum manifests).
func httpGetSmall(ctx context.Context, rawURL string) ([]byte, error) {
	if err := requireSecureAssetURL(rawURL); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", redactURL(rawURL), err)
	}
	resp, err := pluginHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", redactURL(rawURL), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", redactURL(rawURL), resp.Status)
	}
	// readWithinLimit, not a bare LimitReader: this fetches checksums.txt, and
	// silently truncating it would drop the line covering our asset — turning a
	// verifiable download into an "unverified" one for a reason nobody could see.
	data, err := readWithinLimit(resp.Body, maxChecksumsSize)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", redactURL(rawURL), err)
	}
	return data, nil
}

// fetchAndVerify downloads rawURL into stagingDir, streaming the SHA-256 as
// it writes. A non-empty wantDigest is enforced; an empty one records the
// computed digest. 404/410 map to errAssetNotFound. Partial files are
// removed on verification failure so a hostile or truncated download never
// lingers in staging.
// Note on credentials: rawURL may carry userinfo, because releaseAssetBaseURL
// derives the asset URL from the repo URL and url.String() re-serializes it.
// The request keeps it — that is how the download authenticates to a private
// forge — but every message below goes through redactURL, since main.go prints
// command errors to stderr and a download failure is an ordinary event
// (network hiccup, 5xx, checksum mismatch), not an exceptional one.
func fetchAndVerify(ctx context.Context, rawURL, asset, wantDigest, stagingDir string) (*fetchedAsset, error) {
	// The asset name is normally one of our own candidates, but the
	// single-URL download_url template path derives it from the URL and a
	// future caller could pass a name straight from a remote manifest.
	// Refuse anything that wouldn't stay inside stagingDir. Backslash is
	// rejected on every platform — it's a separator on Windows, and a
	// literal backslash in an asset name is never legitimate.
	if asset == "" || asset == "." || asset == ".." ||
		strings.ContainsAny(asset, `/\`) || asset != filepath.Base(asset) {
		return nil, fmt.Errorf("unsafe release asset name %q", asset)
	}
	if err := requireSecureAssetURL(rawURL); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", redactURL(rawURL), err)
	}
	resp, err := pluginHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", redactURL(rawURL), err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusGone:
		return nil, fmt.Errorf("%w: GET %s: %s", errAssetNotFound, redactURL(rawURL), resp.Status)
	default:
		return nil, fmt.Errorf("download %s: %s", redactURL(rawURL), resp.Status)
	}

	dest := filepath.Join(stagingDir, asset)
	out, err := os.Create(dest) //nolint:gosec // dest is inside the caller-owned staging dir; asset name came from our candidate list or checksum manifest
	if err != nil {
		return nil, fmt.Errorf("create staging file: %w", err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(resp.Body, maxPluginAssetSize+1))
	closeErr := out.Close()
	if err != nil {
		_ = os.Remove(dest)
		return nil, fmt.Errorf("download %s: %w", redactURL(rawURL), err)
	}
	if closeErr != nil {
		_ = os.Remove(dest)
		return nil, fmt.Errorf("write staging file: %w", closeErr)
	}
	if n > maxPluginAssetSize {
		_ = os.Remove(dest)
		return nil, fmt.Errorf("download %s: exceeds %d byte limit", redactURL(rawURL), int64(maxPluginAssetSize))
	}
	got := hex.EncodeToString(h.Sum(nil))
	if wantDigest != "" && !strings.EqualFold(got, wantDigest) {
		_ = os.Remove(dest)
		return nil, fmt.Errorf("checksum mismatch for %s: got %s, want %s", asset, got, wantDigest)
	}
	return &fetchedAsset{Path: dest, Asset: asset, SHA256: got, Verified: wantDigest != ""}, nil
}

// extractPluginBinary locates the plugin executable inside the downloaded
// asset and writes it to destPath (mode 0755 on Unix). Raw binaries are
// copied; .tar.gz/.tgz and .zip archives are searched for an entry whose
// basename matches entire-<name>[.exe], wherever it sits in the archive.
func extractPluginBinary(assetPath, name, destPath string) error {
	binName := pluginBinaryPrefix + name
	lower := strings.ToLower(assetPath)
	switch {
	case strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz"):
		return extractFromTarGz(assetPath, binName, destPath)
	case strings.HasSuffix(lower, ".zip"):
		return extractFromZip(assetPath, binName, destPath)
	default:
		src, err := os.Open(assetPath) //nolint:gosec // staging-dir file we just wrote
		if err != nil {
			return fmt.Errorf("open asset: %w", err)
		}
		defer src.Close()
		return writeExecutable(src, destPath)
	}
}

// matchesPluginBinary reports whether an archive entry basename is the
// plugin binary, tolerating a Windows extension.
func matchesPluginBinary(entryName, binName string) bool {
	base := path.Base(filepath.ToSlash(entryName))
	if base == binName {
		return true
	}
	return runtime.GOOS == windowsGOOS && strings.EqualFold(base, binName+windowsExeExt)
}

// safeArchiveEntry rejects entry names that could escape an extraction
// root. We only ever extract a single matched file to a fixed dest, so this
// is defense in depth against a hostile archive masking as a plugin.
func safeArchiveEntry(entryName string) bool {
	clean := archiveEntryPath(entryName)
	return !strings.HasPrefix(clean, "../") && !path.IsAbs(clean) && !strings.Contains(entryName, "\x00")
}

// preferredArchiveEntry picks among entries whose basename matches the plugin
// binary. Shallowest path wins, then lexicographic order.
//
// Selection has to be deterministic and independent of archive order. Matching
// is by basename, so an archive can legitimately hold more than one candidate —
// goreleaser writes the binary at the root by default but nests it under a
// versioned directory with wrap_in_directory, and archives also ship things like
// completions/entire-<name>. Taking whichever came first meant the installed
// binary depended on the order the author's tar happened to be built in.
//
// Shallowest-first encodes the convention: the real binary sits at the root, or
// at worst one directory down; a same-named file deeper in the tree is a
// completion script or a doc, not the command.
func preferredArchiveEntry(names []string, binName string) string {
	p := archiveEntryPicker{binName: binName}
	for _, name := range names {
		p.consider(name)
	}
	return p.best
}

// archiveEntryPicker keeps only the best candidate seen so far, so an archive
// can be scanned without materializing every entry name.
//
// That matters because the entry list is remote-controlled and the only prior
// check on it is that the asset's sha256 matched what the release published.
// Entry headers compress extremely well, so the 512 MiB maxPluginAssetSize
// admits tens of millions of them — enough that collecting the names would
// cost gigabytes of heap to end up choosing between the one or two entries a
// real release actually contains. A running minimum costs one string.
type archiveEntryPicker struct {
	binName string
	best    string
	depth   int
}

func (p *archiveEntryPicker) consider(name string) {
	if !safeArchiveEntry(name) || !matchesPluginBinary(name, p.binName) {
		return
	}
	clean := archiveEntryPath(name)
	depth := strings.Count(clean, "/")
	if p.best == "" || depth < p.depth || (depth == p.depth && clean < p.best) {
		p.best, p.depth = clean, depth
	}
}

// archiveEntryPath normalizes an archive entry name to cleaned, slash-separated
// form. Selection and extraction must normalize identically — otherwise the
// entry preferredArchiveEntry picked would not match the one extraction
// compares against, and the install would fail on an archive that contains the
// binary.
func archiveEntryPath(name string) string {
	return path.Clean(filepath.ToSlash(name))
}

// errNoArchiveEntry reports an archive that holds no usable binary, either
// because nothing matched or because the chosen entry vanished between passes.
func errNoArchiveEntry(archivePath, binName string) error {
	return fmt.Errorf("archive %s contains no %s entry", filepath.Base(archivePath), binName)
}

// walkTarGz calls fn for each regular-file entry in a tar.gz, stopping early
// when fn returns stop. Both the listing pass and the extraction pass go
// through it, so the open/gzip/tar scaffolding exists in one place.
func walkTarGz(archivePath string, fn func(hdr *tar.Header, r io.Reader) (stop bool, err error)) error {
	f, err := os.Open(archivePath) //nolint:gosec // staging-dir file we just wrote
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("open gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		stop, err := fn(hdr, tr)
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
	}
}

// selectTarEntry scans a tar.gz and returns the entry extraction should take,
// holding one candidate name at a time rather than the whole listing.
func selectTarEntry(archivePath, binName string) (string, error) {
	p := archiveEntryPicker{binName: binName}
	if err := walkTarGz(archivePath, func(hdr *tar.Header, _ io.Reader) (bool, error) {
		p.consider(hdr.Name)
		return false, nil
	}); err != nil {
		return "", err
	}
	return p.best, nil
}

func extractFromTarGz(archivePath, binName, destPath string) error {
	// Two passes: a tar stream cannot seek backwards, so choosing among
	// candidates means selecting first and extracting second. The cost is one
	// extra inflation of an archive already capped at maxPluginAssetSize —
	// ~100ms on a 50 MiB archive, against a download measured in seconds —
	// which buys a deterministic choice.
	want, err := selectTarEntry(archivePath, binName)
	if err != nil {
		return err
	}
	if want == "" {
		return errNoArchiveEntry(archivePath, binName)
	}
	found := false
	if err := walkTarGz(archivePath, func(hdr *tar.Header, r io.Reader) (bool, error) {
		if archiveEntryPath(hdr.Name) != want {
			return false, nil
		}
		found = true
		return true, writeExecutable(r, destPath)
	}); err != nil {
		return err
	}
	if !found {
		return errNoArchiveEntry(archivePath, binName)
	}
	return nil
}

func extractFromZip(archivePath, binName, destPath string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer zr.Close()
	p := archiveEntryPicker{binName: binName}
	for _, f := range zr.File {
		if !f.FileInfo().IsDir() {
			p.consider(f.Name)
		}
	}
	want := p.best
	if want == "" {
		return errNoArchiveEntry(archivePath, binName)
	}
	// zip is random-access, so the chosen entry is reachable directly — no
	// second scan, and no Close deferred inside a loop.
	i := slices.IndexFunc(zr.File, func(f *zip.File) bool {
		return !f.FileInfo().IsDir() && archiveEntryPath(f.Name) == want
	})
	if i < 0 {
		return errNoArchiveEntry(archivePath, binName)
	}
	rc, err := zr.File[i].Open()
	if err != nil {
		return fmt.Errorf("open zip entry: %w", err)
	}
	defer rc.Close()
	return writeExecutable(rc, destPath)
}

// fileSHA256 returns the hex SHA-256 of the file at path. Used to record the
// installed binary's digest and to re-check it in `plugin doctor`.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // path is inside the managed plugin tree
	if err != nil {
		return "", fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeExecutable writes r to destPath with the executable bit set, bounded by
// maxPluginAssetSize. Explicit chmod (not inherited archive mode) because
// zip-built and raw-binary releases routinely lose the bit — the #1
// "downloaded plugin doesn't run" failure.
func writeExecutable(r io.Reader, destPath string) error {
	return writeExecutableLimited(r, destPath, maxPluginAssetSize)
}

// writeExecutableLimited is writeExecutable with an explicit cap, so the
// oversize path is testable without materializing 512 MiB.
//
// The cap lives here rather than in each caller's io.LimitReader because a
// bare LimitReader makes truncation invisible: io.Copy returns a nil error at
// the limit, so an archive entry larger than the cap was silently truncated
// and installed as the plugin binary with the install reporting success. Worse
// since binary_sha256 landed — the digest is computed from the truncated bytes,
// so `plugin doctor` would confirm the corrupt binary as intact. Reading one
// byte past the cap makes the overflow detectable, mirroring fetchAndVerify.
func writeExecutableLimited(r io.Reader, destPath string, limit int64) error {
	// Write to a sibling temp file and rename into place. Two reasons, both
	// load-bearing:
	//
	//  - O_CREATE without O_EXCL follows a symlink at destPath, so a symlink
	//    planted at pkg/<name>/entire-<name> by an earlier malicious plugin
	//    would redirect this write anywhere the user can write (~/.zshrc, a git
	//    hook). Creating a fresh temp name and renaming cannot follow anything.
	//  - Opening destPath with O_TRUNC destroys the working binary before the
	//    new bytes are known-good, so an interrupted or failed copy left the
	//    plugin broken. This path is the *normal* one whenever staging and the
	//    managed dir are on different filesystems (a tmpfs /tmp), not an edge
	//    case.
	tmp, err := os.CreateTemp(filepath.Dir(destPath), ".tmp-"+filepath.Base(destPath)+"-*")
	if err != nil {
		return fmt.Errorf("create staging binary: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename succeeds

	n, err := io.Copy(tmp, io.LimitReader(r, limit+1))
	if err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close binary: %w", err)
	}
	if n > limit {
		return fmt.Errorf("plugin binary exceeds the %d byte limit", limit)
	}
	// CreateTemp makes 0600; the dispatcher needs it executable.
	if err := os.Chmod(tmpName, 0o755); err != nil { //nolint:gosec // a plugin binary must be executable
		return fmt.Errorf("set executable bit: %w", err)
	}
	if err := os.Rename(tmpName, destPath); err != nil {
		return fmt.Errorf("place binary: %w", err)
	}
	return nil
}
