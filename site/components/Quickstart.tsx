import Reveal from "./Reveal";
import CopyBlock from "./CopyBlock";

const steps = [
  {
    n: "01",
    title: "Build and run the daemon",
    body: "Install from the tap (or make build) and run it on a Mac — or anywhere with a mock fleet.",
    label: "install & run",
    code: `brew tap baribarigood/tap \\
  https://github.com/BariBariGood/homebrew-tap
brew trust baribarigood/tap
brew install manzanasd
brew services start manzanasd   # port 7433

# anywhere (Linux/dev/CI), with a mock fleet:
./bin/manzanasd --addr :7433 --mock`,
  },
  {
    n: "02",
    title: "Lease a simulator and act",
    body: "Ask for a simulator. If it's busy, you queue. Then tap, type, and observe.",
    label: "manzanas cli",
    code: `./bin/manzanas lease acquire \\
  --labels ios26 --agent claude-1

export MANZANAS_LEASE=lse_9f2
./bin/manzanas tap 187 412
./bin/manzanas type "hello manzanas"
./bin/manzanas observe`,
  },
  {
    n: "03",
    title: "Watch live, keep the evidence",
    body: "Or declare the whole run in YAML. Watch live, follow the journal, or serve the fleet as MCP tools.",
    label: "run · journal · mcp",
    code: `# or skip steps 2–3: one call, whole run
./bin/manzanas run spec.yaml -o evidence.md

./bin/manzanas stream url --lease $MANZANAS_LEASE
./bin/manzanas journal tail $MANZANAS_LEASE

# serve MCP tools over stdio:
./bin/manzanas mcp`,
  },
];

export default function Quickstart() {
  return (
    <section id="quickstart" className="bg-white py-24 sm:py-32">
      <div className="mx-auto max-w-[980px] px-6">
        <Reveal className="text-center">
          <h2 className="headline headline-xl text-[40px] sm:text-[56px]">
            Up and running
            <br />
            <span className="phrase-tint">in three steps.</span>
          </h2>
        </Reveal>

        <div className="mt-14 space-y-10">
          {steps.map((s, i) => (
            <Reveal key={s.n} delay={i * 0.05}>
              <div className="grid items-start gap-6 md:grid-cols-[240px_1fr] md:gap-10">
                <div>
                  <p className="font-mono text-[13px] font-semibold text-[#c22214]">
                    {s.n}
                  </p>
                  <h3 className="headline mt-1 text-[22px]">{s.title}</h3>
                  <p className="copy-secondary mt-2 text-[15px] leading-relaxed">
                    {s.body}
                  </p>
                </div>
                <CopyBlock code={s.code} label={s.label} />
              </div>
            </Reveal>
          ))}
        </div>
      </div>
    </section>
  );
}
