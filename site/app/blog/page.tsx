import Link from "next/link";

const posts: {
  slug: string;
  date: string;
  title: string;
  teaser: string;
  fieldNote?: boolean;
}[] = [
  {
    slug: "v0-5-0-one-call-runs",
    date: "August 11, 2026",
    title: "v0.5.0: one call from clean simulator to evidence",
    teaser:
      "POST /v0/runs takes a declarative YAML run-spec and executes the whole agent loop — lease, boot, install, launch, steps, artifacts, release — with the lease always released and a PR-ready evidence export attached, even on failure.",
  },
  {
    slug: "testing-ios-without-a-mac",
    date: "August 11, 2026",
    title: "Testing iOS apps without a Mac fleet: mock mode",
    teaser:
      "manzanasd --mock runs the daemon on any Linux box with a full deterministic action backend — the real observe/tap/audit handlers against a synthetic app screen. A tutorial from raw HTTP to a one-call run with evidence.",
  },
  {
    slug: "why-agents-need-simulator-leases",
    date: "August 11, 2026",
    title: "Why agents need simulator leases",
    teaser:
      "Two agents on one Mac will boot each other's simulators and type into the wrong window. Why locks fail, why leases (TTL + FIFO queues) work, and how ~0.28 s warm-pool thaws make short leases actually viable.",
  },
  {
    slug: "field-notes-aug-3",
    date: "August 3, 2026",
    title: "Field notes: real devices, a smarter broker, video",
    fieldNote: true,
    teaser:
      "Wave 3 in one day — 79 files, +6,808 lines: physical iPhones as leasable targets (devicectl + WebDriverAgent), warm-first broker placement on daemon-truth load, an embedded /dash dashboard, per-lease video capture, and a Go 1.22 toolchain quietly shipping binaries that crash on macOS 26.",
  },
  {
    slug: "field-notes-aug-2",
    date: "August 2, 2026",
    title: "Field notes: optimizing the fleet",
    fieldNote: true,
    teaser:
      "Seven measured findings in one post: apsd taking idle sims from 213% to 1–6% CPU, 26ms thaws vs multi-second boots, golden images that silently lost their slimming, boot storms, 26x smaller screenshots, and a workstation that never throttles.",
  },
  {
    slug: "trashcan-mac-pro",
    date: "August 1, 2026",
    title: "Adding a trashcan to the orchard",
    teaser:
      "A 2013 Mac Pro 6,1 joins the fleet as a headless always-on CI runner: dying GPUs, non-throttling cores, and the OCLP path to modern Xcode.",
  },
  {
    slug: "sequoia-on-shitboxes",
    date: "July 29, 2026",
    title: "Growing an orchard out of two $250 shitboxes",
    teaser:
      "Two retired 2017 MacBook Pros, OpenCore Legacy Patcher, Tailscale, and the measurements from turning them into a simulator farm for AI agents.",
  },
];

export default function BlogIndex() {
  return (
    <section className="mx-auto max-w-[720px] px-6 pb-24 pt-16 sm:pt-24">
      <h1 className="headline headline-xl text-[40px] sm:text-[56px]">Blog</h1>
      <p className="copy-secondary mt-4 text-[18px] leading-relaxed">
        Notes from building and running an orchard of Macs and simulators.
      </p>
      <div className="mt-12 space-y-8">
        {posts.map((p) => (
          <Link
            key={p.slug}
            href={`/blog/${p.slug}`}
            className="tile tile-hover block p-8"
          >
            <div className="flex items-center gap-3">
              {p.fieldNote ? (
                <span className="rounded-full bg-[#c22214]/10 px-2.5 py-0.5 font-mono text-[11px] font-semibold text-[#c22214]">
                  Field notes
                </span>
              ) : null}
              <p className="font-mono text-[12px] text-[#6e6e73]">{p.date}</p>
            </div>
            <h2 className="headline mt-2 text-[24px] sm:text-[28px]">
              {p.title}
            </h2>
            <p className="copy-secondary mt-3 text-[16px] leading-relaxed">
              {p.teaser}
            </p>
            <p className="mt-4 text-[14px] font-medium text-[#c22214]">
              Read the post →
            </p>
          </Link>
        ))}
      </div>
    </section>
  );
}
