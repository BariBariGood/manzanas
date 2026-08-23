import type { Metadata } from "next";
import { H2, P } from "../../../components/BlogKit";
import CopyBlock from "../../../components/CopyBlock";

export const metadata: Metadata = {
  title:
    "manzanas v0.7.0: teaching the agent to see — manzanas blog",
  description:
    "v0.7.0 ships the resolver ladder — accessibility first, then element map, template/NCC image match, and OCR as the last resort — plus deterministic UI asserts, a doctor command, and a one-command setup path via npx.",
};

const RESOLVER = `# every action resolves through the ladder, cheapest reliable layer first
manzanas tap_element {label: "Sign In"}     # 1. accessibility tree (fastest, exact)
manzanas tap_element {id: "login-button"}   #    falls through on miss...

# 2. element map — stable per-app element ids across launches
# 3. template match — normalized cross-correlation on the frame (NCC threshold)
# 4. OCR — text exists somewhere, but no structure; last resort, always reported

# whichever layer resolved, the journal records it:
$ manzanas journal tail $MANZANAS_LEASE
lse_9f2 seq=14 tap_element label="Sign In" resolved_by=ax elapsed_ms=36 ok=true`;

const ASSERT = `- name: quality gate
  assert:
    - visible: {label: "Welcome, agent!"}
    - not_visible: {label: "Invalid credentials"}
    - touch_target: {label: "Sign In", min_pt: 44}
    - contrast: {label: "Welcome, agent!", min_ratio: 4.5}`;

export default function Post() {
  return (
    <article className="prose mx-auto px-6 py-16">
      <P>
        Every agent-driving-a-simulator demo you have seen quietly skips the
        hard part: <strong>how does the agent know what it is looking at?</strong>{" "}
        Coordinates break when layout moves. Screenshots-and-OCR guess. v0.7.0
        is about replacing guessing with resolution — and then checking the
        result mechanically.
      </P>

      <H2>The resolver ladder</H2>
      <P>
        Element references now resolve through four layers, tried cheapest and
        most-reliable first:
      </P>
      <ol className="my-4 list-decimal space-y-1 pl-6 text-[15px]">
        <li>
          <strong>Accessibility tree</strong> — labels and ids straight from
          the AX tree. Exact, fast (~36 ms warm), and stable across layout
          changes.
        </li>
        <li>
          <strong>Element map</strong> — persistent per-app element ids that
          survive relaunches, so a spec keeps working across runs.
        </li>
        <li>
          <strong>Template match</strong> — grayscale normalized
          cross-correlation against a captured template, for custom-drawn
          controls the AX tree never exposes.
        </li>
        <li>
          <strong>OCR</strong> — text-only fallback. Always reported as such,
          so you know when your spec is standing on sand.
        </li>
      </ol>
      <CopyBlock code={RESOLVER} />

      <H2>Deterministic asserts</H2>
      <P>
        Resolution answers &quot;where is it.&quot; Asserts answer &quot;is it
        right.&quot; The same run-spec can now carry mechanical checks —
        visibility, absence, touch-target size, label contrast — that fail the
        run with an annotated screenshot in the journal instead of passing
        silently while the button moved.
      </P>
      <CopyBlock code={ASSERT} />
      <P>
        These are the checks that catch what green unit tests never see: the
        light-mode button nobody re-checked, the tap target that shrank under
        44pt, the label that lost contrast after a color-token refactor.
      </P>

      <H2>Also in v0.7.0</H2>
      <ul className="my-4 list-disc space-y-1 pl-6 text-[15px]">
        <li>
          <code>manzanas doctor</code> — one-shot setup diagnosis where every
          failure names its fix.
        </li>
        <li>
          <code>npx manzanasd-client@latest doctor</code> — the whole install
          collapsed to one line; the npm wrapper fetches the right binary for
          your platform and proxies everything.
        </li>
        <li>
          Setup wizard, wait-for-pixel-stability, scroll gestures with
          momentum, and mirrord capture/input hardening across three macOS
          capture backends.
        </li>
      </ul>

      <H2>Get it</H2>
      <P>
        Everything above is in the released v0.7.0 binaries, the homebrew tap,
        and npm today.
      </P>
      <CopyBlock
        code={`npx manzanasd-client@latest doctor`}
      />
      <P>
        Full changelog on the{" "}
        <a className="underline" href="https://github.com/BariBariGood/manzanas/releases/tag/v0.7.0">
          v0.7.0 release
        </a>
        .
      </P>
    </article>
  );
}
