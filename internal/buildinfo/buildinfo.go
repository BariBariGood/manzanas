// Package buildinfo carries the build version stamped into every binary
// at link time. All three commands (manzanasd, manzanas, manzanas-broker)
// share this single variable so one -X flag stamps them all.
package buildinfo

// Version is stamped at build time via
//
//	-ldflags "-X github.com/BariBariGood/manzanas/internal/buildinfo.Version=..."
//
// (see the Makefile and .goreleaser.yaml). It stays "dev" for plain
// `go build` / `go run` builds.
var Version = "dev"
