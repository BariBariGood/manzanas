import type { Metadata } from "next";
import { Bar, FieldNote, H2, P, StatCard } from "../../../components/BlogKit";

export const metadata: Metadata = {
  title: "Field notes: optimizing the fleet — manzanas blog",
  description:
    "Seven measured findings from optimizing the fleet: apsd retry loops, SIGSTOP parking, golden images that lost their slimming, boot storms, a 40ms link, a workstation that never throttles, and everything that landed since v0.1.",
};

export default function Post() {
  return (
    <article className="mx-auto max-w-[720px] px-6 pb-24 pt-16 sm:pt-24">
      <FieldNote date="August 2, 2026" />
      <h1 className="headline headline-xl mt-3 text-[36px] sm:text-[52px]">
        Field notes: optimizing the fleet
      </h1>
      <P>
        Seven findings from a week of measuring and fixing the fleet. All
        numbers measured, no estimates.
      </P>

      <H2>The push daemon that ate the fleet</H2>
      <P>
        Idle iOS 26.5 simulators were burning 50–213% CPU each. The culprit
        was apsd stuck in a failing APNs connect-retry loop. Disabling apsd
        and diagnosticd inside the sim took idle CPU to 1–6% — roughly 50x
        less — and is what makes a warm pool affordable.
      </P>

      <H2>Pause, don&apos;t reboot</H2>
      <P>
        SIGSTOP a simulator&apos;s whole launchd_sim process tree and it
        parks at 0% CPU; SIGCONT thaws it in 26 ms on an M3 (~225 ms on
        Intel) with a cached PID list, vs 20 s–2 min boots. A 2 h 50 m park
        thawed perfectly. Never run simctl against a parked sim — it hangs
        instead of erroring.
      </P>

      <H2>The golden image that silently lost its slimming</H2>
      <P>
        launchctl disable state lives outside the simulator data directory,
        so data-dir-swap stamping dropped all 141 disabled services and the
        sims booted effectively stock (~204 processes vs 77 slim). The fix:
        boot, re-apply, and verify after every stamp and erase — 141/141
        disables, 65 services vs ~330 stock, 4.6 s boot-to-ready.
      </P>

      <H2>Boot storms, not steady state</H2>
      <P>
        Every load catastrophe (load averages past 500 on 8 cores) was
        concurrent simulator boots, never steady-state work. Boots now go
        through a semaphore — 1 at a time on Intel, 2 on Apple Silicon —
        with a load gate at 2x core count and a 5 GiB free-disk gate.
      </P>

      <H2>Designing for a 40 ms link</H2>
      <P>
        Agents reach the Macs over a 40–65 ms relayed link at 5–20 Mbit/s.
        JPEG q70 downscaled to 800 px cut screenshots 26x (574,479 B →
        22,252 B; end-to-end 2.6 s → 1.2 s). tap_element does
        observe→find→tap in one request, and actions:batch ships up to 32
        actions in one round trip.
      </P>

      <H2>The workstation that never throttles</H2>
      <P>
        A 2013 Mac Pro locks 92 W and 3.6 GHz on all cores indefinitely;
        2017 MacBook Pros hit their thermal wall in ~2 minutes, cap near
        30 W, and hold ~73% of starting speed. Clean build: 13 m 10 s vs
        15 m 26 s and 16 m 31 s — and the gap widens on back-to-back CI
        builds.
      </P>

      <H2>Warm sims, golden images, and a broker</H2>
      <P>
        Everything else that landed since v0.1: simbridge, a resident
        helper that takes taps from 3.05 s to 30 ms on Intel (~100x);
        golden images that stamp pre-slimmed sims in seconds instead of
        ~2 minutes of slimming; copy-on-write simctl-clone snapshots that
        survive restore; manzanas-broker fronting N daemons behind one
        address without proxying frames; and manzanas-eval, a harness that
        reports determinism rate and per-step latency percentiles in CI.
      </P>

      <H2>By the numbers</H2>
      <div className="mt-6 space-y-4">
        <StatCard title="Thaw a parked sim" note="~190x faster than a 5s boot">
          <Bar label="cold boot (best case)" value="5,000+ ms" pct={100} />
          <Bar label="SIGCONT thaw, Intel" value="~225 ms" pct={4.5} />
          <Bar label="SIGCONT thaw, M3" value="26 ms" pct={0.5} green />
        </StatCard>
        <StatCard title="End-to-end tap" note="~8x warm">
          <Bar label="cold (per-action AXe spawn)" value="~1,050 ms" pct={100} />
          <Bar label="warm (resident helper)" value="~130 ms" pct={12} green />
        </StatCard>
        <StatCard title="Idle host CPU per sim" note="~50x">
          <Bar label="stock iOS 26.5 sim" value="50–213%" pct={100} />
          <Bar label="apsd + diagnosticd disabled" value="1–6%" pct={3} green />
        </StatCard>
        <StatCard title="Screenshot over a 40 ms link" note="26x smaller">
          <Bar label="raw PNG" value="574,479 B / 2.6 s" pct={100} />
          <Bar label="JPEG q70 @ 800 px" value="22,252 B / 1.2 s" pct={3.9} green />
        </StatCard>
      </div>
    </article>
  );
}
