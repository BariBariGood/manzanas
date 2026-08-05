import Reveal from "./Reveal";

const stats = [
  {
    big: "N agents",
    small: "share one Mac's simulators.",
  },
  {
    big: "0 collisions",
    small: "the daemon decides who owns what, and everyone else waits in line.",
  },
  {
    big: "1 protocol",
    small: "the CLI, MCP tools, and browser viewer all speak the same API.",
  },
];

export default function Problem() {
  return (
    <section id="problem" className="bg-white py-24 sm:py-32">
      <div className="mx-auto max-w-[980px] px-6">
        <Reveal>
          <h2 className="headline headline-xl text-center text-[40px] sm:text-[64px]">
            N agents. One Mac.
            <br />
            <span className="phrase-gray">Chaos, until now.</span>
          </h2>
          <p className="copy-secondary mx-auto mt-7 max-w-[640px] text-center text-[19px] leading-relaxed sm:text-[21px]">
            Run two agents on the same Mac and they boot each other&apos;s
            simulators and type into the wrong window. manzanasd is the
            referee: one agent per simulator, everyone else queues.
          </p>
        </Reveal>

        <div className="mt-16 grid gap-x-10 gap-y-12 sm:grid-cols-3">
          {stats.map((s, i) => (
            <Reveal key={s.big} delay={i * 0.08}>
              <div className="hairline-top pt-6">
                <p className="headline text-[44px] sm:text-[52px]">
                  <span className="phrase-tint">{s.big}</span>
                </p>
                <p className="copy-secondary mt-3 text-[15px] leading-relaxed">
                  {s.small}
                </p>
              </div>
            </Reveal>
          ))}
        </div>
      </div>
    </section>
  );
}
