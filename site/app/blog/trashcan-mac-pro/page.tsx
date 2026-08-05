import type { Metadata } from "next";
import CopyBlock from "../../../components/CopyBlock";

export const metadata: Metadata = {
  title: "Adding a trashcan Mac Pro to the orchard — manzanas blog",
  description:
    "Onboarding a 2013 Mac Pro 6,1 as a headless always-on CI and build box: self-hosted GitHub Actions runner today, OCLP to Sequoia and iOS simulators next.",
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
      <p className="font-mono text-[12px] text-[#6e6e73]">August 1, 2026</p>
      <h1 className="headline headline-xl mt-3 text-[36px] sm:text-[52px]">
        Adding a trashcan to the orchard
      </h1>
      <P>
        The newest tree in the orchard is a 2013 Mac Pro 6,1: 3.5 GHz 6-core
        Xeon E5-1650 v2, 16 GB of ECC RAM, dual FirePro D500s, bought in
        2017 and still on Monterey. On paper it benchmarks below a 2017
        laptop. As a headless always-on box, it beats both of ours.
      </P>

      <H2>Why a 12-year-old desktop</H2>
      <P>
        Our other Intel boxes are laptops, and laptop coolers give up
        minutes into a sustained build. The trashcan has one big slow fan
        and a 450W thermal core, so its six cores hold full turbo
        indefinitely at around 14 dBA. It idles at about 43 watts, which
        rounds to $5 a month to leave on forever.
      </P>
      <div className="mt-6 grid gap-4 sm:grid-cols-2">
        <StatCard title="Geekbench 6 multi-core">
          <Bar label="M3 Pro MacBook" value="~15,300" pct={100} />
          <Bar label="2017 MBP (throttles)" value="~3,736" pct={24} />
          <Bar label="trashcan (sustained)" value="~3,450" pct={23} green />
        </StatCard>
        <StatCard title="Idle power">
          <p className="headline mt-3 text-[40px]">
            43 <span className="text-[22px] text-[#6e6e73]">W</span>
          </p>
          <p className="copy-secondary mt-2 text-[13px] leading-relaxed">
            ~14 dBA under load. Roughly $5/month always-on.
          </p>
        </StatCard>
        <StatCard title="RAM ceiling" note="~$40 upgrade">
          <p className="headline mt-3 text-[40px]">
            64 <span className="text-[22px] text-[#6e6e73]">GB</span>
          </p>
          <p className="copy-secondary mt-2 text-[13px] leading-relaxed">
            Four DDR3-1866 ECC slots. Used RDIMMs are the cheapest upgrade
            in the fleet.
          </p>
        </StatCard>
        <StatCard title="Booted sims (planned)">
          <p className="headline mt-3 text-[40px]">
            2<span className="text-[22px] text-[#6e6e73]">–3</span>
          </p>
          <p className="copy-secondary mt-2 text-[13px] leading-relaxed">
            Same CPU-bound ceiling as the other Intel boxes, once it gets
            Sequoia and Xcode 26.
          </p>
        </StatCard>
      </div>

      <H2>The GPUs are dying. It doesn&apos;t matter.</H2>
      <P>
        The D500 is the 6,1&apos;s famous weak point; Apple ran a repair
        program for these cards. Ours already flashed a black screen at
        login once. Headless, none of that matters: a CI box over SSH never
        touches the GPU. That&apos;s the trick with this machine: take the
        one component that fails and remove it from the job description.
      </P>

      <H2>Onboarding: same as any tree</H2>
      <P>
        The usual steps: Tailscale, Remote Login, SSH key, passwordless
        sudo, never sleep. Being a desktop it gets one bonus the laptops
        can&apos;t offer: auto-restart after a power failure.
      </P>
      <div className="my-6">
        <CopyBlock
          code={`sudo systemsetup -setremotelogin on
sudo pmset -a disablesleep 1
sudo pmset -a autorestart 1   # desktops only`}
        />
      </div>

      <H2>First job: self-hosted CI runner</H2>
      <P>
        A GitHub Actions macOS runner only needs macOS 11+, so the box is
        useful today, still on stock Monterey. Go builds don&apos;t need
        AVX2 or a GPU, and six non-throttling cores with ECC RAM make a
        better always-on runner than either laptop.
      </P>
      <div className="my-6">
        <CopyBlock
          code={`# on the trashcan: install and register the runner
./config.sh --url https://github.com/<org>/<repo> --token <token>
./svc.sh install && ./svc.sh start

# in the workflow:
runs-on: self-hosted`}
        />
      </div>

      <H2>How it stacks up on a real build</H2>
      <P>
        Same clean xcodebuild plus a simulator boot, run on every box in
        the fleet. The trashcan is the fastest Intel machine we have, and
        it holds that pace all day without a fan tantrum.
      </P>
      <div className="mt-6">
        <StatCard title="Clean build + sim boot" note="fastest Intel box">
          <Bar label="M3 Pro (build 4:18, boot 5s)" value="5:53" pct={31} />
          <Bar
            label="trashcan (build 13:10, boot 21s)"
            value="16:21"
            pct={85}
            green
          />
          <Bar label="work Mac (build 15:26, boot 29s)" value="18:12" pct={94} />
          <Bar label="eMac (build 16:31, boot 29s)" value="19:16" pct={100} />
        </StatCard>
      </div>
      <P>
        The M3 is in another league, as expected. Among the Intels, the
        trashcan beats both laptops on a single run, and the gap widens on
        back-to-back builds once the laptop coolers heat-soak.
      </P>

      <H2>Next: Sequoia and simulators</H2>
      <P>
        Stock Monterey caps it at Xcode 14, which is too old for iOS 26
        testing. OpenCore Legacy Patcher supports the 6,1 through Sequoia,
        the same OS our other Intel boxes run, which unlocks Xcode 26.3 and
        current iOS simulator runtimes. That upgrade plus a cheap RAM bump
        turns it into simulator overflow capacity behind the same manzanasd
        leases as everything else.
      </P>
      <P>
        Intel ends after this generation: no Tahoe on OCLP, and macOS 27
        drops Intel entirely. Sequoia is the end of the line, and that&apos;s
        fine. The orchard doesn&apos;t need its trees to be young, just to
        hold a lease and do the work.
      </P>
    </article>
  );
}
