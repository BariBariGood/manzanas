import type { Metadata } from "next";
import { H2, P } from "../../../components/BlogKit";
import CopyBlock from "../../../components/CopyBlock";

export const metadata: Metadata = {
  title: "Testing iOS apps without a Mac fleet: mock mode — manzanas blog",
  description:
    "A tutorial: run manzanasd --mock on any Linux box and drive the full agent loop — lease, observe, tap_element, type, wait_for_element, audit, screenshot — against a deterministic synthetic app screen, then wrap it all in one runs-API call.",
};

const START = `git clone https://github.com/BariBariGood/manzanas
cd manzanas && make build

./bin/manzanasd --addr :7433 --mock
# 3 mock simulators, full action backend, journal enabled`;

const LOOP = `D=http://localhost:7433
L=$(curl -s -X POST $D/v0/leases \\
  -d '{"labels":["ios26"],"agent_id":"demo","ttl_seconds":300}')
LID=$(echo "$L" | jq -r .id); UDID=$(echo "$L" | jq -r .target_udid)
curl -s -X POST $D/v0/targets/$UDID/boot -d "{\\"lease_id\\":\\"$LID\\"}"

curl -s -X POST $D/v0/actions:batch -d "{
  \\"lease_id\\":\\"$LID\\", \\"stop_on_error\\":true, \\"actions\\":[
   {\\"kind\\":\\"type_into_element\\",\\"payload\\":{\\"id\\":\\"username\\",\\"text\\":\\"agent\\",\\"require_focus\\":true}},
   {\\"kind\\":\\"type_into_element\\",\\"payload\\":{\\"id\\":\\"password\\",\\"text\\":\\"pw\\"}},
   {\\"kind\\":\\"tap_element\\",\\"payload\\":{\\"label\\":\\"Sign In\\"}},
   {\\"kind\\":\\"wait_for_element\\",\\"payload\\":{\\"label\\":\\"Welcome, agent!\\",\\"timeout_ms\\":5000}},
   {\\"kind\\":\\"audit\\",\\"payload\\":{\\"inline\\":false}},
   {\\"kind\\":\\"screenshot\\",\\"payload\\":{\\"inline\\":false}}]}"

curl -s -X DELETE $D/v0/leases/$LID`;

const RUNSPEC = `# mock-smoke.yaml
name: mock-smoke
target:
  labels: [ios26]
steps:
  - action: type_into_element
    with: {id: username, text: agent, require_focus: true}
  - action: type_into_element
    with: {id: password, text: pw}
  - action: tap_element
    with: {label: "Sign In"}
  - action: wait_for_element
    with: {label: "Welcome, agent!", timeout_ms: 5000}
  - name: quality gate
    action: audit`;

const RUNIT = `$ ./bin/manzanas run mock-smoke.yaml -o evidence.md
run run_…: passed
  journal run: lse_…
  target: MOCK-UDID-1
  step 0 type_into_element: ok
  ...
  step 4 audit: ok`;

export default function Post() {
  return (
    <article className="mx-auto max-w-[720px] px-6 pb-24 pt-16 sm:pt-24">
      <p className="font-mono text-[12px] text-[#6e6e73]">
        August 11, 2026 · BariBariGood
      </p>
      <h1 className="headline headline-xl mt-3 text-[36px] sm:text-[52px]">
        Testing iOS apps without a Mac fleet: mock mode
      </h1>
      <P>
        Most of the code in an agent-drives-a-simulator pipeline isn&apos;t
        macOS code. It&apos;s leases, queues, action payloads, wait loops,
        journal exports — protocol and logic. But testing any of it used to
        require a Mac with Xcode, which means CI needs Mac runners and
        contributors on Linux can&apos;t run anything at all. v0.5.0&apos;s{" "}
        <code className="font-mono text-[14px]">--mock</code> fixes that: the
        daemon runs anywhere, and mock targets carry a{" "}
        <strong>full deterministic action backend</strong>, not just a fake
        target list.
      </P>

      <H2>Start the daemon</H2>
      <div className="mt-6">
        <CopyBlock code={START} label="any linux box, ci, a codespace" />
      </div>
      <P>
        On a non-macOS host the daemon falls back to mock mode automatically;
        the flag just makes it explicit. You get three mock simulators, and
        every one of them renders the same synthetic app: a 390×844 login
        screen with a username field, a password field, a Wi-Fi switch, a
        Sign In button, and a footer that starts below the fold so scrolling
        is real. State transitions are synchronous and deterministic —
        repeated runs produce identical trees, hashes, and screenshots.
      </P>

      <H2>Drive the full loop</H2>
      <P>
        This is the same wire protocol an agent uses against a real Mac —
        lease, boot, batch of actions, release:
      </P>
      <div className="mt-6">
        <CopyBlock code={LOOP} label="the raw-HTTP version" />
      </div>
      <P>
        The interesting part is what&apos;s <em>not</em> mocked. The action
        pipeline reuses the production backend handlers — observe compaction,
        the predicate/matcher DSL, wait loops, composite element actions,
        batches, audit checks, screenshot transcoding — with only the
        simulator process boundary replaced by an in-process synthetic app.
        The <code className="font-mono text-[14px]">audit</code> step runs the
        real checks over the synthetic tree and returns findings with an
        annotated screenshot. What CI exercises through mock mode is the same
        code an agent hits in production, minus the simulator itself.
      </P>

      <H2>Now make it one call</H2>
      <P>
        Raw HTTP is for understanding; day to day you&apos;d wrap the loop in
        a <a className="underline" href="/blog/v0-5-0-one-call-runs">run</a>:
      </P>
      <div className="mt-6">
        <CopyBlock code={RUNSPEC} label="mock-smoke.yaml" />
      </div>
      <div className="mt-4">
        <CopyBlock code={RUNIT} label="one call" />
      </div>
      <P>
        <code className="font-mono text-[14px]">evidence.md</code> is the
        journal&apos;s PR-ready markdown export — steps, tree hashes, the
        audit findings, the screenshots. Since mock screenshots are rendered
        from the synthetic tree, the pixels match the accessibility tree
        byte-for-byte on every run, which makes it a nice fixture for testing
        your <em>own</em> evidence tooling too.
      </P>

      <H2>What it&apos;s for (and not for)</H2>
      <P>
        I use mock mode for three things: CI for the daemon and clients on
        Linux runners; developing agent prompts and run-specs without burning
        a real simulator lease; and demos — the entire quickstart on this
        site works on a $5 VPS. What it is <em>not</em> for is performance
        numbers (mock latency is near-zero; use a real host and{" "}
        <code className="font-mono text-[14px]">make bench</code>) or testing
        your actual app — there is one fixed synthetic screen, and{" "}
        <code className="font-mono text-[14px]">launch_app</code> resets it
        whatever bundle ID you pass. When you outgrow it, point the same
        specs at a real daemon and nothing else changes. Details in{" "}
        <a
          className="underline"
          href="https://github.com/BariBariGood/manzanas/blob/main/docs/mock.md"
        >
          docs/mock.md
        </a>
        .
      </P>
    </article>
  );
}
