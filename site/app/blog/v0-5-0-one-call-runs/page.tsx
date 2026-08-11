import type { Metadata } from "next";
import { H2, P } from "../../../components/BlogKit";
import CopyBlock from "../../../components/CopyBlock";

export const metadata: Metadata = {
  title:
    "manzanas v0.5.0: one call from clean simulator to evidence — manzanas blog",
  description:
    "v0.5.0 ships the one-call runs API: POST /v0/runs takes a declarative YAML run-spec and executes the whole agent loop — lease, boot, install, launch, steps, artifacts, release — returning a PR-ready evidence export. Plus a fleet dashboard, broker-transparent clients, and a deterministic mock backend.",
};

const SPEC = `# login-smoke.yaml
name: login-smoke
target:
  labels: [ios26]
app:
  path: /Users/ci/builds/MyApp.app   # .app bundle on the daemon host
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
  step 2 type_into_element: ok
  step 3 tap_element: ok
  step 4 wait_for_element: ok
  step 5 audit: ok`;

const STEP = `steps:
  - name: optional human label
    action: tap_element        # any action kind: tap, swipe, type,
    with: {id: username}       #   tap_element, wait_for_element,
    timeout_seconds: 15        #   scroll_to_element, observe,
    continue_on_error: false   #   screenshot, audit, batch, ...`;

export default function Post() {
  return (
    <article className="mx-auto max-w-[720px] px-6 pb-24 pt-16 sm:pt-24">
      <p className="font-mono text-[12px] text-[#6e6e73]">
        August 11, 2026 · BariBariGood
      </p>
      <h1 className="headline headline-xl mt-3 text-[36px] sm:text-[52px]">
        v0.5.0: one call from clean simulator to evidence
      </h1>
      <P>
        Until now, driving a simulator through manzanas meant hand-sequencing
        the loop yourself: acquire a lease, boot the target, apply fixtures,
        install the app, launch it, dispatch a dozen actions, capture
        artifacts, and — on <em>every</em> failure path — remember to release
        the lease. Every agent I watched do this wrote the same boilerplate,
        and every one of them occasionally forgot the release and orphaned a
        simulator.
      </P>
      <P>
        v0.5.0 makes the whole loop one call. A <strong>run</strong> is a
        declarative spec; the daemon owns the choreography:
      </P>
      <div className="mt-6">
        <CopyBlock
          code={
            "acquire lease → boot → fixtures → install app → launch → steps →\nartifact capture → release lease (applying its reset)"
          }
          label="the run lifecycle"
        />
      </div>

      <H2>The YAML run-spec</H2>
      <P>
        You describe what to lease, what to install, and what to do — the
        native step DSL is just the existing action surface, one action per
        step, dispatched and journaled exactly like{" "}
        <code className="font-mono text-[14px]">POST /v0/actions</code>:
      </P>
      <div className="mt-6">
        <CopyBlock code={SPEC} label="login-smoke.yaml" />
      </div>
      <P>Then:</P>
      <div className="mt-6">
        <CopyBlock code={RUN} label="one call" />
      </div>
      <P>
        <code className="font-mono text-[14px]">-o evidence.md</code> writes
        the journal&apos;s markdown export — the same document as{" "}
        <code className="font-mono text-[14px]">
          GET /v0/journal/{"{run}"}/export.md
        </code>{" "}
        — with every step, tree hash, screenshot, and audit finding. Paste it
        into a PR comment and the run argues for itself.
      </P>

      <H2>Steps are actions, not a new language</H2>
      <P>
        I deliberately did not invent a test framework. Each step is one
        action kind from the protocol, with the payload passed through
        verbatim:
      </P>
      <div className="mt-6">
        <CopyBlock code={STEP} label="step schema" />
      </div>
      <P>
        Steps stop at the first failure (later steps report{" "}
        <code className="font-mono text-[14px]">skipped</code>);{" "}
        <code className="font-mono text-[14px]">continue_on_error</code> lets
        cleanup steps run anyway. Whatever fails — a step, a boot, an install,
        the run budget expiring — the lease is <em>always</em> released with
        its reset. The evidence trail of a red run is as complete as a green
        one.
      </P>

      <H2>Three frontends, one schema</H2>
      <P>
        The same spec drives{" "}
        <code className="font-mono text-[14px]">POST /v0/runs</code> (the wire
        API, sync by default, async with polling for long runs),{" "}
        <code className="font-mono text-[14px]">manzanas run spec.yaml</code>{" "}
        (the CLI), and the MCP <code className="font-mono text-[14px]">run</code>{" "}
        tool — so an agent that speaks MCP gets the one-call loop for free.
        All three also work pointed at a broker: the run is placed on a fleet
        host with the same warm-first ranking as lease scheduling, then
        proxied to the owning daemon.
      </P>

      <H2>Also in v0.5.0</H2>
      <P>
        <strong>Broker-transparent clients.</strong> The CLI and MCP server
        now follow a broker lease&apos;s{" "}
        <code className="font-mono text-[14px]">host_addr</code> annotation
        automatically — point them at the broker and every lease-scoped call
        routes to the owning daemon. <strong>Fleet dashboard.</strong> The
        broker serves an aggregated{" "}
        <code className="font-mono text-[14px]">/dash</code> across every
        daemon, next to each daemon&apos;s own.{" "}
        <strong>Version surfacing + optional auth.</strong> Binaries report
        the build version stamped at link time, and{" "}
        <code className="font-mono text-[14px]">--auth-token</code> puts a
        shared bearer token in front of the whole API.{" "}
        <strong>Mock actions backend.</strong>{" "}
        <code className="font-mono text-[14px]">--mock</code> now carries a
        full deterministic action backend, so the entire loop — including
        runs — works on a Linux box with no Mac at all. That one gets{" "}
        <a className="underline" href="/blog/testing-ios-without-a-mac">
          its own post
        </a>
        .
      </P>
      <P>
        v0.5.0 is on{" "}
        <a className="underline" href="https://github.com/BariBariGood/manzanas">
          GitHub
        </a>{" "}
        and the Homebrew tap. Full run-spec reference in{" "}
        <a
          className="underline"
          href="https://github.com/BariBariGood/manzanas/blob/main/docs/runs.md"
        >
          docs/runs.md
        </a>
        .
      </P>
    </article>
  );
}
