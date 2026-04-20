# Changelog

All notable changes to the Better Align for VS Code extension are documented here.

## [0.1.0] - Initial release

- Runs `betteralign` on Go files when saved and highlights suboptimal struct layouts via the Problems panel.
- Commands: `Betteralign: Run on current file`, `Betteralign: Apply fixes to current file`, `Betteralign: Install Binary`.
- Settings: `enable`, `runOnSave`, `applyPrompt`, `severity`, `debounceMs`, `binPath` (machine-scoped), `extraArgs` (safelisted flags only).
- Coalesces concurrent analyses per document ("latest-document-wins") and caps the number of concurrent `betteralign` processes.
- Verifies installed binary via `-V=full` handshake after `go install`.
