class Brw < Formula
  desc "Semantic browser control for agents"
  homepage "https://brw.donworks.co.uk/"
  version "@BRW_VERSION@"
  license "AGPL-3.0-only"

  on_macos do
    on_arm do
      url "https://github.com/Don-Works/brw/releases/download/v@BRW_VERSION@/brw_@BRW_VERSION@_darwin_arm64.tar.gz"
      sha256 "@SHA256_DARWIN_ARM64@"
    end
    on_intel do
      url "https://github.com/Don-Works/brw/releases/download/v@BRW_VERSION@/brw_@BRW_VERSION@_darwin_amd64.tar.gz"
      sha256 "@SHA256_DARWIN_AMD64@"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/Don-Works/brw/releases/download/v@BRW_VERSION@/brw_@BRW_VERSION@_linux_arm64.tar.gz"
      sha256 "@SHA256_LINUX_ARM64@"
    end
    on_intel do
      url "https://github.com/Don-Works/brw/releases/download/v@BRW_VERSION@/brw_@BRW_VERSION@_linux_amd64.tar.gz"
      sha256 "@SHA256_LINUX_AMD64@"
    end
  end

  livecheck do
    url :stable
    strategy :github_latest
  end

  def install
    prefix.install Dir["*"]
  end

  def caveats
    <<~EOS
      Finish setup (writes a profile policy under the platform user config
      directory and registers the MCP server; nothing here needs sudo):
        brwctl setup

      This formula's app dir is the Homebrew prefix, so point doctor at it:
        brwctl doctor --app-dir "#{opt_prefix}"

      The Chrome extension ships at:
    EOS
  end

  test do
    assert_match "brwctl", shell_output("#{bin}/brwctl 2>&1", 2)
    assert_match "brw <verb>", shell_output("#{bin}/brw 2>&1", 2)
  end
end
