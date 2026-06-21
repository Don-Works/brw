BINDIR ?= $(HOME)/.local/bin
DATADIR ?= $(HOME)/.local/share/brw
MAC_APPDIR ?= $(HOME)/Library/Application Support/brw

EXTENSION_ID = amocjcgddnoakjijfggdpnefdnboilpe

.PHONY: build test install install-mac install-extension package-darwin-arm64

build:
	go build -o bin/brwd ./cmd/brwd
	go build -o bin/brwcheck ./cmd/brwcheck
	go build -o bin/brwctl ./cmd/brwctl
	go build -o bin/brw-devtools-mcp ./cmd/brw-devtools-mcp

test:
	go test ./...

install: build
	mkdir -p "$(BINDIR)" "$(DATADIR)/extension" "$(DATADIR)/tests"
	cp bin/brwd "$(BINDIR)/brwd"
	cp bin/brwcheck "$(BINDIR)/brwcheck"
	cp bin/brwctl "$(BINDIR)/brwctl"
	cp bin/brw-devtools-mcp "$(BINDIR)/brw-devtools-mcp"
	cp -R extension/. "$(DATADIR)/extension/"
	cp -R tests/. "$(DATADIR)/tests/"
	@# On Apple Silicon, copying a Go binary invalidates its ad-hoc code
	@# signature and the OS then SIGKILLs it ("Killed: 9"); re-sign the copies.
	@if [ "$$(uname)" = "Darwin" ] && command -v codesign >/dev/null 2>&1; then \
		codesign --force --sign - "$(BINDIR)/brwd" "$(BINDIR)/brwcheck" "$(BINDIR)/brwctl" "$(BINDIR)/brw-devtools-mcp"; \
	fi

install-mac: build
	mkdir -p "$(MAC_APPDIR)/bin" "$(MAC_APPDIR)/extension" "$(MAC_APPDIR)/tests" "$(MAC_APPDIR)/config"
	cp bin/brwd "$(MAC_APPDIR)/bin/brwd"
	cp bin/brwcheck "$(MAC_APPDIR)/bin/brwcheck"
	cp bin/brwctl "$(MAC_APPDIR)/bin/brwctl"
	cp bin/brw-devtools-mcp "$(MAC_APPDIR)/bin/brw-devtools-mcp"
	cp -R extension/. "$(MAC_APPDIR)/extension/"
	cp -R tests/. "$(MAC_APPDIR)/tests/"
	if [ -f "$(HOME)/.config/brw/browser-profiles.json" ]; then cp "$(HOME)/.config/brw/browser-profiles.json" "$(MAC_APPDIR)/config/browser-profiles.json"; fi
	@# Re-sign: copying a Go binary on Apple Silicon breaks its ad-hoc signature
	@# and the OS SIGKILLs it on launch ("Killed: 9").
	@if command -v codesign >/dev/null 2>&1; then \
		codesign --force --sign - "$(MAC_APPDIR)/bin/brwd" "$(MAC_APPDIR)/bin/brwcheck" "$(MAC_APPDIR)/bin/brwctl" "$(MAC_APPDIR)/bin/brw-devtools-mcp"; \
	fi

# Streamline the one-time load-unpacked install of the brw Chrome extension.
# Prints the exact folder + stable id, then (best-effort on macOS) opens
# chrome://extensions and reveals the folder in Finder so you can pick it.
install-extension:
	@echo ""
	@echo "  Load the brw Chrome extension (one-time, Developer Mode):"
	@echo ""
	@echo "    1. In chrome://extensions, enable Developer mode (top right)."
	@echo "    2. Click 'Load unpacked'."
	@echo "    3. Select this folder:"
	@echo ""
	@echo "         $(CURDIR)/extension"
	@echo ""
	@echo "    The extension id will be: $(EXTENSION_ID)"
	@echo "    (matches DefaultBridgeExtensionID, so the bridge trusts it with no config)"
	@echo ""
	-@open -a "Google Chrome" "chrome://extensions" 2>/dev/null || true
	-@open -R "$(CURDIR)/extension" 2>/dev/null || true

package-darwin-arm64:
	GOOS=darwin GOARCH=arm64 go build -o bin/brwd-darwin-arm64 ./cmd/brwd
	GOOS=darwin GOARCH=arm64 go build -o bin/brwcheck-darwin-arm64 ./cmd/brwcheck
	GOOS=darwin GOARCH=arm64 go build -o bin/brwctl-darwin-arm64 ./cmd/brwctl
	GOOS=darwin GOARCH=arm64 go build -o bin/brw-devtools-mcp-darwin-arm64 ./cmd/brw-devtools-mcp
