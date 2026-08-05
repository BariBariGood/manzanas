#!/usr/bin/env node
"use strict";

// Proxy all args to the downloaded native manzanas binary.

const fs = require("fs");
const { spawnSync } = require("child_process");
const { binaryPath } = require("../lib/platform");

const bin = process.env.MANZANAS_BINARY_PATH || binaryPath();
if (!fs.existsSync(bin)) {
  console.error(
    "manzanasd-client: manzanas binary missing; re-run `npm install` (postinstall downloads it)."
  );
  process.exit(1);
}

const res = spawnSync(bin, process.argv.slice(2), { stdio: "inherit" });
process.exit(res.status === null ? 1 : res.status);
