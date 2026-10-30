package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v6/plumbing/object"
)

// requireCommitSignersEnv makes a missing signing tool fail the signing tests
// instead of skipping them. CI sets it, so every format is exercised there.
const requireCommitSignersEnv = "ENTIRE_TEST_REQUIRE_COMMIT_SIGNERS"

// commitSigners are the signing formats git supports (gpg.format), each with a
// setup that gives the test repository a signing key in that format and
// returns it, with another key the user can't sign with.
var commitSigners = []struct {
	format string
	setup  func(t *testing.T) (yours, other string)
}{
	{"ssh", useSSHSigning},
	{"openpgp", useOpenPGPSigning},
	{"x509", useX509Signing},
}

// forEachSigner runs fn in a fresh attach test repository once per signing
// format, with the user's key configured for that format.
func forEachSigner(t *testing.T, fn func(t *testing.T, yours, other string)) {
	t.Helper()
	for _, s := range commitSigners {
		t.Run(s.format, func(t *testing.T) {
			setupAttachTestRepo(t)
			yours, other := s.setup(t)
			fn(t, yours, other)
		})
	}
}

// requireTools skips the test when one of tools isn't on PATH, or fails it
// when requireCommitSignersEnv is set.
func requireTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv(requireCommitSignersEnv) != "" {
				t.Fatalf("%s is not on PATH, and %s is set", tool, requireCommitSignersEnv)
			}
			t.Skipf("%s is not on PATH", tool)
		}
	}
}

// runTool runs a signing tool and returns its stdout, failing the test on error.
func runTool(t *testing.T, name string, args ...string) string {
	t.Helper()
	cmd := execx.NonInteractive(t.Context(), name, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, stderr.String())
	}
	return string(out)
}

// useSigningKey configures the test repository to sign in format with key.
func useSigningKey(t *testing.T, format, key string) {
	t.Helper()
	dir := mustGetwd(t)
	testutil.RunGit(t, dir, "config", "gpg.format", format)
	testutil.RunGit(t, dir, "config", "user.signingkey", key)
}

// useSSHSigning signs with SSH keys, named by their private key files.
func useSSHSigning(t *testing.T) (yours, other string) {
	t.Helper()
	requireTools(t, "ssh-keygen")
	keys := t.TempDir()
	yours, other = filepath.Join(keys, "yours"), filepath.Join(keys, "other")
	for _, key := range []string{yours, other} {
		runTool(t, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", filepath.Base(key), "-f", key)
	}
	useSigningKey(t, "ssh", yours)
	return yours, other
}

// useGnuPGHome points gpg and gpgsm, and the agent they start, at a fresh home
// for this test, and stops that agent when the test ends.
func useGnuPGHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("GNUPGHOME", home)
	// Registered after TempDir and Setenv, so it runs first: the agent is
	// stopped while GNUPGHOME still names its home, before the home is removed.
	t.Cleanup(func() {
		if out, err := execx.NonInteractive(context.Background(), "gpgconf", "--kill", "all").CombinedOutput(); err != nil {
			t.Logf("stop gpg-agent: %v: %s", err, out)
		}
	})
	return home
}

// secretKeyFingerprints lists the fingerprints of the secret keys tool (gpg or
// gpgsm) has.
func secretKeyFingerprints(t *testing.T, tool string) []string {
	t.Helper()
	var fprs []string
	for _, line := range strings.Split(runTool(t, tool, "--with-colons", "--list-secret-keys"), "\n") {
		if fields := strings.Split(line, ":"); fields[0] == "fpr" && len(fields) > 9 {
			fprs = append(fprs, fields[9])
		}
	}
	return fprs
}

// newKey runs generate and returns the fingerprint of the one secret key it
// added to tool's keyring.
func newKey(t *testing.T, tool string, generate func()) string {
	t.Helper()
	before := secretKeyFingerprints(t, tool)
	generate()
	for _, fpr := range secretKeyFingerprints(t, tool) {
		if !slices.Contains(before, fpr) {
			return fpr
		}
	}
	t.Fatalf("%s generated no new secret key", tool)
	return ""
}

// useOpenPGPSigning signs with OpenPGP keys, named by their fingerprints.
func useOpenPGPSigning(t *testing.T) (yours, other string) {
	t.Helper()
	requireTools(t, "gpg", "gpgconf")
	useGnuPGHome(t)
	gen := func(uid string) string {
		return newKey(t, "gpg", func() {
			runTool(t, "gpg", "--batch", "--pinentry-mode", "loopback", "--passphrase", "", "--quick-gen-key", uid, "ed25519", "sign", "never")
		})
	}
	yours, other = gen("Test User <test@example.com>"), gen("Someone Else <someone@example.com>")
	useSigningKey(t, "openpgp", yours)
	return yours, other
}

// useX509Signing signs with self-signed X.509 certificates in gpgsm, named by
// their fingerprints.
func useX509Signing(t *testing.T) (yours, other string) {
	t.Helper()
	requireTools(t, "gpgsm", "gpgconf")
	home := useGnuPGHome(t)
	testutil.WriteFile(t, home, "gpgsm.conf", "disable-crl-checks\n")
	testutil.WriteFile(t, home, "gpg-agent.conf", "allow-loopback-pinentry\n")
	gen := func(name, email string) string {
		return newKey(t, "gpgsm", func() {
			params := "Key-Type: RSA\nKey-Length: 2048\nKey-Usage: sign\nSerial: random\nName-DN: CN=" + name + "\nName-Email: " + email + "\n"
			testutil.WriteFile(t, home, "params", params)
			cert := runTool(t, "gpgsm", "--batch", "--pinentry-mode", "loopback", "--passphrase", "", "--gen-key", filepath.Join(home, "params"))
			testutil.WriteFile(t, home, "cert.der", cert)
			runTool(t, "gpgsm", "--batch", "--import", filepath.Join(home, "cert.der"))
		})
	}
	yours, other = gen("Test User", "test@example.com"), gen("Someone Else", "someone@example.com")
	// Self-signed certificates sign only as trusted roots.
	testutil.WriteFile(t, home, "trustlist.txt", yours+" S relax\n"+other+" S relax\n")
	useSigningKey(t, "x509", yours)
	return yours, other
}

// signedCommitAt commits name on top of HEAD, signed with key, with extra git
// options (such as another committer), and returns the commit.
func signedCommitAt(t *testing.T, name, key string, opts ...string) *object.Commit {
	t.Helper()
	dir := mustGetwd(t)
	testutil.WriteFile(t, dir, name, name)
	testutil.GitAdd(t, dir, name)
	args := append(slices.Clone(opts), "-c", "user.signingkey="+key, "commit", "-q", "-S", "-m", "add "+name)
	testutil.RunGit(t, dir, args...)
	c := headCommitOf(t)
	if c.Signature == "" {
		t.Fatalf("commit %s was not signed", name)
	}
	return c
}

// assertRewrittenKeepsKey attaches to target and checks that the rewritten
// target and the commit replayed after it are signed by the key that signed
// the originals.
func assertRewrittenKeepsKey(t *testing.T, sessionID string, target, later *object.Commit) {
	t.Helper()
	key, good := signingKey(context.Background(), target.Hash.String())
	if !good {
		t.Fatal("the original commit has no good signature")
	}
	out, err := attachHeadless(t, sessionID, attachOptions{Commit: target.Hash.String()})
	if err != nil {
		t.Fatalf("runAttach: %v\n%s", err, out)
	}
	head := headCommitOf(t)
	parent, err := head.Parent(0)
	if err != nil {
		t.Fatal(err)
	}
	if head.Hash == later.Hash || parent.Hash == target.Hash {
		t.Fatalf("commits were not rewritten:\n%s", out)
	}
	for _, c := range []*object.Commit{parent, head} {
		if got, good := signingKey(context.Background(), c.Hash.String()); !good || got != key {
			t.Errorf("rewritten commit %s signed by %q (good=%v), want %q", c.Hash, got, good, key)
		}
	}
	if strings.Contains(out, "no longer signed") {
		t.Errorf("no commit should lose its signature:\n%s", out)
	}
}

// A commit the user signed is still signed by the same key after attach
// rewrites it, the target and a commit replayed after it alike (#2974).
// log.showSignature must not leak verification text into the key lookup.
func TestAttachCommit_RewriteKeepsYourSignature(t *testing.T) {
	forEachSigner(t, func(t *testing.T, yours, _ string) {
		target := signedCommitAt(t, "work.txt", yours)
		later := signedCommitAt(t, "later.txt", yours)
		testutil.RunGit(t, mustGetwd(t), "config", "log.showSignature", "true")
		assertRewrittenKeepsKey(t, "attach-signed", target, later)
	})
}

// The user is whoever git commits as: committer.email wins over user.email.
func TestAttachCommit_RewriteKeepsYourSignatureUnderCommitterEmail(t *testing.T) {
	setupAttachTestRepo(t)
	yours, _ := useSSHSigning(t)
	dir := mustGetwd(t)
	testutil.RunGit(t, dir, "config", "committer.email", "committer@example.com")
	testutil.RunGit(t, dir, "config", "user.email", "author@example.com")
	target := signedCommitAt(t, "work.txt", yours)
	later := signedCommitAt(t, "later.txt", yours)
	assertRewrittenKeepsKey(t, "attach-signed-committer-email", target, later)
}

// Without user.signingkey, OpenPGP signing picks the key by the committer
// identity. The replay keeps the original committer, which is the user, so the
// user's key still signs it.
func TestAttachCommit_RewriteKeepsAnOpenPGPSignatureChosenByIdentity(t *testing.T) {
	setupAttachTestRepo(t)
	yours, _ := useOpenPGPSigning(t)
	target := signedCommitAt(t, "work.txt", yours)
	later := signedCommitAt(t, "later.txt", yours)
	testutil.RunGit(t, mustGetwd(t), "config", "--unset", "user.signingkey")
	assertRewrittenKeepsKey(t, "attach-signed-by-identity", target, later)
}

// A commit someone else committed and signed is replayed unsigned, not signed
// with the user's key, and the signer is never asked.
func TestAttachCommit_RewriteDoesNotSignSomeoneElsesCommit(t *testing.T) {
	forEachSigner(t, func(t *testing.T, _, other string) {
		target := commitAt(t, "work.txt")
		signedCommitAt(t, "theirs.txt", other, "-c", "user.name=Someone Else", "-c", "user.email=someone@example.com")
		// Signing with this would fail, so a "signing failed" reason means attach tried.
		testutil.RunGit(t, mustGetwd(t), "config", "user.signingkey", filepath.Join(t.TempDir(), "missing"))

		out, err := attachHeadless(t, "attach-signed-theirs", attachOptions{Commit: target.Hash.String()})
		if err != nil {
			t.Fatalf("runAttach: %v\n%s", err, out)
		}
		if head := headCommitOf(t); head.Signature != "" {
			t.Errorf("someone else's commit was signed on replay")
		}
		if !strings.Contains(out, "is no longer signed: it was committed by someone@example.com, not you") {
			t.Errorf("expected a warning that their commit is no longer signed, got:\n%s", out)
		}
	})
}

// A commit the user committed but signed with a key they can't sign with now
// is replayed unsigned: the new signature would be by a different key. The
// user's key never signs its content, not even in an object attach discards.
func TestAttachCommit_RewriteDropsASignatureByAnotherKey(t *testing.T) {
	forEachSigner(t, func(t *testing.T, _, other string) {
		target := signedCommitAt(t, "work.txt", other)

		out, err := attachHeadless(t, "attach-signed-other-key", attachOptions{Commit: target.Hash.String()})
		if err != nil {
			t.Fatalf("runAttach: %v\n%s", err, out)
		}
		if head := headCommitOf(t); head.Signature != "" || head.Hash == target.Hash {
			t.Errorf("the rewrite should be unsigned: %s signed=%v", head.Hash, head.Signature != "")
		}
		if !strings.Contains(out, "is no longer signed: it was signed with a key other than your signing key") {
			t.Errorf("expected a warning naming the other key, got:\n%s", out)
		}
		if signed := signedCommitsOfTree(t, target.TreeHash.String()); !slices.Equal(signed, []string{target.Hash.String()}) {
			t.Errorf("signed commits of the target's tree = %v, want only the original %s", signed, target.Hash)
		}
	})
}

// signedCommitsOfTree lists every signed commit object in the repository,
// reachable or not, whose tree is tree.
func signedCommitsOfTree(t *testing.T, tree string) []string {
	t.Helper()
	dir := mustGetwd(t)
	var signed []string
	for _, line := range strings.Split(strings.TrimSpace(testutil.RunGit(t, dir, "cat-file", "--batch-all-objects", "--batch-check=%(objectname) %(objecttype)")), "\n") {
		sha, typ, _ := strings.Cut(line, " ")
		if typ != "commit" {
			continue
		}
		raw := testutil.RunGit(t, dir, "cat-file", "commit", sha)
		if strings.HasPrefix(raw, "tree "+tree+"\n") && strings.Contains(raw, "\ngpgsig") {
			signed = append(signed, sha)
		}
	}
	return signed
}

// A commit whose signature doesn't verify (its content changed after signing)
// is replayed unsigned: git still reports the key that signed the original
// content, but re-signing would put the user's signature on content they never
// signed.
func TestAttachCommit_RewriteDropsASignatureThatDoesNotVerify(t *testing.T) {
	forEachSigner(t, func(t *testing.T, yours, _ string) {
		dir := mustGetwd(t)
		signed := signedCommitAt(t, "work.txt", yours)
		raw := testutil.RunGit(t, dir, "cat-file", "commit", signed.Hash.String())
		cmd := execx.NonInteractive(t.Context(), "git", "hash-object", "-t", "commit", "-w", "--stdin")
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader(strings.Replace(raw, "add work.txt", "add work.txt, altered", 1))
		forged, err := cmd.Output()
		if err != nil {
			t.Fatalf("write the altered commit: %v", err)
		}
		testutil.RunGit(t, dir, "update-ref", "HEAD", strings.TrimSpace(string(forged)))
		target := headCommitOf(t)
		if _, good := signingKey(context.Background(), target.Hash.String()); good {
			t.Fatal("the altered commit's signature should not verify")
		}

		out, err := attachHeadless(t, "attach-signed-altered", attachOptions{Commit: target.Hash.String()})
		if err != nil {
			t.Fatalf("runAttach: %v\n%s", err, out)
		}
		if head := headCommitOf(t); head.Signature != "" || head.Hash == target.Hash {
			t.Errorf("the rewrite should be unsigned: %s signed=%v", head.Hash, head.Signature != "")
		}
		if !strings.Contains(out, "is no longer signed: its signature can't be verified") {
			t.Errorf("expected a warning that the signature doesn't verify, got:\n%s", out)
		}
	})
}

// When signing fails, the commit is still rewritten, unsigned, with the reason.
func TestAttachCommit_RewriteFallsBackWhenSigningFails(t *testing.T) {
	forEachSigner(t, func(t *testing.T, yours, _ string) {
		target := signedCommitAt(t, "work.txt", yours)
		testutil.RunGit(t, mustGetwd(t), "config", "user.signingkey", filepath.Join(t.TempDir(), "missing"))

		out, err := attachHeadless(t, "attach-signing-fails", attachOptions{Commit: target.Hash.String()})
		if err != nil {
			t.Fatalf("runAttach: %v\n%s", err, out)
		}
		head := headCommitOf(t)
		if _, ok := trailers.ParseCheckpoint(head.Message); !ok || head.Signature != "" {
			t.Errorf("want an unsigned rewrite with the trailer: signed=%v %q", head.Signature != "", head.Message)
		}
		if !strings.Contains(out, "is no longer signed: signing failed:") {
			t.Errorf("expected the signing failure, got:\n%s", out)
		}
	})
}

// The confirmation says up front which signed commits keep a signature: the
// user's own are signed again, someone else's are named as losing theirs.
func TestRewriteWarning_SaysWhichSignedCommitsKeepTheirSignature(t *testing.T) {
	setupAttachTestRepo(t)
	yours, other := useSSHSigning(t)
	mine := signedCommitAt(t, "work.txt", yours)
	theirs := signedCommitAt(t, "theirs.txt", other, "-c", "user.name=Someone Else", "-c", "user.email=someone@example.com")

	got := strings.Join(rewriteWarning(context.Background(), []*object.Commit{mine, theirs}, nil), "\n")
	if !strings.Contains(got, "Commits you signed are signed again with your signing key") {
		t.Errorf("expected the user's signed commits to be re-signed, got:\n%s", got)
	}
	want := "committed by someone else and lose their signatures:\n  " + describeCommit(theirs)
	if !strings.Contains(got, want) || strings.Count(got, describeCommit(mine)) != 1 {
		t.Errorf("expected only %s listed as losing its signature, got:\n%s", theirs.Hash.String()[:12], got)
	}

	if got := rewriteWarning(context.Background(), []*object.Commit{commitAt(t, "plain.txt")}, nil); strings.Contains(strings.Join(got, "\n"), "sign") {
		t.Errorf("an unsigned commit needs no signature line, got:\n%s", strings.Join(got, "\n"))
	}
}
