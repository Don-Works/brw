BINDIR ?= $(HOME)/.local/bin
DATADIR ?= $(HOME)/.local/share/agent-browser
MAC_APPDIR ?= $(HOME)/Library/Application Support/agent-browser

.PHONY: build test install install-mac install-target package-darwin-arm64

# Build + deploy (daemon binary + bridge extension) to a target machine.
#   make install-target TARGET=local
#   make install-target TARGET=maxrevitt@max-air
# Wraps scripts/install.sh; prints the one manual step (reload the Chrome
# bridge extension) it cannot do headlessly.
install-target:
	scripts/install.sh "$(TARGET)"

build:
	go build -o bin/agent-browserd ./cmd/browserd
	go build -o bin/browsercheck ./cmd/browsercheck
	go build -o bin/agent-browserctl ./cmd/agent-browserctl
	go build -o bin/agent-browser-devtools-mcp ./cmd/devtools-mcp

test:
	go test ./...

install: build
	mkdir -p "$(BINDIR)" "$(DATADIR)/extension" "$(DATADIR)/tests"
	cp bin/agent-browserd "$(BINDIR)/agent-browserd"
	cp bin/browsercheck "$(BINDIR)/browsercheck"
	cp bin/agent-browserctl "$(BINDIR)/agent-browserctl"
	cp bin/agent-browser-devtools-mcp "$(BINDIR)/agent-browser-devtools-mcp"
	cp -R extension/. "$(DATADIR)/extension/"
	cp -R tests/. "$(DATADIR)/tests/"

install-mac: build
	mkdir -p "$(MAC_APPDIR)/bin" "$(MAC_APPDIR)/extension" "$(MAC_APPDIR)/tests" "$(MAC_APPDIR)/config"
	cp bin/agent-browserd "$(MAC_APPDIR)/bin/agent-browserd"
	cp bin/browsercheck "$(MAC_APPDIR)/bin/browsercheck"
	cp bin/agent-browserctl "$(MAC_APPDIR)/bin/agent-browserctl"
	cp bin/agent-browser-devtools-mcp "$(MAC_APPDIR)/bin/agent-browser-devtools-mcp"
	cp -R extension/. "$(MAC_APPDIR)/extension/"
	cp -R tests/. "$(MAC_APPDIR)/tests/"
	if [ -f ../.mcplexer/config/browser-profiles.json ]; then cp ../.mcplexer/config/browser-profiles.json "$(MAC_APPDIR)/config/browser-profiles.json"; fi

package-darwin-arm64:
	GOOS=darwin GOARCH=arm64 go build -o bin/agent-browserd-darwin-arm64 ./cmd/browserd
	GOOS=darwin GOARCH=arm64 go build -o bin/browsercheck-darwin-arm64 ./cmd/browsercheck
	GOOS=darwin GOARCH=arm64 go build -o bin/agent-browserctl-darwin-arm64 ./cmd/agent-browserctl
	GOOS=darwin GOARCH=arm64 go build -o bin/agent-browser-devtools-mcp-darwin-arm64 ./cmd/devtools-mcp
