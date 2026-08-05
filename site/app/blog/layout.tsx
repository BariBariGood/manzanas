import Link from "next/link";
import type { Metadata } from "next";
import { AppleLeaf } from "../../components/Nav";

export const metadata: Metadata = {
  title: "Blog — manzanas",
  description: "Notes from building and running an orchard of Macs and simulators.",
};

export default function BlogLayout({
  children,
}: Readonly<{ children: React.ReactNode }>) {
  return (
    <div className="bg-white">
      <header className="nav-blur fixed inset-x-0 top-0 z-50 border-b border-black/10">
        <nav className="mx-auto flex h-12 max-w-5xl items-center justify-between px-5 sm:px-6">
          <Link href="/" className="flex items-center gap-2">
            <AppleLeaf className="h-[17px] w-[17px] text-[#1d1d1f]" />
            <span className="text-[15px] font-semibold tracking-tight text-[#1d1d1f]">
              manzanas
            </span>
          </Link>
          <div className="flex items-center gap-6">
            <Link
              href="/blog"
              className="text-xs text-[#424245] transition-colors hover:text-[#1d1d1f]"
            >
              Blog
            </Link>
            <a
              href="https://github.com/BariBariGood/manzanas"
              target="_blank"
              rel="noreferrer"
              className="rounded-full bg-[#1d1d1f] px-3.5 py-1 text-xs font-medium text-white transition-colors hover:bg-[#37373a]"
            >
              GitHub
            </a>
          </div>
        </nav>
      </header>
      <main className="pt-12">{children}</main>
      <footer className="border-t border-[#d2d2d7] bg-white">
        <div className="mx-auto flex max-w-[720px] items-center justify-between px-6 py-8">
          <div className="flex items-center gap-2">
            <AppleLeaf className="h-4 w-4 text-[#248a3d]" />
            <span className="text-xs font-semibold text-[#1d1d1f]">manzanas</span>
            <span className="text-xs text-[#6e6e73]">· MIT License</span>
          </div>
          <Link
            href="/"
            className="text-xs text-[#6e6e73] transition-colors hover:text-[#1d1d1f]"
          >
            manzanas home →
          </Link>
        </div>
      </footer>
    </div>
  );
}
