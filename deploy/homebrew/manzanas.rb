# manzanas — client CLI for manzanasd. Canonical copy; mirrored to the
# BariBariGood/homebrew-tap repo (Formula/manzanas.rb) — keep both in sync
# and bump tag/revision together on each release.
#
class Manzanas < Formula
  desc "Client CLI for manzanasd, the multi-agent iOS simulator fleet daemon"
  homepage "https://github.com/BariBariGood/manzanas"
  url "https://github.com/BariBariGood/manzanas.git",
      tag:      "v0.4.0",
      revision: "15c4b4ec6ea730c3288abc048d3e926ab8a51899"
  license "MIT"
  head "https://github.com/BariBariGood/manzanas.git", branch: "main"

  depends_on "go" => :build

  def install
    system "go", "build", *std_go_args(ldflags: "-s -w -X main.version=#{version}"), "./cmd/manzanas"
  end

  test do
    assert_match "manzanas #{version}", shell_output("#{bin}/manzanas --version")
  end
end
