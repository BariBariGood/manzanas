# Contributing to manzanasd

Thanks for your interest! Issues and pull requests are welcome.

## Building and testing

Pure Go (module `github.com/BariBariGood/manzanas`); a recent stable Go
toolchain is all you need. Everything runs on Linux — no Mac required
for the core gate:

```sh
go build ./...
go vet ./...
go test ./...
gofmt -l .        # must print nothing
```

`make build` produces `bin/manzanasd`, `bin/manzanas`, `bin/manzanas-broker`;
`make lint` runs vet + the gofmt check. Unit tests are Linux-safe by
design (mock registry, httptest fake daemons); macOS-only behavior sits
behind interfaces with fakes — please keep it that way.

The marketing site in `site/` is a Next.js app; if you touch it, verify
with `cd site && npm ci && npm run build`.

## Conventions

- **Protocol changes**: the Go types in `proto/` are the source of
  truth; `proto/PROTOCOL.md` describes semantics. Changes within v0
  must be additive. The same rule applies to the journal format
  (`docs/journal.md`).
- The daemon owns all stateful policy in Go (unit-testable on Linux);
  helpers like `simbridge` stay deliberately dumb.
- Keep PRs focused; run the full gate above before opening one.

## Security issues

Please do not open public issues for vulnerabilities — see
[SECURITY.md](SECURITY.md).
