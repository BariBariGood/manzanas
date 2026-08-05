// DRAFT — intentionally not listed in app/blog/page.tsx yet. Add it to the
// posts array there when the repo actually flips public.
import type { Metadata } from "next";
import { Bar, FieldNote, H2, P, StatCard } from "../../../components/BlogKit";
import CopyBlock from "../../../components/CopyBlock";

export const metadata: Metadata = {
  title: "manzanas is open source — manzanas blog",
  description:
    "The Mac daemon for multi-agent iOS simulator fleets is now public: leases, a park/thaw warm pool with 26 ms thaws, ~130 ms warm taps, MJPEG streaming, deterministic state, and an exportable run journal. MIT licensed.",
  robots: { index: false, follow: false },
};

export default function Post() {
  return (
    <article className="mx-auto max-w-[720px] px-6 pb-24 pt-16 sm:pt-24">
      <FieldNote date="August 2026" />
      <h1 className="headline headline-xl mt-3 text-[36px] sm:text-[52px]">
        manzanas is open source
      </h1>
      <P>
        manzanasd — the Mac daemon we built to let a swarm of AI agents share
        iOS simulators without tripping over each other — is now public on{" "}
        <a
          className="underline"
          href="https://github.com/BariBariGood/manzanas"
        >
          GitHub
        </a>
        , MIT licensed. One daemon per Mac owns everything stateful: the
        simulator registry, TTL-bounded leases with FIFO queues, a park/thaw
        warm pool, cold and warm action backends, MJPEG streaming, snapshots
        and golden images, video capture, and an append-only run journal
        with a PR-ready markdown export. Thin clients (CLI, MCP, npm, a
        GitHub Action) speak a versioned JSON protocol from anywhere.
      </P>

      <H2>Why it exists</H2>
      <P>
        Agents driving simulators over raw SSH pay two costs: they collide
        (two agents typing into the same sim), and they wait (5 s+ boots,
        1 s+ per-action tool spawns). Leases remove the collisions; the warm
        pool and a resident per-sim helper remove the waiting. Every mutating
        op lands in a journal, so a QA run leaves evidence instead of vibes.
      </P>

      <H2>The numbers</H2>
      <div className="mt-6 space-y-4">
        <StatCard title="Sim ready to drive" note="thaw vs boot">
          <Bar label="cold boot (best case)" value="5,000+ ms" pct={100} />
          <Bar label="thaw a parked sim (M3)" value="26 ms" pct={0.5} green />
        </StatCard>
        <StatCard title="End-to-end tap" note="~8x warm">
          <Bar label="cold (per-action AXe spawn)" value="~1,050 ms" pct={100} />
          <Bar label="warm (resident helper)" value="~130 ms" pct={12} green />
        </StatCard>
        <StatCard title="Idle host CPU per parked sim" note="SIGSTOP is free">
          <Bar label="booted stock sim" value="50–213%" pct={100} />
          <Bar label="parked pool sim" value="~0%" pct={1} green />
        </StatCard>
      </div>

      <H2>Try it</H2>
      <P>All it needs is a Mac with Xcode:</P>
      <div className="mt-6">
        <CopyBlock
          code={`brew tap baribarigood/tap https://github.com/BariBariGood/homebrew-tap
brew trust baribarigood/tap
brew install manzanasd
brew services start manzanasd

manzanas targets
manzanas lease acquire --labels ios26 --agent me --wait
manzanas tap 200 400 --lease lse_...
manzanas screenshot --lease lse_... -o shot.png`}
        />
      </div>
      <P>
        No Mac handy? <code>manzanasd --mock</code> runs anywhere with a fake
        fleet. The{" "}
        <a
          className="underline"
          href="https://github.com/BariBariGood/manzanas/blob/main/docs/quickstart.md"
        >
          quickstart
        </a>{" "}
        walks the whole loop; <code>manzanas mcp</code> exposes it all as
        lease-scoped MCP tools for agents.
      </P>
    </article>
  );
}
