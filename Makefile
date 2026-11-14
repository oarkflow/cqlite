EXTENSION_ID := sujit.cqlite-tools-0.1.37
EXTENSION_SRC := vscode-extension
EXTENSION_DIR ?= $(HOME)/.vscode/extensions/$(EXTENSION_ID)
BRIDGE_BIN := bin/cqlite-vscode-bridge
CODE ?= code
VSCODE_APP ?= Visual Studio Code
GOCACHE ?= $(CURDIR)/.gocache
VSCE ?= vsce
VSCE_TARGET ?= $(shell node -e "process.stdout.write(process.platform + '-' + ({x64:'x64',arm64:'arm64',ia32:'ia32'}[process.arch] || process.arch))")

.PHONY: help build-bridge install-extension package-extension reload-vscode install-and-reload test-extension clean-extension

help:
	@echo "Targets:"
	@echo "  build-bridge        Build the VS Code bridge binary"
	@echo "  install-extension   Copy the VS Code extension into ~/.vscode/extensions"
	@echo "  package-extension   Build a platform-specific VSIX with the current bridge"
	@echo "  reload-vscode       Ask VS Code to reload the active window"
	@echo "  install-and-reload  Install extension and reload VS Code"
	@echo "  test-extension      Validate the bridge, manifest, webview SQL flow, and extension JavaScript"
	@echo "  clean-extension     Remove the installed extension copy"

build-bridge:
	@mkdir -p bin
	GOPROXY=off GOCACHE="$(GOCACHE)" go build -o $(BRIDGE_BIN) ./cmd/cqlite-vscode-bridge

install-extension: build-bridge
	@mkdir -p "$(EXTENSION_DIR)/bin"
	@cp -R "$(EXTENSION_SRC)/." "$(EXTENSION_DIR)/"
	@cp "$(BRIDGE_BIN)" "$(EXTENSION_DIR)/bin/cqlite-vscode-bridge"
	@echo "Installed $(EXTENSION_ID) to $(EXTENSION_DIR)"

package-extension: build-bridge
	@mkdir -p "$(EXTENSION_SRC)/bin"
	@cp "$(BRIDGE_BIN)" "$(EXTENSION_SRC)/bin/cqlite-vscode-bridge"
	cd "$(EXTENSION_SRC)" && $(VSCE) package --no-dependencies --target "$(VSCE_TARGET)"

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
	GOPROXY=off GOCACHE="$(GOCACHE)" go test ./cmd/cqlite-vscode-bridge
	GOPROXY=off GOCACHE="$(GOCACHE)" go build -o $(BRIDGE_BIN) ./cmd/cqlite-vscode-bridge
	node --check vscode-extension/src/extension.js
	node -e "JSON.parse(require('fs').readFileSync('vscode-extension/package.json','utf8')); console.log('package ok')"
	node vscode-extension/test/workbench-smoke.js

clean-extension:
	@rm -rf "$(EXTENSION_DIR)"
	@echo "Removed $(EXTENSION_DIR)"
