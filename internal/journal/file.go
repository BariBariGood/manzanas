package journal

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

// ErrRunNotFound means the run ID has no journal on disk.
var ErrRunNotFound = errors.New("journal run not found")

// ErrUnknownFormat means the run's meta.json declares a format_version this
// daemon does not know how to read (versioning rules: readers refuse
// formats they don't know).
var ErrUnknownFormat = errors.New("journal: unknown format version")

// entriesFile is the JSONL file holding a run's entries.
const entriesFile = "entries.jsonl"

// metaFile holds the run's determinism metadata (RunMeta).
const metaFile = "meta.json"

// artifactsDir is the per-run subdirectory holding content-addressed artifacts.
const artifactsDir = "artifacts"

// FileStore is the on-disk journal: one directory per run under root,
// containing meta.json, entries.jsonl, and an artifacts/ subdirectory.
// It implements Store.
type FileStore struct {
	root string
	now  func() time.Time

	mu   sync.Mutex
	runs map[string]*runState
	open map[string]struct{} // runs whose lease is still live; GC skips these
	// reclaimed holds high-water seqs for runs whose files and runState
	// were reclaimed by GC: one int64 per run instead of a full runState,
	// so a later append can't reissue a seq but memory stays bounded to
	// seq-only bookkeeping.
	reclaimed map[string]int64
}

type runState struct {
	mu      sync.Mutex
	nextSeq int64
	watch   map[chan Entry]struct{}
}

// NewFileStore opens (or creates) a journal rooted at dir.
func NewFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("journal: create root: %w", err)
	}
	return &FileStore{
		root:      dir,
		now:       time.Now,
		runs:      make(map[string]*runState),
		open:      make(map[string]struct{}),
		reclaimed: make(map[string]int64),
	}, nil
}

// Root returns the journal's root directory.
func (s *FileStore) Root() string { return s.root }

func (s *FileStore) runDir(runID string) (string, error) {
	if runID == "" || strings.ContainsAny(runID, `/\`) || strings.Contains(runID, "..") {
		return "", fmt.Errorf("journal: invalid run id %q", runID)
	}
	return filepath.Join(s.root, runID), nil
}

// state returns the in-memory state for a run, recovering nextSeq from disk
// after a daemon restart.
func (s *FileStore) state(runID string) (*runState, string, error) {
	dir, err := s.runDir(runID)
	if err != nil {
		return nil, "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.runs[runID]
	if !ok {
		st = &runState{nextSeq: 1, watch: make(map[chan Entry]struct{})}
		// Adopt a partial scan's highest seq too (an oversized line's seq
		// is recovered from its prefix, a torn one from the lines before
		// it): restarting at 1 would duplicate existing seqs.
		if last, err := lastSeq(filepath.Join(dir, entriesFile)); last > 0 || err == nil {
			st.nextSeq = last + 1
		}
		// A GC'd run's files are gone but its high-water seq survives in
		// s.reclaimed; resume above it rather than reissuing old seqs.
		if hw := s.reclaimed[runID]; hw >= st.nextSeq {
			st.nextSeq = hw + 1
		}
		delete(s.reclaimed, runID)
		s.runs[runID] = st
	}
	return st, dir, nil
}

// lastSeqTailWindow is how many trailing bytes lastSeqFast reads before
// falling back to a full scan.
const lastSeqTailWindow = 64 * 1024

// lastSeqFast returns the highest seq without scanning the whole file: it
// prefers the in-memory counter for runs touched since daemon start, then
// parses only the file's tail, and full-scans only as a last resort.
func (s *FileStore) lastSeqFast(runID, path string) (int64, error) {
	s.mu.Lock()
	st, ok := s.runs[runID]
	s.mu.Unlock()
	if ok {
		st.mu.Lock()
		next := st.nextSeq
		st.mu.Unlock()
		return next - 1, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	off := fi.Size() - lastSeqTailWindow
	if off < 0 {
		off = 0
	}
	buf := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	var last int64
	for _, line := range strings.Split(string(buf), "\n") {
		var e Entry
		if json.Unmarshal([]byte(line), &e) == nil && e.Ref.Seq > last {
			last = e.Ref.Seq
		}
	}
	if last > 0 {
		return last, nil
	}
	return lastSeq(path) // tail was all torn/oversized lines
}

// LastSeq implements Store: the highest seq recorded for a run, using the
// in-memory counter (or the file tail) rather than a full scan.
func (s *FileStore) LastSeq(runID string) (int64, error) {
	dir, err := s.runDir(runID)
	if err != nil {
		return 0, err
	}
	last, err := s.lastSeqFast(runID, filepath.Join(dir, entriesFile))
	if err != nil && os.IsNotExist(err) {
		return 0, nil
	}
	return last, err
}

// lastSeq scans a JSONL file for the highest seq. Missing file → 0.
func lastSeq(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var last int64
	err = forEachLine(f, func(line []byte, truncated bool) bool {
		var seq int64
		if truncated {
			// The line was too long to materialize, but ref is marshaled
			// first, so its seq survives in the retained prefix.
			seq = seqFromPrefix(line)
		} else {
			var e Entry
			if json.Unmarshal(line, &e) == nil {
				seq = e.Ref.Seq
			}
		}
		if seq > last {
			last = seq
		}
		return true
	})
	return last, err
}

// seqFromPrefix best-effort extracts ref.seq from the head of an entry
// line. Entries are marshaled with Ref first, so `"seq":N` appears within
// the first few dozen bytes even when the rest of the line is oversized.
func seqFromPrefix(prefix []byte) int64 {
	i := bytes.Index(prefix, []byte(`"seq":`))
	if i < 0 {
		return 0
	}
	var n int64
	for _, c := range prefix[i+6:] {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

// maxLineBytes bounds a single JSONL line the reader will materialize;
// longer lines are not decoded (like torn/corrupt lines) so one oversized
// entry can't make the rest of the run unreadable.
const maxLineBytes = 4 * 1024 * 1024

// truncPrefixBytes is how much of an oversized line is retained so its
// ref.seq can still be recovered (see seqFromPrefix).
const truncPrefixBytes = 4096

// forEachLine calls fn for each newline-terminated line. Lines longer than
// maxLineBytes are passed with truncated=true and only their first
// truncPrefixBytes retained, instead of aborting the whole read. fn returns
// false to stop.
func forEachLine(f *os.File, fn func(line []byte, truncated bool) bool) error {
	r := bufio.NewReaderSize(f, 64*1024)
	var (
		buf     []byte
		tooLong bool
	)
	for {
		part, isPrefix, err := r.ReadLine()
		if len(part) > 0 && !tooLong {
			if len(buf)+len(part) > maxLineBytes {
				tooLong = true
				if len(buf) > truncPrefixBytes {
					buf = buf[:truncPrefixBytes]
				} else if need := truncPrefixBytes - len(buf); need < len(part) {
					buf = append(buf, part[:need]...)
				} else {
					buf = append(buf, part...)
				}
			} else {
				buf = append(buf, part...)
			}
		}
		if err == nil && isPrefix {
			continue
		}
		if len(buf) > 0 {
			if !fn(buf, tooLong) {
				return nil
			}
		}
		buf = buf[:0]
		tooLong = false
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// Append implements Journal: it assigns the next seq, stamps the payload
// with "ts" (RFC 3339 UTC) if absent, and appends one JSONL line.
func (s *FileStore) Append(ctx context.Context, runID string, kind string, payload map[string]any) (proto.JournalRef, error) {
	st, dir, err := s.state(runID)
	if err != nil {
		return proto.JournalRef{}, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return proto.JournalRef{}, fmt.Errorf("journal: create run dir: %w", err)
	}
	if payload == nil {
		payload = map[string]any{}
	}
	if _, ok := payload["ts"]; !ok {
		payload["ts"] = s.now().UTC().Format(time.RFC3339Nano)
	}
	e := Entry{Ref: proto.JournalRef{RunID: runID, Seq: st.nextSeq}, Kind: kind, Payload: payload}
	line, err := json.Marshal(e)
	if err != nil {
		return proto.JournalRef{}, fmt.Errorf("journal: marshal entry: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, entriesFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return proto.JournalRef{}, fmt.Errorf("journal: open entries: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return proto.JournalRef{}, fmt.Errorf("journal: append entry: %w", err)
	}
	st.nextSeq++
	for ch := range st.watch {
		select {
		case ch <- e:
		default: // drop rather than block the appender on a slow tailer
		}
	}
	return e.Ref, nil
}

// Read implements Journal: entries with seq >= fromSeq, up to limit
// (limit <= 0 means no limit).
func (s *FileStore) Read(ctx context.Context, runID string, fromSeq int64, limit int) ([]Entry, error) {
	dir, err := s.runDir(runID)
	if err != nil {
		return nil, err
	}
	if err := s.checkFormat(runID); err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(dir, entriesFile))
	if err != nil {
		if os.IsNotExist(err) {
			// The run exists (meta.json written at grant) but nothing has
			// been appended yet: an empty page, not a missing run.
			if _, statErr := os.Stat(dir); statErr == nil {
				return nil, nil
			}
			return nil, ErrRunNotFound
		}
		return nil, err
	}
	defer f.Close()
	var out []Entry
	err = forEachLine(f, func(line []byte, truncated bool) bool {
		if truncated {
			return true // oversized lines are skipped, like torn ones
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			return true // skip torn/corrupt lines rather than failing the read
		}
		if e.Ref.Seq < fromSeq {
			return true
		}
		out = append(out, e)
		return limit <= 0 || len(out) < limit
	})
	return out, err
}

// checkFormat refuses runs whose meta declares a format_version this
// reader does not implement. A missing/empty version is treated as v0
// (this daemon is also the writer).
func (s *FileStore) checkFormat(runID string) error {
	meta, err := s.ReadMeta(runID)
	if err != nil {
		if errors.Is(err, ErrRunNotFound) {
			return nil // let callers produce their own not-found handling
		}
		return err
	}
	if meta.FormatVersion != "" && meta.FormatVersion != FormatVersion {
		return fmt.Errorf("%w %q (this reader implements %q)", ErrUnknownFormat, meta.FormatVersion, FormatVersion)
	}
	return nil
}

// WriteMeta stores the run's determinism metadata (overwrites).
func (s *FileStore) WriteMeta(runID string, meta RunMeta) error {
	_, dir, err := s.state(runID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, metaFile), raw, 0o644)
}

// ReadMeta loads the run's metadata; a missing meta.json yields a zero RunMeta.
func (s *FileStore) ReadMeta(runID string) (RunMeta, error) {
	dir, err := s.runDir(runID)
	if err != nil {
		return RunMeta{}, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, metaFile))
	if err != nil {
		if os.IsNotExist(err) {
			if _, statErr := os.Stat(dir); statErr != nil {
				return RunMeta{}, ErrRunNotFound
			}
			return RunMeta{FormatVersion: FormatVersion, RunID: runID}, nil
		}
		return RunMeta{}, err
	}
	var m RunMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		return RunMeta{}, err
	}
	return m, nil
}

// List returns summaries of all runs on disk, newest first.
func (s *FileStore) List(ctx context.Context) ([]RunSummary, error) {
	dirs, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	var out []RunSummary
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		runID := d.Name()
		sum := RunSummary{RunID: runID}
		if meta, err := s.ReadMeta(runID); err == nil {
			sum.Meta = meta
		}
		path := filepath.Join(s.root, runID, entriesFile)
		if last, err := s.lastSeqFast(runID, path); err == nil {
			sum.LastSeq = last
		}
		if fi, err := os.Stat(path); err == nil {
			sum.UpdatedAt = fi.ModTime().UTC()
		} else if fi, err := os.Stat(filepath.Join(s.root, runID)); err == nil {
			sum.UpdatedAt = fi.ModTime().UTC()
		}
		out = append(out, sum)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// MarkOpen flags a run as belonging to a live lease. Open runs are never
// garbage-collected. Call at lease grant.
func (s *FileStore) MarkOpen(runID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.open[runID] = struct{}{}
}

// MarkClosed clears a run's open flag, making it eligible for GC again.
// Call when the lease ends (release or expiry).
func (s *FileStore) MarkClosed(runID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.open, runID)
}

// hasWatchers reports whether a run has live tail subscribers.
func (s *FileStore) hasWatchers(runID string) bool {
	s.mu.Lock()
	st, ok := s.runs[runID]
	s.mu.Unlock()
	if !ok {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.watch) > 0
}

// Watch subscribes to live entries for a run. Cancel with the returned func.
func (s *FileStore) Watch(runID string) (<-chan Entry, func(), error) {
	st, dir, err := s.state(runID)
	if err != nil {
		return nil, nil, err
	}
	ch := make(chan Entry, 64)
	st.mu.Lock()
	st.watch[ch] = struct{}{}
	st.mu.Unlock()
	cancel := func() {
		st.mu.Lock()
		delete(st.watch, ch)
		empty := len(st.watch) == 0
		st.mu.Unlock()
		if !empty {
			return
		}
		// Drop the lazily-created state for a run that never materialized
		// on disk, so watching arbitrary run IDs can't grow memory forever.
		// A run that has appended (nextSeq > 1) leaves its high-water seq
		// behind so the counter can never rewind.
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			s.mu.Lock()
			st.mu.Lock()
			if len(st.watch) == 0 && s.runs[runID] == st {
				if st.nextSeq > 1 {
					s.reclaimed[runID] = st.nextSeq - 1
				}
				delete(s.runs, runID)
			}
			st.mu.Unlock()
			s.mu.Unlock()
		}
	}
	return ch, cancel, nil
}
