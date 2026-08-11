package server

import (
	"context"
	"net/http"

	"github.com/BariBariGood/manzanas/internal/buildinfo"
	"github.com/BariBariGood/manzanas/internal/warm"
	"github.com/BariBariGood/manzanas/proto"
)

// PoolStatusFunc snapshots the warm pool for /v0/status; nil means the
// daemon runs without a pool (capacity fields stay zero, gates report ok).
type PoolStatusFunc func(ctx context.Context) warm.PoolStatus

// SetPoolStatus wires the warm pool's status snapshot; may stay nil when
// there is no pool.
func (s *Server) SetPoolStatus(fn PoolStatusFunc) { s.poolStatus = fn }

// handleStatus serves GET /v0/status: the daemon's load/occupancy
// snapshot for fleet schedulers (see proto.HostStatus).
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	st := proto.HostStatus{
		Build:      buildinfo.Version,
		Gates:      proto.HostGates{LoadOK: true, DiskOK: true},
		PoolAdvice: s.adviceSnapshot(),
	}
	if s.leases != nil {
		st.LeasesActive = s.leases.ActiveCount()
		st.LeasesQueued = s.leases.QueuedCount()
	}
	if s.poolStatus != nil {
		ps := s.poolStatus(r.Context())
		st.Capacity = proto.HostCapacity{
			MaxBootedRunning:   ps.Class.MaxBootedRunning,
			MaxParked:          ps.Class.MaxParked,
			MaxConcurrentBoots: ps.Class.MaxConcurrentBoots,
		}
		st.Running = ps.Running
		st.UnmanagedSims = ps.Unmanaged
		st.ReapedSims = ps.ReapedStaleLocks
		st.Parked = ps.Parked
		st.BootsInFlight = ps.BootsInFlight
		st.LoadAvg1 = ps.LoadAvg1
		st.CPUs = ps.CPUs
		st.FreeDiskBytes = ps.FreeDiskBytes
		st.Gates = proto.HostGates{LoadOK: ps.LoadOK, DiskOK: ps.DiskOK}
	}
	writeJSON(w, http.StatusOK, st)
}
