import type { Metadata } from "next";
import { Bar, H2, P, StatCard } from "../../../components/BlogKit";
import CopyBlock from "../../../components/CopyBlock";

export const metadata: Metadata = {
  title: "Why agents need simulator leases — manzanas blog",
  description:
    "Two agents on one Mac will boot each other's simulators and type into the wrong window. Why I built leases instead of locks: TTL-bounded exclusive claims, FIFO queues, park/thaw warm pools, golden images, and the design philosophy behind manzanasd.",
};

const LEASE = `$ curl -s -X POST mac-host:7433/v0/leases -d '{
    "labels": ["ios26"],
    "agent_id": "claude-1",
    "ttl_seconds": 300,
    "reset": "snapshot:logged-in"
  }'
{"id":"lse_9f2","target_udid":"…","state":"active",
 "expires_at":"…","queue_position":0}`;

export default function Post() {
  return (
    <article className="mx-auto max-w-[720px] px-6 pb-24 pt-16 sm:pt-24">
      <p className="font-mono text-[12px] text-[#6e6e73]">
        August 11, 2026 · BariBariGood
      </p>
      <h1 className="headline headline-xl mt-3 text-[36px] sm:text-[52px]">
        Why agents need simulator leases
      </h1>
      <P>
        The first time I ran two coding agents against the same Mac, they
        found the same booted simulator within a minute. One was mid-way
        through a login flow; the other decided the sim was stale, shut it
        down, and booted a different one. The first agent kept tapping
        coordinates on a screen that no longer existed and confidently
        reported the login test as failed. Nothing was broken except the
        assumption that a simulator belongs to whoever found it.
      </P>
      <P>
        Simulators are shared mutable state with no access control.{" "}
        <code className="font-mono text-[14px]">simctl</code> happily lets any
        process boot, erase, or type into any device. Humans coordinate over
        Slack; agents don&apos;t coordinate at all. Once you run more than one,
        you need an owner.
      </P>

      <H2>Locks are the obvious answer, and the wrong one</H2>
      <P>
        My first fix was a lock file per UDID. It failed in every way lock
        files fail: an agent crashed holding the lock and everything wedged;
        an agent&apos;s shell died and the lock leaked; two agents on different
        machines couldn&apos;t see each other&apos;s locks at all. The failure
        mode of a lock is a stuck fleet, and agents crash a lot.
      </P>
      <P>
        A <strong>lease</strong> is a lock with a clock and a line. It is
        TTL-bounded: if the agent dies, the claim expires and the simulator
        returns to the pool by itself. It is queued: a busy target means a
        FIFO position, not a spin loop. And it is granted by the one process
        that actually owns the state — a daemon on the Mac — so it works for
        N agents on N machines:
      </P>
      <div className="mt-6">
        <CopyBlock code={LEASE} label="POST /v0/leases" />
      </div>
      <P>
        Every mutating call after that carries the lease ID, and the daemon
        rejects calls against targets you don&apos;t hold. That single check
        is most of the value: the wrong-window class of bug is gone, not
        mitigated.
      </P>

      <H2>Expiry needs cheap handovers</H2>
      <P>
        Leases only work if losing one is cheap. If a fresh simulator costs a
        cold boot, agents hoard leases forever and you&apos;re back to locks
        with extra steps. So the daemon keeps a <strong>warm pool</strong>:
        idle sims are parked with SIGSTOP — a stopped process tree is
        unschedulable, so a parked sim costs ~0 host CPU no matter what its
        daemons want to do — and thawed with SIGCONT on lease grant.
      </P>
      <div className="mt-6 space-y-4">
        <StatCard title="Lease to live simulator" note="M3, macOS 26">
          <Bar label="first boot" value="~29 s" pct={100} />
          <Bar label="cold boot" value="~7 s" pct={24} />
          <Bar label="thaw from warm pool" value="~0.28 s" pct={1} green />
        </StatCard>
      </div>
      <P>
        At ~0.28 s lease-to-live, giving a simulator back is painless, so
        TTLs can be short, so the queue actually moves. The mechanisms
        reinforce each other; neither is much good alone.
      </P>

      <H2>Determinism closes the loop</H2>
      <P>
        The last piece is what the next agent inherits. A lease can declare a{" "}
        <code className="font-mono text-[14px]">reset</code> — erase, or
        restore a named snapshot — applied automatically at release. Golden
        images stamp out slimmed sims (~0.75 GB vs ~5 GB stock) in seconds,
        so &quot;a clean iPhone 17 Pro with the app installed and a logged-in
        account&quot; is a starting state, not a 10-minute setup script.
        Every retry starts from the same bytes.
      </P>

      <H2>The philosophy</H2>
      <P>
        The design rule behind all of this: <strong>everything stateful
        lives in the daemon; clients stay thin.</strong> Agents are terrible
        at distributed coordination and great at making API calls, so the
        coordination problem should live in one process per Mac that owns
        the registry, the lease table, the warm pool, and the journal — and
        the agents should just ask. It&apos;s the same reason databases have
        servers instead of every client fencing over the same files on an
        NFS mount.
      </P>
      <P>
        And because the referee sees every mutating op, it can journal them —
        who held the lease, what they did, what the screen looked like. Once
        no two agents can trip over each other, the evidence of what each one
        did is suddenly trustworthy. That journal became the run primitive in{" "}
        <a className="underline" href="/blog/v0-5-0-one-call-runs">
          v0.5.0
        </a>
        , but it started here: leases aren&apos;t a scheduling feature, they
        are what makes anything an agent reports about a simulator believable.
      </P>
    </article>
  );
}
