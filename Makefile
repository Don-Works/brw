BINDIR ?= $(HOME)/.local/bin
DATADIR ?= $(HOME)/.local/share/brw
MAC_APPDIR ?= $(HOME)/Library/Application Support/brw

.PHONY: build test install install-mac package-darwin-arm64

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

install-mac: build
	mkdir -p "$(MAC_APPDIR)/bin" "$(MAC_APPDIR)/extension" "$(MAC_APPDIR)/tests" "$(MAC_APPDIR)/config"
	cp bin/brwd "$(MAC_APPDIR)/bin/brwd"
	cp bin/brwcheck "$(MAC_APPDIR)/bin/brwcheck"
	cp bin/brwctl "$(MAC_APPDIR)/bin/brwctl"
	cp bin/brw-devtools-mcp "$(MAC_APPDIR)/bin/brw-devtools-mcp"
	cp -R extension/. "$(MAC_APPDIR)/extension/"
	cp -R tests/. "$(MAC_APPDIR)/tests/"
	if [ -f "$(HOME)/.config/brw/browser-profiles.json" ]; then cp "$(HOME)/.config/brw/browser-profiles.json" "$(MAC_APPDIR)/config/browser-profiles.json"; fi

package-darwin-arm64:
	GOOS=darwin GOARCH=arm64 go build -o bin/brwd-darwin-arm64 ./cmd/brwd
	GOOS=darwin GOARCH=arm64 go build -o bin/brwcheck-darwin-arm64 ./cmd/brwcheck
	GOOS=darwin GOARCH=arm64 go build -o bin/brwctl-darwin-arm64 ./cmd/brwctl
	GOOS=darwin GOARCH=arm64 go build -o bin/brw-devtools-mcp-darwin-arm64 ./cmd/brw-devtools-mcp
