export function H2({ children }: { children: React.ReactNode }) {
  return (
    <h2 className="headline mt-12 text-[26px] sm:text-[30px]">{children}</h2>
  );
}

export function P({ children }: { children: React.ReactNode }) {
  return (
    <p className="copy-secondary mt-5 text-[17px] leading-relaxed">
      {children}
    </p>
  );
}

export function Bar({
  label,
  value,
  pct,
  green,
}: {
  label: string;
  value: string;
  pct: number;
  green?: boolean;
}) {
  return (
    <div className="mt-3">
      <div className="flex items-baseline justify-between font-mono text-[12px]">
        <span className="text-[#424245]">{label}</span>
        <span className={green ? "font-semibold text-[#c22214]" : "text-[#6e6e73]"}>
          {value}
        </span>
      </div>
      <div className="mt-1.5 h-2.5 overflow-hidden rounded-full bg-black/[0.06]">
        <div
          className={`h-full rounded-full ${green ? "bg-[#e0301e]" : "bg-[#86868b]"}`}
          style={{ width: pct === 0 ? "0%" : `${Math.max(pct, 1.5)}%` }}
        />
      </div>
    </div>
  );
}

export function StatCard({
  title,
  note,
  children,
}: {
  title: string;
  note?: string;
  children: React.ReactNode;
}) {
  return (
    <div className="tile p-6 sm:p-7">
      <div className="flex items-baseline justify-between gap-3">
        <p className="headline text-[17px]">{title}</p>
        {note ? (
          <span className="whitespace-nowrap rounded-full bg-[#c22214]/10 px-2.5 py-0.5 font-mono text-[11px] font-semibold text-[#c22214]">
            {note}
          </span>
        ) : null}
      </div>
      {children}
    </div>
  );
}

export function FieldNote({ date }: { date: string }) {
  return (
    <div className="flex items-center gap-3">
      <span className="rounded-full bg-[#c22214]/10 px-2.5 py-0.5 font-mono text-[11px] font-semibold text-[#c22214]">
        Field notes
      </span>
      <p className="font-mono text-[12px] text-[#6e6e73]">{date}</p>
    </div>
  );
}
