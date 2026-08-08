import Reveal from "./Reveal";

const daemonRows: [string, string][] = [
  ["registry", "sims via simctl · boot / shutdown / health"],
  ["lease", "TTL · labels · FIFO queues"],
  ["actions", "HID + a11y observe (AXe) · screenshots"],
  ["stream", "MJPEG · N viewers per target"],
  ["state", "snapshot / restore · fixtures"],
  ["journal", "actions + tree hashes + artifacts"],
];

const clientRows = [
  "CLI — lease / tap / type / observe",
  "MCP server — manzanas mcp (stdio)",
  "stream viewer — browser",
];

export default function Architecture() {
  return (
    <section id="architecture" className="bg-white py-24 sm:py-32">
      <div className="mx-auto max-w-[1200px] px-6">
        <Reveal className="text-center">
          <h2 className="headline headline-xl text-[40px] sm:text-[56px]">
            Thin clients.
            <br />
            <span className="phrase-gray">One stateful daemon per Mac.</span>
          </h2>
          <p className="copy-secondary mx-auto mt-6 max-w-[600px] text-[19px] leading-relaxed">
            Agents and humans talk to the daemon over one simple JSON API —
            HTTP for requests, WebSocket for streams.
          </p>
        </Reveal>

        <Reveal delay={0.1}>
          {/* Desktop / tablet: SVG diagram */}
          <div className="well mt-12 hidden overflow-hidden p-6 md:block sm:p-10">
            <Diagram />
          </div>
          {/* Mobile: stacked HTML layout */}
          <div className="mt-12 space-y-3 md:hidden">
            <MobileBox
              eyebrow="LINUX · CI · ANYWHERE"
              title="manzanas"
              subtitle="thin client · Go · single binary"
              rows={clientRows}
            />
            <Wire />
            <MobileBox
              eyebrow="EACH MAC HOST"
              title="manzanasd"
              subtitle="Go daemon · owns everything stateful"
              rows={daemonRows.map(([n, d]) => `${n} — ${d}`)}
              mono
            />
            <Wire />
            <MobileBox
              eyebrow="SIMULATORS"
              title="iOS simulators"
              subtitle="leased · queued · idle — sims + devices"
              rows={[]}
            />
          </div>
        </Reveal>
      </div>
    </section>
  );
}

function Wire() {
  return (
    <div className="flex flex-col items-center py-1" aria-hidden>
      <span className="h-6 w-px bg-gradient-to-b from-transparent via-[#c22214] to-transparent" />
      <span className="font-mono text-[10px] font-semibold text-[#c22214]">
        HTTP + WS · /v0 · JSON
      </span>
      <span className="h-6 w-px bg-gradient-to-b from-transparent via-[#c22214] to-transparent" />
    </div>
  );
}

function MobileBox({
  eyebrow,
  title,
  subtitle,
  rows,
  mono = false,
}: {
  eyebrow: string;
  title: string;
  subtitle: string;
  rows: string[];
  mono?: boolean;
}) {
  return (
    <div className="tile p-6">
      <p className="font-mono text-[10px] tracking-[0.2em] text-[#6e6e73]">
        {eyebrow}
      </p>
      <p className="headline mt-2 text-[20px]">{title}</p>
      <p className="copy-secondary mt-0.5 text-[13px]">{subtitle}</p>
      {rows.length > 0 && (
        <div className="mt-4 space-y-2">
          {rows.map((r) => (
            <div
              key={r}
              className={`rounded-lg bg-[#f5f5f7] px-3 py-2 text-[12px] text-[#424245] ${
                mono ? "font-mono" : ""
              }`}
            >
              {r}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function Diagram() {
  const green = "#c22214";
  const dim = "#424245";
  const dimmer = "#86868b";

  return (
    <svg
      viewBox="0 0 920 460"
      role="img"
      aria-label="manzanasd architecture: thin clients on Linux/CI connect over HTTP and WebSocket to the manzanasd daemon on each Mac host, which owns the registry, lease manager, actions, streaming, state, and journal, driving iOS simulators"
      className="w-full"
    >
      <defs>
        <linearGradient id="wire" x1="0" y1="0" x2="1" y2="0">
          <stop offset="0" stopColor={green} stopOpacity="0.1" />
          <stop offset="0.5" stopColor={green} stopOpacity="0.9" />
          <stop offset="1" stopColor={green} stopOpacity="0.1" />
        </linearGradient>
      </defs>

      {/* client box */}
      <rect x="24" y="90" width="270" height="280" rx="24" fill="#fff" stroke="rgba(0,0,0,0.06)" />
      <text x="159" y="72" textAnchor="middle" fill={dimmer} fontSize="11" letterSpacing="2.5" fontFamily="ui-monospace, SF Mono, Menlo, monospace">
        LINUX · CI · ANYWHERE
      </text>
      <text x="159" y="128" textAnchor="middle" fill="#1d1d1f" fontSize="18" fontWeight="600" fontFamily="-apple-system, Inter, sans-serif">
        manzanas
      </text>
      <text x="159" y="148" textAnchor="middle" fill={dimmer} fontSize="12" fontFamily="-apple-system, Inter, sans-serif">
        thin client · Go · single binary
      </text>
      {clientRows.map((t, i) => (
        <g key={t}>
          <rect x="46" y={172 + i * 54} width="226" height="40" rx="12" fill="#f5f5f7" />
          <text x="159" y={197 + i * 54} textAnchor="middle" fill={dim} fontSize="12.5" fontFamily="-apple-system, Inter, sans-serif">
            {t}
          </text>
        </g>
      ))}
      <text x="159" y="392" textAnchor="middle" fill={dimmer} fontSize="12" fontFamily="-apple-system, Inter, sans-serif">
        any № of agents + humans
      </text>

      {/* wire */}
      <line x1="294" y1="228" x2="376" y2="228" stroke="url(#wire)" strokeWidth="2" />
      <circle cx="298" cy="228" r="3.5" fill={green} className="packet-dot" />
      <text x="335" y="206" textAnchor="middle" fill={green} fontSize="11.5" fontWeight="600" fontFamily="ui-monospace, SF Mono, Menlo, monospace">
        HTTP + WS
      </text>
      <text x="335" y="254" textAnchor="middle" fill={dimmer} fontSize="10.5" fontFamily="ui-monospace, SF Mono, Menlo, monospace">
        /v0 · JSON
      </text>

      {/* daemon box */}
      <rect x="376" y="40" width="330" height="392" rx="24" fill="#fff" stroke="rgba(0,0,0,0.06)" />
      <text x="541" y="26" textAnchor="middle" fill={dimmer} fontSize="11" letterSpacing="2.5" fontFamily="ui-monospace, SF Mono, Menlo, monospace">
        EACH MAC HOST
      </text>
      <text x="541" y="76" textAnchor="middle" fill="#1d1d1f" fontSize="18" fontWeight="600" fontFamily="-apple-system, Inter, sans-serif">
        manzanasd
      </text>
      <text x="541" y="96" textAnchor="middle" fill={dimmer} fontSize="12" fontFamily="-apple-system, Inter, sans-serif">
        Go daemon · owns everything stateful
      </text>
      {daemonRows.map(([name, desc], i) => (
        <g key={name}>
          <rect x="398" y={114 + i * 50} width="286" height="38" rx="12" fill="#fdf1e7" />
          <text x="414" y={138 + i * 50} fill={green} fontSize="12" fontWeight="600" fontFamily="ui-monospace, SF Mono, Menlo, monospace">
            {name}
          </text>
          <text x="486" y={138 + i * 50} fill={dim} fontSize="11.5" fontFamily="-apple-system, Inter, sans-serif">
            {desc}
          </text>
        </g>
      ))}

      {/* wire to sims */}
      <line x1="706" y1="228" x2="770" y2="228" stroke="url(#wire)" strokeWidth="2" />

      {/* simulators */}
      <text x="833" y="96" textAnchor="middle" fill={dimmer} fontSize="11" letterSpacing="2.5" fontFamily="ui-monospace, SF Mono, Menlo, monospace">
        SIMULATORS
      </text>
      {[2, 1, 0].map((i) => (
        <rect
          key={i}
          x={788 + i * 10}
          y={118 + i * 14}
          width="76"
          height="150"
          rx="16"
          fill={i === 0 ? "#fdf1e7" : "#f5f5f7"}
          stroke="rgba(0,0,0,0.05)"
        />
      ))}
      <rect x="796" y="132" width="60" height="98" rx="8" fill="rgba(194,34,20,0.12)" />
      <circle cx="826" cy="248" r="5" fill="none" stroke={green} strokeOpacity="0.5" />
      <text x="833" y="316" textAnchor="middle" fill={dimmer} fontSize="11.5" fontFamily="-apple-system, Inter, sans-serif">
        leased · queued · idle
      </text>
      <text x="833" y="352" textAnchor="middle" fill={dimmer} fontSize="11.5" fontFamily="-apple-system, Inter, sans-serif">
        sims + devices
      </text>
    </svg>
  );
}
