import Reveal from "./Reveal";

export default function Streaming() {
  return (
    <section id="streaming" className="bg-[#f5f5f7] py-20 sm:py-28">
      <div className="mx-auto max-w-[1200px] px-6">
        <Reveal className="text-center">
          <h2 className="headline headline-xl text-[40px] sm:text-[56px]">
            Watch your agents work.
            <br />
            <span className="phrase-gray">Live, in any browser.</span>
          </h2>
          <p className="copy-secondary mx-auto mt-6 max-w-[620px] text-[19px] leading-relaxed">
            Open a stream on any leased simulator and watch it live —
            while the agent keeps working.
          </p>
        </Reveal>

        <Reveal delay={0.1}>
          <ViewerMock />
        </Reveal>
      </div>
    </section>
  );
}

function ViewerMock() {
  return (
    <div className="mx-auto mt-12 max-w-[900px] overflow-hidden rounded-3xl border border-black/[0.06] bg-[#161617] shadow-[0_40px_100px_-40px_rgba(0,0,0,0.4)]">
      {/* browser chrome */}
      <div className="flex items-center gap-2 border-b border-white/[0.06] px-4 py-3 sm:px-5">
        <span className="h-3 w-3 rounded-full bg-[#ff5f57]" />
        <span className="h-3 w-3 rounded-full bg-[#febc2e]" />
        <span className="h-3 w-3 rounded-full bg-[#28c840]" />
        <span className="ml-3 min-w-0 truncate rounded-md bg-white/[0.06] px-3 py-1 font-mono text-[11px] text-[#9a9aa0]">
          mac-01.local:7433/v0/streams/str_4kd/mjpeg
        </span>
      </div>

      <div className="flex flex-col gap-6 p-6 sm:flex-row sm:gap-8 sm:p-10">
        {/* phone */}
        <div className="relative mx-auto w-[150px] shrink-0 sm:w-[180px]">
          <div className="relative overflow-hidden rounded-[28px] border border-white/10 bg-black p-1.5 shadow-[0_0_60px_-15px_rgba(52,199,89,0.35)]">
            <div className="relative aspect-[9/19.5] overflow-hidden rounded-[22px] bg-gradient-to-b from-[#1a2b1e] via-[#12241a] to-[#0a1510]">
              <div className="absolute left-1/2 top-2 h-4 w-14 -translate-x-1/2 rounded-full bg-black" />
              <div className="absolute inset-x-3 top-10 space-y-2">
                <div className="h-7 rounded-lg bg-white/10" />
                <div className="h-16 rounded-lg bg-[#34c759]/15" />
                <div className="h-7 rounded-lg bg-white/[0.07]" />
                <div className="h-7 w-2/3 rounded-lg bg-white/[0.07]" />
              </div>
              <div className="absolute bottom-4 left-1/2 h-1 w-16 -translate-x-1/2 rounded-full bg-white/25" />
              {/* shimmer sweep */}
              <div className="pointer-events-none absolute inset-y-0 w-1/2 overflow-hidden">
                <div className="stream-shimmer h-full w-full bg-gradient-to-r from-transparent via-white/[0.05] to-transparent" />
              </div>
            </div>
          </div>
        </div>

        {/* info panel — visible at every width */}
        <div className="flex min-w-0 flex-1 flex-col justify-center gap-4">
          <div className="flex items-center gap-2 font-mono text-[12px] text-white/80">
            <span className="pulse-dot h-2 w-2 rounded-full bg-red-500" />
            LIVE · mjpeg · 30 fps
          </div>
          <div className="grid grid-cols-3 gap-3 font-mono text-[11px]">
            {[
              ["lease", "lse_9f2"],
              ["target", "iPhone 17 Pro"],
              ["viewers", "3"],
            ].map(([k, v]) => (
              <div key={k} className="rounded-xl bg-white/[0.05] px-3 py-2.5">
                <p className="text-[#6e6e73]">{k}</p>
                <p className="mt-0.5 truncate text-[#d6d6d9]">{v}</p>
              </div>
            ))}
          </div>
          <div className="rounded-xl bg-white/[0.04] p-4 font-mono text-[11px] leading-relaxed text-[#9a9aa0]">
            <p><span className="text-[#34c759]">→</span> actions.dispatch tap 187,412</p>
            <p><span className="text-[#34c759]">→</span> actions.dispatch type &quot;hello manzanas&quot;</p>
            <p><span className="text-[#6e6e73]">←</span> observe · tree hash a2ff…09be</p>
          </div>
        </div>
      </div>
    </div>
  );
}
