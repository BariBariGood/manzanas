import type { Metadata } from "next";
import CopyBlock from "../../../components/CopyBlock";

export const metadata: Metadata = {
  title: "Growing an orchard out of two $250 shitboxes — manzanas blog",
  description:
    "Putting macOS Sequoia on unsupported 2017 MacBook Pros with OpenCore Legacy Patcher, wiring them into a Tailscale tailnet, and turning them into a simulator farm.",
};

function H2({ children }: { children: React.ReactNode }) {
  return (
    <h2 className="headline mt-12 text-[26px] sm:text-[30px]">{children}</h2>
  );
}

function P({ children }: { children: React.ReactNode }) {
  return (
    <p className="copy-secondary mt-5 text-[17px] leading-relaxed">
      {children}
    </p>
  );
}

function Bar({
  label,
  value,
  pct,
  green,
}: {
  label: string;
  value: string;
  pct: number;
  green?: boolean;
}) {
  return (
    <div className="mt-3">
      <div className="flex items-baseline justify-between font-mono text-[12px]">
        <span className="text-[#424245]">{label}</span>
        <span className={green ? "font-semibold text-[#248a3d]" : "text-[#6e6e73]"}>
          {value}
        </span>
      </div>
      <div className="mt-1.5 h-2.5 overflow-hidden rounded-full bg-black/[0.06]">
        <div
          className={`h-full rounded-full ${green ? "bg-[#34c759]" : "bg-[#86868b]"}`}
          style={{ width: `${Math.max(pct, 1.5)}%` }}
        />
      </div>
    </div>
  );
}

function StatCard({
  title,
  note,
  children,
}: {
  title: string;
  note?: string;
  children: React.ReactNode;
}) {
  return (
    <div className="tile p-6 sm:p-7">
      <div className="flex items-baseline justify-between gap-3">
        <p className="headline text-[17px]">{title}</p>
        {note ? (
          <span className="whitespace-nowrap rounded-full bg-[#248a3d]/10 px-2.5 py-0.5 font-mono text-[11px] font-semibold text-[#248a3d]">
            {note}
          </span>
        ) : null}
      </div>
      {children}
    </div>
  );
}

export default function Post() {
  return (
    <article className="mx-auto max-w-[720px] px-6 pb-24 pt-16 sm:pt-24">
      <p className="font-mono text-[12px] text-[#6e6e73]">July 29, 2026</p>
      <h1 className="headline headline-xl mt-3 text-[36px] sm:text-[52px]">
        Growing an orchard out of two $250 shitboxes
      </h1>
      <P>
        Two 2017 15&quot; MacBook Pros. Intel i7, 16 GB, about $250 each
        used. This is how they became a simulator farm that AI agents use
        around the clock.
      </P>

      <H2>Sequoia on unsupported hardware</H2>
      <P>
        Current Xcode wants a current macOS, and Apple dropped these
        machines years ago.{" "}
        <a
          className="text-[#248a3d] underline-offset-2 hover:underline"
          href="https://dortania.github.io/OpenCore-Legacy-Patcher/"
          target="_blank"
          rel="noreferrer"
        >
          OpenCore Legacy Patcher
        </a>{" "}
        fixes that: patched USB installer, install Sequoia, run the
        post-install root patches for Wi-Fi and graphics. Solid for months.
        One catch: OS updates can undo the patches, so update manually and
        reapply.
      </P>

      <H2>Tailscale instead of port forwarding</H2>
      <P>
        Every box gets a stable address on a private mesh. No router config,
        no exposed ports, no caring which Wi-Fi it&apos;s on.
      </P>
      <div className="my-6">
        <CopyBlock
          code={`brew install --cask tailscale
tailscale ping <other-box>   # verify the mesh`}
        />
      </div>

      <H2>Making a laptop act like a server</H2>
      <P>
        Remote Login on, SSH key in place, passwordless sudo on the
        dedicated boxes, and never sleep. They run closed-lid on AC power.
      </P>
      <div className="my-6">
        <CopyBlock
          code={`sudo systemsetup -setremotelogin on

sudo pmset -a sleep 0 disksleep 0
sudo pmset -a displaysleep 10`}
        />
      </div>

      <H2>Xcode and slim simulators</H2>
      <div className="my-6">
        <CopyBlock
          code={`sudo xcode-select -s /Applications/Xcode26.app
sudo xcodebuild -license accept
xcodebuild -runFirstLaunch
xcodebuild -downloadPlatform iOS`}
        />
      </div>
      <P>
        Each box runs a small pool of pre-created simulators with animations
        off and unneeded daemons trimmed, erased between tasks instead of
        deleted.
      </P>

      <H2>The numbers</H2>
      <P>
        All measured on these Intel boxes, so treat them as a floor.
      </P>
      <div className="mt-6 grid gap-4 sm:grid-cols-2">
        <StatCard title="Tap latency" note="~100x faster">
          <Bar label="cold (spawn per action)" value="3.05s" pct={100} />
          <Bar label="warm (resident toolchain)" value="30ms" pct={1} green />
        </StatCard>
        <StatCard title="RN iOS build" note="~170x faster">
          <Bar label="cold build" value="28 min" pct={100} />
          <Bar label="warm incremental" value="5.9 min" pct={21} />
          <Bar label="fingerprint cache hit" value="~10s" pct={1} green />
        </StatCard>
        <StatCard title="Live stream rate">
          <p className="headline mt-3 text-[40px]">
            ~1 <span className="text-[22px] text-[#6e6e73]">fps</span>
          </p>
          <p className="copy-secondary mt-2 text-[13px] leading-relaxed">
            simctl screenshots cost ~0.8s each on a 2017 Intel. Fine for
            watching agents, not animations.
          </p>
        </StatCard>
        <StatCard title="Sims per box">
          <p className="headline mt-3 text-[40px]">
            2<span className="text-[22px] text-[#6e6e73]">–3</span>
          </p>
          <p className="copy-secondary mt-2 text-[13px] leading-relaxed">
            The ceiling is CPU, not RAM: each booted sim drags its own
            system daemons along.
          </p>
        </StatCard>
      </div>

      <H2>Why manzanasd exists</H2>
      <P>
        Several agents on two boxes meant constant collisions: booting each
        other&apos;s simulators, typing into the wrong window. Lock files in{" "}
        <code className="font-mono text-[14px]">/tmp</code> worked until an
        agent forgot. manzanasd puts the arbitration in a daemon instead of a
        convention: one process per Mac owns the simulators, hands out
        leases, queues everyone else, and streams the screen.
      </P>
      <div className="my-6">
        <CopyBlock
          code={`./bin/manzanasd --addr :7433
./bin/manzanas lease acquire --labels ios26 --agent claude-1`}
        />
      </div>
      <P>
        Total hardware cost for the orchard: about $500. The daemon is free.
      </P>
    </article>
  );
}
