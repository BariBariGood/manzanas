import Reveal from "./Reveal";

export default function Pillars() {
  return (
    <section id="pillars" className="bg-[#f5f5f7] py-20 sm:py-28">
      <div className="mx-auto max-w-[1200px] px-6">
        <Reveal>
          <h2 className="headline headline-xl text-[40px] sm:text-[56px]">
            Everything stateful
            <br />
            <span className="phrase-gray">lives in the daemon.</span>
          </h2>
        </Reveal>

        {/* Bento grid */}
        <div className="mt-12 grid gap-5 lg:grid-cols-3">
          {/* Leases — large tile */}
          <Reveal className="lg:col-span-2">
            <div className="tile tile-hover flex h-full flex-col justify-between overflow-hidden p-8 sm:p-12">
              <div className="max-w-[440px]">
                <h3 className="headline text-[28px] sm:text-[32px]">Leases</h3>
                <p className="copy-secondary mt-3 text-[17px] leading-relaxed">
                  Ask for a simulator; get it — or a place in line. When a
                  lease expires, the next agent takes over.
                </p>
              </div>
              <div className="mt-10 space-y-2.5 font-mono text-[11.5px] sm:text-[13px]">
                {(
                  [
                    ["claude-1 · iPhone 17 Pro", "active · 214s", "#c22214"],
                    ["codex-2 · any ios26", "queued · #1", "#b25000"],
                    ["gemini-3 · iPad Pro", "active · 87s", "#c22214"],
                  ] as const
                ).map(([who, status, color]) => (
                  <div
                    key={who}
                    className="flex items-center justify-between gap-3 rounded-xl bg-[#f5f5f7] px-3.5 py-3 sm:px-4"
                  >
                    <span className="truncate text-[#1d1d1f]">{who}</span>
                    <span className="whitespace-nowrap" style={{ color }}>
                      {status}
                    </span>
                  </div>
                ))}
              </div>
            </div>
          </Reveal>

          {/* Streaming tile */}
          <Reveal delay={0.08}>
            <div className="tile tile-hover tile-green flex h-full flex-col p-8 sm:p-10">
              <h3 className="headline text-[28px]">Live streaming</h3>
              <p className="copy-secondary mt-3 text-[17px] leading-relaxed">
                Watch any simulator live in your browser while an agent
                drives it. MJPEG live view in the browser; per-lease H.264 recordings land in the journal.
              </p>
              <div className="mt-auto overflow-hidden rounded-xl bg-[#1d1d1f] p-4 pt-8">
                <div className="flex items-center gap-2 font-mono text-[11px] text-white/70">
                  <span className="pulse-dot h-2 w-2 rounded-full bg-red-500" />
                  LIVE · mjpeg · 3 viewers
                </div>
                <div className="mt-3 grid grid-cols-3 gap-1.5">
                  {[0.12, 0.17, 0.22].map((opacity) => (
                    <div
                      key={opacity}
                      className="h-10 rounded-md bg-[#e0301e]"
                      style={{ opacity }}
                    />
                  ))}
                </div>
              </div>
            </div>
          </Reveal>

          {/* Deterministic state */}
          <Reveal>
            <div className="tile tile-hover flex h-full flex-col p-8 sm:p-10">
              <h3 className="headline text-[28px]">Deterministic state</h3>
              <p className="copy-secondary mt-3 text-[17px] leading-relaxed">
                Snapshot a simulator, restore it later. Every retry starts
                clean.
              </p>
              <div className="mt-auto pt-8 font-mono text-[12px]">
                <div className="flex items-center gap-2">
                  <span className="h-2.5 w-2.5 rounded-full bg-[#c22214]" />
                  <span className="text-[#424245]">snap_a1 · clean install</span>
                </div>
                <div className="ml-[4px] h-4 border-l border-[#d2d2d7]" />
                <div className="flex items-center gap-2">
                  <span className="h-2.5 w-2.5 rounded-full bg-[#c22214]" />
                  <span className="text-[#424245]">snap_b7 · logged-in</span>
                </div>
                <div className="ml-[4px] h-4 border-l border-[#d2d2d7]" />
                <div className="flex items-center gap-2">
                  <span className="h-2.5 w-2.5 rounded-full border-2 border-[#c22214] bg-white" />
                  <span className="text-[#1d1d1f]">restore → snap_b7</span>
                </div>
              </div>
            </div>
          </Reveal>

          {/* Evidence journal */}
          <Reveal delay={0.06}>
            <div className="tile tile-hover flex h-full flex-col p-8 sm:p-10">
              <h3 className="headline text-[28px]">Evidence journal</h3>
              <p className="copy-secondary mt-3 text-[17px] leading-relaxed">
                Every action is recorded with proof — what changed,
                screenshots included.
              </p>
              <div className="mt-auto space-y-1.5 pt-8 font-mono text-[11px] text-[#6e6e73]">
                <p><span className="text-[#c22214]">#41</span> action.tap 187,412 · tree 4c91→a2ff</p>
                <p><span className="text-[#c22214]">#42</span> action.type &quot;hello&quot; · tree a2ff→09be</p>
                <p><span className="text-[#c22214]">#43</span> screenshot · artifacts/43.png</p>
              </div>
            </div>
          </Reveal>

          {/* MCP-native */}
          <Reveal delay={0.12}>
            <div className="tile tile-hover flex h-full flex-col p-8 sm:p-10">
              <h3 className="headline text-[28px]">MCP-native</h3>
              <p className="copy-secondary mt-3 text-[17px] leading-relaxed">
                One command — <code className="font-mono text-[14px]">manzanas mcp</code> —
                turns the fleet into MCP tools for any agent.
              </p>
              <div className="mt-auto rounded-xl bg-[#f5f5f7] p-4 pt-3 font-mono text-[11px] leading-relaxed text-[#6e6e73]">
                <p className="text-[#424245]">→ tools/call lease_acquire</p>
                <p>{`{"labels":["ios26"],"ttl_seconds":300}`}</p>
                <p className="mt-1 text-[#c22214]">← lse_9f2 · iPhone 17 Pro</p>
              </div>
            </div>
          </Reveal>
        </div>

        <Reveal delay={0.1}>
          <div className="tile mx-auto mt-12 max-w-[640px] px-8 py-8 text-center sm:px-12">
            <p className="headline text-[22px] sm:text-[24px]">
              Sims and devices. One API.
            </p>
            <p className="copy-secondary mt-2 text-[16px] leading-relaxed">
              Simulators and physical iPhones (devicectl + WebDriverAgent) — same API.
            </p>
          </div>
        </Reveal>
      </div>
    </section>
  );
}
