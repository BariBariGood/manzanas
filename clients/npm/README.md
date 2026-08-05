# manzanasd-client

Thin npm wrapper for the [`manzanas`](https://github.com/BariBariGood/manzanas)
CLI. On install it downloads the matching native `manzanas` binary from the
GitHub Release whose version equals the package version (esbuild-style),
and `manzanas` on your PATH proxies to it.

```sh
npx manzanasd-client --version
# or
npm i -g manzanasd-client && manzanas --version
```

Supported platforms: darwin/linux × x64/arm64.

For local development against an unreleased binary:

```sh
MANZANAS_BINARY_PATH=/path/to/manzanas npm install
```

If a `GITHUB_TOKEN` (or `GH_TOKEN`) is set it is used for the postinstall
download (useful for rate-limited CI).

Not yet published to npm, and the postinstall needs a published GitHub
Release matching the package version; until one exists, use
`MANZANAS_BINARY_PATH` as above and verify packaging with `npm pack`.
