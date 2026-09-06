# Entire Audit & Handoff Engine (`entire audit`)

## One-sentence summary
A checkpoint-native release readiness audit engine and handoff system built on top of Entire CLI and Entire Graph.

## Problem, intended user and why it matters
When AI agents or software developers collaborate on codebases, Git diffs only capture *what* lines changed (`+` and `-`), completely losing *why* the changes were made, what tool attempts failed, what edge cases remain untested, and what unresolved risks exist. 

**Intended Users**: Software Engineers, Technical Leads, and AI Coding Agents performing code reviews, release-readiness assessments, or task handoffs.

**Why it Matters**: `entire audit` turns passive session tracking into active developer intelligence by verifying prompt intent against implementation, discovering pending risks/TODOs, scoring release readiness (0-100), and outputting machine-readable handoff context (`handoff.json`) so another developer or agent can resume work without repeating past mistakes.

## Selected Entire track and why Entire is essential
**Selected Track**: Track 1 — Build a Checkpoint-Native Developer Experience

**Why Entire is Essential**:
Checkpoint context is the core input for `entire audit`. Without Entire Checkpoints and transcripts, intent verification, agent attempt history, and handoff packages would be impossible to reconstruct from raw Git commits alone.

## Architecture and main workflow
The application consists of three integrated, clean layers:

1. **Primary Interface — VS Code Extension (`vscode-extension/`)**:
   - Built in TypeScript (`vscode-extension/src/`).
   - Automatically checks environment readiness (`entire` CLI installed, repo enabled, `entire graph` available).
   - Sidebar Webview & Panel displaying Readiness Status, Requirements Audit Tree, Redacted Checkpoints, Entire Graph Impact analysis, and Handoff Card.
   - Status Bar item showing readiness audit score (e.g. `$(shield-check) Entire Audit: 85/100`).
   - Enable/Connect Entire setup workflow button for non-enabled workspaces.

2. **Core Backend Engine (`app/`)**:
   - `app/config/`: Environment configuration.
   - `app/privacy/`: `PrivacySanitizer` engine redacting raw prompt transcripts, tokens, and PII.
   - `app/databricks/`: `DatabricksExporter` sending non-PII, privacy-safe metrics.
   - `app/models/`: Domain models (`Repository`, `Requirement`, `Checkpoint`, `GraphFinding`, `Handoff`).
   - `app/providers/`: Abstraction interfaces (`EntireCheckpointProvider`, `EntireGraphProvider`, `GitHubProvider`, `RepositoryAnalyzer`, `RequirementAnalyzer`).
   - `app/api/`: REST API Server (`/api/health`, `/api/readiness`, `/api/enable`, `/api/repositories/...`).

3. **Secondary Interface — Web Dashboard (`app/frontend/`)**:
   - Glassmorphism browser dashboard consuming identical `/api/...` endpoints.

4. **CLI Extension (`entire audit` / `cmd/entire/cli/audit`)**:
   - `entire audit`: Full CLI release readiness & intent audit.
   - `entire audit intent`: Intent verification matrix.
   - `entire audit risks`: Codebase & session risk scanner.
   - `entire audit report`: Markdown readiness report exporter.
   - `entire audit handoff`: Structured JSON/Markdown handoff briefing.
   - `entire audit tui`: Interactive Bubbletea multi-tab TUI dashboard.

2. **Core Application Foundation (`app/`)**:
   - `app/config/`: Environment configuration (`.env.example`).
   - `app/models/`: Domain models (`Repository`, `Requirement`, `Checkpoint`, `GraphFinding`, `Handoff`).
   - `app/providers/`: Abstraction interfaces (`EntireCheckpointProvider`, `EntireGraphProvider`, `GitHubProvider`, `RepositoryAnalyzer`, `RequirementAnalyzer`).
   - `app/api/`: REST API Server (`/api/health`, `/api/repositories`, `/api/repositories/:id/checkpoints`, `/api/repositories/:id/requirements`, `/api/repositories/:id/graph`, `/api/repositories/:id/handoff`) with standardized error payloads and `slog` request logging.
   - `app/frontend/`: Glassmorphism web dashboard shell connecting to REST API endpoints.

```
                  ┌───────────────────────────────┐
                  │    Entire Checkpoints & Graph │
                  └───────────────┬───────────────┘
                                  │
                                  ▼
                ┌───────────────────────────────────┐
                │   Entire Audit Engine (`audit/`)  │
                └─┬───────────────┬───────────────┬─┘
                  │               │               │
                  ▼               ▼               ▼
         ┌────────────────┐┌──────────────┐┌───────────────┐
         │ VS Code Ext UI ││  REST API    ││ Handoff JSON  │
         │ (Primary UX)   ││ (`/api/...`) ││ (`handoff.json`)│
         └────────────────┘└──────┬───────┘└───────────────┘
                                  │
                                  ▼
                         ┌─────────────────┐
                         │ Web Dashboard   │
                         │ (`app/frontend`)│
                         └─────────────────┘
```

## Entire Graph findings and verification
Entire Graph structural findings are integrated to verify symbol definitions, call relationships, and semantic diff impact:
- Indexed AST definitions and call-chains across modified modules.
- Impact Analysis: 0 breaking API schema changes detected.
- Verified test assertions match modified business logic.

## Privacy Boundary & Curveball Adaptation
- **Strict Privacy Rule**: Raw prompts, transcripts, PII, and credentials are **never** transmitted to external services.
- **Privacy Sanitizer**: `app/privacy/sanitizer.go` redacts tokens and sensitive prompt contents before REST API output.
- **Redaction Representation**: Redacted or missing fields are explicitly rendered as `[REDACTED]` or marked with `RedactionStatus: "redacted"`.
- **Databricks Metrics**: Receives only privacy-safe, non-PII numerical scores and category counts (`app/databricks/exporter.go`).

## Noon Curveball: what changed and how we adapted
*(To be populated during the 12:00 PM Noon Curveball phase).*

## Checkpoint links and what each checkpoint proves
- **Checkpoint 1 (Baseline)**: Initial understanding & intended architecture (`app/` foundation & `entire audit` CLI).
- **Checkpoint 2 (Pre-Curveball Stable State)**: Complete working foundation with green unit tests.
- **Checkpoint 3 (Curveball Adaptation)**: Response to Noon Curveball constraint.
- **Checkpoint 4 (Final Release Verification)**: Final submission build & verified readiness report.

## Setup, run and test instructions

### 1. Prerequisites
- **Go**: Version 1.26 or higher
- **Git**: Installed and configured

### 2. Installation & Compilation
```bash
# Build the Entire CLI binary with native audit capabilities
go build -o entire.exe ./cmd/entire
```

### 3. Environment Configuration
Copy `.env.example` to configure server port and optional integration tokens:
```bash
cp .env.example .env
```

### 4. Running the Application Server & Frontend Dashboard
```bash
# Start backend server (serves REST API & static Web Dashboard)
go run ./app/main.go
```
- **REST API Base**: `http://localhost:8080/api/health`
- **Web Dashboard**: `http://localhost:8080/`

### 5. Running CLI & TUI Audit Tools
```bash
./entire.exe audit
./entire.exe audit intent
./entire.exe audit risks
./entire.exe audit report --output RELEASE_READINESS.md
./entire.exe audit handoff --json
./entire.exe audit tui
```

### 3. Run Backend Server & Dashboard
```bash
go run ./app/main.go
```
Open browser at: `http://localhost:8080`

### 4. Build VS Code Extension
```bash
cd vscode-extension
npm install
npm run compile
### 6. Executing Automated Tests
```bash
# Run backend foundation tests
go test -v ./app/...

# Run audit engine tests
go test -v ./cmd/entire/cli/audit
```

## API Standardized Error Format
All REST API error responses adhere to the unified format:
```json
{
  "error": {
    "code": "REPOSITORY_NOT_FOUND",
    "message": "Repository was not found"
  }
}
```

## Databricks use, data sources and limitations (if applicable)
Databricks integration is implemented in `app/databricks/exporter.go` as an optional telemetry sink for non-PII audit scores and requirement completion counts. Raw transcripts and prompts are strictly blocked at the local privacy boundary.

## Known limitations and next steps
- **Known Limitations**: GitHub API client uses dev fixtures when `GITHUB_TOKEN` is unconfigured.
- **Next Steps**: Expand agent transcript sentiment analysis and add webhook notification triggers for CI/CD pipelines.
