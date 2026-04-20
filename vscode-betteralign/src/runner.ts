import * as vscode from 'vscode';
import * as cp from 'child_process';
import * as os from 'os';
import * as path from 'path';
import { BinaryManager } from './binary';

export interface BetteralignDiagnostic {
    posn: string; // e.g. "/path/to/file.go:12:3"
    message: string;
    suggested_fixes: unknown;
}

export interface BetteralignResult {
    [pkgPath: string]: {
        betteralign?: BetteralignDiagnostic[];
    };
}

type ExecError = Error & { stdout?: string; stderr?: string; code?: number | string };

function isAbortError(e: Error & { code?: number | string; name?: string }): boolean {
    return e.code === 'ABORT_ERR' || e.name === 'AbortError';
}

// Safelist. String flags must use -flag=value form to avoid positional-arg injection.
const SAFE_FLAGS: Record<string, 'bool' | 'string'> = {
    '-test_files': 'bool',
    '-generated_files': 'bool',
    '-opt_in': 'bool',
    '-exclude_dirs': 'string',
    '-exclude_files': 'string',
};

const MAX_BUFFER_BYTES = 64 * 1024 * 1024;
const EXEC_TIMEOUT_MS = 120_000;

/** Caps concurrent betteralign runs to avoid thrashing under many open editors. */
class Semaphore {
    private permits: number;
    private waiters: Array<() => void> = [];
    constructor(permits: number) {
        this.permits = Math.max(1, permits);
    }
    async acquire(): Promise<() => void> {
        if (this.permits > 0) {
            this.permits--;
        } else {
            await new Promise<void>((resolve) => this.waiters.push(resolve));
        }
        let released = false;
        return () => {
            if (released) {
                return;
            }
            released = true;
            const next = this.waiters.shift();
            if (next) {
                next();
            } else {
                this.permits++;
            }
        };
    }
}

const concurrencyGate = new Semaphore(Math.max(1, Math.floor((os.cpus().length || 2) / 2)));

export class Runner implements vscode.Disposable {
    // Dedupes repeat warnings per session.
    private loggedRejections = new Set<string>();
    private resolveSubscription: vscode.Disposable;

    constructor(
        private outputChannel: vscode.OutputChannel,
        private binaryManager: BinaryManager
    ) {
        // Re-arm no-binary warning so it fires again if the binary disappears later.
        this.resolveSubscription = this.binaryManager.onDidResolve(() => {
            this.loggedRejections.delete('no-binary');
        });
    }

    public dispose(): void {
        this.resolveSubscription.dispose();
    }

    private warnOnce(key: string, message: string): void {
        if (this.loggedRejections.has(key)) {
            return;
        }
        this.loggedRejections.add(key);
        this.outputChannel.appendLine(message);
    }

    private getExtraArgs(): string[] {
        const inspection = vscode.workspace.getConfiguration('betteralign').inspect<unknown>('extraArgs');
        // Untrusted workspaces: strip workspace/folder layers; keep user + default.
        const trusted = vscode.workspace.isTrusted;
        const layered = trusted
            ? inspection?.workspaceFolderValue ?? inspection?.workspaceValue ?? inspection?.globalValue ?? inspection?.defaultValue
            : inspection?.globalValue ?? inspection?.defaultValue;
        if (!trusted && (inspection?.workspaceFolderValue !== undefined || inspection?.workspaceValue !== undefined)) {
            this.warnOnce('untrusted', 'Workspace is untrusted; ignoring workspace-scoped betteralign.extraArgs.');
        }
        // Guard against hand-edited non-array settings.
        const raw: string[] = Array.isArray(layered)
            ? layered.filter((v): v is string => typeof v === 'string')
            : [];
        const safe: string[] = [];
        for (const arg of raw) {
            const normalized = arg.startsWith('--') ? arg.slice(1) : arg;
            const eqIdx = normalized.indexOf('=');
            const bareFlag = eqIdx === -1 ? normalized : normalized.slice(0, eqIdx);
            const kind = SAFE_FLAGS[bareFlag];
            if (!kind) {
                this.warnOnce(`drop:${arg}`, `Dropping disallowed betteralign extraArg: ${arg}`);
                continue;
            }
            if (eqIdx === -1) {
                if (kind !== 'bool') {
                    this.warnOnce(`missing:${arg}`, `Rejecting betteralign extraArg missing value (use ${bareFlag}=VALUE): ${arg}`);
                    continue;
                }
                safe.push(normalized);
            } else if (eqIdx === bareFlag.length && eqIdx < normalized.length - 1) {
                safe.push(normalized);
            } else {
                this.warnOnce(`malformed:${arg}`, `Rejecting malformed betteralign extraArg: ${arg}`);
            }
        }
        return safe;
    }

    public async run(filePath: string, signal?: AbortSignal): Promise<BetteralignResult | null> {
        if (signal?.aborted) {
            return null;
        }
        const bin = await this.binaryManager.getBinaryPath();
        if (!bin) {
            this.warnOnce('no-binary', 'Skipping betteralign run: binary not found. Invoke "Betteralign: Install Binary" to install it.');
            return null;
        }

        const args = ['-json', ...this.getExtraArgs(), filePath];
        const cwd = path.dirname(filePath);

        const release = await concurrencyGate.acquire();
        try {
            if (signal?.aborted) {
                return null;
            }
            const { stdout } = await this.execCommand(bin, args, cwd, signal);
            return this.tryParse(stdout);
        } catch (err) {
            const e = err as ExecError;
            // Abort is expected on close/deactivate; not an error.
            if (signal?.aborted || isAbortError(e)) {
                return null;
            }
            // -json should exit 0, but try stdout before declaring failure.
            const parsed = this.tryParse(e.stdout ?? '');
            if (parsed) {
                return parsed;
            }
            if (e.stderr) {
                this.outputChannel.appendLine(`betteralign error: ${e.stderr}`);
            } else if (e.message) {
                this.outputChannel.appendLine(`betteralign failed: ${e.message}`);
            }
            return null;
        } finally {
            release();
        }
    }

    public async apply(filePath: string): Promise<boolean> {
        const bin = await this.binaryManager.getBinaryPath();
        if (!bin) {
            vscode.window.showErrorMessage('betteralign binary not found.');
            return false;
        }

        // -json + -apply: exit 0 on clean analysis; non-zero means real failure (load/compile/IO).
        const args = ['-json', '-apply', ...this.getExtraArgs(), filePath];
        const cwd = path.dirname(filePath);

        const release = await concurrencyGate.acquire();
        try {
            await this.execCommand(bin, args, cwd);
            return true;
        } catch (err) {
            const e = err as ExecError;
            if (e.stderr) {
                this.outputChannel.appendLine(`betteralign apply error: ${e.stderr}`);
            } else if (e.message) {
                this.outputChannel.appendLine(`betteralign apply failed: ${e.message}`);
            }
            return false;
        } finally {
            release();
        }
    }

    private tryParse(raw: string): BetteralignResult | null {
        const trimmed = raw.trim();
        if (!trimmed || trimmed[0] !== '{') {
            return null;
        }
        try {
            return JSON.parse(trimmed) as BetteralignResult;
        } catch (parseErr) {
            this.outputChannel.appendLine(`Failed to parse betteralign JSON output: ${parseErr}`);
            return null;
        }
    }

    private execCommand(bin: string, args: string[], cwd: string, signal?: AbortSignal): Promise<{ stdout: string; stderr: string }> {
        return new Promise((resolve, reject) => {
            cp.execFile(bin, args, { cwd, maxBuffer: MAX_BUFFER_BYTES, timeout: EXEC_TIMEOUT_MS, signal }, (error, stdout, stderr) => {
                if (error) {
                    // Preserve .code/.stack by attaching stdio to the Error.
                    const augmented = error as ExecError;
                    augmented.stdout = stdout;
                    augmented.stderr = stderr;
                    reject(augmented);
                    return;
                }
                resolve({ stdout, stderr });
            });
        });
    }
}
