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
            const graph = await this._apiClient.getGraphFindings();
            const handoff = await this._apiClient.getHandoff();

            this._view.webview.html = this.getHtmlForWebview(readiness, reqs, checkpoints, commits, graph, handoff);
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

    private getHtmlForWebview(readiness: any, reqs: any[], checkpoints: any[], commits: any[], graph: any[], handoff: any): string {
        const reqRows = reqs.map(r => `
            <div style="background: var(--vscode-sideBar-background); border: 1px solid var(--vscode-widget-border); padding: 8px; margin-bottom: 8px; border-radius: 4px;">
                <div style="display: flex; justify-content: space-between; font-weight: bold;">
                    <span>${r.id}: ${r.title}</span>
                    <span style="color: ${r.status === 'COMPLETED' ? '#4CAF50' : '#FF9800'}; font-size: 0.85em;">[${r.status}]</span>
                </div>
                <div style="font-size: 0.85em; color: var(--vscode-descriptionForeground); margin-top: 4px;">${r.description}</div>
            </div>
        `).join('');

        const commitRows = commits.map(c => {
            const hasCp = checkpoints.some(cp => cp.commit_ref === c.short_sha || cp.commit_ref === c.sha);
            const statusBadge = hasCp 
                ? '<span style="color: #4CAF50; font-size: 0.75em; font-weight: bold;">[Entire Checkpoint Available]</span>' 
                : '<span style="color: #888; font-size: 0.75em;">[Git-Only / Checkpoint Unavailable]</span>';
            return `
                <div style="padding: 8px 0; border-bottom: 1px dashed var(--vscode-widget-border); font-size: 0.85em;">
                    <div style="display: flex; justify-content: space-between;">
                        <strong><code>${c.short_sha}</code></strong>
                        ${statusBadge}
                    </div>
                    <div>${c.message}</div>
                    <div style="font-size: 0.8em; color: var(--vscode-descriptionForeground); margin-top: 2px;">
                        By ${c.author_name} &bull; ${c.files_changed ? c.files_changed.length : 0} files changed
                    </div>
                </div>
            `;
        }).join('');

        const cpRows = checkpoints.map(cp => `
            <div style="padding: 6px 0; border-bottom: 1px dashed var(--vscode-widget-border); font-size: 0.85em;">
                <strong>${cp.checkpoint_id}</strong> (<code>${cp.commit_ref}</code>)<br/>
                <span>${cp.intent_context}</span>
            </div>
        `).join('');

        return `
            <!DOCTYPE html>
            <html lang="en">
            <head>
                <meta charset="UTF-8">
                <style>
                    body { font-family: var(--vscode-font-family); padding: 12px; color: var(--vscode-foreground); line-height: 1.4; }
                    .badge { display: inline-block; padding: 2px 6px; border-radius: 3px; font-size: 0.75em; font-weight: bold; background: #4CAF50; color: #fff; }
                    .card { background: var(--vscode-editor-background); border: 1px solid var(--vscode-widget-border); padding: 10px; margin-bottom: 12px; border-radius: 6px; }
                    button { width: 100%; padding: 6px; background: var(--vscode-button-background); color: var(--vscode-button-foreground); border: none; border-radius: 4px; cursor: pointer; margin-top: 6px; }
                    button:hover { background: var(--vscode-button-hoverBackground); }
                </style>
            </head>
            <body>
                <div class="card">
                    <div style="display: flex; justify-content: space-between; align-items: center;">
                        <strong>Repository Readiness</strong>
                        <span class="badge">${readiness.status} (${readiness.readiness_score}/100)</span>
                    </div>
                    <div style="font-size: 0.8em; margin-top: 6px; color: var(--vscode-descriptionForeground);">
                        Entire CLI: ${readiness.entire_installed ? 'Installed' : 'Missing'}<br/>
                        Entire Graph: ${readiness.graph_available ? 'Available' : 'Unavailable'}<br/>
                        Privacy Redaction: Active
                    </div>
                    ${!readiness.entire_enabled ? '<button onclick="post(\'enable\')">Enable Entire Checkpoints</button>' : ''}
                </div>

                <h4>Commits & Development History</h4>
                <div class="card">
                    ${commitRows}
                </div>

                <h4>Requirements Audit</h4>
                ${reqRows}

                <h4>Redacted Checkpoints Context</h4>
                <div class="card">
                    ${cpRows}
                </div>

                <h4>Developer Handoff Briefing</h4>
                <div class="card" style="font-size: 0.85em;">
                    <strong>Intent:</strong> ${handoff.original_intent}<br/><br/>
                    <strong>Next Action:</strong> ${handoff.recommended_next_action}
                </div>

                <button onclick="post('openDashboard')">Open Web Dashboard</button>
                <button onclick="post('refresh')" style="background: transparent; border: 1px solid var(--vscode-widget-border);">Refresh Audit</button>

                <script>
                    const vscode = acquireVsCodeApi();
                    function post(type) { vscode.postMessage({ type }); }
                </script>
            </body>
            </html>
        `;
    }
}
