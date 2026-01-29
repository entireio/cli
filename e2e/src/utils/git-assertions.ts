import { expect } from 'vitest';
import type { TestRepo } from './git-repo.js';

/**
 * Assert that a shadow branch exists for the given HEAD hash.
 * Shadow branches are named: entire/<commit-hash[:7]>
 */
export function assertShadowBranchExists(repo: TestRepo, headHash: string): void {
  const shortHash = headHash.substring(0, 7);
  const shadowBranch = `entire/${shortHash}`;

  const exists = repo.branchExists(shadowBranch);
  expect(exists, `Shadow branch ${shadowBranch} should exist`).toBe(true);
}

/**
 * Assert that the metadata branch exists.
 * The metadata branch is: entire/sessions
 */
export function assertMetadataBranchExists(repo: TestRepo): void {
  const exists = repo.branchExists('entire/sessions');
  expect(exists, 'Metadata branch entire/sessions should exist').toBe(true);
}

/**
 * Assert that checkpoint metadata exists on the metadata branch.
 * Metadata is stored at sharded path: <id[:2]>/<id[2:]>/metadata.json
 */
export function assertCheckpointMetadata(
  repo: TestRepo,
  checkpointId: string
): void {
  const shard = checkpointId.substring(0, 2);
  const rest = checkpointId.substring(2);
  const metadataPath = `${shard}/${rest}/metadata.json`;

  const content = repo.getFileAtRef('entire/sessions', metadataPath);
  expect(content, `Checkpoint metadata should exist at ${metadataPath}`).not.toBeNull();

  // Parse and validate metadata structure
  const metadata = JSON.parse(content!);
  expect(metadata.checkpoint_id).toBe(checkpointId);
  expect(metadata.session_id).toBeDefined();
  expect(metadata.strategy).toBeDefined();
}

/**
 * Assert that a file exists with specific content at a checkpoint.
 */
export function assertFileAtCheckpoint(
  repo: TestRepo,
  branch: string,
  filePath: string,
  expectedContent: string
): void {
  const content = repo.getFileAtRef(branch, filePath);
  expect(content, `File ${filePath} should exist on ${branch}`).not.toBeNull();
  expect(content).toBe(expectedContent);
}

/**
 * Assert that a file exists at a checkpoint (content doesn't matter).
 */
export function assertFileExistsAtCheckpoint(
  repo: TestRepo,
  branch: string,
  filePath: string
): void {
  const content = repo.getFileAtRef(branch, filePath);
  expect(content, `File ${filePath} should exist on ${branch}`).not.toBeNull();
}

/**
 * Assert that a file does NOT exist at a checkpoint.
 */
export function assertFileNotAtCheckpoint(
  repo: TestRepo,
  branch: string,
  filePath: string
): void {
  const content = repo.getFileAtRef(branch, filePath);
  expect(content, `File ${filePath} should NOT exist on ${branch}`).toBeNull();
}

/**
 * Assert that a commit has the Entire-Checkpoint trailer.
 */
export function assertHasCheckpointTrailer(repo: TestRepo, ref: string): string {
  const trailers = repo.getCommitTrailers(ref);
  const checkpointId = trailers.get('Entire-Checkpoint');
  expect(checkpointId, 'Commit should have Entire-Checkpoint trailer').toBeDefined();
  expect(checkpointId).toMatch(/^[a-f0-9]{12}$/);
  return checkpointId!;
}

/**
 * Assert that shadow branch has session metadata files.
 */
export function assertShadowBranchHasMetadata(
  repo: TestRepo,
  headHash: string,
  sessionId: string
): void {
  const shortHash = headHash.substring(0, 7);
  const shadowBranch = `entire/${shortHash}`;

  // Check for session metadata directory
  const metadataPath = `.entire/metadata/${sessionId}`;
  const files = repo.listFilesAtRef(shadowBranch);

  const hasMetadata = files.some(f => f.startsWith(metadataPath));
  expect(hasMetadata, `Shadow branch should have metadata at ${metadataPath}`).toBe(true);
}

/**
 * Assert that a session transcript exists.
 */
export function assertSessionTranscriptExists(
  repo: TestRepo,
  branch: string,
  sessionId: string
): void {
  const transcriptPath = `.entire/metadata/${sessionId}/full.jsonl`;
  const content = repo.getFileAtRef(branch, transcriptPath);
  expect(content, `Session transcript should exist at ${transcriptPath}`).not.toBeNull();
  expect(content!.length).toBeGreaterThan(0);
}

/**
 * Get all shadow branches in the repository.
 */
export function getShadowBranches(repo: TestRepo): string[] {
  const branches = repo.listBranches();
  return branches.filter(b => b.startsWith('entire/') && !b.includes('sessions'));
}

/**
 * Assert that the working directory matches expected state.
 */
export function assertWorkingDirectory(
  repo: TestRepo,
  expectedFiles: Record<string, string | null>
): void {
  for (const [path, expectedContent] of Object.entries(expectedFiles)) {
    if (expectedContent === null) {
      expect(repo.fileExists(path), `File ${path} should NOT exist`).toBe(false);
    } else {
      expect(repo.fileExists(path), `File ${path} should exist`).toBe(true);
      const actualContent = repo.readFile(path);
      expect(actualContent, `File ${path} content mismatch`).toBe(expectedContent);
    }
  }
}
