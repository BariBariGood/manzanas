"use client";

import { useRef, useState } from "react";
import {
  motion,
  useReducedMotion,
  useScroll,
  useTransform,
} from "framer-motion";

const INSTALL_CMD = "git clone https://github.com/BariBariGood/manzanas && cd manzanas && make build";

export default function Hero() {
  const reduce = useReducedMotion();
  const ref = useRef<HTMLDivElement>(null);
  const { scrollYProgress } = useScroll({
    target: ref,
    offset: ["start start", "end start"],
  });
  const termY = useTransform(scrollYProgress, [0, 1], [0, reduce ? 0 : -60]);
  const termScale = useTransform(scrollYProgress, [0, 1], [1, reduce ? 1 : 0.96]);

  const up = (delay: number) => ({
    initial: reduce ? false : { opacity: 0, y: 22 },
    animate: { opacity: 1, y: 0 },
    transition: { duration: 0.9, delay, ease: [0.22, 1, 0.36, 1] as const },
  });

  return (
    <section ref={ref} className="hero-glow relative overflow-hidden pb-16 pt-28 sm:pb-24 sm:pt-32">
      <div className="relative mx-auto grid max-w-[1200px] grid-cols-1 items-center gap-12 px-6 lg:grid-cols-[minmax(0,1.05fr)_minmax(0,1fr)] lg:gap-14">
        <div className="min-w-0 text-center lg:text-left">
          <motion.p
            {...up(0)}
            className="text-lg font-semibold tracking-tight text-[#1d1d1f] sm:text-xl"
          >
            manzanas
          </motion.p>

          <motion.h1
            {...up(0.06)}
            className="headline headline-xl mt-3 text-[40px] sm:text-[64px] lg:text-[72px]"
          >
            <span className="block">Tends your orchard</span>
            <span className="phrase-tint block">of Macs and simulators.</span>
          </motion.h1>

          <motion.p
            {...up(0.16)}
            className="copy-secondary mx-auto mt-6 max-w-[560px] text-[18px] leading-relaxed sm:text-[20px] lg:mx-0"
          >
            One daemon per Mac. Your AI agents share its simulators
            without stepping on each other.
          </motion.p>

          <motion.div
            {...up(0.26)}
            className="mt-8 flex flex-wrap items-center justify-center gap-4 lg:justify-start"
          >
            <a href="#quickstart" className="btn btn-primary">
              Get started
            </a>
            <a
              href="https://github.com/BariBariGood/manzanas"
              target="_blank"
              rel="noreferrer"
              className="btn btn-secondary"
            >
              View on GitHub
            </a>
          </motion.div>

          <motion.div {...up(0.34)} className="mt-7 flex justify-center lg:justify-start">
            <InstallChip />
          </motion.div>
        </div>

        <motion.div
          {...up(0.42)}
          style={{ y: termY, scale: termScale }}
          className="mx-auto w-full min-w-0 max-w-[560px]"
        >
          <TerminalCard />
        </motion.div>
      </div>
    </section>
  );
}

function InstallChip() {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(INSTALL_CMD);
      setCopied(true);
      setTimeout(() => setCopied(false), 1600);
    } catch {
      /* clipboard unavailable */
    }
  };
  return (
    <button
      onClick={copy}
      aria-label="Copy install command"
      className="group flex max-w-full items-center gap-3 rounded-full border border-black/10 bg-black/[0.04] px-4 py-2 backdrop-blur transition-colors hover:border-black/20 hover:bg-black/[0.07]"
    >
      <code className="truncate font-mono text-[12px] text-[#424245]">
        git clone …/manzanas && make build
      </code>
      <span className="shrink-0 rounded-full bg-black/[0.07] px-2 py-0.5 text-[10px] font-medium text-[#1d1d1f]">
        {copied ? "Copied" : "Copy"}
      </span>
    </button>
  );
}

function TerminalCard() {
  return (
    <div className="overflow-hidden rounded-2xl border border-white/[0.07] bg-[#161617] text-left shadow-[0_60px_140px_-40px_rgba(0,0,0,0.9),0_0_80px_-30px_rgba(52,199,89,0.25)]">
      <div className="flex items-center gap-2 border-b border-white/[0.06] px-4 py-3">
        <span className="h-3 w-3 rounded-full bg-[#ff5f57]" />
        <span className="h-3 w-3 rounded-full bg-[#febc2e]" />
        <span className="h-3 w-3 rounded-full bg-[#28c840]" />
        <span className="ml-3 font-mono text-xs text-[#6e6e73]">
          manzanasd — :7433
        </span>
      </div>
      <pre className="code-scroll px-5 pb-6 pt-4 font-mono text-[11px] leading-[1.75] text-[#d6d6d9] sm:px-6 sm:text-[12.5px]">
        <code>{`$ ./bin/manzanasd --addr :7433
time=02:17:09 level=INFO msg="manzanasd listening" addr=:7433 protocol=v0

$ ./bin/manzanas lease acquire --labels ios26 --agent claude-1
`}</code>
        <code className="text-[#34c759]">{`lease lse_9f2 active on iPhone 17 Pro (ttl 300s)`}</code>
        <code>{`

$ ./bin/manzanas lease acquire --labels ios26 --agent codex-2
`}</code>
        <code className="text-[#6e6e73]">{`lease lse_a41 queued at position 1`}</code>
        <code>{`
$ `}</code>
        <span className="caret" aria-hidden />
      </pre>
    </div>
  );
}
