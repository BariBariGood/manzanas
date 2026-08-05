"use client";

import { useEffect, useState } from "react";

const links = [
  { href: "#problem", label: "Why" },
  { href: "#pillars", label: "Pillars" },
  { href: "#architecture", label: "Architecture" },
  { href: "#streaming", label: "Streaming" },
  { href: "#quickstart", label: "Quickstart" },
];

export default function Nav() {
  const [active, setActive] = useState("");
  const [scrolled, setScrolled] = useState(false);

  useEffect(() => {
    const sections = links
      .map((l) => document.querySelector<HTMLElement>(l.href))
      .filter((el): el is HTMLElement => el !== null);

    const onScroll = () => {
      setScrolled(window.scrollY > 8);
      const y = window.scrollY + window.innerHeight * 0.35;
      let current = "";
      for (const s of sections) {
        if (s.offsetTop <= y) current = `#${s.id}`;
      }
      setActive(current);
    };
    onScroll();
    window.addEventListener("scroll", onScroll, { passive: true });
    return () => window.removeEventListener("scroll", onScroll);
  }, []);

  return (
    <header
      className={`nav-blur fixed inset-x-0 top-0 z-50 border-b transition-colors duration-300 ${
        scrolled ? "border-black/10" : "border-transparent"
      }`}
    >
      <nav className="mx-auto flex h-12 max-w-5xl items-center justify-between px-5 sm:px-6">
        <a href="#" className="flex items-center gap-2">
          <AppleLeaf className="h-[17px] w-[17px] text-[#1d1d1f]" />
          <span className="text-[15px] font-semibold tracking-tight text-[#1d1d1f]">
            manzanas
          </span>
        </a>
        <div className="hidden items-center gap-8 md:flex">
          {links.map((l) => (
            <a
              key={l.href}
              href={l.href}
              data-active={active === l.href}
              className="nav-link text-xs text-[#424245] transition-colors hover:text-[#1d1d1f]"
            >
              {l.label}
            </a>
          ))}
          <a
            href="/blog"
            className="nav-link text-xs text-[#424245] transition-colors hover:text-[#1d1d1f]"
          >
            Blog
          </a>
        </div>
        <a
          href="https://github.com/BariBariGood/manzanas"
          target="_blank"
          rel="noreferrer"
          className="rounded-full bg-[#1d1d1f] px-3.5 py-1 text-xs font-medium text-white transition-colors hover:bg-[#37373a]"
        >
          GitHub
        </a>
      </nav>
    </header>
  );
}

export function AppleLeaf({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 24 24" fill="none" className={className} aria-hidden>
      <path
        d="M12 21c-4.5 0-8-3.4-8-8.2C4 8 7 5.2 12 5.2S20 8 20 12.8c0 4.8-3.5 8.2-8 8.2Z"
        fill="currentColor"
        opacity="0.9"
      />
      <path
        d="M12.2 5.4c.2-2 1.6-3.3 3.6-3.4-.1 2-1.5 3.3-3.6 3.4Z"
        fill="currentColor"
      />
    </svg>
  );
}
