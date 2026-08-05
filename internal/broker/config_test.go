package broker

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseHostSpec(t *testing.T) {
	cases := []struct {
		spec string
		want HostConfig
		err  bool
	}{
		{spec: "localhost:7433", want: HostConfig{Name: "localhost:7433", Addr: "http://localhost:7433"}},
		{spec: "emac=http://100.64.0.1:7433", want: HostConfig{Name: "emac", Addr: "http://100.64.0.1:7433"}},
		{spec: "emac=100.64.0.1:7433,intel,fast", want: HostConfig{Name: "emac", Addr: "http://100.64.0.1:7433", Labels: []string{"intel", "fast"}}},
		{spec: "https://mac.example:7433/", want: HostConfig{Name: "mac.example:7433", Addr: "https://mac.example:7433"}},
		{spec: "", err: true},
		{spec: "name=", err: true},
	}
	for _, c := range cases {
		got, err := ParseHostSpec(c.spec)
		if c.err {
			if err == nil {
				t.Errorf("ParseHostSpec(%q): want error, got %+v", c.spec, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseHostSpec(%q): %v", c.spec, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParseHostSpec(%q) = %+v, want %+v", c.spec, got, c.want)
		}
	}
}

func TestParseHostSpecs(t *testing.T) {
	got, err := ParseHostSpecs("emac=a:1,intel; work=b:2 ;")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "emac" || got[1].Name != "work" {
		t.Fatalf("got %+v", got)
	}
	if !reflect.DeepEqual(got[0].Labels, []string{"intel"}) {
		t.Fatalf("labels: %+v", got[0].Labels)
	}
}

func TestLoadConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broker.json")
	data := `{"hosts":[{"name":"emac","addr":"100.64.0.1:7433","labels":["intel"]},{"addr":"http://100.64.0.2:7433"}]}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Hosts) != 2 {
		t.Fatalf("hosts: %+v", cfg.Hosts)
	}
	if cfg.Hosts[0].Addr != "http://100.64.0.1:7433" {
		t.Errorf("addr not normalized: %q", cfg.Hosts[0].Addr)
	}
	if cfg.Hosts[1].Name != "100.64.0.2:7433" {
		t.Errorf("default name: %q", cfg.Hosts[1].Name)
	}
}

func TestConfigValidate(t *testing.T) {
	if err := (Config{}).Validate(); err == nil {
		t.Error("empty config should be invalid")
	}
	dup := Config{Hosts: []HostConfig{{Name: "a", Addr: "http://x:1"}, {Name: "a", Addr: "http://y:1"}}}
	if err := dup.Validate(); err == nil {
		t.Error("duplicate names should be invalid")
	}
	ok := Config{Hosts: []HostConfig{{Name: "a", Addr: "http://x:1"}, {Name: "b", Addr: "http://y:1"}}}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}
