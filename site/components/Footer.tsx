import Reveal from "./Reveal";
import { AppleLeaf } from "./Nav";

const REPO = "https://github.com/BariBariGood/manzanas";

export default function Footer() {
  return (
    <>
      <section className="relative overflow-hidden bg-[#f5f5f7] py-24 sm:py-32">
        <div
          className="pointer-events-none absolute inset-0"
          style={{
            background:
              "radial-gradient(720px 380px at 50% 108%, rgba(224,48,30,0.1), transparent 70%)",
          }}
          aria-hidden
        />
        <div className="relative mx-auto max-w-[820px] px-6 text-center">
          <Reveal>
            <AppleLeaf className="mx-auto h-9 w-9 text-[#c22214]" />
            <h2 className="headline headline-xl mx-auto mt-8 text-[40px] sm:text-[56px]">
              Open source.
              <br />
              <span className="phrase-gray">Protocol-first. Apache-2.0.</span>
            </h2>
            <p className="copy-secondary mx-auto mt-6 max-w-[540px] text-[19px] leading-relaxed">
              The protocol is a published spec — anyone can build on it.
              Free, open, and yours to run.
            </p>
            <div className="mt-10 flex flex-wrap items-center justify-center gap-4">
              <a
                href={REPO}
                target="_blank"
                rel="noreferrer"
                className="btn btn-primary"
              >
                Star on GitHub
              </a>
              <a
                href={`${REPO}/blob/main/proto/PROTOCOL.md`}
                target="_blank"
                rel="noreferrer"
                className="btn btn-secondary"
              >
                Read the protocol spec
              </a>
            </div>
          </Reveal>
        </div>
      </section>

      <footer className="border-t border-[#d2d2d7] bg-white">
        <div className="mx-auto flex max-w-[1200px] flex-col items-center justify-between gap-4 px-6 py-8 sm:flex-row">
          <div className="flex items-center gap-3">
            <AppleLeaf className="h-4 w-4 text-[#c22214]" />
            <span className="text-xs font-semibold text-[#1d1d1f]">manzanas</span>
            <span className="text-xs text-[#6e6e73]">· Apache-2.0 License</span>
            <a href="https://allmcps.com/mcp/manzanas">
              <img
                src="https://allmcps.com/api/badge/manzanas?style=directory"
                alt="AllMCPs"
                height="40"
                className="h-7 w-auto"
              />
            </a>
          </div>
          <div className="flex items-center gap-6 text-xs text-[#6e6e73]">
            <a
              href="/blog"
              className="transition-colors hover:text-[#1d1d1f]"
            >
              Blog
            </a>
            <a
              href={`${REPO}/blob/main/docs/architecture.md`}
              target="_blank"
              rel="noreferrer"
              className="transition-colors hover:text-[#1d1d1f]"
            >
              Architecture
            </a>
            <a
              href={`${REPO}/blob/main/proto/PROTOCOL.md`}
              target="_blank"
              rel="noreferrer"
              className="transition-colors hover:text-[#1d1d1f]"
            >
              Protocol
            </a>
            <a
              href={REPO}
              target="_blank"
              rel="noreferrer"
              className="transition-colors hover:text-[#1d1d1f]"
            >
              GitHub
            </a>
          </div>
        </div>
      </footer>
    </>
  );
}
