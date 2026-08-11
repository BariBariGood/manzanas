// Package broker federates multiple manzanasd daemons behind one scheduling
// endpoint: it merges target enumeration, routes lease acquisition to the
// least-loaded matching host, and proxies per-lease ops to the owning
// daemon. The broker is a scheduler, not a data-plane proxy: after a lease
// is granted, clients talk to the owning daemon directly (its address is
// returned on the lease as host_addr).
package broker

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// HostConfig describes one manzanasd daemon fronted by the broker.
type HostConfig struct {
	// Name is a short label for the host (e.g. "emac"). Defaults to Addr.
	Name string `json:"name,omitempty"`
	// Addr is the daemon base URL, e.g. "http://100.64.0.1:7433".
	// A bare host:port is normalized to http://host:port.
	Addr string `json:"addr"`
	// Labels are extra labels this host contributes to all of its targets
	// (e.g. ["emac", "intel"]), matchable in lease acquisition.
	Labels []string `json:"labels,omitempty"`
	// Token is the bearer token sent on every request to this daemon,
	// for daemons running with --auth-token. Empty falls back to the
	// broker's --daemon-token, then to no auth.
	Token string `json:"token,omitempty"`
}

// Config is the broker's static host list.
type Config struct {
	Hosts []HostConfig `json:"hosts"`
}

// ParseHostSpec parses one --host flag / env entry of the form
//
//	[name=]addr[,label1,label2,...]
//
// e.g. "emac=http://100.64.0.1:7433,intel" or "localhost:7433".
func ParseHostSpec(spec string) (HostConfig, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return HostConfig{}, fmt.Errorf("empty host spec")
	}
	var hc HostConfig
	parts := strings.Split(spec, ",")
	head := parts[0]
	if name, addr, ok := strings.Cut(head, "="); ok {
		hc.Name = strings.TrimSpace(name)
		hc.Addr = strings.TrimSpace(addr)
	} else {
		hc.Addr = strings.TrimSpace(head)
	}
	if hc.Addr == "" {
		return HostConfig{}, fmt.Errorf("host spec %q has no address", spec)
	}
	for _, l := range parts[1:] {
		if l = strings.TrimSpace(l); l != "" {
			hc.Labels = append(hc.Labels, l)
		}
	}
	hc.normalize()
	return hc, nil
}

// ParseHostSpecs parses a comma-of-semicolons env value: host specs
// separated by ";" (since "," separates labels within one spec).
func ParseHostSpecs(specs string) ([]HostConfig, error) {
	var out []HostConfig
	for _, s := range strings.Split(specs, ";") {
		if strings.TrimSpace(s) == "" {
			continue
		}
		hc, err := ParseHostSpec(s)
		if err != nil {
			return nil, err
		}
		out = append(out, hc)
	}
	return out, nil
}

// LoadConfigFile reads a JSON config file ({"hosts":[{name,addr,labels}]}).
func LoadConfigFile(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	for i := range cfg.Hosts {
		if cfg.Hosts[i].Addr == "" {
			return Config{}, fmt.Errorf("%s: hosts[%d] has no addr", path, i)
		}
		cfg.Hosts[i].normalize()
	}
	return cfg, nil
}

// Validate checks the merged config for duplicates and emptiness.
func (c Config) Validate() error {
	if len(c.Hosts) == 0 {
		return fmt.Errorf("no hosts configured")
	}
	names := map[string]bool{}
	addrs := map[string]bool{}
	for _, h := range c.Hosts {
		if names[h.Name] {
			return fmt.Errorf("duplicate host name %q", h.Name)
		}
		if addrs[h.Addr] {
			return fmt.Errorf("duplicate host addr %q", h.Addr)
		}
		names[h.Name] = true
		addrs[h.Addr] = true
	}
	return nil
}

func (hc *HostConfig) normalize() {
	if !strings.Contains(hc.Addr, "://") {
		hc.Addr = "http://" + hc.Addr
	}
	hc.Addr = strings.TrimRight(hc.Addr, "/")
	if hc.Name == "" {
		hc.Name = strings.TrimPrefix(strings.TrimPrefix(hc.Addr, "http://"), "https://")
	}
}
