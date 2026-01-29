import { describe, it, expect, afterEach, beforeEach } from 'vitest';
import { createTestEnv, type TestEnv } from '../utils/test-env.js';
import {
  assertShadowBranchExists,
  getShadowBranches,
} from '../utils/git-assertions.js';

describe('Checkpoint Creation', () => {
  let env: TestEnv;

  beforeEach(async () => {
    env = await createTestEnv({ strategy: 'manual-commit' });
  });

  afterEach(() => {
    env?.cleanup();
  });

  it('creates shadow branch after Claude session', async () => {
    // Record HEAD hash before Claude session
    const headHash = env.repo.getHeadHash();

    // Run Claude to create a file
    const result = await env.createFile('hello.txt', 'Hello World');
    expect(result.success).toBe(true);

    // Verify the file was created
    expect(env.repo.fileExists('hello.txt')).toBe(true);
    expect(env.repo.readFile('hello.txt')).toContain('Hello World');

    // Verify shadow branch was created
    assertShadowBranchExists(env.repo, headHash);
  });

  it.skip('lists checkpoints after Claude session', async () => {
    // Run Claude to create a file
    const result = await env.createFile('test.txt', 'Test content');
    expect(result.success).toBe(true);

    // Get rewind points
    const rewindPoints = env.cli.rewindList();

    // Should have at least one checkpoint
    expect(rewindPoints.length).toBeGreaterThan(0);

    // First checkpoint should exist
    const checkpoint = rewindPoints[0];
    expect(checkpoint.id).toBeDefined();
    expect(checkpoint.timestamp).toBeDefined();
  });

  it.skip('creates checkpoints for multiple file operations', async () => {
    const headHash = env.repo.getHeadHash();

    // First Claude session - create file A
    let result = await env.createFile('a.txt', 'File A content');
    expect(result.success).toBe(true);

    // Second Claude session - create file B
    result = await env.createFile('b.txt', 'File B content');
    expect(result.success).toBe(true);

    // Verify shadow branch exists
    assertShadowBranchExists(env.repo, headHash);

    // Get all checkpoints
    const rewindPoints = env.cli.rewindList();

    // Should have checkpoints for both sessions
    expect(rewindPoints.length).toBeGreaterThanOrEqual(2);

    // Both files should exist in working directory
    expect(env.repo.fileExists('a.txt')).toBe(true);
    expect(env.repo.fileExists('b.txt')).toBe(true);
  });

  it.skip('creates shadow branch with correct naming convention', async () => {
    const headHash = env.repo.getHeadHash();
    const shortHash = headHash.substring(0, 7);

    // Run Claude session
    const result = await env.createFile('naming.txt', 'Test naming');
    expect(result.success).toBe(true);

    // Get all shadow branches
    const shadowBranches = getShadowBranches(env.repo);

    // Should have exactly one shadow branch with correct format
    const expectedBranch = `entire/${shortHash}`;
    expect(shadowBranches).toContain(expectedBranch);
  });

  it.skip('preserves session metadata in checkpoint', async () => {
    // Run Claude session with identifiable prompt
    const result = await env.runClaude('Create a file called metadata-test.txt with content "metadata test"');
    expect(result.success).toBe(true);

    // Get rewind points
    const rewindPoints = env.cli.rewindList();
    expect(rewindPoints.length).toBeGreaterThan(0);

    // Check that the checkpoint has prompt metadata
    const checkpoint = rewindPoints[0];
    expect(checkpoint.prompt).toBeDefined();
  });
});
