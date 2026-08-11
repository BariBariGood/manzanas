import Reveal from "./Reveal";
import CopyBlock from "./CopyBlock";

const SPEC = `# login-smoke.yaml
name: login-smoke
target:
  labels: [ios26]
app:
  path: /Users/ci/builds/MyApp.app
  bundle_id: com.example.myapp
steps:
  - action: tap_element
    with: {id: username}
  - action: type_into_element
    with: {id: username, text: agent}
  - action: type_into_element
    with: {id: password, text: hunter2}
  - action: tap_element
    with: {label: "Sign In"}
  - action: wait_for_element
    with: {label: "Welcome, agent!", timeout_ms: 5000}
  - name: quality gate
    action: audit`;

const RUN = `$ manzanas run login-smoke.yaml -o evidence.md
run run_1a2b3c4d5e6f7a8b: passed
  journal run: lse_0123456789abcdef
  step 0 tap_element: ok
  step 1 type_into_element: ok
  ...
  step 5 audit: ok · 0 findings

# evidence.md is the journal's PR-ready
# markdown export — paste it into a PR.`;

export default function Runs() {
  return (
    <section id="runs" className="bg-white py-24 sm:py-32">
      <div className="mx-auto max-w-[1100px] px-6">
        <Reveal className="text-center">
          <p className="font-mono text-[13px] font-semibold text-[#c22214]">
            NEW IN v0.5.0
          </p>
          <h2 className="headline headline-xl mt-3 text-[40px] sm:text-[56px]">
            One call, from clean simulator
            <br />
            <span className="phrase-tint">to evidence.</span>
          </h2>
          <p className="copy-secondary mx-auto mt-6 max-w-[640px] text-[19px] leading-relaxed">
            A <strong>run</strong> executes the whole agent loop from a single
            request: acquire lease → boot → fixtures → install → launch →
            steps → artifacts → release. Declare it once in YAML; the daemon
            owns the choreography — and the lease is always released, whatever
            fails.
          </p>
        </Reveal>

        <Reveal delay={0.08}>
          <div className="mt-12 grid items-start gap-6 md:grid-cols-2">
            <CopyBlock code={SPEC} label="run-spec · yaml" />
            <CopyBlock code={RUN} label="manzanas run" />
          </div>
        </Reveal>

        <Reveal delay={0.12}>
          <div className="mt-8 grid gap-4 sm:grid-cols-3">
            {(
              [
                [
                  "POST /v0/runs",
                  "The wire API. Sync by default; async with polling for long runs.",
                ],
                [
                  "manzanas run spec.yaml",
                  "The CLI. -o writes the journal's markdown export for your PR.",
                ],
                [
                  "MCP run tool",
                  "Agents pass the same YAML spec; run_status polls async runs.",
                ],
              ] as const
            ).map(([title, body]) => (
              <div key={title} className="tile p-6">
                <p className="font-mono text-[13px] font-semibold text-[#c22214]">
                  {title}
                </p>
                <p className="copy-secondary mt-2 text-[14px] leading-relaxed">
                  {body}
                </p>
              </div>
            ))}
          </div>
          <p className="copy-secondary mx-auto mt-8 max-w-[640px] text-center text-[15px] leading-relaxed">
            Three frontends, one schema. All three also work pointed at a
            broker: the run is placed on a fleet host with the same warm-first
            ranking as lease scheduling.
          </p>
        </Reveal>
      </div>
    </section>
  );
}
