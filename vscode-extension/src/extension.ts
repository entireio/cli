import * as vscode from 'vscode';
import { CheckpointApiClient } from './client';
import { CheckpointSidebarViewProvider } from './views/sidebarView';

export function activate(context: vscode.ExtensionContext) {
    const apiClient = new CheckpointApiClient();

    const sidebarProvider = new CheckpointSidebarViewProvider(context.extensionUri, apiClient);
    context.subscriptions.push(
        vscode.window.registerWebviewViewProvider(
            CheckpointSidebarViewProvider.viewType,
            sidebarProvider
        )
    );

    // Status Bar Item
    const statusBarItem = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Right, 100);
    statusBarItem.text = '$(shield-check) Entire Audit: 85/100';
    statusBarItem.tooltip = 'Checkpoint Intelligence: Audit Score 85/100 (Ready)';
    statusBarItem.command = 'entire.audit.run';
    statusBarItem.show();
    context.subscriptions.push(statusBarItem);

    // Register Commands
    context.subscriptions.push(
        vscode.commands.registerCommand('entire.audit.run', async () => {
            await sidebarProvider.updateHtml();
            vscode.window.showInformationMessage('Checkpoint Intelligence Audit refreshed!');
        })
    );

    context.subscriptions.push(
        vscode.commands.registerCommand('entire.audit.enable', async () => {
            await apiClient.enableEntire();
            vscode.window.showInformationMessage('Entire Checkpoints connected and enabled for current workspace!');
            await sidebarProvider.updateHtml();
        })
    );

    context.subscriptions.push(
        vscode.commands.registerCommand('entire.audit.openDashboard', () => {
            vscode.env.openExternal(vscode.Uri.parse('http://localhost:8080'));
        })
    );
}

export function deactivate() {}
