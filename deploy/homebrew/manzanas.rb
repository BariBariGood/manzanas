# manzanas — client CLI for manzanasd. Canonical copy; mirrored to the
# BariBariGood/homebrew-tap repo (Formula/manzanas.rb) — keep both in sync
# and bump tag/revision together on each release.
#
class Manzanas < Formula
  desc "Client CLI for manzanasd, the multi-agent iOS simulator fleet daemon"
  homepage "https://github.com/BariBariGood/manzanas"
  url "https://github.com/BariBariGood/manzanas.git",
      tag:      "v0.7.0",
      revision: "ed32c98d7d076abb8429479fa4c9c50550a83501"
  license "Apache-2.0"
  head "https://github.com/BariBariGood/manzanas.git", branch: "main"

  depends_on "go" => :build

  def install
    system "go", "build", *std_go_args(ldflags: "-s -w -X github.com/BariBariGood/manzanas/internal/buildinfo.Version=#{version}"), "./cmd/manzanas"
  end

  test do
    assert_match "manzanas #{version}", shell_output("#{bin}/manzanas --version")
  end
end
