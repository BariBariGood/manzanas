import type { Metadata } from "next";
import { Inter } from "next/font/google";
import "./globals.css";

const inter = Inter({
  subsets: ["latin"],
  display: "swap",
  variable: "--font-inter",
});

export const metadata: Metadata = {
  metadataBase: new URL("https://manzanasd.vercel.app"),
  title: "manzanas — tends your orchard of Macs and simulators",
  description:
    "Multi-agent iOS simulator fleet orchestration. Leases, queues, live MJPEG streaming, deterministic state, and an evidence journal — MIT-licensed, MCP-native.",
  openGraph: {
    title: "manzanas",
    description:
      "The daemon that tends your orchard of Macs and simulators. Fleet leasing, live streaming, and deterministic state for AI agents.",
    type: "website",
    url: "https://manzanasd.vercel.app",
    siteName: "manzanas",
    images: [{ url: "/og.png", width: 1200, height: 630 }],
  },
  twitter: {
    card: "summary_large_image",
    title: "manzanas",
    description:
      "The daemon that tends your orchard of Macs and simulators.",
    images: ["/og.png"],
  },
};

export default function RootLayout({
  children,
}: Readonly<{ children: React.ReactNode }>) {
  return (
    <html lang="en" className={inter.variable}>
      <body className="font-sans">{children}</body>
    </html>
  );
}
