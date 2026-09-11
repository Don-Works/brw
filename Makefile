BINDIR ?= $(HOME)/.local/bin
DATADIR ?= $(HOME)/.local/share/brw
MAC_APPDIR ?= $(HOME)/Library/Application Support/brw

EXTENSION_ID = amocjcgddnoakjijfggdpnefdnboilpe

VERSION ?= $(shell git describe --tags --always --dirty | sed 's/^v//')
GOARCH ?= $(shell go env GOARCH)

# Inject the resolved version into the MCP serverInfo.version so the version an
# agent sees over MCP always matches the binary, instead of a hand-edited constant.
GO_LDFLAGS ?= -X github.com/Don-Works/brw/internal/mcp.Version=$(VERSION)

TARBALL_OS ?= $(shell go env GOOS)
HOMEBREW_TAP ?= Don-Works/homebrew-tap

.PHONY: build test test-extension test-functional install install-mac install-agent-skills sync-installed-extensions install-extension package-web-store package-darwin-arm64 package-linux package-macos package-tarball package-tarballs homebrew-formula

build:
	go build -ldflags "$(GO_LDFLAGS)" -o bin/brwd ./cmd/brwd
	go build -ldflags "$(GO_LDFLAGS)" -o bin/brwcheck ./cmd/brwcheck
	go build -ldflags "$(GO_LDFLAGS)" -o bin/brwctl ./cmd/brwctl
	go build -ldflags "$(GO_LDFLAGS)" -o bin/brw-devtools-mcp ./cmd/brw-devtools-mcp

test: test-extension
	# browser, snapshot, and readability tests each launch real headless Chrome.
	# Serialize package test binaries so CI does not run dozens of Chrome roots at
	# once and turn the Manager's intentional 20s operation timeout into a load race.
	go test -p=1 ./...

# Extension service-worker regression tests (run the real service_worker.js in a
# vm with a mocked chrome API). Skipped — not failed — when node is unavailable.
test-extension:
	@if command -v node >/dev/null 2>&1; then \
		node extension/tab_resolution_test.mjs; \
	else \
		echo "node not found; skipping extension tests"; \
	fi

# End-to-end gate against a real headless browser and the deterministic local
# fixture suite. Artifacts/profile live in a disposable directory outside git.
test-functional: build
	./scripts/test-functional.sh

# The binaries live under DATADIR and reach PATH through BINDIR symlinks, the
# same shape scripts/install.sh produces. brwctl doctor's default --app-dir on
# Linux is DATADIR and it looks for bin/brwd there, so binaries installed only
# into BINDIR would leave doctor reporting a broken install.
install: build
	mkdir -p "$(BINDIR)" "$(DATADIR)/bin" "$(DATADIR)/extension" "$(DATADIR)/tests" "$(DATADIR)/skills/brw"
	cp bin/brwd "$(DATADIR)/bin/brwd"
	cp bin/brwcheck "$(DATADIR)/bin/brwcheck"
	cp bin/brwctl "$(DATADIR)/bin/brwctl"
	cp bin/brw-devtools-mcp "$(DATADIR)/bin/brw-devtools-mcp"
	cp -R extension/. "$(DATADIR)/extension/"
	cp -R tests/. "$(DATADIR)/tests/"
	cp -R skills/brw/. "$(DATADIR)/skills/brw/"
	@# On Apple Silicon, copying a Go binary invalidates its ad-hoc code
	@# signature and the OS then SIGKILLs it ("Killed: 9"); re-sign the copies.
	@if [ "$$(uname)" = "Darwin" ] && command -v codesign >/dev/null 2>&1; then \
		codesign --force --sign - "$(DATADIR)/bin/brwd" "$(DATADIR)/bin/brwcheck" "$(DATADIR)/bin/brwctl" "$(DATADIR)/bin/brw-devtools-mcp"; \
	fi
	ln -sf "$(DATADIR)/bin/brwd" "$(BINDIR)/brwd"
	ln -sf "$(DATADIR)/bin/brwcheck" "$(BINDIR)/brwcheck"
	ln -sf "$(DATADIR)/bin/brwctl" "$(BINDIR)/brwctl"
	ln -sf "$(DATADIR)/bin/brw-devtools-mcp" "$(BINDIR)/brw-devtools-mcp"

install-mac: build
	mkdir -p "$(MAC_APPDIR)/bin" "$(MAC_APPDIR)/extension" "$(MAC_APPDIR)/tests" "$(MAC_APPDIR)/skills/brw" "$(MAC_APPDIR)/config"
	cp bin/brwd "$(MAC_APPDIR)/bin/brwd"
	cp bin/brwcheck "$(MAC_APPDIR)/bin/brwcheck"
	cp bin/brwctl "$(MAC_APPDIR)/bin/brwctl"
	cp bin/brw-devtools-mcp "$(MAC_APPDIR)/bin/brw-devtools-mcp"
	cp -R extension/. "$(MAC_APPDIR)/extension/"
	cp -R tests/. "$(MAC_APPDIR)/tests/"
	cp -R skills/brw/. "$(MAC_APPDIR)/skills/brw/"
	@$(MAKE) --no-print-directory sync-installed-extensions
	if [ -f "$(HOME)/.config/brw/browser-profiles.json" ]; then cp "$(HOME)/.config/brw/browser-profiles.json" "$(MAC_APPDIR)/config/browser-profiles.json"; fi
	@# Re-sign: copying a Go binary on Apple Silicon breaks its ad-hoc signature
	@# and the OS SIGKILLs it on launch ("Killed: 9").
	@if command -v codesign >/dev/null 2>&1; then \
		codesign --force --sign - "$(MAC_APPDIR)/bin/brwd" "$(MAC_APPDIR)/bin/brwcheck" "$(MAC_APPDIR)/bin/brwctl" "$(MAC_APPDIR)/bin/brw-devtools-mcp"; \
	fi

# Keep every existing per-profile unpacked extension payload current. rsync's
# exclusion preserves the local bridge endpoint/token file in each copy while
# --delete prevents removed source files from lingering as executable code.
sync-installed-extensions:
	@command -v rsync >/dev/null 2>&1 || { echo "rsync is required to sync installed extension copies" >&2; exit 1; }
	@for extdir in "$(MAC_APPDIR)/extension" "$(MAC_APPDIR)"/extension-*; do \
		if [ -d "$$extdir" ] && [ ! -L "$$extdir" ]; then \
			rsync -a --delete --exclude '/bridge-defaults.json' extension/ "$$extdir"/; \
		fi; \
	done

# Install the public operating skill where the common agent harnesses discover
# it globally. Operational recipes remain in the private provider, never here.
install-agent-skills:
	mkdir -p "$(HOME)/.agents/skills/brw" "$(HOME)/.codex/skills/brw"
	rsync -a --delete skills/brw/ "$(HOME)/.agents/skills/brw"/
	rsync -a --delete skills/brw/ "$(HOME)/.codex/skills/brw"/

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

package-web-store:
	scripts/package-web-store.sh dist/web-store

package-darwin-arm64:
	GOOS=darwin GOARCH=arm64 go build -ldflags "$(GO_LDFLAGS)" -o bin/brwd-darwin-arm64 ./cmd/brwd
	GOOS=darwin GOARCH=arm64 go build -ldflags "$(GO_LDFLAGS)" -o bin/brwcheck-darwin-arm64 ./cmd/brwcheck
	GOOS=darwin GOARCH=arm64 go build -ldflags "$(GO_LDFLAGS)" -o bin/brwctl-darwin-arm64 ./cmd/brwctl
	GOOS=darwin GOARCH=arm64 go build -ldflags "$(GO_LDFLAGS)" -o bin/brw-devtools-mcp-darwin-arm64 ./cmd/brw-devtools-mcp

package-linux:
	scripts/package-linux.sh "$(VERSION)" "$(GOARCH)" dist/release

package-macos:
	scripts/package-macos.sh "$(VERSION)" dist/release

# The sudo-free install path: relocatable archives for scripts/install.sh and
# the Homebrew formula. One OS/arch, defaulting to the host.
package-tarball:
	scripts/package-tarball.sh "$(VERSION)" "$(TARBALL_OS)" "$(GOARCH)" dist/release

# darwin/arm64 is built last so a macOS host leaves a natively signable archive
# in place; cross-built darwin binaries cannot be ad-hoc signed off a Mac.
package-tarballs:
	scripts/package-tarball.sh "$(VERSION)" linux amd64 dist/release
	scripts/package-tarball.sh "$(VERSION)" linux arm64 dist/release
	scripts/package-tarball.sh "$(VERSION)" darwin amd64 dist/release
	scripts/package-tarball.sh "$(VERSION)" darwin arm64 dist/release

# Print the tap formula for VERSION with the four release sha256 sums filled in,
# so a release bumps the tap mechanically rather than by hand. Reads the sums
# from dist/release when the archives are local, otherwise from the published
# release. Redirect into the tap checkout's Formula/brw.rb.
homebrew-formula:
	@packaging/homebrew/render-formula.sh "$(VERSION)"
