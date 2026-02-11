# Entire CLI - Windows Support

This fork adds full Windows support to the Entire CLI, Nat Friedman's $60M-funded AI agent developer platform.

## 🚀 Quick Start for Windows

### Option 1: Download Pre-built Binary
```powershell
# Download the latest Windows release
Invoke-WebRequest -Uri "https://github.com/entireio/cli/releases/download/v0.4.2/entire_windows_amd64.zip" -OutFile "entire.zip"
Expand-Archive -Path "entire.zip" -DestinationPath "$env:LOCALAPPDATA\entire"

# Add to PATH
[Environment]::SetEnvironmentVariable("Path", $env:Path + ";$env:LOCALAPPDATA\entire", "User")

# Verify installation
entire version
```

### Option 2: Build from Source
```powershell
# Prerequisites: Go 1.23+
# Clone the repo
git clone https://github.com/entireio/cli.git
cd cli

# Build for Windows
go build -ldflags "-s -w" -o entire-windows-amd64.exe ./cmd/entire

# Use the binary
.\entire-windows-amd64.exe --help
```

## ✅ Windows Compatibility

### What's Working
| Feature | Status | Notes |
|---------|--------|-------|
| Core CLI | ✅ Full | All commands functional |
| Git Integration | ✅ Full | Hooks work on Windows |
| Checkpoints | ✅ Full | Session tracking works |
| Claude Code | ✅ Full | Windows CLI agent support |
| Gemini CLI | ✅ Full | Windows CLI agent support |
| Transcript Parsing | ✅ Full | All formats supported |
| Interactive TUI | ✅ Full | Bubble Tea works on Windows |
| Git Operations | ✅ Full | go-git library used |

### Known Limitations
| Feature | Status | Notes |
|---------|--------|-------|
| Detached Analytics | ⚠️ No-op | Telemetry disabled on Windows (non-critical) |
| Shell Completions | ⚠️ Limited | PowerShell support added, Bash/ZSH N/A |
| File Permissions | ⚠️ Different | Windows ACLs vs Unix permissions |

## 🔧 Technical Implementation

### Platform-Specific Code

The codebase uses Go build tags for platform-specific features:

**Unix-specific (`detached_unix.go`):**
```go
//go:build unix
func spawnDetachedAnalytics(payloadJSON string) {
    // Uses syscall.SysProcAttr with Setpgid
    // Process group detachment for Unix
}
```

**Windows/Other (`detached_other.go`):**
```go
//go:build !unix
func spawnDetachedAnalytics(string) {
    // No-op: detached subprocess spawning not implemented
    // This is telemetry (best-effort), so safe to skip
}
```

### Build Configuration

Updated `.goreleaser.yaml`:
```yaml
builds:
  - goos:
      - darwin
      - linux
      - windows  # Added Windows
    goarch:
      - amd64
      - arm64

archives:
  - format_overrides:
      - goos: windows
        formats:
          - zip  # Windows uses ZIP
```

## 🛠️ Development Setup

### Prerequisites
- Windows 10/11
- Git for Windows
- Go 1.23 or later
- PowerShell 5.1+ or PowerShell Core

### Build Steps
```powershell
# Install Go
winget install GoLang.Go

# Clone repo
git clone https://github.com/entireio/cli.git
cd cli

# Build
$env:CGO_ENABLED = "0"
go build -ldflags "-s -w -X github.com/entireio/cli/cmd/entire/cli/buildinfo.Version=dev" `
    -o entire-windows-amd64.exe ./cmd/entire

# Test
.\entire-windows-amd64.exe version
```

## 📦 Distribution

### Release Artifacts
The Windows release includes:
- `entire_windows_amd64.zip` - Main binary
- `completions/entire.ps1` - PowerShell completions
- `LICENSE` - Apache 2.0
- `README.md` - Documentation

### Installation Methods

1. **Manual Download**: Download ZIP from releases
2. **Scoop** (planned): `scoop install entire`
3. **Chocolatey** (planned): `choco install entire`
4. **WinGet** (planned): `winget install entireio.entire`

## 🧪 Testing on Windows

### Unit Tests
```powershell
go test ./... -tags=windows
```

### Integration Tests
```powershell
# Requires Git and a test repo
go test ./cmd/entire/cli/integration_test/... -v
```

### Manual Testing
```powershell
# Enable in a test repo
cd test-repo
..\entire-windows-amd64.exe enable

# Check status
..\entire-windows-amd64.exe status

# Run with Claude Code (if installed)
claude
# Make some changes, commit

# View checkpoints
..\entire-windows-amd64.exe rewind
```

## 🐛 Troubleshooting

### Issue: "entire is not recognized"
**Solution**: Add the binary location to your PATH environment variable.

### Issue: Git hooks not firing
**Solution**: Ensure Git for Windows is installed and in PATH.

### Issue: "failed to create checkpoint"
**Solution**: Check that you have write permissions to the `.git` directory.

## 🤝 Contributing

### Reporting Windows Issues
1. Include Windows version: `winver`
2. Include Git version: `git --version`
3. Include Go version: `go version`
4. Run with debug logging: `$env:ENTIRE_DEBUG = "1"`

### Code Changes
When adding Windows-specific code:
1. Use build tags: `//go:build windows`
2. Keep Unix behavior as default
3. Document any limitations
4. Add tests where possible

## 📄 License

Apache 2.0 - See LICENSE file

## 🙏 Credits

- Original: [Entire](https://entire.io) by Nat Friedman
- Windows Port: OpenClawMind Community
- Special thanks to the Go team for excellent Windows support

---

**Status**: ✅ Windows support complete and tested
**Last Updated**: 2026-02-10
**Version**: 0.4.2
