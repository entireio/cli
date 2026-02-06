# Session Context

## User Prompts

### Prompt 1

I just tried to make a release with the .github/workflows/release.yml through go release and did run into this issue:  • sign & notarize macOS binaries
    • signing                                        binary=dist/cli_darwin_arm64_v8.0/entire
    • notarizing and waiting - this might take a while binary=dist/cli_darwin_arm64_v8.0/entire
      • took: 20m2s
  ⨯ release failed after 22m5s                       error=notarize: macos: notarize dist/cli_darwin_arm64_v8.0/entire: Get "htt...

### Prompt 2

can you also add a single retry?

