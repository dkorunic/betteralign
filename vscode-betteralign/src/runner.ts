import * as vscode from 'vscode';
import * as cp from 'child_process';
import * as path from 'path';
import { BinaryManager } from './binary';

export interface BetteralignDiagnostic {
    posn: string; // e.g. "/path/to/file.go:12:3"
    message: string;
    suggested_fixes: any | null;
}

export interface BetteralignResult {
    [pkgPath: string]: {
        betteralign?: BetteralignDiagnostic[];
    };
}

export class Runner {
    constructor(
        private outputChannel: vscode.OutputChannel,
        private binaryManager: BinaryManager
    ) {}

    private getExtraArgs(): string[] {
        const config = vscode.workspace.getConfiguration('betteralign');
        const args = config.get<string[]>('extraArgs');
        return args || [];
    }

    public async run(filePath: string): Promise<BetteralignResult | null> {
        const bin = await this.binaryManager.getBinaryPath();
        if (!bin) {
            return null;
        }

        const args = ['-json', ...this.getExtraArgs(), filePath];
        const cwd = path.dirname(filePath);

        try {
            const { stdout, stderr } = await this.execCommand(bin, args, cwd);
            // betteralign outputs JSON when there are no issues and exit code is 0
            if (stdout.trim()) {
                return JSON.parse(stdout) as BetteralignResult;
            }
            return null;
        } catch (err: any) {
            // When betteralign finds issues, it returns exit code 3 from singlechecker
            // The JSON output will be in stdout
            if (err.stdout && err.stdout.trim().startsWith('{')) {
                try {
                    return JSON.parse(err.stdout) as BetteralignResult;
                } catch (parseErr) {
                    this.outputChannel.appendLine(`Failed to parse betteralign JSON output: ${parseErr}`);
                }
            } else if (err.stderr) {
                // Not standard lint output, maybe compile error
                this.outputChannel.appendLine(`betteralign error: ${err.stderr}`);
            } else {
                this.outputChannel.appendLine(`betteralign failed: ${err.message}`);
            }
            return null;
        }
    }

    public async apply(filePath: string): Promise<boolean> {
        const bin = await this.binaryManager.getBinaryPath();
        if (!bin) {
            vscode.window.showErrorMessage('betteralign binary not found.');
            return false;
        }

        const args = ['-apply', ...this.getExtraArgs(), filePath];
        const cwd = path.dirname(filePath);

        try {
            await this.execCommand(bin, args, cwd);
            return true;
        } catch (err: any) {
            if (err.stderr) {
                this.outputChannel.appendLine(`betteralign apply error: ${err.stderr}`);
            } else if (err.stdout) {
                this.outputChannel.appendLine(`betteralign apply error output: ${err.stdout}`);
                // sometimes exit code is non-zero even on success if issues were still found elsewhere
                return true; 
            }
            // we will consider it a success if it doesn't throw a hard crash usually
            return true;
        }
    }

    private execCommand(bin: string, args: string[], cwd: string): Promise<{ stdout: string, stderr: string }> {
        return new Promise((resolve, reject) => {
            cp.execFile(bin, args, { cwd, maxBuffer: 1024 * 1024 * 10 }, (error, stdout, stderr) => {
                if (error) {
                    reject({ ...error, stdout, stderr });
                    return;
                }
                resolve({ stdout, stderr });
            });
        });
    }
}
