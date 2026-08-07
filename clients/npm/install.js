"use strict";

// Postinstall: download the matching manzanas release tarball and extract
// the `manzanas` binary into bin-native/ (esbuild-style native binary
// distribution). Set MANZANAS_BINARY_PATH to skip the download and use a
// locally-built binary instead. While the repo is private, set
// GITHUB_TOKEN (or GH_TOKEN) so the release download is authorized.

const fs = require("fs");
const path = require("path");
const zlib = require("zlib");
const crypto = require("crypto");
const { execFileSync } = require("child_process");
const {
  VERSION,
  REPO,
  tarballURL,
  checksumsURL,
  binaryPath,
} = require("./lib/platform");

const TOKEN = process.env.GITHUB_TOKEN || process.env.GH_TOKEN;

async function download(url, headers) {
  const res = await fetch(url, { redirect: "follow", headers: headers || {} });
  if (!res.ok) {
    let hint = "";
    if (res.status === 404) {
      hint = TOKEN
        ? " (no release matching this package version, or the token lacks access)"
        : " (404 can mean the release tag does not exist yet, or the repo is" +
          " private — set GITHUB_TOKEN/GH_TOKEN to authorize the download)";
    }
    throw new Error(
      `download failed: ${res.status} ${res.statusText} for ${url}${hint}`
    );
  }
  return Buffer.from(await res.arrayBuffer());
}

// Private repos don't honor token auth on browser download URLs; resolve
// asset ids via the REST API and download through the assets endpoint.
async function downloadAsset(name) {
  const relURL = `https://api.github.com/repos/${REPO}/releases/tags/v${VERSION}`;
  const auth = { Authorization: `Bearer ${TOKEN}` };
  const rel = JSON.parse(
    (await download(relURL, { ...auth, Accept: "application/vnd.github+json" })).toString()
  );
  const asset = (rel.assets || []).find((a) => a.name === name);
  if (!asset) throw new Error(`asset ${name} not found in release v${VERSION}`);
  return download(asset.url, { ...auth, Accept: "application/octet-stream" });
}

async function fetchReleaseFile(name, publicURL) {
  return TOKEN ? downloadAsset(name) : download(publicURL);
}

function extractManzanas(tgz, dest) {
  // Minimal tar reader: find the `manzanas` entry in the gunzipped stream.
  const tar = zlib.gunzipSync(tgz);
  let off = 0;
  while (off + 512 <= tar.length) {
    const name = tar.subarray(off, off + 100).toString().replace(/\0.*$/, "");
    if (!name) break;
    const size = parseInt(
      tar.subarray(off + 124, off + 136).toString().replace(/\0.*$/, "").trim(),
      8
    );
    const typeflag = tar[off + 156];
    const isFile = typeflag === 0x30 || typeflag === 0; // '0' or NUL: regular file
    const dataStart = off + 512;
    if (isFile && (name === "manzanas" || name.endsWith("/manzanas"))) {
      fs.writeFileSync(dest, tar.subarray(dataStart, dataStart + size), {
        mode: 0o755,
      });
      return true;
    }
    off = dataStart + Math.ceil(size / 512) * 512;
  }
  return false;
}

async function main() {
  const dest = binaryPath();
  fs.mkdirSync(path.dirname(dest), { recursive: true });

  const local = process.env.MANZANAS_BINARY_PATH;
  if (local) {
    fs.copyFileSync(local, dest);
    fs.chmodSync(dest, 0o755);
    console.log(`manzanasd-client: using local binary ${local}`);
    return;
  }

  const url = tarballURL();
  const name = url.split("/").pop();
  console.log(`manzanasd-client: downloading ${url}`);
  const tgz = await fetchReleaseFile(name, url);

  // Verify against the release's published checksums.txt.
  const checksums = (
    await fetchReleaseFile("checksums.txt", checksumsURL())
  ).toString();
  const line = checksums.split("\n").find((l) => l.trim().endsWith(` ${name}`) || l.trim().endsWith(`  ${name}`));
  if (!line) throw new Error(`no checksum for ${name} in checksums.txt`);
  const expected = line.trim().split(/\s+/)[0];
  const actual = crypto.createHash("sha256").update(tgz).digest("hex");
  if (actual !== expected) {
    throw new Error(`sha256 mismatch for ${name}: expected ${expected}, got ${actual}`);
  }

  if (!extractManzanas(tgz, dest)) {
    throw new Error("manzanas binary not found in release tarball");
  }
  execFileSync(dest, ["--version"], { stdio: "inherit" });
}

main().catch((err) => {
  console.error(String(err.message || err));
  console.error(
    "manzanasd-client: install failed. Set MANZANAS_BINARY_PATH to a local manzanas binary to skip the download."
  );
  process.exit(1);
});
