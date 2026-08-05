"use client";

import { useState } from "react";

export default function CopyBlock({
  code,
  label,
}: {
  code: string;
  label?: string;
}) {
  const [copied, setCopied] = useState(false);

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(code);
      setCopied(true);
      setTimeout(() => setCopied(false), 1600);
    } catch {
      /* clipboard unavailable */
    }
  };

  return (
    <div className="overflow-hidden rounded-2xl border border-black/[0.06] bg-[#161617]">
      <div className="flex items-center justify-between border-b border-white/[0.07] px-4 py-2">
        <span className="font-mono text-[11px] text-[#6e6e73]">
          {label ?? "shell"}
        </span>
        <button
          onClick={copy}
          aria-label="Copy to clipboard"
          className="rounded-full bg-white/[0.08] px-3 py-1 text-xs text-[#d6d6d9] transition-colors hover:bg-white/[0.16] hover:text-white"
        >
          {copied ? "Copied" : "Copy"}
        </button>
      </div>
      <pre className="code-scroll px-5 py-4 font-mono text-[12.5px] leading-[1.75] text-[#d6d6d9]">
        <code>{code}</code>
      </pre>
    </div>
  );
}
