GO ?= go

.PHONY: build test vet lint clean

build:
	$(GO) build -o bin/manzanasd ./cmd/manzanasd
	$(GO) build -o bin/manzanas ./cmd/manzanas
	$(GO) build -o bin/manzanas-broker ./cmd/manzanas-broker

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
