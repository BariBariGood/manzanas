# manzanas — client CLI for manzanasd. Canonical copy; mirrored to the
# BariBariGood/homebrew-tap repo (Formula/manzanas.rb) — keep both in sync
# and bump tag/revision together on each release.
#
class Manzanas < Formula
  desc "Client CLI for manzanasd, the multi-agent iOS simulator fleet daemon"
  homepage "https://github.com/BariBariGood/manzanas"
  url "https://github.com/BariBariGood/manzanas.git",
      tag:      "v0.5.0",
      revision: "c6ef1d4792e4d85e62abddbe0c5096e086bc25a2"
  license "MIT"
  head "https://github.com/BariBariGood/manzanas.git", branch: "main"

  depends_on "go" => :build

  def install
    system "go", "build", *std_go_args(ldflags: "-s -w -X github.com/BariBariGood/manzanas/internal/buildinfo.Version=#{version}"), "./cmd/manzanas"
  end

  test do
    assert_match "manzanas #{version}", shell_output("#{bin}/manzanas --version")
  end
end
