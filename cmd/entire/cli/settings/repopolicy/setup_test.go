package repopolicy

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupRecord_IsPerWorktree(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	initPolicyRepo(t, root)
	writePolicyFile(t, root, "README.md", "test\n")
	runPolicyGit(t, root, "add", "README.md")
	runPolicyGit(t, root, "commit", "--no-gpg-sign", "-m", "initial")
	linked := filepath.Join(t.TempDir(), "linked")
	cmd := exec.CommandContext(t.Context(), "git", "worktree", "add", "-b", "setup-record-linked", linked)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}

	mainRepo, err := ResolveRepositoryAt(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	linkedRepo, err := ResolveRepositoryAt(t.Context(), linked)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteSetupRecord(mainRepo, SetupRecord{GitHooksSpec: 1}); err != nil {
		t.Fatal(err)
	}
	if err := WriteSetupRecord(linkedRepo, SetupRecord{PrimaryRefSpec: 1}); err != nil {
		t.Fatal(err)
	}

	mainRecord, found, err := ReadSetupRecord(mainRepo)
	if err != nil || !found {
		t.Fatalf("ReadSetupRecord(main) = (%+v, %v, %v)", mainRecord, found, err)
	}
	linkedRecord, found, err := ReadSetupRecord(linkedRepo)
	if err != nil || !found {
		t.Fatalf("ReadSetupRecord(linked) = (%+v, %v, %v)", linkedRecord, found, err)
	}
	if mainRecord.GitHooksSpec != 1 || mainRecord.PrimaryRefSpec != 0 {
		t.Fatalf("main record = %+v, want hook-only state", mainRecord)
	}
	if linkedRecord.GitHooksSpec != 0 || linkedRecord.PrimaryRefSpec != 1 {
		t.Fatalf("linked record = %+v, want primary-ref-only state", linkedRecord)
	}
}

func TestReadSetupRecord_RejectsWrongVersion(t *testing.T) {
	t.Parallel()

	repository := setupRecordRepository(t)
	record := SetupRecord{
		Version:            recordVersion + 1,
		CanonicalWorktree:  repository.WorktreeRoot,
		CanonicalGitCommon: repository.GitCommonDir,
	}
	writeSetupRecordFixture(t, repository, record)
	if _, _, err := ReadSetupRecord(repository); err == nil || !strings.Contains(err.Error(), "unsupported setup record version") {
		t.Fatalf("ReadSetupRecord() error = %v, want unsupported version", err)
	}
}

func TestReadSetupRecord_RejectsIdentityMismatch(t *testing.T) {
	t.Parallel()

	repository := setupRecordRepository(t)
	record := SetupRecord{
		Version:            recordVersion,
		CanonicalWorktree:  repository.WorktreeRoot + "-other",
		CanonicalGitCommon: repository.GitCommonDir,
	}
	writeSetupRecordFixture(t, repository, record)
	if _, _, err := ReadSetupRecord(repository); err == nil || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("ReadSetupRecord() error = %v, want identity mismatch", err)
	}
}

func setupRecordRepository(t *testing.T) Repository {
	t.Helper()
	root := t.TempDir()
	initPolicyRepo(t, root)
	repository, err := ResolveRepositoryAt(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func writeSetupRecordFixture(t *testing.T, repository Repository, record SetupRecord) {
	t.Helper()
	if err := os.MkdirAll(registryDir(repository), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(setupPath(repository), data, 0o600); err != nil {
		t.Fatal(err)
	}
}
