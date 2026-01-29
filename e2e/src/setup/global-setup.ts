import { execSync } from 'node:child_process';
import { mkdtempSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';

/**
 * Global setup runs once before all tests.
 * Builds the CLI binary and sets environment variables for test utilities.
 */
export default async function globalSetup(): Promise<() => void> {
  const projectRoot = resolve(import.meta.dirname, '../../..');
  const tempDir = mkdtempSync(join(tmpdir(), 'entire-e2e-'));
  const binaryPath = join(tempDir, 'entire');

  console.log(`Building CLI binary to ${binaryPath}...`);

  // Build the CLI binary with version info
  const buildCmd = `
    VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo "e2e-test")
    COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
    go build -ldflags "-X entire.io/cli/cmd/entire/cli.Version=\${VERSION} -X entire.io/cli/cmd/entire/cli.Commit=\${COMMIT}" -o "${binaryPath}" ./cmd/entire
  `;

  try {
    execSync(buildCmd, {
      cwd: projectRoot,
      shell: '/bin/bash',
      stdio: 'inherit',
    });
  } catch (error) {
    console.error('Failed to build CLI binary');
    throw error;
  }

  if (!existsSync(binaryPath)) {
    throw new Error(`CLI binary not found at ${binaryPath}`);
  }

  console.log('CLI binary built successfully');

  // Set environment variable for test utilities
  process.env.ENTIRE_BIN_PATH = binaryPath;
  process.env.ENTIRE_PROJECT_ROOT = projectRoot;

  // Return teardown function
  return () => {
    console.log(`Cleaning up temp directory: ${tempDir}`);
    try {
      execSync(`rm -rf "${tempDir}"`, { stdio: 'ignore' });
    } catch {
      // Ignore cleanup errors
    }
  };
}
