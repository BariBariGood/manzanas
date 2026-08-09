# manzanasd — the fleet daemon. Canonical copy; mirrored to the
# BariBariGood/homebrew-tap repo (Formula/manzanasd.rb) — keep both in sync
# and bump tag/revision together on each release.
#
#
# The service block mirrors deploy/com.baribarigood.manzanasd.plist in the
# manzanas repo (RunAtLoad, KeepAlive unless clean exit, port 7433,
# Interactive process type), with logs/state under Homebrew's var instead
# of ~/.manzanasd so the formula stays multi-user friendly.
class Manzanasd < Formula
  desc "Mac daemon for multi-agent iOS simulator fleet orchestration"
  homepage "https://github.com/BariBariGood/manzanas"
  url "https://github.com/BariBariGood/manzanas.git",
      tag:      "v0.4.0",
      revision: "15c4b4ec6ea730c3288abc048d3e926ab8a51899"
  license "MIT"
  head "https://github.com/BariBariGood/manzanas.git", branch: "main"

  depends_on "baribarigood/tap/manzanas"
  depends_on "go" => :build

  def install
    system "go", "build", *std_go_args(ldflags: "-s -w -X main.version=#{version}"), "./cmd/manzanasd"
  end

  def post_install
    (var/"manzanasd").mkpath
    (var/"log").mkpath
  end

  service do
    run [opt_bin/"manzanasd", "--addr", ":7433"]
    keep_alive successful_exit: false
    working_dir var/"manzanasd"
    log_path var/"log/manzanasd.out.log"
    error_log_path var/"log/manzanasd.err.log"
    process_type :interactive
  end

  test do
    assert_match "manzanasd #{version}", shell_output("#{bin}/manzanasd --version")
    port = free_port
    pid = fork { exec bin/"manzanasd", "--addr", ":#{port}", "--mock" }
    sleep 2
    output = shell_output("curl -s http://localhost:#{port}/v0/healthz")
    assert_match "ok", output
  ensure
    if pid
      Process.kill("TERM", pid)
      Process.wait(pid)
    end
  end
end
