import * as vscode from 'vscode';
import { BetteralignResult, BetteralignDiagnostic } from './runner';

export class DiagnosticManager {
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
        let hasIssues = false;

        const severity = this.getConfiguredSeverity();

        for (const pkgName of Object.keys(result)) {
            const pkg = result[pkgName];
            if (pkg.betteralign && pkg.betteralign.length > 0) {
                for (const item of pkg.betteralign) {
                    const diag = this.parseDiagnostic(item, severity, document);
                    if (diag) {
                        diagnostics.push(diag);
                        hasIssues = true;
                    }
                }
            }
        }

        if (diagnostics.length > 0) {
            this.collection.set(document.uri, diagnostics);
        }

        return hasIssues;
    }

    public clear(document: vscode.TextDocument) {
        this.collection.delete(document.uri);
    }

    public dispose() {
        this.collection.dispose();
    }

    private getConfiguredSeverity(): vscode.DiagnosticSeverity {
        const config = vscode.workspace.getConfiguration('betteralign');
        const severityStr = config.get<string>('severity', 'information');
        
        switch (severityStr) {
            case 'error': return vscode.DiagnosticSeverity.Error;
            case 'warning': return vscode.DiagnosticSeverity.Warning;
            case 'hint': return vscode.DiagnosticSeverity.Hint;
            case 'information':
            default:
                return vscode.DiagnosticSeverity.Information;
        }
    }

    private parseDiagnostic(item: BetteralignDiagnostic, severity: vscode.DiagnosticSeverity, doc: vscode.TextDocument): vscode.Diagnostic | null {
        // format is "file.go:line:col"
        const parts = item.posn.split(':');
        if (parts.length < 3) return null;

        const line = parseInt(parts[parts.length - 2], 10) - 1; // VS Code is 0-indexed
        const col = parseInt(parts[parts.length - 1], 10) - 1;

        if (isNaN(line) || isNaN(col)) return null;

        // Try to get word boundary based on where the error points (it usually points to `type X struct`)
        // A minimal range is fine
        const lineText = doc.lineAt(line).text;
        
        // Give a little range of the line or word
        const start = new vscode.Position(line, col);
        const end = new vscode.Position(line, lineText.length);
        const range = new vscode.Range(start, end);

        const diagnostic = new vscode.Diagnostic(range, item.message, severity);
        diagnostic.source = 'betteralign';
        diagnostic.code = 'struct-alignment';
        return diagnostic;
    }
}
