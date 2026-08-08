"use client";

import { useRef, useState } from "react";
import {
  motion,
  useReducedMotion,
  useScroll,
  useTransform,
} from "framer-motion";

const INSTALL_CMD = "git clone https://github.com/BariBariGood/manzanas && cd manzanas && make build";
const FILM_URL =
  "https://github.com/user-attachments/assets/deb577d0-1123-4a89-90d8-8d9c7684a3cf";

export default function Hero() {
  const reduce = useReducedMotion();
  const ref = useRef<HTMLDivElement>(null);
  const { scrollYProgress } = useScroll({
    target: ref,
    offset: ["start start", "end start"],
  });
  const filmY = useTransform(scrollYProgress, [0, 1], [0, reduce ? 0 : -60]);
  const filmScale = useTransform(scrollYProgress, [0, 1], [1, reduce ? 1 : 0.97]);

  const up = (delay: number) => ({
    initial: reduce ? false : { opacity: 0, y: 22 },
    animate: { opacity: 1, y: 0 },
    transition: { duration: 0.9, delay, ease: [0.22, 1, 0.36, 1] as const },
  });

  return (
    <section ref={ref} className="hero-glow relative overflow-hidden pb-16 pt-28 sm:pb-24 sm:pt-32">
      <div className="relative mx-auto max-w-[1200px] px-6 text-center">
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
          className="copy-secondary mx-auto mt-6 max-w-[560px] text-[18px] leading-relaxed sm:text-[20px]"
        >
          One daemon per Mac. Your AI agents share its simulators
          without stepping on each other.
        </motion.p>

        <motion.div
          {...up(0.26)}
          className="mt-8 flex flex-wrap items-center justify-center gap-4"
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

        <motion.div {...up(0.34)} className="mt-7 flex justify-center">
          <InstallChip />
        </motion.div>

        <motion.div
          {...up(0.42)}
          style={{ y: filmY, scale: filmScale }}
          className="mx-auto mt-14 w-full max-w-[980px]"
        >
          <FilmCard />
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

function FilmCard() {
  return (
    <div className="overflow-hidden rounded-2xl border border-white/[0.07] bg-[#161617] shadow-[0_60px_140px_-40px_rgba(0,0,0,0.9),0_0_80px_-30px_rgba(224,48,30,0.25)]">
      <div className="flex items-center gap-2 border-b border-white/[0.06] px-4 py-3">
        <span className="h-3 w-3 rounded-full bg-[#ff5f57]" />
        <span className="h-3 w-3 rounded-full bg-[#febc2e]" />
        <span className="h-3 w-3 rounded-full bg-[#28c840]" />
        <span className="ml-3 font-mono text-xs text-[#6e6e73]">
          manzanas — launch film
        </span>
      </div>
      <video
        src={FILM_URL}
        className="block w-full"
        autoPlay
        muted
        loop
        playsInline
        controls
        preload="metadata"
      />
    </div>
  );
}
