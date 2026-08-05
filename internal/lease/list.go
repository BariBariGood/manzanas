package lease

import (
	"sort"

	"github.com/BariBariGood/manzanas/proto"
)

// List returns all known leases (active, queued, and recently terminal),
// newest first, with fresh queue positions. Listing does not refresh queue
// deadlines — only Get keeps a queued lease alive.
func (m *Manager) List() []proto.Lease {
	m.mu.Lock()
	pending := m.expireLocked()
	defer func() {
		resets := m.takeResetsLocked()
		m.mu.Unlock()
		m.emit(pending)
		m.startResets(resets)
	}()
	out := make([]proto.Lease, 0, len(m.leases))
	for _, l := range m.leases {
		c := *l
		if c.State == proto.LeaseQueued {
			c.QueuePosition = m.queuePositionLocked(l)
		} else {
			c.QueuePosition = 0
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}
