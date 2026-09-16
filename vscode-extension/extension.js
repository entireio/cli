const vscode = require('vscode');

/**
 * Activates the Entire Checkpoint Intelligence VS Code Extension.
 * @param {vscode.ExtensionContext} context
 */
function activate(context) {
    const provider = new EntireRequirementsViewProvider(context.extensionUri);

    context.subscriptions.push(
        vscode.window.registerWebviewViewProvider(
            EntireRequirementsViewProvider.viewType,
            provider
        )
    );

    context.subscriptions.push(
        vscode.commands.registerCommand('entire.openRequirementsView', () => {
            vscode.commands.executeCommand('workbench.view.extension.entire-intelligence-sidebar');
        })
    );
}

class EntireRequirementsViewProvider {
    static viewType = 'entire-requirements-view';

    constructor(extensionUri) {
        this._extensionUri = extensionUri;
    }

    resolveWebviewView(webviewView, context, _token) {
        this._view = webviewView;

        webviewView.webview.options = {
            enableScripts: true,
            localResourceRoots: [this._extensionUri]
        };

        webviewView.webview.html = this._getHtmlForWebview(webviewView.webview);
    }

    _getHtmlForWebview(webview) {
        return `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Entire Requirement Workflow</title>
    <style>
        :root {
            --bg-primary: #0f172a;
            --bg-card: rgba(30, 41, 59, 0.7);
            --border: rgba(255, 255, 255, 0.1);
            --text: #f8fafc;
            --text-muted: #94a3b8;
            --accent-cyan: #38bdf8;
            --accent-green: #10b981;
            --accent-red: #ef4444;
            --accent-amber: #f59e0b;
        }

        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
            background: var(--bg-primary);
            color: var(--text);
            padding: 12px;
            margin: 0;
            font-size: 13px;
        }

        h2, h3 { margin: 0 0 8px 0; color: #fff; font-weight: 600; }
        .card {
            background: var(--bg-card);
            border: 1px solid var(--border);
            border-radius: 8px;
            padding: 12px;
            margin-bottom: 12px;
        }

        .step-badge {
            display: inline-block;
            background: rgba(56, 189, 248, 0.2);
            color: var(--accent-cyan);
            padding: 2px 8px;
            border-radius: 4px;
            font-size: 11px;
            font-weight: 600;
            margin-bottom: 8px;
        }

        .readiness-grid {
            display: grid;
            grid-template-columns: repeat(2, 1fr);
            gap: 6px;
            margin-top: 8px;
        }

        .status-item {
            display: flex;
            align-items: center;
            gap: 6px;
            font-size: 12px;
            padding: 4px 8px;
            background: rgba(255,255,255,0.03);
            border-radius: 4px;
        }

        .indicator {
            width: 8px;
            height: 8px;
            border-radius: 50%;
        }
        .indicator.detected { background: var(--accent-green); box-shadow: 0 0 6px var(--accent-green); }
        .indicator.missing { background: var(--accent-red); }

        select, button, input {
            width: 100%;
            padding: 8px;
            background: #1e293b;
            border: 1px solid var(--border);
            color: #fff;
            border-radius: 6px;
            font-size: 12px;
            box-sizing: border-box;
            margin-top: 4px;
        }

        button {
            background: #0284c7;
            cursor: pointer;
            font-weight: 600;
            border: none;
            margin-top: 8px;
        }

        button:hover { background: #0369a1; }

        .req-card {
            border-left: 3px solid var(--accent-cyan);
            padding: 8px 10px;
            margin-top: 8px;
            background: rgba(255, 255, 255, 0.02);
            border-radius: 0 6px 6px 0;
        }

        .req-card.closed { border-left-color: var(--accent-green); }

        .tag {
            font-size: 10px;
            padding: 2px 6px;
            border-radius: 3px;
            text-transform: uppercase;
            font-weight: 600;
        }
        .tag.completed { background: rgba(16, 185, 129, 0.2); color: var(--accent-green); }
        .tag.needs_verification { background: rgba(245, 158, 11, 0.2); color: var(--accent-amber); }

        .alert-box {
            background: rgba(239, 68, 68, 0.1);
            border: 1px solid rgba(239, 68, 68, 0.3);
            color: #fca5a5;
            padding: 8px;
            border-radius: 6px;
            font-size: 12px;
            margin-top: 8px;
        }
    </style>
</head>
<body>
    <h2>Repository → GitHub → Requirement</h2>

    <!-- Step 1: Repository Selection & Readiness -->
    <div class="card">
        <span class="step-badge">STEP 1</span>
        <h3>Repository & Integration Readiness</h3>
        <select id="vscode-repo-select">
            <option value="repo-cli-btw">KAUSHALK123/cli_BTW</option>
        </select>
        <div class="readiness-grid" id="vscode-readiness">
            <div class="status-item"><div class="indicator detected"></div> Git</div>
            <div class="status-item"><div class="indicator detected"></div> GitHub</div>
            <div class="status-item"><div class="indicator detected"></div> Entire</div>
            <div class="status-item"><div class="indicator detected"></div> Entire Graph</div>
        </div>
    </div>

    <!-- Step 2: GitHub Milestones -->
    <div class="card">
        <span class="step-badge">STEP 2</span>
        <h3>GitHub Milestones</h3>
        <select id="vscode-milestone-select">
            <option value="">Loading milestones...</option>
        </select>
        <button id="vscode-fetch-reqs-btn">Load Milestone Issues as Requirements</button>
    </div>

    <!-- Step 3: Requirements Matrix -->
    <div class="card">
        <span class="step-badge">STEP 3</span>
        <h3>Requirements Matrix</h3>
        <div id="vscode-requirements-list">
            <p style="color: var(--text-muted);">Select a milestone to load GitHub issues as requirements.</p>
        </div>
    </div>

    <script>
        const API_BASE = 'http://localhost:8080/api';

        document.addEventListener('DOMContentLoaded', () => {
            loadRepoInfo();
            loadMilestones();

            document.getElementById('vscode-fetch-reqs-btn').addEventListener('click', loadRequirements);
        });

        async function loadRepoInfo() {
            try {
                const res = await fetch(\`\${API_BASE}/repositories/repo-cli-btw\`);
                if (!res.ok) return;
                const repo = await res.json();
                if (repo.readiness) {
                    renderReadiness(repo.readiness);
                }
            } catch(e) {
                console.log('Local API unavailable, rendering dev state');
            }
        }

        function renderReadiness(r) {
            const el = document.getElementById('vscode-readiness');
            el.innerHTML = \`
                <div class="status-item"><div class="indicator \${r.git}"></div> Git</div>
                <div class="status-item"><div class="indicator \${r.github}"></div> GitHub</div>
                <div class="status-item"><div class="indicator \${r.entire}"></div> Entire</div>
                <div class="status-item"><div class="indicator \${r.entire_graph}"></div> Entire Graph</div>
            \`;
        }

        async function loadMilestones() {
            const select = document.getElementById('vscode-milestone-select');
            try {
                const res = await fetch(\`\${API_BASE}/repositories/repo-cli-btw/milestones\`);
                if (!res.ok) {
                    select.innerHTML = '<option value="2">Phase 2: Integration (Offline Fixture)</option>';
                    return;
                }
                const milestones = await res.json();
                select.innerHTML = milestones.map(m => 
                    \`<option value="\${m.number}">\${m.title} (\${m.state})</option>\`
                ).join('');
            } catch(e) {
                select.innerHTML = '<option value="2">Phase 2: Integration (Dev Fixture)</option>';
            }
        }

        async function loadRequirements() {
            const select = document.getElementById('vscode-milestone-select');
            const milestoneNum = select.value || 2;
            const container = document.getElementById('vscode-requirements-list');
            container.innerHTML = '<p style="color: var(--text-muted);">Loading issues...</p>';

            try {
                const res = await fetch(\`\${API_BASE}/repositories/repo-cli-btw/milestones/\${milestoneNum}/requirements\`);
                if (!res.ok) {
                    throw new Error('API error ' + res.status);
                }
                const reqs = await res.json();
                renderRequirements(reqs);
            } catch(e) {
                // Fallback to dev fixture when offline
                renderRequirements([
                    {
                        id: "13",
                        github_issue_ref: "13",
                        title: "COMPLETE THE REPOSITORY -> GITHUB -> REQUIREMENT WORKFLOW",
                        description: "Turn GitHub development goals into requirements for Checkpoint Intelligence engine.",
                        status: "needs_verification",
                        milestone: "Phase 2: Integration",
                        state: "open"
                    }
                ]);
            }
        }

        function renderRequirements(reqs) {
            const container = document.getElementById('vscode-requirements-list');
            if (!reqs || reqs.length === 0) {
                container.innerHTML = '<p style="color: var(--text-muted);">No issues found in milestone.</p>';
                return;
            }

            container.innerHTML = reqs.map(r => \`
                <div class="req-card \${r.state}">
                    <div style="display: flex; justify-content: space-between; align-items: flex-start;">
                        <strong>#\${r.github_issue_ref || r.id}: \${r.title}</strong>
                        <span class="tag \${r.status}">\${r.status}</span>
                    </div>
                    <p style="color: var(--text-muted); margin: 4px 0 0 0; font-size: 11px;">\${r.description || 'No description provided.'}</p>
                </div>
            \`).join('');
        }
    </script>
</body>
</html>`;
    }
}

function deactivate() {}

module.exports = {
    activate,
    deactivate
};
