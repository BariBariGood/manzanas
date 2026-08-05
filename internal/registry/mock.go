package registry

import (
	"context"
	"sync"

	"github.com/BariBariGood/manzanas/proto"
)

// MockRegistry is an in-memory Registry for tests and for running manzanasd
// on non-macOS hosts (--mock). Boot/Shutdown flip state instantly.
type MockRegistry struct {
	mu      sync.RWMutex
	targets map[string]proto.Target
	order   []string
}

// NewMock returns a MockRegistry seeded with the given targets. If none are
// given, a small default fleet is created.
func NewMock(seed ...proto.Target) *MockRegistry {
	if len(seed) == 0 {
		seed = defaultMockFleet()
	}
	m := &MockRegistry{targets: make(map[string]proto.Target)}
	for _, t := range seed {
		if len(t.Labels) == 0 {
			t.Labels = DeriveLabels(string(t.Kind), t.Runtime, t.DeviceType)
		}
		m.targets[t.UDID] = t
		m.order = append(m.order, t.UDID)
	}
	return m
}

func defaultMockFleet() []proto.Target {
	mk := func(udid, name, runtime, dt string) proto.Target {
		return proto.Target{
			UDID: udid, Kind: proto.TargetSimulator, Name: name,
			Runtime: runtime, DeviceType: dt, State: proto.StateShutdown,
		}
	}
	return []proto.Target{
		mk("MOCK-0000-0000-0001", "iPhone 17 Pro", "iOS 26.5", "iPhone 17 Pro"),
		mk("MOCK-0000-0000-0002", "iPhone 17 Pro", "iOS 26.5", "iPhone 17 Pro"),
		mk("MOCK-0000-0000-0003", "iPhone 16", "iOS 18.5", "iPhone 16"),
	}
}

// Add registers an additional target (labels derived if empty).
func (m *MockRegistry) Add(t proto.Target) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(t.Labels) == 0 {
		t.Labels = DeriveLabels(string(t.Kind), t.Runtime, t.DeviceType)
	}
	if _, exists := m.targets[t.UDID]; !exists {
		m.order = append(m.order, t.UDID)
	}
	m.targets[t.UDID] = t
}

func (m *MockRegistry) List(ctx context.Context) ([]proto.Target, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]proto.Target, 0, len(m.order))
	for _, udid := range m.order {
		out = append(out, m.targets[udid])
	}
	return out, nil
}

func (m *MockRegistry) Get(ctx context.Context, udid string) (proto.Target, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.targets[udid]
	if !ok {
		return proto.Target{}, &NotFoundError{UDID: udid}
	}
	return t, nil
}

func (m *MockRegistry) setState(udid string, s proto.TargetState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.targets[udid]
	if !ok {
		return &NotFoundError{UDID: udid}
	}
	t.State = s
	m.targets[udid] = t
	return nil
}

func (m *MockRegistry) Boot(ctx context.Context, udid string) error {
	return m.setState(udid, proto.StateBooted)
}

func (m *MockRegistry) Shutdown(ctx context.Context, udid string) error {
	return m.setState(udid, proto.StateShutdown)
}

func (m *MockRegistry) Health(ctx context.Context, udid string) (proto.TargetState, error) {
	t, err := m.Get(ctx, udid)
	if err != nil {
		return proto.StateUnknown, err
	}
	return t.State, nil
}
