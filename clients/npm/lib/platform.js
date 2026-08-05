"use strict";

const path = require("path");

// The npm package version tracks the manzanasd release version 1:1
// (esbuild-style): manzanasd-client@0.2.0 downloads manzanas v0.2.0.
const VERSION = require("../package.json").version;
const REPO = "BariBariGood/manzanas";

const OS_MAP = { darwin: "darwin", linux: "linux" };
const ARCH_MAP = { x64: "amd64", arm64: "arm64" };

function target() {
  const os = OS_MAP[process.platform];
  const arch = ARCH_MAP[process.arch];
  if (!os || !arch) {
    throw new Error(
      `manzanasd-client: unsupported platform ${process.platform}/${process.arch}`
    );
  }
  return { os, arch };
}

function tarballURL() {
  const { os, arch } = target();
  return (
    `https://github.com/${REPO}/releases/download/` +
    `v${VERSION}/manzanasd_${VERSION}_${os}_${arch}.tar.gz`
  );
}

function checksumsURL() {
  return (
    `https://github.com/${REPO}/releases/download/v${VERSION}/checksums.txt`
  );
}

function binaryPath() {
  return path.join(__dirname, "..", "bin-native", "manzanas");
}

module.exports = { VERSION, REPO, target, tarballURL, checksumsURL, binaryPath };
