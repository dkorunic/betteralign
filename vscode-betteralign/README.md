# Betteralign VS Code Extension

VS Code extension that integrates [betteralign](https://github.com/dkorunic/betteralign) to automatically detect and fix suboptimal Go struct memory layouts.

## Features

- Runs `betteralign` on `.go` files when they are opened or saved.
- Highlights unoptimized structs with squiggly lines using the standard VS Code Problems pane.
- Optionally displays a prompt to quickly apply memory layout optimizations.

## Requirements

The extension requires Go to be installed and available on your `PATH`.
It installs `betteralign` using `go install github.com/dkorunic/betteralign/cmd/betteralign@latest` under the hood.

## Extension Settings

This extension contributes the following settings:

* `betteralign.enable`: Enable/disable this extension.
* `betteralign.runOnSave`: Enable/disable running betteralign on file save.
* `betteralign.applyPrompt`: When to show the "Apply fixes?" prompt (`onDiagnostics` (default), `never`).
* `betteralign.severity`: The VS Code diagnostic severity to use (`hint` (default, faint dotted underline), `information`, `warning`, `error`).
* `betteralign.debounceMs`: Delay in milliseconds between saving a file and running the analyzer. Bursts of saves collapse into one run. Default `300`, clamped to `0..5000`.
* `betteralign.binPath`: Custom path to the `betteralign` binary if not in your `$PATH` or `$GOPATH/bin`. Machine-scoped: cannot be overridden from a workspace, for safety.
* `betteralign.extraArgs`: Extra CLI flags to pass to the tool. Only these are accepted: `-test_files`, `-generated_files`, `-opt_in` (boolean); `-exclude_dirs=...`, `-exclude_files=...` (use the `-flag=value` form). Anything else is silently dropped with a log line in the Betteralign output channel.
