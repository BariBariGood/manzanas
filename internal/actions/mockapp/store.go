package mockapp

import "sync"

// Store holds one App (and pasteboard) per mock target UDID, created
// lazily so every mock target starts from the same deterministic screen.
// Shared between the action backend and the stream source factory so the
// MJPEG stream shows the same UI the actions drive.
type Store struct {
	mu   sync.Mutex
	apps map[string]*App
	pb   map[string]string
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{apps: map[string]*App{}, pb: map[string]string{}}
}

// App returns the target's synthetic app, creating it on first use.
func (s *Store) App(udid string) *App {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.apps[udid]
	if !ok {
		a = NewApp()
		s.apps[udid] = a
	}
	return a
}

// SetPasteboard stores the target's pasteboard content (simctl pbcopy).
func (s *Store) SetPasteboard(udid, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pb[udid] = text
}

// Pasteboard returns the target's pasteboard content (simctl pbpaste).
func (s *Store) Pasteboard(udid string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pb[udid]
}
