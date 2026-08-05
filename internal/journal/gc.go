package journal

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// GCConfig bounds the journal's disk usage. Zero values disable a bound.
type GCConfig struct {
	MaxAge   time.Duration // runs untouched longer than this are removed
	MaxBytes int64         // total journal size cap; oldest runs removed first
	Interval time.Duration // sweep period for RunGC (default 10m)
}

// GC removes whole runs (entries + artifacts) that exceed MaxAge, then
// removes oldest runs until total size fits MaxBytes. Returns the run IDs
// removed. Runs are the GC unit: entries and their artifacts stay consistent.
// Open runs (live leases, see MarkOpen) and runs with live watchers are
// never removed, and a run with no files yet is treated as just-modified
// rather than ancient. When a reclaimed run has in-memory state, its seq
// counter is preserved so later appends never reissue seqs already handed
// out in journal refs.
func (s *FileStore) GC(cfg GCConfig) ([]string, error) {
	dirs, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	type run struct {
		id    string
		mtime time.Time
		bytes int64
	}
	var runs []run
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		r := run{id: d.Name()}
		root := filepath.Join(s.root, d.Name())
		_ = filepath.Walk(root, func(_ string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() {
				return nil
			}
			r.bytes += fi.Size()
			if fi.ModTime().After(r.mtime) {
				r.mtime = fi.ModTime()
			}
			return nil
		})
		if r.mtime.IsZero() {
			// A run with no files yet is brand new, not ancient.
			r.mtime = s.now()
		}
		runs = append(runs, r)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].mtime.Before(runs[j].mtime) })

	now := s.now()
	var removed []string
	var total int64
	for _, r := range runs {
		total += r.bytes
	}
	for _, r := range runs {
		expired := cfg.MaxAge > 0 && now.Sub(r.mtime) > cfg.MaxAge
		over := cfg.MaxBytes > 0 && total > cfg.MaxBytes
		if !expired && !over {
			continue
		}
		s.mu.Lock()
		_, open := s.open[r.id]
		st := s.runs[r.id]
		s.mu.Unlock()
		if open || s.hasWatchers(r.id) {
			continue
		}
		// Hold the run lock across removal so it can't interleave with an
		// in-flight Append.
		if st != nil {
			st.mu.Lock()
		}
		err := os.RemoveAll(filepath.Join(s.root, r.id))
		if st != nil {
			st.mu.Unlock()
		}
		if err != nil {
			return removed, err
		}
		// Retire the runState but keep its high-water seq: dropping the
		// counter outright would make the next Append recover from the
		// now-missing file and restart at 1, rewinding the
		// strictly-increasing seq and reusing refs already handed out.
		if st != nil {
			s.mu.Lock()
			st.mu.Lock()
			if s.runs[r.id] == st && len(st.watch) == 0 {
				if st.nextSeq > 1 {
					s.reclaimed[r.id] = st.nextSeq - 1
				}
				delete(s.runs, r.id)
			}
			st.mu.Unlock()
			s.mu.Unlock()
		}
		total -= r.bytes
		removed = append(removed, r.id)
	}
	return removed, nil
}

// RunGC sweeps periodically until ctx is done.
func (s *FileStore) RunGC(ctx context.Context, cfg GCConfig) {
	interval := cfg.Interval
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = s.GC(cfg)
		}
	}
}
