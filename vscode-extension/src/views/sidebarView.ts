import * as vscode from 'vscode';
import { CheckpointApiClient } from '../client';

export class CheckpointSidebarViewProvider implements vscode.WebviewViewProvider {
    public static readonly viewType = 'checkpoint-intelligence-sidebar';
    private _view?: vscode.WebviewView;

    constructor(
        private readonly _extensionUri: vscode.Uri,
        private readonly _apiClient: CheckpointApiClient
    ) {}

    public resolveWebviewView(
        webviewView: vscode.WebviewView,
        context: vscode.WebviewViewResolveContext,
        _token: vscode.CancellationToken
    ) {
        this._view = webviewView;

        webviewView.webview.options = {
            enableScripts: true,
            localResourceRoots: [this._extensionUri]
        };

        this.updateHtml();

        webviewView.webview.onDidReceiveMessage(async (message) => {
            switch (message.type) {
                case 'refresh':
                    await this.updateHtml();
                    break;
                case 'enable':
                    await this._apiClient.enableEntire();
                    vscode.window.showInformationMessage('Entire Checkpoints connected and enabled for repository!');
                    await this.updateHtml();
                    break;
                case 'openDashboard':
                    vscode.env.openExternal(vscode.Uri.parse('http://localhost:8080'));
                    break;
            }
        });
    }

    public async updateHtml() {
        if (!this._view) {
            return;
        }

        try {
            const readiness = await this._apiClient.getReadiness();
            const reqs = await this._apiClient.getRequirements();
            const checkpoints = await this._apiClient.getCheckpoints();
            const commits = await this._apiClient.getCommits();
            const intel = await this._apiClient.getIntelligence();

            this._view.webview.html = this.getHtmlForWebview(readiness, reqs, checkpoints, commits, intel);
        } catch (error) {
            this._view.webview.html = `
                <!DOCTYPE html>
                <html>
                <body style="font-family: var(--vscode-font-family); padding: 15px; color: var(--vscode-foreground);">
                    <h3 style="color: var(--vscode-errorForeground);">Backend Server Offline</h3>
                    <p>Unable to connect to Checkpoint Intelligence API backend at <code>http://localhost:8080</code>.</p>
                    <p>Please ensure backend application is running with <code>go run ./app/main.go</code>.</p>
                </body>
                </html>
            `;
        }
    }

    private getHtmlForWebview(readiness: any, reqs: any[], checkpoints: any[], commits: any[], intel: any): string {
        const completenessColor = intel && intel.context_completeness === 'COMPLETE' ? '#4CAF50' : (intel && intel.context_completeness === 'REDACTED' ? '#9C27B0' : '#FF9800');
        const verificationColor = intel && intel.verification_status === 'COMPLETED' ? '#4CAF50' : (intel && intel.verification_status === 'PARTIALLY_VERIFIED' ? '#2196F3' : '#FF9800');

        const evidenceCheckpoint = intel && intel.evidence && intel.evidence.checkpoint ? (intel.evidence.checkpoint.available ? '✓ Checkpoint' : '✗ Checkpoint') : '✗ Checkpoint';
        const evidenceCommit = intel && intel.evidence && intel.evidence.commit ? (intel.evidence.commit.available ? '✓ Commit' : '✗ Commit') : '✗ Commit';
        const evidenceSource = intel && intel.evidence && intel.evidence.source ? (intel.evidence.source.available ? '✓ Source' : '✗ Source') : '✗ Source';
        const evidenceTests = intel && intel.evidence && intel.evidence.tests ? (intel.evidence.tests.available ? '✓ Tests' : '✗ Tests') : '✗ Tests';
        const evidenceGraph = intel && intel.evidence && intel.evidence.graph ? (intel.evidence.graph.available ? '✓ Graph' : '✗ Graph') : '✗ Graph';

        const commitRows = commits.map(c => {
            const hasCp = checkpoints.some(cp => cp.commit_ref === c.short_sha || cp.commit_ref === c.sha);
            const statusBadge = hasCp 
                ? '<span style="color: #4CAF50; font-size: 0.75em; font-weight: bold;">[Checkpoint Available]</span>' 
                : '<span style="color: #888; font-size: 0.75em;">[Git-Only / Unavailable]</span>';
            return `
                <div style="padding: 6px 0; border-bottom: 1px dashed var(--vscode-widget-border); font-size: 0.85em;">
                    <div style="display: flex; justify-content: space-between;">
                        <strong><code>${c.short_sha}</code></strong>
                        ${statusBadge}
                    </div>
                    <div>${c.message}</div>
                </div>
            `;
        }).join('');

        return `
            <!DOCTYPE html>
            <html lang="en">
            <head>
                <meta charset="UTF-8">
                <style>
                    body { font-family: var(--vscode-font-family); padding: 12px; color: var(--vscode-foreground); line-height: 1.4; }
                    .badge { display: inline-block; padding: 2px 6px; border-radius: 3px; font-size: 0.75em; font-weight: bold; color: #fff; }
                    .hero-card { background: var(--vscode-editor-background); border: 2px solid var(--vscode-focusBorder); padding: 12px; margin-bottom: 14px; border-radius: 6px; }
                    .card { background: var(--vscode-editor-background); border: 1px solid var(--vscode-widget-border); padding: 10px; margin-bottom: 12px; border-radius: 6px; }
                    .evidence-tag { display: inline-block; font-size: 0.75em; padding: 2px 5px; margin: 2px; border-radius: 3px; background: var(--vscode-sideBar-background); border: 1px solid var(--vscode-widget-border); }
                    button { width: 100%; padding: 6px; background: var(--vscode-button-background); color: var(--vscode-button-foreground); border: none; border-radius: 4px; cursor: pointer; margin-top: 6px; }
                    button:hover { background: var(--vscode-button-hoverBackground); }
                </style>
            </head>
            <body>
                <!-- CHECKPOINT INTELLIGENCE HERO VIEW -->
                <div class="hero-card">
                    <div style="font-size: 0.8em; text-transform: uppercase; letter-spacing: 0.05em; color: var(--vscode-descriptionForeground); font-weight: bold;">⚡ CHECKPOINT INTELLIGENCE HERO</div>
                    <h3 style="margin: 6px 0 4px 0; color: var(--vscode-symbolIcon-keywordForeground);">${intel ? intel.requirement_title || 'Core Requirement' : 'Intelligence Engine'}</h3>
                    <div style="display: flex; gap: 6px; margin-bottom: 8px;">
                        <span class="badge" style="background: ${completenessColor};">Context: ${intel ? intel.context_completeness : 'LOADING'}</span>
                        <span class="badge" style="background: ${verificationColor};">Status: ${intel ? intel.verification_status : 'PENDING'}</span>
                    </div>

                    <div style="font-size: 0.85em; margin-bottom: 6px;">
                        <strong>🎯 INTENT:</strong> ${intel ? intel.intent : 'Loading checkpoint intent...'}
                    </div>

                    <div style="font-size: 0.85em; margin-bottom: 6px;">
                        <strong>✓ IMPLEMENTED:</strong>
                        <ul style="margin: 2px 0 0 16px; padding: 0;">
                            ${intel && intel.implemented ? intel.implemented.map((i: string) => `<li>${i}</li>`).join('') : '<li>Analyzed source tree diffs</li>'}
                        </ul>
                    </div>

                    <div style="font-size: 0.85em; margin-bottom: 6px; color: #FF9800;">
                        <strong>✗ INCOMPLETE:</strong>
                        <ul style="margin: 2px 0 0 16px; padding: 0;">
                            ${intel && intel.incomplete ? intel.incomplete.map((i: string) => `<li>${i}</li>`).join('') : '<li>None</li>'}
                        </ul>
                    </div>

                    <div style="font-size: 0.8em; margin-bottom: 6px;">
                        <strong>🔍 EVIDENCE MATRIX:</strong><br/>
                        <span class="evidence-tag">${evidenceCheckpoint}</span>
                        <span class="evidence-tag">${evidenceCommit}</span>
                        <span class="evidence-tag">${evidenceSource}</span>
                        <span class="evidence-tag">${evidenceTests}</span>
                        <span class="evidence-tag">${evidenceGraph}</span>
                    </div>

                    <div style="font-size: 0.85em; background: var(--vscode-sideBar-background); padding: 6px; border-radius: 4px; margin-top: 6px;">
                        <strong>🚀 NEXT ACTION:</strong> ${intel ? intel.next_action : 'Review commit evidence.'}
                    </div>
                </div>

                <div class="card">
                    <div style="display: flex; justify-content: space-between; align-items: center;">
                        <strong>Repository Readiness</strong>
                        <span class="badge" style="background: #4CAF50;">${readiness.status} (${readiness.readiness_score}/100)</span>
                    </div>
                </div>

                <h4>Commits & Development History</h4>
                <div class="card">
                    ${commitRows}
                </div>

                <button onclick="post('openDashboard')">Open Web Dashboard</button>
                <button onclick="post('refresh')" style="background: transparent; border: 1px solid var(--vscode-widget-border);">Refresh Intelligence</button>

                <script>
                    const vscode = acquireVsCodeApi();
                    function post(type) { vscode.postMessage({ type }); }
                </script>
            </body>
            </html>
        `;
    }
}
