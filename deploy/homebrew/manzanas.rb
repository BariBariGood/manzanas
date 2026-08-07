# manzanas — client CLI for manzanasd. Canonical copy; mirrored to the
# BariBariGood/homebrew-tap repo (Formula/manzanas.rb) — keep both in sync
# and bump tag/revision together on each release.
#
# Builds from source because the manzanasd repo (and its release artifacts)
# are private; git auth comes from the machine's ~/.netrc, same as any
# other git operation.
class Manzanas < Formula
  desc "Client CLI for manzanasd, the multi-agent iOS simulator fleet daemon"
  homepage "https://github.com/BariBariGood/manzanas"
  url "https://github.com/BariBariGood/manzanas.git",
      tag:      "v0.2.0",
      revision: "3f7df3e0a2c6dc6c46674513de9c82d59067c135"
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
