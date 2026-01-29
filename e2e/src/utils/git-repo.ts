import { execSync, type ExecSyncOptions } from 'node:child_process';
import {
  mkdtempSync,
  writeFileSync,
  readFileSync,
  existsSync,
  rmSync,
  mkdirSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';

/**
 * TestRepo provides isolated git repository management for E2E tests.
 * Each instance creates a fresh temp directory with an initialized git repo.
 */
export class TestRepo {
  public readonly path: string;
  private _cleaned = false;

  constructor() {
    // Create unique temp directory for this test
    this.path = mkdtempSync(join(tmpdir(), 'entire-e2e-repo-'));

    // Initialize git repository
    this.git('init');
    this.git('config', 'user.email', 'test@example.com');
    this.git('config', 'user.name', 'Test User');

    // Create initial commit so HEAD exists
    this.writeFile('.gitignore', '.entire/\n');
    this.git('add', '.gitignore');
    this.git('commit', '-m', 'Initial commit');
  }

  /**
   * Execute a git command in the repo directory.
   */
  git(...args: string[]): string {
    const options: ExecSyncOptions = {
      cwd: this.path,
      encoding: 'utf-8',
      stdio: ['pipe', 'pipe', 'pipe'],
    };

    try {
      const result = execSync(`git ${args.map(a => `"${a}"`).join(' ')}`, {
        ...options,
        shell: '/bin/bash',
      });
      return (result as string).trim();
    } catch (error) {
      const execError = error as { stderr?: Buffer | string };
      const stderr = execError.stderr?.toString() || '';
      throw new Error(`git ${args.join(' ')} failed: ${stderr}`);
    }
  }

  /**
   * Get the current HEAD commit hash.
   */
  getHeadHash(): string {
    return this.git('rev-parse', 'HEAD');
  }

  /**
   * Get the short (7-char) HEAD commit hash.
   */
  getHeadHashShort(): string {
    return this.git('rev-parse', '--short=7', 'HEAD');
  }

  /**
   * Check if a branch exists.
   */
  branchExists(branchName: string): boolean {
    try {
      this.git('rev-parse', '--verify', branchName);
      return true;
    } catch {
      return false;
    }
  }

  /**
   * List all branches (including remote-style refs).
   */
  listBranches(): string[] {
    const output = this.git('branch', '-a');
    return output
      .split('\n')
      .map(line => line.replace(/^\*?\s+/, '').trim())
      .filter(Boolean);
  }

  /**
   * Get file content at a specific ref (branch/commit).
   */
  getFileAtRef(ref: string, filePath: string): string | null {
    try {
      return this.git('show', `${ref}:${filePath}`);
    } catch {
      return null;
    }
  }

  /**
   * List files in a tree at a specific ref.
   */
  listFilesAtRef(ref: string, path = ''): string[] {
    try {
      const treePath = path ? `${ref}:${path}` : ref;
      const output = this.git('ls-tree', '-r', '--name-only', treePath);
      return output.split('\n').filter(Boolean);
    } catch {
      return [];
    }
  }

  /**
   * Write a file to the working directory.
   */
  writeFile(relativePath: string, content: string): void {
    const fullPath = join(this.path, relativePath);
    const dir = dirname(fullPath);
    if (!existsSync(dir)) {
      mkdirSync(dir, { recursive: true });
    }
    writeFileSync(fullPath, content, 'utf-8');
  }

  /**
   * Read a file from the working directory.
   */
  readFile(relativePath: string): string {
    const fullPath = join(this.path, relativePath);
    return readFileSync(fullPath, 'utf-8');
  }

  /**
   * Check if a file exists in the working directory.
   */
  fileExists(relativePath: string): boolean {
    return existsSync(join(this.path, relativePath));
  }

  /**
   * Create a commit with the given message (stages all changes).
   */
  commit(message: string): string {
    this.git('add', '-A');
    this.git('commit', '-m', message);
    return this.getHeadHash();
  }

  /**
   * Get the commit message of a specific ref.
   */
  getCommitMessage(ref: string): string {
    return this.git('log', '-1', '--format=%B', ref);
  }

  /**
   * Get commit trailers from a specific ref.
   */
  getCommitTrailers(ref: string): Map<string, string> {
    const message = this.getCommitMessage(ref);
    const trailers = new Map<string, string>();

    // Parse trailers from the end of commit message
    const lines = message.split('\n');
    for (const line of lines) {
      const match = line.match(/^([A-Za-z-]+):\s*(.+)$/);
      if (match) {
        trailers.set(match[1], match[2]);
      }
    }

    return trailers;
  }

  /**
   * Get log of commits for a branch.
   */
  getLog(ref: string, count = 10): string[] {
    const output = this.git('log', `--format=%H %s`, `-${count}`, ref);
    return output.split('\n').filter(Boolean);
  }

  /**
   * Clean up the temporary directory.
   */
  cleanup(): void {
    if (this._cleaned) return;
    this._cleaned = true;

    try {
      rmSync(this.path, { recursive: true, force: true });
    } catch {
      // Ignore cleanup errors
    }
  }
}

/**
 * Create a fresh test repository for a test.
 * Use with afterEach to ensure cleanup.
 */
export function createTestRepo(): TestRepo {
  return new TestRepo();
}
