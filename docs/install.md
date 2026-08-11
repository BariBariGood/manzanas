# Installing manzanasd

`manzanasd` runs on each Mac host; the `manzanas` CLI runs anywhere. Default
port is `7433`.

## Homebrew (recommended)

Formulas live in the [BariBariGood/homebrew-tap](https://github.com/BariBariGood/homebrew-tap)
repo:

```sh
brew tap baribarigood/tap https://github.com/BariBariGood/homebrew-tap
brew trust baribarigood/tap      # Homebrew >= 6 requires trusting third-party taps
brew install manzanasd            # daemon (+ pulls in the manzanas CLI)
brew services start manzanasd     # launchd via brew services, port 7433

curl -s localhost:7433/v0/healthz   # {"ok":true,...}
manzanas --version
```

The formulas build from source at a release tag. The
`manzanasd` service definition mirrors
[`deploy/com.baribarigood.manzanasd.plist`](../deploy/com.baribarigood.manzanasd.plist):
RunAtLoad, KeepAlive unless clean exit, port 7433, logs under
`$(brew --prefix)/var/log/manzanasd.{out,err}.log`.

Stop/remove:

```sh
brew services stop manzanasd
brew uninstall manzanasd manzanas
brew untap baribarigood/tap      # optional
```

Canonical formula copies are kept in
[`deploy/homebrew/`](../deploy/homebrew/) and mirrored to the tap; bump
`tag:`/`revision:` in both on each release.

## launchd (release tarball or local build)

Each release tarball ships the binaries plus `deploy/install.sh`, a
LaunchAgent template, and `deploy/uninstall.sh`:

```sh
tar xzf manzanasd_<version>_darwin_arm64.tar.gz
cd deploy && ./install.sh --binary ../manzanasd [--port 7433]
```

`install.sh`:

- installs the binary to `~/bin/manzanasd`
- writes `~/Library/LaunchAgents/com.baribarigood.manzanasd.plist`
  (RunAtLoad + KeepAlive, logs to `~/.manzanasd/logs/`)
- sets up log rotation (a daily per-user LaunchAgent that copy-truncates
  logs over 10MB in place; no sudo needed and safe while launchd holds
  the log file descriptors)
- loads the agent and health-checks `GET /v0/healthz`

Remove everything with `./uninstall.sh` (add `--purge` to also delete
`~/.manzanasd` logs/state).

## Manual

```sh
# from a release
tar xzf manzanasd_<version>_<os>_<arch>.tar.gz
./manzanasd --addr :7433            # on a Mac with Xcode
./manzanasd --addr :7433 --mock     # anywhere, mock fleet

# from source
make build && ./bin/manzanasd --addr :7433
```

Check the install (`~/bin` is not on the default macOS PATH, so use the
full path or add `export PATH="$HOME/bin:$PATH"` to your shell profile):

```sh
~/bin/manzanasd --version
curl -s localhost:7433/v0/healthz
```

`--version` (and the `build` field in `/v0/healthz` and `/v0/status`)
reports the build version stamped at link time: release binaries carry
the release tag, `make build` stamps `git describe`, and a plain
`go build` shows `dev`.

### Optional shared-token auth

By default the daemon has no auth — access control is the network
boundary (tailnet/loopback). As defense in depth you can require a
shared bearer token:

```sh
manzanasd --auth-token "$(openssl rand -hex 24)"   # or env MANZANASD_AUTH_TOKEN
```

With a token set, every endpoint except `GET /v0/healthz` requires
`Authorization: Bearer <token>` (or `?token=` for browser-only URLs).
Clients pass it with `manzanas --token <t>` (env `MANZANAS_TOKEN`); the
dashboard prompts for it and keeps it in localStorage.

## manzanas CLI via npm

The `manzanasd-client` package is **not yet published to npm**, and its
postinstall downloads the `manzanas` binary from the GitHub Release
matching the package version — so it needs a release tag matching the
package version. Until it is published, verify it locally:

```sh
cd clients/npm
MANZANAS_BINARY_PATH=/path/to/manzanas npm install   # skip the download
npm pack                                            # package verification
```

After the first release is published:

```sh
npx manzanasd-client --version
```

See [`clients/npm/`](../clients/npm/README.md).

## GitHub Actions

Use the composite action to lease a simulator in CI; see
[`action/README.md`](../action/README.md).
