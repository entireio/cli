package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// staticServer serves body for every request. Four tests hand-rolled this
// three-line handler; the //nolint:errcheck for the test-server write now
// appears once instead of eleven times.
func staticServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body) //nolint:errcheck // test server write; failure surfaces as a client error
	}))
	t.Cleanup(srv.Close)
	return srv
}

// assetServer serves one release asset and, when checksums is non-empty, a
// checksums.txt alongside it. Everything else 404s — which is how a release
// that publishes no checksum manifest behaves.
func assetServer(t *testing.T, asset string, payload []byte, checksums string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case checksums != "" && strings.HasSuffix(r.URL.Path, checksumsFileName):
			_, _ = w.Write([]byte(checksums)) //nolint:errcheck // test server write
		case strings.HasSuffix(r.URL.Path, asset):
			_, _ = w.Write(payload) //nolint:errcheck // test server write
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// notFoundServer 404s everything: a tag with no published release.
func notFoundServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	return srv
}

func TestReleaseAssetBaseURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		repo, tag, want string
		wantErr         bool
	}{
		{repo: "https://github.com/entireio/entire-run", tag: "v1.0.0", want: "https://github.com/entireio/entire-run/releases/download/v1.0.0/"},
		{repo: "https://github.com/entireio/entire-run.git", tag: "v1.0.0", want: "https://github.com/entireio/entire-run/releases/download/v1.0.0/"},
		{repo: "https://gitlab.com/group/entire-foo", tag: "v2.1.0", want: "https://gitlab.com/group/entire-foo/-/releases/v2.1.0/downloads/"},
		{repo: "https://gitlab.example.com/group/entire-foo", tag: "v2.1.0", want: "https://gitlab.example.com/group/entire-foo/-/releases/v2.1.0/downloads/"},
		{repo: "https://codeberg.org/me/entire-bar", tag: "v0.1.0", want: "https://codeberg.org/me/entire-bar/releases/download/v0.1.0/"},
		// Unknown hosts default to the GitHub-style convention.
		{repo: "https://git.example.com/me/entire-bar", tag: "v0.1.0", want: "https://git.example.com/me/entire-bar/releases/download/v0.1.0/"},
		// Non-HTTP remotes can't derive a download URL.
		{repo: "git@github.com:entireio/entire-run.git", tag: "v1.0.0", wantErr: true},
		{repo: "ssh://git@example.com/entire-run", tag: "v1.0.0", wantErr: true},
	}
	for _, tt := range tests {
		got, err := releaseAssetBaseURL(tt.repo, tt.tag)
		if tt.wantErr {
			if err == nil {
				t.Errorf("releaseAssetBaseURL(%q) = %q, want error", tt.repo, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("releaseAssetBaseURL(%q): %v", tt.repo, err)
			continue
		}
		if got != tt.want {
			t.Errorf("releaseAssetBaseURL(%q) = %q, want %q", tt.repo, got, tt.want)
		}
	}
}

func TestExpandDownloadTemplate(t *testing.T) {
	t.Parallel()
	got := expandDownloadTemplate("https://dl.example.com/{name}/{tag}/{version}/{os}_{arch}/{asset}", "run", "v1.2.3", "a.tar.gz")
	want := fmt.Sprintf("https://dl.example.com/run/v1.2.3/1.2.3/%s_%s/a.tar.gz", runtime.GOOS, runtime.GOARCH)
	if got != want {
		t.Errorf("expandDownloadTemplate = %q, want %q", got, want)
	}
}

// A fully-specified download_url may carry a query string (signed URLs,
// artifact proxies). path.Base on the whole URL folds it into the filename,
// which then misses the archive-extension sniff in extractPluginBinary and
// gets written out as a raw binary — a silently broken install.
func TestAssetNameFromURL(t *testing.T) {
	t.Parallel()
	tests := []struct{ url, want string }{
		{url: "https://ex.com/rel/v1/entire-foo.tar.gz", want: "entire-foo.tar.gz"},
		{url: "https://ex.com/rel/entire-foo.tar.gz?token=abc", want: "entire-foo.tar.gz"},
		{url: "https://ex.com/rel/entire-foo.zip#frag", want: "entire-foo.zip"},
		{url: "https://ex.com/rel/entire-foo", want: "entire-foo"},
	}
	for _, tt := range tests {
		if got := assetNameFromURL(tt.url); got != tt.want {
			t.Errorf("assetNameFromURL(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

// Plaintext HTTP would let a network attacker rewrite the asset *and* the
// checksums.txt meant to authenticate it, since both come from one origin.
// Loopback is exempt so httptest fixtures and local forges still work.
func TestRequireSecureAssetURL(t *testing.T) {
	t.Parallel()
	ok := []string{
		"https://github.com/e/entire-run/releases/download/v1/a.tar.gz",
		"http://127.0.0.1:8080/dl/a.tar.gz",
		"http://[::1]:9000/dl/a.tar.gz",
		"http://localhost:1234/dl/a.tar.gz",
	}
	for _, u := range ok {
		if err := requireSecureAssetURL(u); err != nil {
			t.Errorf("requireSecureAssetURL(%q) = %v, want nil", u, err)
		}
	}
	bad := []string{
		"http://example.com/dl/a.tar.gz",
		"http://192.168.1.10/dl/a.tar.gz",
		"ftp://example.com/a.tar.gz",
		"file:///etc/passwd",
	}
	for _, u := range bad {
		if err := requireSecureAssetURL(u); err == nil {
			t.Errorf("requireSecureAssetURL(%q) = nil, want error", u)
		}
	}
}

// A plaintext non-loopback download must be refused at the fetch boundary,
// not merely discouraged, so the refusal covers the author-declared
// download_url escape hatch as well as the derived forge URLs.
func TestDownloadPluginAsset_RefusesPlaintextHTTP(t *testing.T) {
	t.Parallel()
	meta := &PluginMetadata{DownloadURL: "http://plugins.example.com/dl/{asset}"}
	_, err := downloadPluginAsset(context.Background(), meta,
		"https://example.invalid/entire-run", "run", "v1.0.0", t.TempDir(), false)
	if err == nil || !strings.Contains(err.Error(), "plaintext HTTP") {
		t.Errorf("err = %v, want a plaintext-HTTP refusal", err)
	}
}

// A release that publishes an asset but no checksums.txt is refused by
// default: nothing authenticates those bytes, and we are about to make them
// executable. --allow-unverified is the explicit opt-in.
func TestDownloadPluginAsset_UnverifiedRequiresOptIn(t *testing.T) {
	t.Parallel()
	asset := fmt.Sprintf("entire-run_1.0.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	payload := makeTarGz(t, map[string][]byte{"entire-run": []byte("payload")})
	srv := assetServer(t, asset, payload, "") // no checksums.txt published
	meta := &PluginMetadata{DownloadURL: srv.URL + "/dl/{asset}"}

	staging := t.TempDir()
	_, err := downloadPluginAsset(context.Background(), meta,
		"https://example.invalid/entire-run", "run", "v1.0.0", staging, false)
	if !errors.Is(err, errUnverifiedAsset) {
		t.Fatalf("err = %v, want errUnverifiedAsset", err)
	}
	// The refused download must not be left behind in staging.
	entries, dirErr := os.ReadDir(staging)
	if dirErr != nil {
		t.Fatal(dirErr)
	}
	if len(entries) != 0 {
		t.Errorf("refused download left %d file(s) in staging", len(entries))
	}

	fa, err := downloadPluginAsset(context.Background(), meta,
		"https://example.invalid/entire-run", "run", "v1.0.0", t.TempDir(), true)
	if err != nil {
		t.Fatalf("downloadPluginAsset with opt-in: %v", err)
	}
	if fa.Verified {
		t.Error("Verified = true for a download with no checksum manifest")
	}
}

// A fixed download_url (no {asset}) can't locate a checksum manifest at all,
// so it is unverifiable by construction and needs the same opt-in.
func TestDownloadPluginAsset_FixedURLRequiresOptIn(t *testing.T) {
	t.Parallel()
	meta := &PluginMetadata{DownloadURL: "https://dl.example.com/entire-run.tar.gz"}
	_, err := downloadPluginAsset(context.Background(), meta,
		"https://example.invalid/entire-run", "run", "v1.0.0", t.TempDir(), false)
	if !errors.Is(err, errUnverifiedAsset) {
		t.Errorf("err = %v, want errUnverifiedAsset", err)
	}
}

func TestAssetCandidates_CoverConventions(t *testing.T) {
	t.Parallel()
	cands := assetCandidates("run", "v1.2.3")
	mustContain := []string{
		fmt.Sprintf("entire-run_1.2.3_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH),
		fmt.Sprintf("entire-run_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH),
		fmt.Sprintf("entire-run_1.2.3_%s_%s.zip", runtime.GOOS, runtime.GOARCH),
	}
	for _, want := range mustContain {
		found := false
		for _, c := range cands {
			if c == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("assetCandidates missing %q in %v", want, cands)
		}
	}
	// Arch aliases: amd64 hosts must also try x86_64 spellings.
	if runtime.GOARCH == "amd64" {
		found := false
		for _, c := range cands {
			if strings.Contains(c, "x86_64") {
				found = true
				break
			}
		}
		if !found {
			t.Error("assetCandidates lacks x86_64 alias on amd64")
		}
	}
}

func TestParseChecksums_AndSelect(t *testing.T) {
	t.Parallel()
	osName, arch := runtime.GOOS, runtime.GOARCH
	manifest := fmt.Sprintf(`
abc123  entire-run_1.0.0_%s_%s.tar.gz
def456  *entire-run_1.0.0_other_other.zip

malformed line without two fields maybe three
`, osName, arch)
	sums := parseChecksums([]byte(manifest))
	if len(sums) != 2 {
		t.Fatalf("parseChecksums = %d entries, want 2: %v", len(sums), sums)
	}
	asset, digest, ok := selectAssetFromChecksums(sums, "run", "v1.0.0")
	if !ok {
		t.Fatal("selectAssetFromChecksums found nothing")
	}
	if digest != "abc123" || !strings.Contains(asset, osName) {
		t.Errorf("selected %q/%q, want platform asset with digest abc123", asset, digest)
	}
	// A manifest with no matching platform asset must report not-ok.
	if _, _, ok := selectAssetFromChecksums(map[string]string{"entire-run_1.0.0_plan9_mips.tar.gz": "x"}, "run", "v1.0.0"); ok {
		t.Error("selectAssetFromChecksums matched a foreign platform")
	}
}

// makeTarGz builds an in-memory tar.gz with the given entries.
func makeTarGz(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeZip(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractPluginBinary_TarGz(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	archive := filepath.Join(dir, "a.tar.gz")
	payload := []byte("#!/bin/sh\necho run\n")
	if err := os.WriteFile(archive, makeTarGz(t, map[string][]byte{
		"README.md":               []byte("docs"),
		"subdir/entire-run":       payload,
		"../escape-attempt":       []byte("nope"),
		"unrelated/entire-runner": []byte("close but no"),
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "out")
	if err := extractPluginBinary(archive, "run", dest); err != nil {
		t.Fatalf("extractPluginBinary: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("extracted content mismatch")
	}
	if runtime.GOOS != windowsGOOS {
		info, err := os.Stat(dest)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&0o111 == 0 {
			t.Error("extracted binary is not executable")
		}
	}
}

func TestExtractPluginBinary_Zip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	archive := filepath.Join(dir, "a.zip")
	payload := []byte("zip payload")
	if err := os.WriteFile(archive, makeZip(t, map[string][]byte{"entire-run": payload}), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "out")
	if err := extractPluginBinary(archive, "run", dest); err != nil {
		t.Fatalf("extractPluginBinary: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("extracted content mismatch")
	}
}

func TestExtractPluginBinary_MissingEntry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	archive := filepath.Join(dir, "a.tar.gz")
	if err := os.WriteFile(archive, makeTarGz(t, map[string][]byte{"other": []byte("x")}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := extractPluginBinary(archive, "run", filepath.Join(dir, "out")); err == nil {
		t.Error("extractPluginBinary succeeded on archive without the binary")
	}
}

func TestExtractPluginBinary_RawBinary(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	raw := filepath.Join(dir, "entire-run_1.0.0_x_y")
	payload := []byte("raw binary bytes")
	if err := os.WriteFile(raw, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "out")
	if err := extractPluginBinary(raw, "run", dest); err != nil {
		t.Fatalf("extractPluginBinary: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("raw copy mismatch")
	}
}

func TestSafeArchiveEntry(t *testing.T) {
	t.Parallel()
	for entry, want := range map[string]bool{
		"entire-run":        true,
		"dist/entire-run":   true,
		"../evil":           false,
		"a/../../evil":      false,
		"/abs/path":         false,
		"with\x00null":      false,
		"./fine/entire-run": true,
	} {
		if got := safeArchiveEntry(entry); got != want {
			t.Errorf("safeArchiveEntry(%q) = %t, want %t", entry, got, want)
		}
	}
}

func TestFetchAndVerify_ChecksumEnforced(t *testing.T) {
	t.Parallel()
	payload := []byte("plugin bytes")
	srv := staticServer(t, payload)

	dir := t.TempDir()
	sum := sha256.Sum256(payload)
	good := hex.EncodeToString(sum[:])

	fa, err := fetchAndVerify(context.Background(), srv.URL+"/asset", "asset", good, dir)
	if err != nil {
		t.Fatalf("fetchAndVerify with good digest: %v", err)
	}
	if fa.SHA256 != good {
		t.Errorf("SHA256 = %s, want %s", fa.SHA256, good)
	}

	if _, err := fetchAndVerify(context.Background(), srv.URL+"/asset", "asset2", strings.Repeat("0", 64), dir); err == nil {
		t.Error("fetchAndVerify accepted a wrong digest")
	}
}

func TestFetchAndVerify_404IsAssetNotFound(t *testing.T) {
	t.Parallel()
	srv := notFoundServer(t)
	_, err := fetchAndVerify(context.Background(), srv.URL+"/missing", "missing", "", t.TempDir())
	if !errors.Is(err, errAssetNotFound) {
		t.Errorf("404 error = %v, want errAssetNotFound", err)
	}
}

func TestDownloadPluginAsset_ViaChecksumManifest(t *testing.T) {
	t.Parallel()
	payload := makeTarGz(t, map[string][]byte{"entire-run": []byte("bin")})
	asset := fmt.Sprintf("entire-run_1.0.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	sum := sha256.Sum256(payload)
	checksums := hex.EncodeToString(sum[:]) + "  " + asset + "\n"

	srv := assetServer(t, asset, payload, checksums)

	meta := &PluginMetadata{DownloadURL: srv.URL + "/dl/{tag}/{asset}"}
	fa, err := downloadPluginAsset(context.Background(), meta, "https://example.invalid/entire-run", "run", "v1.0.0", t.TempDir(), false)
	if err != nil {
		t.Fatalf("downloadPluginAsset: %v", err)
	}
	if fa.Asset != asset {
		t.Errorf("Asset = %q, want %q", fa.Asset, asset)
	}
}

func TestDownloadPluginAsset_ProbeFallbackWithoutChecksums(t *testing.T) {
	t.Parallel()
	payload := makeTarGz(t, map[string][]byte{"entire-run": []byte("bin")})
	asset := fmt.Sprintf("entire-run_1.0.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	srv := assetServer(t, asset, payload, "")

	meta := &PluginMetadata{DownloadURL: srv.URL + "/dl/{asset}"}
	fa, err := downloadPluginAsset(context.Background(), meta, "https://example.invalid/entire-run", "run", "v1.0.0", t.TempDir(), true)
	if err != nil {
		t.Fatalf("downloadPluginAsset: %v", err)
	}
	if fa.Asset != asset {
		t.Errorf("Asset = %q, want %q", fa.Asset, asset)
	}
}

// allowUnverified stays false here on purpose: with verification required,
// "this tag has no release yet" must still surface as errAssetNotFound so the
// next-highest-tag walk keeps working. Only a *found but unauthenticated*
// asset becomes errUnverifiedAsset.
func TestDownloadPluginAsset_NoAssetForPlatform(t *testing.T) {
	t.Parallel()
	srv := notFoundServer(t)
	meta := &PluginMetadata{DownloadURL: srv.URL + "/dl/{asset}"}
	_, err := downloadPluginAsset(context.Background(), meta, "https://example.invalid/entire-run", "run", "v1.0.0", t.TempDir(), false)
	if !errors.Is(err, errAssetNotFound) {
		t.Errorf("err = %v, want errAssetNotFound", err)
	}
}

func TestFetchAndVerify_RejectsUnsafeAssetNames(t *testing.T) {
	t.Parallel()
	srv := staticServer(t, []byte("x"))
	for _, asset := range []string{"", ".", "..", "../escape", "a/b", `a\b`} {
		if _, err := fetchAndVerify(context.Background(), srv.URL, asset, "", t.TempDir()); err == nil {
			t.Errorf("fetchAndVerify accepted unsafe asset name %q", asset)
		}
	}
}

func TestFetchAndVerify_RemovesPartialFileOnMismatch(t *testing.T) {
	t.Parallel()
	srv := staticServer(t, []byte("payload"))
	dir := t.TempDir()
	if _, err := fetchAndVerify(context.Background(), srv.URL+"/a", "a", strings.Repeat("0", 64), dir); err == nil {
		t.Fatal("fetchAndVerify accepted a wrong digest")
	}
	if _, err := os.Stat(filepath.Join(dir, "a")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("partial download left behind after checksum mismatch: stat err = %v", err)
	}
}

func TestDownloadPluginAsset_StaleManifestFallsThroughToProbe(t *testing.T) {
	t.Parallel()
	// The root checksums.txt lists only a foreign platform, but the real
	// asset is published under its conventional name. A stale manifest
	// must not block the install: selection falls through to the probe.
	payload := makeTarGz(t, map[string][]byte{"entire-run": []byte("bin")})
	asset := fmt.Sprintf("entire-run_1.0.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	// A stale root manifest that lists only a foreign platform's asset.
	srv := assetServer(t, asset, payload, "abc  entire-run_1.0.0_plan9_mips.tar.gz\n")
	meta := &PluginMetadata{DownloadURL: srv.URL + "/dl/{asset}"}
	fa, err := downloadPluginAsset(context.Background(), meta, "https://example.invalid/entire-run", "run", "v1.0.0", t.TempDir(), true)
	if err != nil {
		t.Fatalf("downloadPluginAsset with stale manifest: %v", err)
	}
	if fa.Asset != asset {
		t.Errorf("Asset = %q, want %q via probe fallback", fa.Asset, asset)
	}
}

// A bare io.LimitReader makes truncation invisible: io.Copy returns a nil
// error at the cap, so an oversize archive entry was written out as the plugin
// binary and the install reported success. Since binary_sha256 is computed from
// the bytes on disk, doctor would then confirm the truncated binary as intact.
func TestWriteExecutableLimited_RejectsOversize(t *testing.T) {
	t.Parallel()
	const limit = 32
	dest := filepath.Join(t.TempDir(), "bin")

	// Exactly at the limit is a complete file, not an overflow.
	if err := writeExecutableLimited(bytes.NewReader(bytes.Repeat([]byte("a"), limit)), dest, limit); err != nil {
		t.Fatalf("writeExecutableLimited at the limit: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != limit {
		t.Errorf("wrote %d bytes, want %d", len(got), limit)
	}

	// One byte over must fail loudly rather than truncate.
	err = writeExecutableLimited(bytes.NewReader(bytes.Repeat([]byte("a"), limit+1)), dest, limit)
	if err == nil {
		t.Fatal("writeExecutableLimited accepted an oversize source")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("err = %v, want a size-limit error", err)
	}
	// A failed write must leave the previously-installed binary intact — the
	// write goes to a sibling temp file and only renames on success, so an
	// interrupted upgrade can no longer strand a broken plugin.
	after, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("failed write destroyed the existing binary: %v", err)
	}
	if len(after) != limit {
		t.Errorf("existing binary changed: %d bytes, want %d", len(after), limit)
	}
	// And it must not leave staging debris behind.
	entries, dirErr := os.ReadDir(filepath.Dir(dest))
	if dirErr != nil {
		t.Fatal(dirErr)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("leftover staging file %q", e.Name())
		}
	}
}

// A symlink planted at the destination must not be followed: an earlier
// malicious plugin could otherwise redirect a later install's write to any
// path the user can write.
func TestWriteExecutableLimited_DoesNotFollowSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("ORIGINAL"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "bin")
	if err := os.Symlink(victim, dest); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := writeExecutableLimited(bytes.NewReader([]byte("PAYLOAD")), dest, 1<<20); err != nil {
		t.Fatalf("writeExecutableLimited: %v", err)
	}
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ORIGINAL" {
		t.Errorf("symlink was followed: victim content = %q", got)
	}
	written, err := os.ReadFile(dest)
	if err != nil || string(written) != "PAYLOAD" {
		t.Errorf("dest content = %q, %v; want PAYLOAD written in place of the link", written, err)
	}
}

// extractPluginBinary must inherit the cap: a highly-compressible archive
// entry can expand well past the download cap that bounded the archive itself.
func TestExtractPluginBinary_OversizeEntryIsRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	archive := filepath.Join(dir, "a.tar.gz")
	// maxPluginAssetSize+1 zero bytes compress to a few KB, so this stays a
	// cheap test while exercising the real cap.
	big := make([]byte, maxPluginAssetSize+1)
	if err := os.WriteFile(archive, makeTarGz(t, map[string][]byte{"entire-run": big}), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "out")
	err := extractPluginBinary(archive, "run", dest)
	if err == nil {
		t.Fatal("extractPluginBinary accepted an oversize archive entry")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("err = %v, want a size-limit error", err)
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Error("oversize extraction left a truncated binary on disk")
	}
}

// Go's default redirect policy follows hops across schemes and only the initial
// URL was ever transport-checked, so an https:// entry point could deliver the
// asset — and its checksums.txt — over plaintext. CheckRedirect closes that.
func TestPluginHTTPClient_RevalidatesRedirects(t *testing.T) {
	t.Parallel()
	// A loopback origin that bounces to a non-loopback plaintext URL.
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, "http://plaintext.example.com/asset.tar.gz", http.StatusFound)
	}))
	defer origin.Close()

	_, err := fetchAndVerify(context.Background(), origin.URL+"/a.tar.gz", "a.tar.gz", "", t.TempDir())
	if err == nil {
		t.Fatal("redirect to plaintext was followed")
	}
	if !strings.Contains(err.Error(), "plaintext HTTP") {
		t.Errorf("err = %v, want the transport refusal to surface", err)
	}

	// A redirect that stays on an allowed transport must still work, so CDN
	// hops aren't broken.
	payload := []byte("binary")
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload) //nolint:errcheck // test server write
	}))
	defer final.Close()
	hop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL+"/a.tar.gz", http.StatusFound)
	}))
	defer hop.Close()
	if _, err := fetchAndVerify(context.Background(), hop.URL+"/a.tar.gz", "a.tar.gz", "", t.TempDir()); err != nil {
		t.Errorf("loopback-to-loopback redirect should still work: %v", err)
	}
}

// goreleaser names universal macOS builds entire-<x>_<ver>_darwin_all, so "all"
// belongs in the arch slot. It used to sit in the OS slot, generating
// _all_<arch> — a spelling no forge produces — so the darwin_all fallback the
// docs promise never matched.
func TestAssetCandidates_DarwinUniversal(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != darwinGOOS {
		t.Skip("darwin-only naming")
	}
	var hasDarwinAll bool
	for _, c := range assetCandidates("demo", "v0.1.0") {
		if strings.Contains(c, "darwin_all") {
			hasDarwinAll = true
		}
		if strings.Contains(c, "_all_") {
			t.Errorf("candidate %q puts 'all' in the OS slot; no forge produces that", c)
		}
	}
	if !hasDarwinAll {
		t.Error("no darwin_all candidate; universal-only releases cannot install")
	}
}

func TestRedactURL(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"https://user:s3cr3t@git.example.com/o/r", "https://git.example.com/o/r"},
		{"https://token@git.example.com/o/r", "https://git.example.com/o/r"},
		{"https://git.example.com/o/r", "https://git.example.com/o/r"},
		{"git@github.com:o/r.git", "git@github.com:o/r.git"},
		// Signed CDN redirects carry the secret in the query, not the
		// userinfo — release hosts redirect asset downloads to exactly this
		// shape, so stripping only userinfo would still leak.
		{"https://cdn.example.com/a.tar.gz?X-Amz-Signature=deadbeef", "https://cdn.example.com/a.tar.gz"},
		{"https://cdn.example.com/a.tar.gz?token=s3cr3t", "https://cdn.example.com/a.tar.gz"},
	}
	for _, tt := range tests {
		if got := redactURL(tt.in); got != tt.want {
			t.Errorf("redactURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	// Free-text scrubbing for git's stderr, where we don't know the URL.
	got := redactCredentials("fatal: could not read from https://bob:hunter2@git.example.com/x")
	if strings.Contains(got, "hunter2") || strings.Contains(got, "bob") {
		t.Errorf("redactCredentials leaked userinfo: %q", got)
	}
}

// releaseAssetBaseURL derives the asset URL from the repo URL, and url.String()
// re-serializes embedded userinfo — so a private-forge remote like
// https://user:token@host/o/r produces a credentialed download URL. The request
// must keep it (that is how it authenticates), but no message may carry it:
// main.go prints command errors to stderr, and a download failure is an
// ordinary event, so an unscrubbed message leaks the token to a terminal or CI
// log. This is the HTTP counterpart of the git-path redaction.
func TestDownloadErrors_NeverLeakCredentials(t *testing.T) {
	t.Parallel()
	const secret = "hunter2-should-never-appear"

	// The asset URL inherits userinfo from the repo URL.
	base, err := releaseAssetBaseURL("https://bob:"+secret+"@git.example.com/o/entire-run", "v1.0.0")
	if err != nil {
		t.Fatalf("releaseAssetBaseURL: %v", err)
	}
	if !strings.Contains(base, secret) {
		t.Fatalf("precondition changed: asset base no longer carries userinfo (%s)", base)
	}

	// Every reachable failure mode on that URL.
	staging := t.TempDir()
	var errs []error
	_, e := fetchAndVerify(context.Background(), base+"a.tar.gz", "a.tar.gz", "", staging)
	errs = append(errs, e) // unreachable host
	_, e = httpGetSmall(context.Background(), base+checksumsFileName)
	errs = append(errs, e) // checksum manifest fetch
	_, e = downloadPluginAsset(context.Background(), nil,
		"https://bob:"+secret+"@git.example.com/o/entire-run", "run", "v1.0.0", staging, true)
	errs = append(errs, e) // full orchestration
	e = requireSecureAssetURL("http://bob:" + secret + "@git.example.com/a.tar.gz")
	errs = append(errs, e) // transport refusal

	// A checksum mismatch, which quotes the URL on a *successful* request.
	srv := staticServer(t, []byte("bytes"))
	credURL := strings.Replace(srv.URL, "http://", "http://bob:"+secret+"@", 1)
	_, e = fetchAndVerify(context.Background(), credURL+"/a.tar.gz", "a.tar.gz", "deadbeef", staging)
	errs = append(errs, e)

	for i, err := range errs {
		if err == nil {
			t.Errorf("case %d: expected an error to inspect", i)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("case %d leaked the credential: %v", i, err)
		}
	}
}

// A plugin gets exactly one binary, named entire-<name>, and the archive does
// not get to choose either fact. The entry name only *selects* what to extract;
// the destination is built from the validated plugin name, and every extraction
// branch returns on the first match.
//
// Both properties are emergent from how the code is written rather than checked
// anywhere, so this pins them: a refactor that used the archive's own name, or
// kept looping after a match, would let a plugin write a second command onto
// PATH — including one belonging to somebody else.
func TestExtractPluginBinary_TakesOnlyItsOwnBinary(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	archive := filepath.Join(dir, "a.tar.gz")
	payload := []byte("the real one")
	if err := os.WriteFile(archive, makeTarGz(t, map[string][]byte{
		"entire-mine":      payload,
		"entire-otherplug": []byte("a second command name"),
		"helper":           []byte("no prefix"),
		"docs/README.md":   []byte("docs"),
		"../escape":        []byte("traversal"),
		"bin/entire-mine":  []byte("same name, nested"),
	}), 0o600); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(dir, "out", pluginBinaryName("mine"))
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := extractPluginBinary(archive, "mine", dest); err != nil {
		t.Fatalf("extractPluginBinary: %v", err)
	}

	entries, err := os.ReadDir(filepath.Dir(dest))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("extraction wrote %v, want only %s", names, filepath.Base(dest))
	}
	if got := entries[0].Name(); !strings.HasPrefix(got, pluginBinaryPrefix) {
		t.Errorf("wrote %q, which is not an %s* command", got, pluginBinaryPrefix)
	}
	// The other entire-* entry must not have been taken: a plugin claiming a
	// second command name is the squatting case the name checks exist to stop.
	written, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != string(payload) {
		t.Errorf("extracted %q, want the entry matching this plugin's own name", written)
	}
}

// Matching is by basename, so an archive can hold several candidates: the
// binary at the root, the same name nested under a versioned directory
// (goreleaser's wrap_in_directory), and unrelated files like
// completions/entire-<name>. Selection must not depend on the order the
// author's tar happened to be built in.
func TestPreferredArchiveEntry_IsDeterministic(t *testing.T) {
	t.Parallel()
	tests := []struct {
		label string
		names []string
		want  string
	}{
		{"root wins over nested", []string{"bin/entire-mine", "entire-mine"}, "entire-mine"},
		{"order does not matter", []string{"entire-mine", "bin/entire-mine"}, "entire-mine"},
		{"shallowest of several", []string{"a/b/c/entire-mine", "a/entire-mine", "a/b/entire-mine"}, "a/entire-mine"},
		{"equal depth is lexicographic", []string{"z/entire-mine", "a/entire-mine"}, "a/entire-mine"},
		{"a different plugin's name is not a candidate", []string{"entire-other"}, ""},
		{"traversal entries are not candidates", []string{"../entire-mine"}, ""},
		{"nested-only still resolves", []string{"pkg/v1/entire-mine"}, "pkg/v1/entire-mine"},
	}
	for _, tt := range tests {
		if got := preferredArchiveEntry(tt.names, "entire-mine"); got != tt.want {
			t.Errorf("%s: preferredArchiveEntry(%v) = %q, want %q", tt.label, tt.names, got, tt.want)
		}
	}
}

// The end-to-end version of the above: the same archive extracted repeatedly
// must always yield the root-level entry, whatever order tar wrote it in.
func TestExtractPluginBinary_PrefersRootEntryEveryTime(t *testing.T) {
	t.Parallel()
	for i := range 8 {
		dir := t.TempDir()
		archive := filepath.Join(dir, "a.tar.gz")
		if err := os.WriteFile(archive, makeTarGz(t, map[string][]byte{
			"entire-mine":             []byte("root"),
			"bin/entire-mine":         []byte("nested"),
			"completions/entire-mine": []byte("completion script"),
		}), 0o600); err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(dir, "out")
		if err := extractPluginBinary(archive, "mine", dest); err != nil {
			t.Fatalf("extractPluginBinary: %v", err)
		}
		got, err := os.ReadFile(dest)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "root" {
			t.Fatalf("run %d extracted %q, want the root entry", i, got)
		}
	}
}
