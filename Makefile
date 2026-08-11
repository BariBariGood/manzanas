GO ?= go

# Build version stamped into all three binaries (git describe, falling
# back to "dev" outside a git checkout). Override with `make VERSION=...`.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/BariBariGood/manzanas/internal/buildinfo.Version=$(VERSION)

.PHONY: build test vet lint clean

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/manzanasd ./cmd/manzanasd
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/manzanas ./cmd/manzanas
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/manzanas-broker ./cmd/manzanas-broker

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

lint: vet
	test -z "$$(gofmt -l .)"

clean:
	rm -rf bin

# Reproduce the README's latency numbers on your Mac (see eval/bench/bench.sh).
bench:
	eval/bench/bench.sh
