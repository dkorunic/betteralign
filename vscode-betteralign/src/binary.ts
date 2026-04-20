import * as vscode from 'vscode';
import * as cp from 'child_process';
import * as fs from 'fs';
import * as path from 'path';
import * as util from 'util';

const execFile = util.promisify(cp.execFile);

// Bounded probes so a wedged binary can't stall activation.
const VERIFY_TIMEOUT_MS = 5_000;
const GO_ENV_TIMEOUT_MS = 5_000;
// go install downloads + compiles; needs more headroom.
const INSTALL_TIMEOUT_MS = 300_000;
// Cap noisy compile-error output so stderr doesn't overflow the default buffer.
const INSTALL_MAX_BUFFER_BYTES = 16 * 1024 * 1024;

export class BinaryManager {
    private outputChannel: vscode.OutputChannel;
    private cachedBinaryPath: string | undefined;
    // Dedupes concurrent lookups racing on activation.
    private pendingLookup: Promise<string | undefined> | undefined;
    // Only successful lookups cached; misses retry so later installs are picked up.
    private cachedGoEnv = new Map<string, string>();
    // Lets consumers re-arm sticky warnings after a successful resolve.
    private readonly _onDidResolve = new vscode.EventEmitter<string>();
    public readonly onDidResolve: vscode.Event<string> = this._onDidResolve.event;

    constructor(outputChannel: vscode.OutputChannel) {
        this.outputChannel = outputChannel;
    }

    public dispose(): void {
        this._onDidResolve.dispose();
    }

    public getBinaryPath(): Promise<string | undefined> {
        if (this.pendingLookup) {
            return this.pendingLookup;
        }
        const p = this.resolveBinaryPath()
            .then((resolved) => {
                if (resolved) {
                    this._onDidResolve.fire(resolved);
                }
                return resolved;
            })
            .finally(() => {
                this.pendingLookup = undefined;
            });
        this.pendingLookup = p;
        return p;
    }

    private async resolveBinaryPath(): Promise<string | undefined> {
        const config = vscode.workspace.getConfiguration('betteralign');
        const customPath = config.get<string>('binPath');

        if (customPath) {
            // Require absolute file path; relative would resolve against per-call cwd.
            if (!path.isAbsolute(customPath)) {
                this.outputChannel.appendLine(`Custom betteralign.binPath '${customPath}' is not an absolute path; ignoring.`);
            } else if (await this.isFile(customPath)) {
                return customPath;
            } else {
                this.outputChannel.appendLine(`Custom betteralign.binPath '${customPath}' does not exist or is not a regular file.`);
            }
        }

        if (this.cachedBinaryPath && await this.exists(this.cachedBinaryPath)) {
            return this.cachedBinaryPath;
        }

        // GOPATH may be multi-entry, PATH-delimited.
        const envGoPath = process.env.GOPATH?.trim();
        const goPath = envGoPath || await this.getGoEnv('GOPATH');
        if (goPath) {
            const ext = process.platform === 'win32' ? '.exe' : '';
            for (const entry of goPath.split(path.delimiter)) {
                if (!entry) {
                    continue;
                }
                const candidate = path.join(entry, 'bin', `betteralign${ext}`);
                if (await this.exists(candidate)) {
                    this.cachedBinaryPath = candidate;
                    return candidate;
                }
            }
        }

        // In-process PATH walk avoids spawning `which`/`where`.
        const found = await this.findInPath();
        if (found) {
            this.cachedBinaryPath = found;
            return found;
        }

        return undefined;
    }

    public async installBinary(): Promise<boolean> {
        return vscode.window.withProgress(
            {
                location: vscode.ProgressLocation.Notification,
                title: 'Installing betteralign...',
                cancellable: true
            },
            async (_progress, token) => {
                try {
                    this.outputChannel.appendLine('Running: go install github.com/dkorunic/betteralign/cmd/betteralign@latest');

                    const { stdout, stderr, cancelled } = await this.runGoInstall(token);
                    if (stdout) {
                        this.outputChannel.appendLine(stdout);
                    }
                    if (stderr) {
                        this.outputChannel.appendLine(stderr);
                    }
                    if (cancelled) {
                        this.outputChannel.appendLine('Install cancelled by user.');
                        return false;
                    }

                    this.cachedBinaryPath = undefined;
                    // GOPATH may have changed since activation; force re-query.
                    this.cachedGoEnv.clear();
                    const newPath = await this.getBinaryPath();
                    if (!newPath) {
                        this.outputChannel.appendLine('Install reported success but betteralign could not be located.');
                        vscode.window.showErrorMessage('betteralign installed but was not found on PATH or GOPATH/bin. Check Output → Betteralign.');
                        return false;
                    }
                    if (!await this.verifyBinary(newPath)) {
                        this.outputChannel.appendLine(`Binary at ${newPath} failed version handshake.`);
                        vscode.window.showErrorMessage('Installed binary did not respond as betteralign. See Output → Betteralign.');
                        return false;
                    }
                    this.outputChannel.appendLine(`Successfully installed betteralign at ${newPath}.`);
                    vscode.window.showInformationMessage('Successfully installed betteralign.');
                    return true;
                } catch (err) {
                    const msg = err instanceof Error ? err.message : String(err);
                    this.outputChannel.appendLine(`Failed to install betteralign: ${msg}`);
                    vscode.window.showErrorMessage('Failed to install betteralign. Make sure Go is installed and on your PATH. Check Output → Betteralign.');
                    return false;
                }
            }
        );
    }

    /** Cancellable `go install` — kills whole process tree (go spawns build/compile/link). */
    private runGoInstall(token: vscode.CancellationToken): Promise<{ stdout: string; stderr: string; cancelled: boolean }> {
        return new Promise((resolve, reject) => {
            const cancellation = { cancelled: false };
            // spawn (not execFile) + detached so POSIX kill -pgroup works.
            const child = cp.spawn(
                'go',
                ['install', 'github.com/dkorunic/betteralign/cmd/betteralign@latest'],
                {
                    detached: process.platform !== 'win32',
                    stdio: ['ignore', 'pipe', 'pipe'],
                }
            );
            let stdoutBuf = '';
            let stderrBuf = '';
            let truncated = false;
            // Idempotent: prevents noisy ESRCH when timeout + cancel fire together.
            let killed = false;
            const killOnce = () => {
                if (killed) {
                    return;
                }
                killed = true;
                this.killProcessTree(child);
            };
            const appendCapped = (buf: string, chunk: string): string => {
                if (buf.length + chunk.length > INSTALL_MAX_BUFFER_BYTES) {
                    truncated = true;
                    return (buf + chunk).slice(0, INSTALL_MAX_BUFFER_BYTES);
                }
                return buf + chunk;
            };
            child.stdout?.setEncoding('utf8');
            child.stderr?.setEncoding('utf8');
            child.stdout?.on('data', (chunk: string) => { stdoutBuf = appendCapped(stdoutBuf, chunk); });
            child.stderr?.on('data', (chunk: string) => { stderrBuf = appendCapped(stderrBuf, chunk); });
            // spawn has no built-in timeout.
            const timeoutHandle = setTimeout(() => {
                this.outputChannel.appendLine(`go install timed out after ${INSTALL_TIMEOUT_MS} ms; killing process tree.`);
                killOnce();
            }, INSTALL_TIMEOUT_MS);
            const disposable = token.onCancellationRequested(() => {
                cancellation.cancelled = true;
                killOnce();
            });
            const cleanup = () => {
                clearTimeout(timeoutHandle);
                disposable.dispose();
            };
            // Node may emit both 'error' and 'close' on ENOENT; cleanup is idempotent, Promise one-shot.
            child.once('error', (err) => {
                cleanup();
                reject(err);
            });
            child.once('close', (code, signal) => {
                cleanup();
                if (truncated) {
                    stderrBuf += `\n[betteralign: output truncated at ${INSTALL_MAX_BUFFER_BYTES} bytes]`;
                }
                if (cancellation.cancelled) {
                    resolve({ stdout: stdoutBuf, stderr: stderrBuf, cancelled: true });
                    return;
                }
                if (code !== 0) {
                    reject(new Error(`go install exited with code ${code}${signal ? ` (signal ${signal})` : ''}`));
                    return;
                }
                resolve({ stdout: stdoutBuf, stderr: stderrBuf, cancelled: false });
            });
        });
    }

    /** Kills child + descendants. Best-effort; swallows errors from already-exited targets. */
    private killProcessTree(child: cp.ChildProcess): void {
        if (child.pid === undefined) {
            return;
        }
        if (process.platform === 'win32') {
            cp.execFile('taskkill', ['/PID', String(child.pid), '/T', '/F'], () => { /* best-effort */ });
            return;
        }
        try {
            process.kill(-child.pid, 'SIGTERM');
        } catch {
            // Already exited.
        }
    }

    private async exists(p: string): Promise<boolean> {
        try {
            await fs.promises.access(p, fs.constants.F_OK);
            return true;
        } catch {
            return false;
        }
    }

    /** Like `exists` but rejects directories; used for `binPath`. */
    private async isFile(p: string): Promise<boolean> {
        try {
            const st = await fs.promises.stat(p);
            return st.isFile();
        } catch {
            return false;
        }
    }

    /** In-process `which` for betteralign; skips relative or `..`-bearing PATH entries. */
    private async findInPath(): Promise<string | undefined> {
        const envPath = process.env.PATH;
        if (!envPath) {
            return undefined;
        }
        // go install only emits .exe on Windows; skipping PATHEXT saves syscalls.
        const ext = process.platform === 'win32' ? '.exe' : '';
        const BIN_NAME = 'betteralign';
        for (const rawDir of envPath.split(path.delimiter)) {
            if (!rawDir || !path.isAbsolute(rawDir)) {
                continue;
            }
            if (rawDir.split(path.sep).includes('..')) {
                continue;
            }
            // Safe: hardcoded name + validated absolute dir — documented $PATH lookup.
            // nosemgrep: javascript.lang.security.audit.path-traversal.path-join-resolve-traversal
            const candidate = path.join(rawDir, BIN_NAME + ext);
            if (await this.exists(candidate)) {
                return candidate;
            }
        }
        return undefined;
    }

    /** Confirms binary identifies as betteralign via singlechecker's `-V=full`. */
    private async verifyBinary(pathToBin: string): Promise<boolean> {
        try {
            const { stdout, stderr } = await execFile(pathToBin, ['-V=full'], { timeout: VERIFY_TIMEOUT_MS });
            const text = `${stdout}\n${stderr}`;
            return /betteralign/i.test(text);
        } catch {
            return false;
        }
    }

    private async getGoEnv(key: string): Promise<string | undefined> {
        const cached = this.cachedGoEnv.get(key);
        if (cached !== undefined) {
            return cached;
        }
        try {
            const { stdout } = await execFile('go', ['env', key], { timeout: GO_ENV_TIMEOUT_MS });
            const value = stdout.trim();
            if (value) {
                this.cachedGoEnv.set(key, value);
                return value;
            }
            return undefined;
        } catch {
            return undefined;
        }
    }
}
