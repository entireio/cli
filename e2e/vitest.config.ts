import { defineConfig } from 'vitest/config';

export default defineConfig({
  test: {
    // Global setup builds CLI binary before tests
    globalSetup: ['./src/setup/global-setup.ts'],

    // Test configuration
    include: ['src/tests/**/*.test.ts'],

    // 2-minute timeout per test (Claude can be slow)
    testTimeout: 120_000,

    // Sequential execution to avoid API rate limits
    pool: 'forks',
    poolOptions: {
      forks: {
        singleFork: true,
      },
    },

    // TypeScript support
    typecheck: {
      enabled: false,
    },

    // Environment
    env: {
      NODE_ENV: 'test',
    },
  },
});
