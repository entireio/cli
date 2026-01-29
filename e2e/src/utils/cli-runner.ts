import { execSync, type ExecSyncOptions } from 'node:child_process';

export interface RewindPoint {
  id: string;
  timestamp: string;
  prompt: string;
  sessionId?: string;
  isCurrentSession: boolean;
}

/**
 * CLIRunner provides methods to execute the Entire CLI binary.
 */
export class CLIRunner {
  private readonly binaryPath: string;
  private readonly repoPath: string;

  constructor(repoPath: string) {
    const binaryPath = process.env.ENTIRE_BIN_PATH;
    if (!binaryPath) {
      throw new Error('ENTIRE_BIN_PATH environment variable not set. Did global setup run?');
    }
    this.binaryPath = binaryPath;
    this.repoPath = repoPath;
  }

  /**
   * Execute an Entire CLI command.
   */
  private exec(args: string[], env: Record<string, string> = {}): string {
    const options: ExecSyncOptions = {
      cwd: this.repoPath,
      encoding: 'utf-8',
      stdio: ['pipe', 'pipe', 'pipe'],
      env: {
        ...process.env,
        ...env,
      },
    };

    const cmd = `"${this.binaryPath}" ${args.map(a => `"${a}"`).join(' ')}`;

    try {
      const result = execSync(cmd, {
        ...options,
        shell: '/bin/bash',
      });
      return (result as string).trim();
    } catch (error) {
      const execError = error as { stdout?: Buffer | string; stderr?: Buffer | string };
      const stdout = execError.stdout?.toString() || '';
      const stderr = execError.stderr?.toString() || '';
      throw new Error(`entire ${args.join(' ')} failed:\nstdout: ${stdout}\nstderr: ${stderr}`);
    }
  }

  /**
   * Enable Entire in the repository with the specified strategy.
   */
  enable(strategy = 'manual-commit'): void {
    this.exec(['enable', '--strategy', strategy]);
  }

  /**
   * Disable Entire in the repository.
   */
  disable(): void {
    this.exec(['disable']);
  }

  /**
   * Get the list of rewind points.
   */
  rewindList(): RewindPoint[] {
    const output = this.exec(['rewind', '--list', '--json']);

    if (!output || output === '[]') {
      return [];
    }

    try {
      const data = JSON.parse(output);
      return data.map((item: Record<string, unknown>) => ({
        id: item.id as string,
        timestamp: item.timestamp as string,
        prompt: item.prompt as string,
        sessionId: item.session_id as string | undefined,
        isCurrentSession: item.is_current_session as boolean,
      }));
    } catch {
      // If JSON parsing fails, try to parse text output
      return this.parseTextRewindList(output);
    }
  }

  /**
   * Parse text-based rewind list output (fallback).
   */
  private parseTextRewindList(output: string): RewindPoint[] {
    const points: RewindPoint[] = [];
    const lines = output.split('\n');

    for (const line of lines) {
      // Expected format: "1. [timestamp] prompt text (session: id)"
      const match = line.match(/^\d+\.\s+\[([^\]]+)\]\s+(.+?)(?:\s+\(session:\s+([^)]+)\))?$/);
      if (match) {
        points.push({
          id: (points.length + 1).toString(),
          timestamp: match[1],
          prompt: match[2],
          sessionId: match[3],
          isCurrentSession: true, // Assume current session for text output
        });
      }
    }

    return points;
  }

  /**
   * Rewind to a specific checkpoint.
   */
  rewind(checkpointId: string): void {
    this.exec(['rewind', '--to', checkpointId]);
  }

  /**
   * Get the current strategy.
   */
  getStrategy(): string {
    const output = this.exec(['status', '--json']);
    try {
      const data = JSON.parse(output);
      return data.strategy || 'unknown';
    } catch {
      return 'unknown';
    }
  }

  /**
   * Check if Entire is enabled in the repository.
   */
  isEnabled(): boolean {
    try {
      this.exec(['status']);
      return true;
    } catch {
      return false;
    }
  }
}

/**
 * Create a CLI runner for a test repository.
 */
export function createCLIRunner(repoPath: string): CLIRunner {
  return new CLIRunner(repoPath);
}
