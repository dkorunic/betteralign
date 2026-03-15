import * as vscode from 'vscode';
import * as cp from 'child_process';
import * as fs from 'fs';
import * as path from 'path';
import * as util from 'util';

const execFile = util.promisify(cp.execFile);

export class BinaryManager {
    private outputChannel: vscode.OutputChannel;
    private cachedBinaryPath: string | undefined;

    constructor(outputChannel: vscode.OutputChannel) {
        this.outputChannel = outputChannel;
    }

    public async getBinaryPath(): Promise<string | undefined> {
        const config = vscode.workspace.getConfiguration('betteralign');
        const customPath = config.get<string>('binPath');

        if (customPath) {
            if (fs.existsSync(customPath)) {
                return customPath;
            }
            this.outputChannel.appendLine(`Custom betteralign.binPath '${customPath}' does not exist.`);
        }

        if (this.cachedBinaryPath && fs.existsSync(this.cachedBinaryPath)) {
            return this.cachedBinaryPath;
        }

        // Check GOPATH
        const goPath = process.env.GOPATH || await this.getGoEnv('GOPATH');
        if (goPath) {
            const ext = process.platform === 'win32' ? '.exe' : '';
            const binPath = path.join(goPath, 'bin', `betteralign${ext}`);
            if (fs.existsSync(binPath)) {
                this.cachedBinaryPath = binPath;
                return binPath;
            }
            
            // support multiple GOPATH entries
            const paths = goPath.split(path.delimiter);
            for (const p of paths) {
                const b = path.join(p, 'bin', `betteralign${ext}`);
                if (fs.existsSync(b)) {
                    this.cachedBinaryPath = b;
                    return b;
                }
            }
        }

        // Try to run from PATH
        try {
            await execFile('betteralign', ['-V']);
            this.cachedBinaryPath = 'betteralign';
            return 'betteralign';
        } catch (e) {
            // Not in PATH
        }

        return undefined;
    }

    public async installBinary(): Promise<void> {
        return vscode.window.withProgress(
            {
                location: vscode.ProgressLocation.Notification,
                title: 'Installing betteralign...',
                cancellable: false
            },
            async (progress) => {
                try {
                    this.outputChannel.appendLine('Running: go install github.com/dkorunic/betteralign/cmd/betteralign@latest');
                    
                    const { stdout, stderr } = await execFile('go', ['install', 'github.com/dkorunic/betteralign/cmd/betteralign@latest']);
                    if (stdout) this.outputChannel.appendLine(stdout);
                    if (stderr) this.outputChannel.appendLine(stderr);
                    
                    this.outputChannel.appendLine('Successfully installed betteralign.');
                    vscode.window.showInformationMessage('Successfully installed betteralign format tool.');
                    this.cachedBinaryPath = undefined; // reset cache to find it again
                } catch (err: any) {
                    this.outputChannel.appendLine(`Failed to install betteralign: ${err.message}`);
                    vscode.window.showErrorMessage(`Failed to install betteralign. Make sure Go is installed and on your PATH. Check output for details.`);
                }
            }
        );
    }

    private async getGoEnv(key: string): Promise<string | undefined> {
        try {
            const { stdout } = await execFile('go', ['env', key]);
            return stdout.trim();
        } catch (err) {
            return undefined;
        }
    }
}
