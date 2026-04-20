import * as vscode from 'vscode';
import { BinaryManager } from './binary';
import { Runner } from './runner';
import { DiagnosticManager } from './diagnostics';

const DEFAULT_DEBOUNCE_MS = 300;
const MIN_DEBOUNCE_MS = 0;
const MAX_DEBOUNCE_MS = 5000;

/** Per-URI coalescing slot: latest-document-wins, cancellable via `abort`. */
interface AnalysisSlot {
    running: Promise<boolean>;
    pending: boolean;
    document: vscode.TextDocument;
    abort: AbortController;
}

let outputChannel: vscode.OutputChannel;
let diagnosticManager: DiagnosticManager;
let installCheckTimer: NodeJS.Timeout | undefined;
// Stops the analyze() loop from writing to a disposed collection post-deactivate.
let disposed = false;
const analysisSlots = new Map<string, AnalysisSlot>();
const saveDebounce = new Map<string, NodeJS.Timeout>();
// One prompt per URI; dedupes rapid save bursts.
const activePrompts = new Set<string>();
// Applies write to disk — awaited (not aborted) in deactivate to avoid truncation.
const activeApplies = new Set<Promise<unknown>>();

export function activate(context: vscode.ExtensionContext) {
    disposed = false;
    outputChannel = vscode.window.createOutputChannel('Betteralign');
    context.subscriptions.push(outputChannel);
    const binaryManager = new BinaryManager(outputChannel);
    const runner = new Runner(outputChannel, binaryManager);
    diagnosticManager = new DiagnosticManager();
    context.subscriptions.push(runner, binaryManager, diagnosticManager);

    // Deferred so activation stays snappy.
    installCheckTimer = setTimeout(async () => {
        installCheckTimer = undefined;
        if (disposed) {
            return;
        }
        const bin = await binaryManager.getBinaryPath();
        if (disposed || bin) {
            return;
        }
        const result = await vscode.window.showWarningMessage(
            'betteralign binary not found. Would you like to install it?',
            'Install'
        );
        if (disposed || result !== 'Install') {
            return;
        }
        await binaryManager.installBinary();
    }, 1000);

    // Commands
    context.subscriptions.push(
        vscode.commands.registerCommand('betteralign.installBinary', async () => {
            await binaryManager.installBinary();
        }),
        vscode.commands.registerCommand('betteralign.run', async () => {
            const editor = vscode.window.activeTextEditor;
            if (!editor || !isAnalysableGoDoc(editor.document)) {
                return;
            }
            // Consistency with save/open paths — surface disabled-state instead of silent no-op.
            const enabled = vscode.workspace
                .getConfiguration('betteralign', editor.document.uri)
                .get<boolean>('enable', true);
            if (!enabled) {
                vscode.window.showInformationMessage('betteralign is disabled (betteralign.enable = false).');
                return;
            }
            await runBetteralign(editor.document, runner);
        }),
        // `targetUri` pins apply to the analysed doc — active editor may have changed during prompt.
        vscode.commands.registerCommand('betteralign.apply', async (targetUri?: vscode.Uri) => {
            let doc: vscode.TextDocument | undefined;
            if (targetUri) {
                doc = vscode.workspace.textDocuments.find(
                    (d) => d.uri.toString() === targetUri.toString()
                );
                if (!doc) {
                    // Doc closed between prompt and click.
                    outputChannel.appendLine(`betteralign.apply: target document is no longer open: ${targetUri.toString()}`);
                    vscode.window.showWarningMessage('Could not apply betteralign fixes: the target file is no longer open.');
                    return;
                }
            } else {
                const editor = vscode.window.activeTextEditor;
                doc = editor?.document;
            }
            if (!doc || !isAnalysableGoDoc(doc)) {
                return;
            }
            // Must save before -apply so disk reflects buffer; abort on veto to avoid losing edits.
            if (doc.isDirty) {
                const saved = await doc.save();
                if (!saved) {
                    outputChannel.appendLine(`betteralign.apply: pre-apply save failed or was vetoed for ${doc.uri.toString()}; skipping apply.`);
                    vscode.window.showErrorMessage('Could not save the file before applying fixes; aborted to avoid losing edits.');
                    return;
                }
            }
            const applyPromise = runner.apply(doc.fileName);
            activeApplies.add(applyPromise);
            let success = false;
            try {
                success = await applyPromise;
            } finally {
                activeApplies.delete(applyPromise);
            }
            if (success) {
                vscode.window.showInformationMessage('Successfully applied betteralign optimizations.');
                // Clear stale diagnostics; next save re-analyzes the reloaded buffer.
                diagnosticManager.clear(doc);
            } else {
                vscode.window.showErrorMessage('Failed to apply betteralign optimizations. See Output → Betteralign.');
            }
        })
    );

    // Events
    context.subscriptions.push(
        vscode.workspace.onDidSaveTextDocument((document) => {
            if (!isAnalysableGoDoc(document)) {
                return;
            }
            // enable/runOnSave re-checked inside scheduleRun post-debounce.
            scheduleRun(document, runner);
        }),
        // Covers files opened after activation — startup loop only sees initial set.
        vscode.workspace.onDidOpenTextDocument((document) => {
            if (!isAnalysableGoDoc(document) || document.isDirty) {
                return;
            }
            const config = vscode.workspace.getConfiguration('betteralign');
            if (!config.get<boolean>('enable', true)) {
                return;
            }
            runBetteralign(document, runner).catch((e) => {
                outputChannel.appendLine(`Error running betteralign on open: ${e}`);
            });
        }),
        vscode.workspace.onDidCloseTextDocument((document) => {
            const key = document.uri.toString();
            const pending = saveDebounce.get(key);
            if (pending) {
                clearTimeout(pending);
                saveDebounce.delete(key);
            }
            // Abort frees the semaphore + subprocess early; analyze loop discards the result.
            const slot = analysisSlots.get(key);
            if (slot) {
                slot.abort.abort();
            }
            diagnosticManager.clear(document);
        })
    );

    // Visible-only to avoid thundering-herd on restored sessions. runOnSave is for save events; this isn't one.
    const config = vscode.workspace.getConfiguration('betteralign');
    if (config.get<boolean>('enable', true)) {
        const seen = new Set<string>();
        for (const editor of vscode.window.visibleTextEditors) {
            const doc = editor.document;
            if (!isAnalysableGoDoc(doc) || doc.isDirty) {
                continue;
            }
            const key = doc.uri.toString();
            if (seen.has(key)) {
                continue;
            }
            seen.add(key);
            runBetteralign(doc, runner).catch((e) => {
                outputChannel.appendLine(`Error running betteralign on startup: ${e}`);
            });
        }
    }
}

/** File-backed Go docs only; betteralign needs the package on disk. */
function isAnalysableGoDoc(document: vscode.TextDocument): boolean {
    return document.languageId === 'go' && document.uri.scheme === 'file';
}

function getDebounceMs(): number {
    const raw = vscode.workspace.getConfiguration('betteralign').get<number>('debounceMs', DEFAULT_DEBOUNCE_MS);
    if (!Number.isFinite(raw)) {
        return DEFAULT_DEBOUNCE_MS;
    }
    return Math.max(MIN_DEBOUNCE_MS, Math.min(MAX_DEBOUNCE_MS, Math.floor(raw)));
}

/** Debounces save bursts; full-package reload is expensive. */
function scheduleRun(document: vscode.TextDocument, runner: Runner) {
    const key = document.uri.toString();
    const existing = saveDebounce.get(key);
    if (existing) {
        clearTimeout(existing);
    }
    const handle = setTimeout(() => {
        saveDebounce.delete(key);
        if (disposed) {
            return;
        }
        // User may have toggled enable/runOnSave mid-debounce.
        const config = vscode.workspace.getConfiguration('betteralign');
        if (!config.get<boolean>('enable', true) || !config.get<boolean>('runOnSave', true)) {
            return;
        }
        runBetteralign(document, runner).catch((e) => {
            outputChannel.appendLine(`Error running betteralign: ${e}`);
        });
    }, getDebounceMs());
    saveDebounce.set(key, handle);
}

async function runBetteralign(document: vscode.TextDocument, runner: Runner) {
    if (document.isDirty) {
        return; // betteralign reads from disk.
    }

    const hasIssues = await analyze(document, runner);
    if (!hasIssues || disposed) {
        return;
    }
    // Skip prompt if doc closed mid-analysis — user can't act on it.
    if (!isStillOpen(document.uri.toString())) {
        return;
    }

    // Resource-scoped so update(..., WorkspaceFolder) resolves the target folder.
    const config = vscode.workspace.getConfiguration('betteralign', document.uri);
    const promptSetting = config.get<string>('applyPrompt', 'onDiagnostics');
    if (promptSetting === 'never') {
        return;
    }

    // Prevent prompt stacking on rapid repeated saves.
    const key = document.uri.toString();
    if (activePrompts.has(key)) {
        return;
    }
    activePrompts.add(key);
    try {
        // Prompt outside analyze() so concurrent saves aren't blocked.
        const action = await vscode.window.showInformationMessage(
            'betteralign found suboptimal struct layouts. Would you like to apply fixes?',
            'Apply', 'Never Show Again'
        );
        if (action === 'Apply') {
            // Pin to this doc — active editor may have changed during prompt.
            await vscode.commands.executeCommand('betteralign.apply', document.uri);
        } else if (action === 'Never Show Again') {
            await config.update('applyPrompt', 'never', neverShowScope(config));
        }
    } finally {
        activePrompts.delete(key);
    }
}

/** Writes "Never Show Again" to the most specific scope that already has a value. */
function neverShowScope(config: vscode.WorkspaceConfiguration): vscode.ConfigurationTarget {
    const inspection = config.inspect<string>('applyPrompt');
    if (inspection?.workspaceFolderValue !== undefined) {
        return vscode.ConfigurationTarget.WorkspaceFolder;
    }
    if (inspection?.workspaceValue !== undefined) {
        return vscode.ConfigurationTarget.Workspace;
    }
    return vscode.ConfigurationTarget.Global;
}

/** Per-URI coalescing: returns true if diagnostics remain after the run settles. */
function analyze(document: vscode.TextDocument, runner: Runner): Promise<boolean> {
    const key = document.uri.toString();
    const existing = analysisSlots.get(key);
    if (existing) {
        existing.pending = true;
        existing.document = document;
        return existing.running;
    }

    const slot: AnalysisSlot = {
        pending: false,
        document,
        running: Promise.resolve(false),
        abort: new AbortController(),
    };
    analysisSlots.set(key, slot);

    slot.running = (async (): Promise<boolean> => {
        let last = false;
        try {
            do {
                slot.pending = false;
                const doc = slot.document;
                if (doc.isDirty || slot.abort.signal.aborted) {
                    last = false;
                    break;
                }
                const result = await runner.run(doc.fileName, slot.abort.signal);
                if (disposed || slot.abort.signal.aborted || !isStillOpen(key)) {
                    last = false;
                    break;
                }
                last = diagnosticManager.update(doc, result);
            } while (slot.pending && !disposed && !slot.abort.signal.aborted);
        } catch (e) {
            outputChannel.appendLine(`Error running betteralign: ${e}`);
            last = false;
        } finally {
            analysisSlots.delete(key);
        }
        return last;
    })();
    return slot.running;
}

function isStillOpen(key: string): boolean {
    for (const doc of vscode.workspace.textDocuments) {
        if (doc.uri.toString() === key) {
            return true;
        }
    }
    return false;
}

export async function deactivate(): Promise<void> {
    disposed = true;
    if (installCheckTimer) {
        clearTimeout(installCheckTimer);
        installCheckTimer = undefined;
    }
    for (const handle of saveDebounce.values()) {
        clearTimeout(handle);
    }
    saveDebounce.clear();
    // Kill analysis subprocesses promptly.
    for (const slot of analysisSlots.values()) {
        slot.abort.abort();
    }
    analysisSlots.clear();
    activePrompts.clear();
    // Applies write to disk — await, don't abort (avoids partial rewrite).
    if (activeApplies.size > 0) {
        await Promise.allSettled(Array.from(activeApplies));
        activeApplies.clear();
    }
    // Other disposables handled by context.subscriptions.
}
