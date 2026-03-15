import * as vscode from 'vscode';
import { BinaryManager } from './binary';
import { Runner } from './runner';
import { DiagnosticManager } from './diagnostics';

let outputChannel: vscode.OutputChannel;
let diagnosticManager: DiagnosticManager;

export function activate(context: vscode.ExtensionContext) {
    outputChannel = vscode.window.createOutputChannel('Betteralign');
    const binaryManager = new BinaryManager(outputChannel);
    const runner = new Runner(outputChannel, binaryManager);
    diagnosticManager = new DiagnosticManager();

    // Check if binary is installed
    setTimeout(async () => {
        const bin = await binaryManager.getBinaryPath();
        if (!bin) {
            const result = await vscode.window.showWarningMessage(
                'betteralign binary not found. Would you like to install it?',
                'Install'
            );
            if (result === 'Install') {
                await binaryManager.installBinary();
            }
        }
    }, 1000);

    // Commands
    context.subscriptions.push(
        vscode.commands.registerCommand('betteralign.installBinary', async () => {
            await binaryManager.installBinary();
        }),
        vscode.commands.registerCommand('betteralign.run', async () => {
            const editor = vscode.window.activeTextEditor;
            if (editor && editor.document.languageId === 'go') {
                await runBetteralign(editor.document, runner, false);
            }
        }),
        vscode.commands.registerCommand('betteralign.apply', async () => {
            const editor = vscode.window.activeTextEditor;
            if (editor && editor.document.languageId === 'go') {
                if (editor.document.isDirty) {
                    await editor.document.save();
                }
                const success = await runner.apply(editor.document.fileName);
                if (success) {
                    vscode.window.showInformationMessage('Successfully applied betteralign optimizations.');
                    // Re-run diagnostics to clear them
                    await runBetteralign(editor.document, runner, true);
                }
            }
        })
    );

    // Events
    context.subscriptions.push(
        vscode.workspace.onDidSaveTextDocument(async (document) => {
            if (document.languageId === 'go') {
                const config = vscode.workspace.getConfiguration('betteralign');
                if (config.get<boolean>('enable', true) && config.get<boolean>('runOnSave', true)) {
                    await runBetteralign(document, runner, false);
                }
            }
        }),
        vscode.workspace.onDidCloseTextDocument((document) => {
            diagnosticManager.clear(document);
        })
    );
    
    // Run for open editors on startup
    if (vscode.window.activeTextEditor && vscode.window.activeTextEditor.document.languageId === 'go') {
        const config = vscode.workspace.getConfiguration('betteralign');
        if (config.get<boolean>('enable', true)) {
            runBetteralign(vscode.window.activeTextEditor.document, runner, false);
        }
    }
}

async function runBetteralign(document: vscode.TextDocument, runner: Runner, isApplyAction: boolean) {
    if (document.isDirty) {
        return; // Don't run on unsaved files since betteralign works on disk
    }

    try {
        const result = await runner.run(document.fileName);
        const hasIssues = diagnosticManager.update(document, result);

        if (hasIssues && !isApplyAction) {
            const config = vscode.workspace.getConfiguration('betteralign');
            const promptSetting = config.get<string>('applyPrompt', 'onDiagnostics');
            
            if (promptSetting === 'always' || promptSetting === 'onDiagnostics') {
                const action = await vscode.window.showInformationMessage(
                    'betteralign found suboptimal struct layouts. Would you like to apply fixes?',
                    'Apply', 'Dismiss', 'Never Show Again'
                );

                if (action === 'Apply') {
                    vscode.commands.executeCommand('betteralign.apply');
                } else if (action === 'Never Show Again') {
                    config.update('applyPrompt', 'never', vscode.ConfigurationTarget.Global);
                }
            }
        }
    } catch (e) {
        outputChannel.appendLine(`Error running betteralign: ${e}`);
    }
}

export function deactivate() {
    if (diagnosticManager) {
        diagnosticManager.dispose();
    }
}
