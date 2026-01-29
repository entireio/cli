import { describe, it, expect, afterEach, beforeEach } from 'vitest';
import { createTestEnv, type TestEnv } from '../utils/test-env.js';
import {
  assertMetadataBranchExists,
  assertHasCheckpointTrailer,
  assertCheckpointMetadata,
} from '../utils/git-assertions.js';

describe.skip('Commit and Condensation', () => {
  let env: TestEnv;

  beforeEach(async () => {
    env = await createTestEnv({ strategy: 'manual-commit' });
  });

  afterEach(() => {
    env?.cleanup();
  });

  it('condenses to entire/sessions on user commit', async () => {
    // Create file with Claude
    const result = await env.createFile('main.go', 'package main\n\nfunc main() {\n\tprintln("Hello")\n}\n');
    expect(result.success).toBe(true);

    // Verify file exists
    expect(env.repo.fileExists('main.go')).toBe(true);

    // Make a user commit
    env.repo.git('add', 'main.go');
    env.repo.git('commit', '-m', 'Add main.go');

    // Verify entire/sessions branch was created
    assertMetadataBranchExists(env.repo);
  });

  it('adds Entire-Checkpoint trailer to user commit', async () => {
    // Create file with Claude
    const result = await env.createFile('feature.ts', 'export const feature = true;');
    expect(result.success).toBe(true);

    // Make a user commit
    env.repo.git('add', 'feature.ts');
    env.repo.git('commit', '-m', 'Add feature');

    // Verify commit has the checkpoint trailer
    const checkpointId = assertHasCheckpointTrailer(env.repo, 'HEAD');

    // Checkpoint ID should be 12 hex characters
    expect(checkpointId).toMatch(/^[a-f0-9]{12}$/);
  });

  it('links commit to metadata via checkpoint ID', async () => {
    // Create file with Claude
    const result = await env.createFile('linked.txt', 'Linked content');
    expect(result.success).toBe(true);

    // Make a user commit
    env.repo.git('add', 'linked.txt');
    env.repo.git('commit', '-m', 'Add linked file');

    // Get the checkpoint ID from the commit trailer
    const checkpointId = assertHasCheckpointTrailer(env.repo, 'HEAD');

    // Verify metadata exists on entire/sessions branch
    assertCheckpointMetadata(env.repo, checkpointId);
  });

  it('condenses multiple sessions to same checkpoint', async () => {
    // Create first file with Claude
    let result = await env.createFile('first.txt', 'First file');
    expect(result.success).toBe(true);

    // Create second file with Claude (same base commit, no user commit yet)
    result = await env.createFile('second.txt', 'Second file');
    expect(result.success).toBe(true);

    // Single user commit captures both sessions
    env.repo.git('add', '.');
    env.repo.git('commit', '-m', 'Add files from multiple sessions');

    // Verify metadata branch exists
    assertMetadataBranchExists(env.repo);

    // Get checkpoint ID
    const checkpointId = assertHasCheckpointTrailer(env.repo, 'HEAD');

    // Verify metadata exists
    assertCheckpointMetadata(env.repo, checkpointId);
  });

  it('preserves session history across multiple commits', async () => {
    // First commit cycle
    await env.createFile('commit1.txt', 'Commit 1');
    env.repo.git('add', 'commit1.txt');
    env.repo.git('commit', '-m', 'First commit');

    const checkpoint1 = assertHasCheckpointTrailer(env.repo, 'HEAD');

    // Second commit cycle
    await env.createFile('commit2.txt', 'Commit 2');
    env.repo.git('add', 'commit2.txt');
    env.repo.git('commit', '-m', 'Second commit');

    const checkpoint2 = assertHasCheckpointTrailer(env.repo, 'HEAD');

    // Both checkpoints should have metadata
    assertCheckpointMetadata(env.repo, checkpoint1);
    assertCheckpointMetadata(env.repo, checkpoint2);

    // Checkpoint IDs should be different
    expect(checkpoint1).not.toBe(checkpoint2);
  });

  it('maintains clean commit messages with only trailer', async () => {
    // Create file with Claude
    const result = await env.createFile('clean.txt', 'Clean content');
    expect(result.success).toBe(true);

    // Make commit with specific message
    env.repo.git('add', 'clean.txt');
    env.repo.git('commit', '-m', 'My custom commit message');

    // Get full commit message
    const commitMessage = env.repo.getCommitMessage('HEAD');

    // Message should contain our custom text
    expect(commitMessage).toContain('My custom commit message');

    // And should have the Entire-Checkpoint trailer
    expect(commitMessage).toMatch(/Entire-Checkpoint: [a-f0-9]{12}/);

    // But should NOT have other Entire metadata cluttering it
    expect(commitMessage).not.toContain('Entire-Session:');
    expect(commitMessage).not.toContain('Entire-Strategy:');
  });

  it('handles empty Claude session gracefully', async () => {
    // Just verify environment is set up (no Claude interaction)
    expect(env.repo.fileExists('.gitignore')).toBe(true);

    // Make a commit without any Claude changes
    env.repo.writeFile('manual.txt', 'Manual content');
    env.repo.git('add', 'manual.txt');
    env.repo.git('commit', '-m', 'Manual commit');

    // The commit should succeed but may not have Entire-Checkpoint trailer
    // (depends on whether there was an active session)
    const commitMessage = env.repo.getCommitMessage('HEAD');
    expect(commitMessage).toContain('Manual commit');
  });
});
