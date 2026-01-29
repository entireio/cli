import { describe, it, expect, afterEach, beforeEach } from 'vitest';
import { createTestEnv, type TestEnv } from '../utils/test-env.js';
import { assertWorkingDirectory } from '../utils/git-assertions.js';

describe.skip('Rewind Functionality', () => {
  let env: TestEnv;

  beforeEach(async () => {
    env = await createTestEnv({ strategy: 'manual-commit' });
  });

  afterEach(() => {
    env?.cleanup();
  });

  it('restores files from previous checkpoint', async () => {
    // Create first file with Claude
    let result = await env.createFile('a.txt', 'File A content');
    expect(result.success).toBe(true);

    // Get checkpoint after first file
    const checkpointsAfterA = env.cli.rewindList();
    expect(checkpointsAfterA.length).toBeGreaterThan(0);
    const checkpoint1 = checkpointsAfterA[0];

    // Create second file with Claude
    result = await env.createFile('b.txt', 'File B content');
    expect(result.success).toBe(true);

    // Verify both files exist
    expect(env.repo.fileExists('a.txt')).toBe(true);
    expect(env.repo.fileExists('b.txt')).toBe(true);

    // Rewind to first checkpoint
    env.cli.rewind(checkpoint1.id);

    // Verify file state: a.txt should exist, b.txt should not
    expect(env.repo.fileExists('a.txt')).toBe(true);
    expect(env.repo.fileExists('b.txt')).toBe(false);
  });

  it('restores file content from checkpoint', async () => {
    // Create file with initial content
    let result = await env.createFile('content.txt', 'Version 1');
    expect(result.success).toBe(true);

    // Get checkpoint after initial version
    const checkpointsV1 = env.cli.rewindList();
    const checkpointV1 = checkpointsV1[0];

    // Modify file content with Claude
    result = await env.runClaude(
      'Edit the file content.txt and change its content to "Version 2"'
    );
    expect(result.success).toBe(true);

    // Verify new content
    const newContent = env.repo.readFile('content.txt');
    expect(newContent).toContain('Version 2');

    // Rewind to V1
    env.cli.rewind(checkpointV1.id);

    // Verify original content is restored
    const restoredContent = env.repo.readFile('content.txt');
    expect(restoredContent).toContain('Version 1');
  });

  it('can rewind multiple times', async () => {
    // Create file A
    await env.createFile('a.txt', 'A');
    const checkpointsA = env.cli.rewindList();
    const checkpointA = checkpointsA[0];

    // Create file B
    await env.createFile('b.txt', 'B');
    const checkpointsB = env.cli.rewindList();
    const checkpointB = checkpointsB[0];

    // Create file C
    await env.createFile('c.txt', 'C');

    // Verify all files exist
    assertWorkingDirectory(env.repo, {
      'a.txt': 'A',
      'b.txt': 'B',
      'c.txt': 'C',
    });

    // Rewind to checkpoint B (after creating A and B)
    env.cli.rewind(checkpointB.id);
    expect(env.repo.fileExists('a.txt')).toBe(true);
    expect(env.repo.fileExists('b.txt')).toBe(true);
    expect(env.repo.fileExists('c.txt')).toBe(false);

    // Rewind to checkpoint A (only A exists)
    env.cli.rewind(checkpointA.id);
    expect(env.repo.fileExists('a.txt')).toBe(true);
    expect(env.repo.fileExists('b.txt')).toBe(false);
    expect(env.repo.fileExists('c.txt')).toBe(false);
  });

  it('preserves untracked files during rewind', async () => {
    // Create a file without Claude
    env.repo.writeFile('manual.txt', 'Created manually');

    // Create file with Claude
    await env.createFile('claude.txt', 'Created by Claude');
    const checkpoints = env.cli.rewindList();
    const checkpoint = checkpoints[0];

    // Create another file with Claude
    await env.createFile('claude2.txt', 'Also by Claude');

    // Rewind
    env.cli.rewind(checkpoint.id);

    // Manual file should still exist (it's untracked/gitignored)
    expect(env.repo.fileExists('manual.txt')).toBe(true);

    // First Claude file should exist
    expect(env.repo.fileExists('claude.txt')).toBe(true);

    // Second Claude file should be gone
    expect(env.repo.fileExists('claude2.txt')).toBe(false);
  });

  it('rewind list shows chronological order', async () => {
    // Create multiple files in sequence
    await env.createFile('first.txt', 'First');
    await env.createFile('second.txt', 'Second');
    await env.createFile('third.txt', 'Third');

    // Get all checkpoints
    const checkpoints = env.cli.rewindList();

    // Should have at least 3 checkpoints
    expect(checkpoints.length).toBeGreaterThanOrEqual(3);

    // Verify timestamps are in order (most recent first)
    for (let i = 1; i < checkpoints.length; i++) {
      const current = new Date(checkpoints[i].timestamp);
      const previous = new Date(checkpoints[i - 1].timestamp);
      expect(previous.getTime()).toBeGreaterThanOrEqual(current.getTime());
    }
  });
});
