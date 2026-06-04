EXTENSION_ID := sujit.sujit-sqlite-tools-0.1.0
EXTENSION_SRC := vscode-extension
EXTENSION_DIR ?= $(HOME)/.vscode/extensions/$(EXTENSION_ID)
BRIDGE_BIN := bin/sqlite-vscode-bridge
CODE ?= code
VSCODE_APP ?= Visual Studio Code
GOCACHE ?= $(CURDIR)/.gocache

.PHONY: help build-bridge install-extension reload-vscode install-and-reload test-extension clean-extension

help:
	@echo "Targets:"
	@echo "  build-bridge        Build the VS Code bridge binary"
	@echo "  install-extension   Copy the VS Code extension into ~/.vscode/extensions"
	@echo "  reload-vscode       Ask VS Code to reload the active window"
	@echo "  install-and-reload  Install extension and reload VS Code"
	@echo "  test-extension      Validate bridge, manifest, and extension JavaScript"
	@echo "  clean-extension     Remove the installed extension copy"

build-bridge:
	@mkdir -p bin
	GOCACHE="$(GOCACHE)" go build -o $(BRIDGE_BIN) ./cmd/sqlite-vscode-bridge

install-extension:
	@mkdir -p "$(EXTENSION_DIR)"
	@cp -R "$(EXTENSION_SRC)/." "$(EXTENSION_DIR)/"
	@echo "Installed $(EXTENSION_ID) to $(EXTENSION_DIR)"

reload-vscode:
	@if command -v osascript >/dev/null 2>&1; then \
		osascript \
			-e 'tell application "$(VSCODE_APP)" to activate' \
			-e 'delay 0.2' \
			-e 'tell application "System Events" to keystroke "p" using {command down, shift down}' \
			-e 'delay 0.2' \
			-e 'tell application "System Events" to keystroke "Developer: Reload Window"' \
			-e 'delay 0.1' \
			-e 'tell application "System Events" to key code 36'; \
	else \
		$(CODE) --reuse-window .; \
		echo "Reload VS Code from the Command Palette: Developer: Reload Window"; \
	fi

install-and-reload: install-extension reload-vscode

test-extension:
	GOCACHE="$(GOCACHE)" go test ./cmd/sqlite-vscode-bridge
	node --check vscode-extension/src/extension.js
	node -e "JSON.parse(require('fs').readFileSync('vscode-extension/package.json','utf8')); console.log('package ok')"

clean-extension:
	@rm -rf "$(EXTENSION_DIR)"
	@echo "Removed $(EXTENSION_DIR)"
