const consolidatedSlugs = [
  "boot-storms-not-steady-state",
  "designing-for-a-40ms-link",
  "the-workstation-that-never-throttles",
  "the-golden-image-that-lost-its-slimming",
  "pause-dont-reboot",
  "the-push-daemon-that-ate-our-fleet",
  "warm-sims-and-golden-images",
];

/** @type {import('next').NextConfig} */
const nextConfig = {
  output: "export",
  images: { unoptimized: true },
  async redirects() {
    return consolidatedSlugs.map((slug) => ({
      source: `/blog/${slug}`,
      destination: "/blog/field-notes-aug-2",
      permanent: true,
    }));
  },
};

export default nextConfig;
