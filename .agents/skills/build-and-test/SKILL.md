---
name: build-and-test
description: Build, lint, and test manzanasd - the Go toolchain gate every PR must pass, the CI matrix (Ubicloud Linux + self-hosted manzanasd-m3 macOS runner), and how to build the simbridge helper. Use before opening any PR to this repo or when debugging CI failures.
---

# Build and test manzanasd

Pure Go (module `github.com/BariBariGood/manzanas`, needs a recent stable Go
toolchain). Everything below runs on Linux — no Mac required for the core
gate.

## The PR gate — run ALL of these before opening a PR

```sh
go build ./...
go vet ./...
go test ./...
gofmt -l .        # must print NOTHING; fix with gofmt -w on the listed files
```

`make build` produces `bin/manzanasd`, `bin/manzanas`, `bin/manzanas-broker`;
`make lint` = vet + gofmt check. Unit tests are Linux-safe by design (mock
registry, httptest fake daemons); code that needs macOS is behind interfaces
(e.g. `warm.Host`) with fakes — keep it that way.

## CI (.github/workflows/ci.yml)

The test job runs the same four commands on a two-leg matrix:

- `ubicloud-standard-2` — Linux. **Org rule: always Ubicloud labels for
  Linux jobs; GitHub-hosted runner billing is broken.**
- `[self-hosted, macOS, ARM64, manzanasd-m3]` — the self-hosted runner on the
  fleet M3. **Never switch macOS jobs to GitHub-hosted macOS.**

manzanasd-m3 runner notes:

- `setup-go` uses `go-version: "stable"`, NOT 1.22 — the Go 1.22 linker
  emits binaries without `LC_UUID`, which macOS 26 refuses to execute. Don't
  pin the version down.
- Actions cache is intentionally disabled on the m3 leg (it keeps its own
  module/build cache on disk; uploading ~388 MB wastes 1-3 min/run).
- The runner shares a maintainer's daily-driver M3: keep macOS-leg tests fast
  and never add steps that boot simulators or need sudo there.

Other workflows: `release.yml` (GoReleaser on `v*` tags), `site.yml`
(`site/` npm build; only triggers on site changes).

## simbridge (warm-actions helper)

CI cannot build it (needs macOS + Xcode + AXe frameworks). Build on a Mac:

```sh
helpers/simbridge/build.sh ~/bin/simbridge          # AXe frameworks at ~/axe/Frameworks
AXE_FRAMEWORKS=/opt/axe/Frameworks helpers/simbridge/build.sh   # other installs
```

The binary rpath-pins the frameworks dir it was built against — build on (or
for) the host that runs it, one binary per architecture. If the shipped
`.swiftmodule`s don't match the local Swift compiler, rebuild the AXe
frameworks first (see `docs/actions-warm.md`).

## Conventions

- Protocol changes: `proto/` Go types are the source of truth;
  `proto/PROTOCOL.md` describes semantics. Within v0 changes must be
  additive. Same rule for the journal format (`docs/journal.md`).
- The daemon owns all stateful policy in Go (unit-testable on Linux);
  helpers like simbridge stay deliberately dumb.
