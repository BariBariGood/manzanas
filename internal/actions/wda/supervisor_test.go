package wda

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// flakyWDA serves /status according to a switchable health flag.
type flakyWDA struct {
	mu      sync.Mutex
	healthy bool
}

func (f *flakyWDA) set(h bool) {
	f.mu.Lock()
	f.healthy = h
	f.mu.Unlock()
}

func (f *flakyWDA) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		h := f.healthy
		f.mu.Unlock()
		if !h {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"value":{"ready":true}}`))
	})
}

// fakeLauncher flips the fake WDA healthy when launched.
type fakeLauncher struct {
	mu       sync.Mutex
	launches int
	stops    int
	fail     bool
	target   *flakyWDA
}

func (l *fakeLauncher) Launch(ctx context.Context) error {
	l.mu.Lock()
	l.launches++
	fail := l.fail
	l.mu.Unlock()
	if fail {
		return context.DeadlineExceeded
	}
	l.target.set(true)
	return nil
}

func (l *fakeLauncher) Stop() {
	l.mu.Lock()
	l.stops++
	l.mu.Unlock()
}

func (l *fakeLauncher) String() string { return "fake" }

func (l *fakeLauncher) counts() (int, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.launches, l.stops
}

func newSupervisorFixture(t *testing.T) (*flakyWDA, *fakeLauncher, *Supervisor) {
	t.Helper()
	f := &flakyWDA{}
	ts := httptest.NewServer(f.handler())
	t.Cleanup(ts.Close)
	l := &fakeLauncher{target: f}
	s := NewSupervisor(New(ts.URL), l, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithProbeInterval(10*time.Millisecond), WithReadyTimeout(2*time.Second))
	return f, l, s
}

func TestSupervisorLaunchesWhenDown(t *testing.T) {
	_, l, s := newSupervisorFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if launches, _ := l.counts(); launches >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("supervisor never launched the runner")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if _, stops := l.counts(); stops != 1 {
		t.Fatalf("stops = %d, want 1 (launcher stopped on shutdown)", stops)
	}
}

func TestSupervisorRelaunchesAfterCrashOnKick(t *testing.T) {
	f, l, s := newSupervisorFixture(t)
	f.set(true) // healthy from the start: no initial launch needed
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	time.Sleep(30 * time.Millisecond)
	f.set(false) // simulate the tunnel dropping / runner dying
	s.Kick()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if launches, _ := l.counts(); launches >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("supervisor never relaunched after the crash")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestSupervisorBacksOffOnLaunchFailure(t *testing.T) {
	_, l, s := newSupervisorFixture(t)
	l.fail = true
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	s.Run(ctx)
	launches, _ := l.counts()
	if launches < 1 {
		t.Fatal("expected at least one launch attempt")
	}
	// With doubling backoff from 10ms the 200ms window fits only a
	// handful of attempts; a broken backoff would fit dozens.
	if launches > 6 {
		t.Fatalf("launches = %d; backoff is not slowing retries", launches)
	}
}

func TestParseLauncher(t *testing.T) {
	if l, err := ParseLauncher("U", "devicectl:com.example.WebDriverAgentRunner.xctrunner"); err != nil {
		t.Fatal(err)
	} else if l.String() != "devicectl:com.example.WebDriverAgentRunner.xctrunner" {
		t.Fatalf("String() = %q", l.String())
	}
	if l, err := ParseLauncher("U", "xctestrun:/tmp/wda.xctestrun"); err != nil {
		t.Fatal(err)
	} else if l.String() != "xctestrun:/tmp/wda.xctestrun" {
		t.Fatalf("String() = %q", l.String())
	}
	for _, bad := range []string{"", "devicectl:", "http://x", "nope:zzz"} {
		if _, err := ParseLauncher("U", bad); err == nil {
			t.Fatalf("ParseLauncher(%q) should fail", bad)
		}
	}
}

func TestDevicectlLauncherCommand(t *testing.T) {
	var got []string
	l := NewDevicectlLauncher("UDID-1", "com.x.Runner")
	l.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		got = append([]string{name}, args...)
		return nil, nil
	}
	if err := l.Launch(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "xcrun devicectl device process launch --terminate-existing --device UDID-1 com.x.Runner"
	if s := join(got); s != want {
		t.Fatalf("command = %q, want %q", s, want)
	}
}

func TestXCTestRunLauncherRestartKillsPrevious(t *testing.T) {
	var starts, kills int
	l := NewXCTestRunLauncher("UDID-1", "/tmp/wda.xctestrun")
	l.start = func(name string, args ...string) (func(), error) {
		starts++
		return func() { kills++ }, nil
	}
	if err := l.Launch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := l.Launch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if starts != 2 || kills != 1 {
		t.Fatalf("starts=%d kills=%d, want 2/1", starts, kills)
	}
	l.Stop()
	if kills != 2 {
		t.Fatalf("kills=%d after Stop, want 2", kills)
	}
}

func join(parts []string) string {
	s := ""
	for i, p := range parts {
		if i > 0 {
			s += " "
		}
		s += p
	}
	return s
}
