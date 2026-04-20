import * as vscode from 'vscode';
import { BetteralignResult, BetteralignDiagnostic } from './runner';

export class DiagnosticManager implements vscode.Disposable {
    private collection: vscode.DiagnosticCollection;

    constructor() {
        this.collection = vscode.languages.createDiagnosticCollection('betteralign');
    }

    public update(document: vscode.TextDocument, result: BetteralignResult | null): boolean {
        this.collection.delete(document.uri);

        if (!result) {
            return false;
        }

        const diagnostics: vscode.Diagnostic[] = [];
        // Resolve per-URI so multi-root folder overrides are honoured.
        const severity = this.getConfiguredSeverity(document.uri);

        for (const pkgName of Object.keys(result)) {
            const pkg = result[pkgName];
            if (!pkg.betteralign || pkg.betteralign.length === 0) {
                continue;
            }
            for (const item of pkg.betteralign) {
                const diag = this.parseDiagnostic(item, severity, document);
                if (diag) {
                    diagnostics.push(diag);
                }
            }
        }

        if (diagnostics.length > 0) {
            this.collection.set(document.uri, diagnostics);
        }

        return diagnostics.length > 0;
    }

    public clear(document: vscode.TextDocument) {
        this.collection.delete(document.uri);
    }

    public dispose() {
        this.collection.dispose();
    }

    private getConfiguredSeverity(resource: vscode.Uri): vscode.DiagnosticSeverity {
        const config = vscode.workspace.getConfiguration('betteralign', resource);
        const severityStr = config.get<string>('severity', 'hint');

        switch (severityStr) {
            case 'error': return vscode.DiagnosticSeverity.Error;
            case 'warning': return vscode.DiagnosticSeverity.Warning;
            case 'information': return vscode.DiagnosticSeverity.Information;
            case 'hint':
            default:
                return vscode.DiagnosticSeverity.Hint;
        }
    }

    private parseDiagnostic(item: BetteralignDiagnostic, severity: vscode.DiagnosticSeverity, doc: vscode.TextDocument): vscode.Diagnostic | null {
        // posn is "file:line:col"; take last two parts so Windows drive letters don't break it.
        // try/catch guards lineAt racing a disposed doc during -apply reload.
        try {
            const parts = item.posn.split(':');
            if (parts.length < 3) {
                return null;
            }

            const line = parseInt(parts[parts.length - 2], 10) - 1;
            const col = parseInt(parts[parts.length - 1], 10) - 1;

            if (isNaN(line) || isNaN(col) || line < 0 || col < 0) {
                return null;
            }
            // Stale position after -apply rewrote the file.
            if (line >= doc.lineCount) {
                return null;
            }

            const lineText = doc.lineAt(line).text;
            const startCol = Math.min(col, lineText.length);
            const start = new vscode.Position(line, startCol);
            const end = new vscode.Position(line, lineText.length);
            const range = new vscode.Range(start, end);

            const diagnostic = new vscode.Diagnostic(range, item.message, severity);
            diagnostic.source = 'betteralign';
            diagnostic.code = 'struct-alignment';
            return diagnostic;
        } catch {
            return null;
        }
    }
}
