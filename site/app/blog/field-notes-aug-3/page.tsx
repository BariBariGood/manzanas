import type { Metadata } from "next";
import { FieldNote, H2, P } from "../../../components/BlogKit";

export const metadata: Metadata = {
  title: "Field notes: real devices, a smarter broker, video — manzanas blog",
  description:
    "Wave 3 in one day: physical iPhones as leasable targets, warm-first broker placement on daemon-truth load, an embedded read-only dashboard, per-lease video capture, and a Go toolchain that quietly ships crashing binaries on macOS 26.",
};

export default function Post() {
  return (
    <article className="mx-auto max-w-[720px] px-6 pb-24 pt-16 sm:pt-24">
      <FieldNote date="August 3, 2026" />
      <h1 className="headline headline-xl mt-3 text-[36px] sm:text-[52px]">
        Field notes: real devices, a smarter broker, video
      </h1>
      <P>
        Wave 3 landed today: 79 files, +6,808/−126 lines across five merged
        branches. Findings below, all verified against the merged code.
      </P>

      <H2>Real iPhones are three different tools in a trenchcoat</H2>
      <P>
        Physical devices now appear as leasable targets behind
        <code> --devices</code>: discovery merges{" "}
        <code>xcrun devicectl list devices</code> into the simulator registry,
        lifecycle goes through devicectl — and interaction can&apos;t. devicectl
        has no screenshot or HID support, and AXe and the resident helper are
        simulator-only, so tap/type/observe/screenshot speak WebDriverAgent&apos;s
        REST protocol directly over HTTP, no Appium server. A paired-but-
        disconnected phone still enumerates from devicectl&apos;s cached
        metadata and is still leasable; connection-requiring actions fail with
        a clean <code>unavailable: device not connected</code> instead of
        hanging.
      </P>

      <H2>The broker stops guessing</H2>
      <P>
        The daemon grew <code>GET /v0/status</code> — capacity, running/parked
        counts, boots in flight, active/queued leases, load average, free
        disk, gate results — and the broker now places leases on it.
        Candidates rank warm-first in three tiers: a warm or booted match
        (a ~26 ms thaw instead of a cold boot), then hosts with boot
        headroom and passing gates, then everything else. Within a tier,
        least effective load wins — the daemon&apos;s own lease counts, which
        see direct-to-daemon clients the broker never granted, bumped by
        broker grants between probes. Queued acquires land on the shallowest
        daemon-reported queue. Old daemons without <code>/v0/status</code>{" "}
        keep the previous behavior unchanged.
      </P>

      <H2>A dashboard with no build step</H2>
      <P>
        <code>/dash</code> is a read-only fleet dashboard embedded in the
        daemon binary via <code>embed.FS</code> — vanilla JS, no node build,
        works offline. Targets, leases, warm-pool chips, and a journal
        browser with inline screenshot artifacts and export links. It listens
        to the existing <code>/v0/ws</code> events purely as invalidation
        signals and re-fetches the lists, falling back to a 5-second poll if
        the socket drops. No mutating controls: it can never thaw a parked
        sim, violate a boot cap, or touch an agent&apos;s lease.
      </P>

      <H2>SIGINT is the only clean way out of recordVideo</H2>
      <P>
        Per-lease video capture wraps <code>simctl io recordVideo</code> in a
        daemon-owned child tied to the lease lifecycle, ingested into the
        journal as <code>segment</code> entries. The sharp edges: SIGINT
        finalizes the mp4 cleanly, but a SIGKILL poisons the sim&apos;s
        recording service until it&apos;s shut down and rebooted; recording a
        non-booted sim &quot;succeeds&quot; with a zero-byte file, so
        non-Booted starts are refused outright. Defaults are conservative —
        300 s / 128 MiB caps, 2 concurrent recordings, a 10 GiB free-disk
        floor — and restart recovery SIGINTs orphaned recorder children and
        salvages their spools.
      </P>

      <H2>The toolchain that shipped crashing binaries</H2>
      <P>
        Go 1.22.5 on macOS 26 builds binaries that crash on launch — the
        linker omits an <code>LC_UUID</code> load command that the OS now
        requires. Nothing in the build fails; the binary just dies. The fix
        is one env var: <code>GOTOOLCHAIN=go1.24.5</code>.
      </P>

      <H2>Open-source prep</H2>
      <P>
        The repo is ready to open: internal IPs and usernames scrubbed to
        example values across docs and site, a single-Mac quickstart
        (install → lease → tap → screenshot), a SECURITY.md spelling out the
        no-auth/tailnet-only trust model, and a git-history secret scan that
        came back clean.
      </P>
    </article>
  );
}
