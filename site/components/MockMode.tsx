import Reveal from "./Reveal";
import CopyBlock from "./CopyBlock";

const CODE = `git clone https://github.com/BariBariGood/manzanas
cd manzanas && make build

# no Mac, no Xcode — a deterministic mock fleet:
./bin/manzanasd --addr :7433 --mock

# the full loop against a synthetic app screen:
cat > smoke.yaml <<'EOF'
name: smoke
target: {labels: [ios26]}
steps:
  - action: type_into_element
    with: {id: username, text: agent}
  - action: type_into_element
    with: {id: password, text: pw}
  - action: tap_element
    with: {label: "Sign In"}
  - action: wait_for_element
    with: {label: "Welcome, agent!", timeout_ms: 5000}
  - action: audit
EOF
./bin/manzanas run smoke.yaml -o evidence.md`;

export default function MockMode() {
  return (
    <section id="mock" className="bg-[#f5f5f7] py-24 sm:py-32">
      <div className="mx-auto max-w-[980px] px-6">
        <Reveal className="text-center">
          <h2 className="headline headline-xl text-[40px] sm:text-[56px]">
            Try it in 60 seconds.
            <br />
            <span className="phrase-gray">No Mac required.</span>
          </h2>
          <p className="copy-secondary mx-auto mt-6 max-w-[620px] text-[19px] leading-relaxed">
            <code className="font-mono text-[16px]">--mock</code> runs the
            daemon anywhere — Linux, CI, your laptop — with a full mock action
            backend. The same lease → observe → tap → audit code an agent hits
            on a real Mac, against a deterministic synthetic app screen.
          </p>
        </Reveal>

        <Reveal delay={0.08}>
          <div className="mx-auto mt-12 max-w-[760px]">
            <CopyBlock code={CODE} label="60-second quickstart · linux / ci / anywhere" />
          </div>
        </Reveal>
      </div>
    </section>
  );
}
