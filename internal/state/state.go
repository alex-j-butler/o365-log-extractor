// Package state persists ingestion progress between runs so that restarting
// the extractor does not re-import content blobs that were already shipped.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// State records which content blobs have been ingested and how far each
// content type has been read.
type State struct {
	// Blobs maps a content ID to the time it stops being retrievable from
	// the API, after which the entry can be pruned.
	Blobs map[string]time.Time `json:"blobs"`
	// Cursors maps a content type to the end of the last window read.
	Cursors map[string]time.Time `json:"cursors"`

	path string
	mu   sync.Mutex
}

// New returns an empty state bound to path.
func New(path string) *State {
	return &State{
		Blobs:   make(map[string]time.Time),
		Cursors: make(map[string]time.Time),
		path:    path,
	}
}

// Load reads state from path. A missing file yields empty state, so a first
// run needs no setup.
func Load(path string) (*State, error) {
	s := New(path)
	if path == "" {
		return s, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return s, nil
		}
		return nil, fmt.Errorf("read state: %w", err)
	}
	if len(data) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	if s.Blobs == nil {
		s.Blobs = make(map[string]time.Time)
	}
	if s.Cursors == nil {
		s.Cursors = make(map[string]time.Time)
	}
	s.path = path
	return s, nil
}

// Seen reports whether a content blob has already been ingested.
func (s *State) Seen(contentID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.Blobs[contentID]
	return ok
}

// MarkSeen records a content blob as ingested until it expires.
func (s *State) MarkSeen(contentID string, expiry time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Blobs[contentID] = expiry
}

// Cursor returns the end of the last successfully read window for a content
// type, and whether one exists.
func (s *State) Cursor(contentType string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.Cursors[contentType]
	return t, ok
}

// SetCursor records how far a content type has been read.
func (s *State) SetCursor(contentType string, t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Cursors[contentType] = t.UTC()
}

// Prune drops blob entries that have expired from the API and can no longer
// be re-delivered.
func (s *State) Prune(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for id, expiry := range s.Blobs {
		if expiry.Before(now) {
			delete(s.Blobs, id)
			removed++
		}
	}
	return removed
}

// Save writes state atomically: a temporary file in the same directory is
// renamed over the target, so a crash mid-write cannot truncate it.
func (s *State) Save() error {
	if s.path == "" {
		return nil
	}
	s.mu.Lock()
	data, err := json.MarshalIndent(s, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(s.path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp state: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	// Windows refuses to rename onto an existing file.
	if err := os.Remove(s.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("replace state: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("rename state: %w", err)
	}
	return nil
}
