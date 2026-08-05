// Command manzanas-broker federates multiple manzanasd daemons: it merges
// target enumeration, schedules lease acquisition onto the least-loaded
// matching host, and proxies per-lease ops to the owning daemon. It is
// cross-platform (typically run on a Linux box on the same tailnet as the
// Mac fleet). See docs/broker.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/BariBariGood/manzanas/internal/broker"
	"github.com/BariBariGood/manzanas/proto"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

// hostFlags collects repeatable --host flags.
type hostFlags []broker.HostConfig

func (f *hostFlags) String() string {
	var parts []string
	for _, h := range *f {
		parts = append(parts, h.Name+"="+h.Addr)
	}
	return strings.Join(parts, ";")
}

func (f *hostFlags) Set(spec string) error {
	hc, err := broker.ParseHostSpec(spec)
	if err != nil {
		return err
	}
	*f = append(*f, hc)
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	var hosts hostFlags
	var (
		addr = flag.String("addr", envOr("MANZANAS_BROKER_ADDR", ":7440"),
			"listen address (env MANZANAS_BROKER_ADDR)")
		configPath = flag.String("config", envOr("MANZANAS_BROKER_CONFIG", ""),
			"JSON config file with {\"hosts\":[{\"name\",\"addr\",\"labels\"}]} (env MANZANAS_BROKER_CONFIG)")
		probeInterval = flag.Duration("probe-interval", broker.DefaultProbeInterval,
			"health-check interval per host")
		probeTimeout = flag.Duration("probe-timeout", broker.DefaultProbeTimeout,
			"health-check timeout per probe")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Var(&hosts, "host",
		"daemon to front, as [name=]addr[,label,...]; repeatable (env MANZANAS_BROKER_HOSTS, ';'-separated)")
	flag.Parse()

	if *showVersion {
		fmt.Printf("manzanas-broker %s (protocol %s)\n", version, proto.Version)
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	var cfg broker.Config
	if *configPath != "" {
		fileCfg, err := broker.LoadConfigFile(*configPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		cfg.Hosts = append(cfg.Hosts, fileCfg.Hosts...)
	}
	if env := os.Getenv("MANZANAS_BROKER_HOSTS"); env != "" && len(hosts) == 0 {
		envHosts, err := broker.ParseHostSpecs(env)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		cfg.Hosts = append(cfg.Hosts, envHosts...)
	}
	cfg.Hosts = append(cfg.Hosts, hosts...)
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "invalid config:", err)
		fmt.Fprintln(os.Stderr, "configure hosts via --host, --config, or MANZANAS_BROKER_HOSTS")
		os.Exit(1)
	}

	b := broker.New(cfg, log, broker.Options{
		ProbeInterval: *probeInterval,
		ProbeTimeout:  *probeTimeout,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Probe every host once before serving so the first requests see real
	// fleet health instead of an empty, all-down view.
	b.WarmUp(ctx)
	go b.Run(ctx)

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           b.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	log.Info("manzanas-broker listening", "addr", ln.Addr().String(), "hosts", len(cfg.Hosts), "protocol", proto.Version)
	if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	stop()
	<-shutdownDone
}
