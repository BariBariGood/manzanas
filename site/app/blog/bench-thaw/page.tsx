// DRAFT — intentionally not listed in app/blog/page.tsx yet. Add it to the
// posts array there when the repo flips public.
import type { Metadata } from "next";
import { Bar, FieldNote, H2, P, StatCard } from "../../../components/BlogKit";
import CopyBlock from "../../../components/CopyBlock";

export const metadata: Metadata = {
  title: "276 ms to a live iOS simulator — manzanas blog",
  description:
    "Re-measuring manzanasd's headline numbers on real hardware: park/thaw latency distributions, warm vs cold action latency, cold boot times, and idle CPU — with a `make bench` harness so you can reproduce every number.",
  robots: { index: false, follow: false },
};

export default function Post() {
  return (
    <article className="mx-auto max-w-[720px] px-6 pb-24 pt-16 sm:pt-24">
      <FieldNote date="August 5, 2026" />
      <h1 className="headline headline-xl mt-3 text-[36px] sm:text-[52px]">
        276 ms to a live iOS simulator
      </h1>
      <P>
        manzanasd&apos;s README makes three performance claims: parked
        simulators thaw in ~26 ms instead of a 5 s+ boot, warm actions run in
        ~130 ms instead of ~1,050 ms, and parking takes idle host CPU from
        ~213% to roughly nothing. Those numbers came from ad-hoc fleet
        measurements over several weeks. Before shipping them on the front
        of the repo, we re-measured everything from scratch on one machine,
        with a harness anyone can run: <code>make bench</code>.
      </P>
      <P>
        Some claims got better, one got worse, and one we could not
        reproduce at all. Details below.
      </P>

      <H2>Setup</H2>
      <P>
        All numbers in this post come from a single run on a MacBook Pro
        (Apple M3 Pro, 12 cores, 36 GB RAM), macOS 26.5.2, Xcode 26.5,
        driving an iPhone 17 simulator on iOS 26.5 slimmed with simslim.
        The machine was in normal use (browser open, other processes
        running) — deliberately, because that is what a fleet box looks
        like. 25 samples per latency phase, 5 per boot phase. Every number
        is client-observed: wall time of the HTTP request against a real
        daemon on localhost, including all protocol and bookkeeping
        overhead. No daemon-internal timers unless labeled as such.
      </P>

      <H2>Thaw: the 26 ms claim was wrong in both directions</H2>
      <P>
        A parked simulator is a booted simulator whose whole process tree is
        SIGSTOPped. Thawing is a SIGCONT to a PID list cached at park time.
        The daemon logs its side of every thaw, and on this hardware it
        rounds to <strong>0 ms</strong> — the signal delivery itself is
        sub-millisecond, not 26 ms. The 26 ms in the README was measured
        before the PID cache landed, when thaw still walked the process
        table.
      </P>
      <P>
        But nobody drives a sim with a raw SIGCONT. The number that matters
        is lease-to-live: POST /v0/leases against a parked pool sim until
        the lease is active and the sim is running. That is{" "}
        <strong>276 ms p50, 337 ms p95</strong> (n=25). The same lease
        acquire against an already-running, un-parked sim is 256 ms p50 —
        so thawing adds about 20 ms end to end, and roughly 0.26 s of the
        total is lease bookkeeping that both paths pay.
      </P>
      <div className="mt-6">
        <StatCard title="Sim ready to drive" note="lease-to-live, p50">
          <Bar label="cold boot, first ever (stock sim)" value="29.3 s" pct={100} />
          <Bar label="cold boot, subsequent (stock sim)" value="6.9 s" pct={24} />
          <Bar label="boot, slimmed sim (to Booted state)" value="1.52 s" pct={5} />
          <Bar label="lease a parked pool sim (thaw incl.)" value="276 ms" pct={1} green />
        </StatCard>
      </div>
      <P>
        Boot numbers deserve honesty too. &quot;5 s+&quot; undersold the
        cold path: a stock simulator&apos;s first boot takes ~29 s (data
        migration), and even subsequent boots take ~7 s to an actually
        usable SpringBoard (<code>simctl bootstatus</code>). The 1.5 s
        figure is a slimmed sim reaching CoreSimulator&apos;s{" "}
        <code>Booted</code> state, which arrives well before the sim is
        ready to tap. Thaw has no such gap: the sim was fully booted when it
        was parked, so 276 ms buys a simulator that is live{" "}
        <em>right now</em>, with warm caches and a resident action helper.
      </P>

      <H2>Warm vs cold actions: better than claimed</H2>
      <P>
        The cold action path spawns the AXe CLI per action and pays
        process spawn plus FBSimulatorControl bootstrap every time. The
        warm path keeps a resident helper per sim that bootstraps once. The
        README claimed ~130 ms warm vs ~1,050 ms cold; on this hardware the
        warm path is measurably better than that:
      </P>
      <div className="mt-6 space-y-4">
        <StatCard title="End-to-end tap" note="~26x warm">
          <Bar label="cold (AXe spawn per call), p50" value="946 ms" pct={100} />
          <Bar label="warm (resident helper), p50" value="36 ms" pct={4} green />
        </StatCard>
        <StatCard title="End-to-end observe (describe-ui)" note="~1.4x warm">
          <Bar label="cold, p50" value="884 ms" pct={100} />
          <Bar label="warm, p50" value="628 ms" pct={71} green />
        </StatCard>
      </div>
      <P>
        Tap spread was tight in both modes (warm: 27–38 ms over 25 samples;
        cold: 937–1,007 ms), so the ~26x is not a percentile trick. Observe
        improves much less because its cost is the simulator serializing
        its accessibility tree, which no client-side residency can remove —
        the warm path only shaves the bootstrap share. The first warm
        action on a sim pays the helper spawn (first tap ~0.18 s, first
        observe ~4 s while the accessibility bridge comes up); the harness
        reports it separately as a warmup sample.
      </P>

      <H2>Idle CPU: we could not reproduce our own claim</H2>
      <P>
        The README says parking took idle host CPU from ~213% to 1–6%. In
        this run, a freshly booted stock simulator idled at{" "}
        <strong>~0% CPU</strong> once boot settled — no runaway{" "}
        <code>apsd</code>, nothing to fix. Parked, it was 0.1%; booted and
        idle (slimmed), 0.2%.
      </P>
      <P>
        The 213% was real when we measured it — idle iOS 26.5 sims burning
        50–213% CPU each in <code>apsd</code> push-notification retry
        loops and <code>diagnosticd</code>, on the same OS and Xcode. But
        it is clearly environment-dependent (network conditions gate the
        APNs retry behavior), and a benchmark that only reproduces
        sometimes is a claim, not a number. What parking actually
        guarantees is an upper bound: a SIGSTOPped tree is unschedulable,
        so a parked sim costs ~0 CPU <em>no matter what</em>{" "}
        <code>apsd</code> is doing that day. During boot we did watch
        single sim daemons spike past 80% CPU each, so the worst case is
        real — parking just makes it structurally impossible.
      </P>

      <H2>Run it yourself</H2>
      <P>
        Every number above comes out of one script that creates a
        throwaway simulator, runs a scratch daemon on a spare port twice
        (once with the warm pool and resident helpers, once with{" "}
        <code>--no-warm</code>), samples every phase, and deletes
        everything it made:
      </P>
      <div className="my-6">
        <CopyBlock
          code={`git clone https://github.com/BariBariGood/manzanas
cd manzanas
make bench            # ~30 min; needs a Mac with Xcode + AXe

# raw per-sample data:
cat eval/bench/out/bench.jsonl`}
        />
      </div>
      <P>
        The harness is ~150 lines of shell plus a small Go protocol client
        (<code>eval/cmd/manzanas-bench</code>) that prints every sample and
        a p50/p90/p95 summary per phase. If your numbers disagree with
        ours, we want to know — open an issue with your{" "}
        <code>bench.jsonl</code> and hardware.
      </P>

      <H2>What&apos;s still janky</H2>
      <P>
        Things this benchmark exposed that we have not fixed: the 256 ms of
        lease bookkeeping dominates the thaw path — every lease acquire
        shells out to <code>simctl list</code> to refresh the target table
        before granting, and that subprocess is most of the 256 ms. The
        SIGCONT is free; the bookkeeping is not.
        Re-parking after a lease release takes ~45–60 s because the pool
        re-verifies the sim&apos;s slim state each cycle, which bounds
        warm-pool churn under heavy lease turnover. Cold <code>observe</code>{" "}
        at ~0.9 s remains the slowest common action and is
        simulator-bound. And boot-to-<code>Booted</code> is a misleading
        readiness signal for anything that wants to tap immediately —
        thaw or <code>bootstatus</code> are the honest paths.
      </P>
      <P>
        The README&apos;s numbers have been updated to match this run.
        Methodology or measurement holes? Open an issue.
      </P>
    </article>
  );
}
