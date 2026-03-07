# Betteralign VS Code Extension

VS Code extension that integrates [betteralign](https://github.com/dkorunic/betteralign) to automatically detect and fix suboptimal Go struct memory layouts.

## Features

- Runs `betteralign` on `.go` files when saved.
- Highlights unoptimized structs with squiggly lines using the standard VS Code Problems pane.
- Optionally displays a prompt to quickly apply memory layout optimizations.

## Requirements

The extension requires Go to be installed and available on your `PATH`.
It installs `betteralign` using `go install github.com/dkorunic/betteralign/cmd/betteralign@latest` under the hood.

## Extension Settings

This extension contributes the following settings:

* `betteralign.enable`: Enable/disable this extension.
* `betteralign.runOnSave`: Enable/disable running betteralign on file save.
* `betteralign.applyPrompt`: When to show the "Apply fixes?" prompt (`onDiagnostics` (default), `always`, `never`).
* `betteralign.severity`: The VS Code diagnostic severity to use (`information` (default blue squiggly), `warning`, `hint`, `error`).
* `betteralign.binPath`: Custom path to the `betteralign` binary if not in your standard `GOPATH`.
* `betteralign.extraArgs`: Array of extra arguments to pass to the tool (e.g. `["-test_files"]`).
