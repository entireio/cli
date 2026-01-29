import { query } from '@anthropic-ai/claude-agent-sdk';
import { mkdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { TestRepo } from './git-repo.js';
import { CLIRunner } from './cli-runner.js';

export interface TestEnvOptions {
  /** Strategy to use (default: 'manual-commit') */
  strategy?: string;
  /** Skip Entire CLI initialization */
  skipEnable?: boolean;
}

/** Message from Claude Agent SDK */
export interface ClaudeMessage {
  type: string;
  subtype?: string;
  result?: string;
  error?: string;
  [key: string]: unknown;
}

export interface ClaudeResult {
  /** All messages from the Claude session */
  messages: ClaudeMessage[];
  /** Final result message (if successful) */
  result?: string;
  /** Whether the session completed successfully */
  success: boolean;
  /** Error message if session failed */
  error?: string;
}

/**
 * TestEnv provides a complete test environment with:
 * - Isolated git repository
 * - Claude Code hooks configuration
 * - Entire CLI initialized with specified strategy
 * - Method to run Claude sessions via Agent SDK
 */
export class TestEnv {
  public readonly repo: TestRepo;
  public readonly cli: CLIRunner;
  private readonly binaryPath: string;

  private constructor(repo: TestRepo, cli: CLIRunner, binaryPath: string) {
    this.repo = repo;
    this.cli = cli;
    this.binaryPath = binaryPath;
  }

  /**
   * Create a new test environment.
   */
  static async create(options: TestEnvOptions = {}): Promise<TestEnv> {
    const strategy = options.strategy ?? 'manual-commit';

    // Get CLI binary path from environment
    const binaryPath = process.env.ENTIRE_BIN_PATH;
    if (!binaryPath) {
      throw new Error('ENTIRE_BIN_PATH not set. Did global setup run?');
    }

    // Create isolated git repo
    const repo = new TestRepo();
    const cli = new CLIRunner(repo.path);

    const env = new TestEnv(repo, cli, binaryPath);

    if (!options.skipEnable) {
      // Enable Entire CLI with the specified strategy
      cli.enable(strategy);

      // Set up Claude Code hooks configuration
      env.setupClaudeHooks();
    }

    return env;
  }

  /**
   * Set up Claude Code hooks in .claude/settings.json.
   * These hooks point to the built CLI binary for testing.
   */
  private setupClaudeHooks(): void {
    const claudeDir = join(this.repo.path, '.claude');
    mkdirSync(claudeDir, { recursive: true });

    const settings = {
      hooks: {
        SessionStart: [
          {
            matcher: '',
            hooks: [
              {
                type: 'command',
                command: `${this.binaryPath} hooks claude-code session-start`,
              },
            ],
          },
        ],
        Stop: [
          {
            matcher: '',
            hooks: [
              {
                type: 'command',
                command: `${this.binaryPath} hooks claude-code stop`,
              },
            ],
          },
        ],
        UserPromptSubmit: [
          {
            matcher: '',
            hooks: [
              {
                type: 'command',
                command: `${this.binaryPath} hooks claude-code user-prompt-submit`,
              },
            ],
          },
        ],
        PreToolUse: [
          {
            matcher: 'Task',
            hooks: [
              {
                type: 'command',
                command: `${this.binaryPath} hooks claude-code pre-task`,
              },
            ],
          },
        ],
        PostToolUse: [
          {
            matcher: 'Task',
            hooks: [
              {
                type: 'command',
                command: `${this.binaryPath} hooks claude-code post-task`,
              },
            ],
          },
          {
            matcher: 'TodoWrite',
            hooks: [
              {
                type: 'command',
                command: `${this.binaryPath} hooks claude-code post-todo`,
              },
            ],
          },
        ],
      },
      permissions: {
        deny: ['Read(./.entire/metadata/**)'],
      },
    };

    writeFileSync(
      join(claudeDir, 'settings.json'),
      JSON.stringify(settings, null, 2)
    );
  }

  /**
   * Run Claude with the given prompt.
   * Returns all messages from the session and the final result.
   */
  async runClaude(prompt: string): Promise<ClaudeResult> {
    const messages: ClaudeMessage[] = [];
    let result: string | undefined;
    let success = false;
    let error: string | undefined;

    try {
      for await (const message of query({
        prompt,
        options: {
          cwd: this.repo.path,
          settingSources: ['project'], // Load .claude/settings.json hooks
          allowedTools: ['Read', 'Write', 'Edit', 'Bash', 'Glob', 'Grep'],
          permissionMode: 'acceptEdits', // Auto-approve for E2E tests
          maxTurns: 10, // Limit turns for testing
          model: 'haiku'
        },
      })) {
        const msg = message as ClaudeMessage;
        messages.push(msg);

        if (msg.type === 'result') {
          if (msg.subtype === 'success') {
            result = msg.result;
            success = true;
          } else if (msg.subtype === 'error') {
            error = msg.error;
            success = false;
          }
        }
      }
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
      success = false;
    }

    return { messages, result, success, error };
  }

  /**
   * Run Claude with a simple file creation task.
   * This is a convenience method for common test scenarios.
   */
  async createFile(filename: string, content: string): Promise<ClaudeResult> {
    return this.runClaude(
      `Create a file named "${filename}" with the following content:\n\n${content}\n\nJust create the file, nothing else.`
    );
  }

  /**
   * Clean up the test environment.
   */
  cleanup(): void {
    this.repo.cleanup();
  }
}

/**
 * Create a test environment for a test.
 * Use with afterEach to ensure cleanup.
 */
export async function createTestEnv(
  options: TestEnvOptions = {}
): Promise<TestEnv> {
  return TestEnv.create(options);
}
