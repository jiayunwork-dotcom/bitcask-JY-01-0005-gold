// Package store is the public bitcask key/value engine. It ties the append-only
// data log (internal/log) to the in-memory index (internal/index) behind a
// small facade: Open, Put, Get, Delete, Merge and Close.
//
// All mutable state lives here (never in main); the CLI only parses flags and
// calls these methods.
package store

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"bitcask/internal/index"
	"bitcask/internal/log"
)

// Errors returned by the store facade.
var (
	// ErrEmptyKey is returned when Put or Delete is given a zero-length key.
	ErrEmptyKey = errors.New("store: empty key")
	// ErrClosed is returned when an operation is attempted on a closed store.
	ErrClosed = errors.New("store: store is closed")
)

// hintFile is the name of the optional hint file used to fast-reload the index.
const hintFile = "hint.bin"

// Options configures optional store behaviour.
type Options struct {
	// SyncOnWrite flushes the active segment to disk after every write when
	// true. It trades throughput for stronger durability.
	SyncOnWrite bool
	// MaxFileSize, when > 0, rotates to a fresh segment once the active segment
	// grows past this many bytes. Zero means "never rotate".
	MaxFileSize int64
}

// DefaultOptions returns the default, zero-rotation, non-sync-on-write config.
func DefaultOptions() Options { return Options{} }

// Store is an open bitcask database.
type Store struct {
	dir   string
	opts  Options
	mu    sync.Mutex
	index *index.Index

	segments map[int64]*log.Segment
	activeID int64
	active   *log.Segment
	closed   bool
}

// Open opens (creating if necessary) a bitcask database rooted at dir. The
// in-memory index is rebuilt either from the hint file (if present) or by
// scanning every segment file in fileID order, applying the latest record per
// key. A corrupt record during the scan is reported as an error.
func Open(dir string) (*Store, error) {
	return OpenWithOptions(dir, DefaultOptions())
}

// OpenWithOptions is Open with custom Options.
func OpenWithOptions(dir string, opts Options) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("store: create dir %s: %w", dir, err)
	}
	ids, err := listSegments(dir)
	if err != nil {
		return nil, err
	}

	s := &Store{
		dir:      dir,
		opts:     opts,
		index:    index.New(),
		segments: make(map[int64]*log.Segment),
	}

	if len(ids) == 0 {
		seg, err := log.NewSegment(dir, 1)
		if err != nil {
			return nil, err
		}
		s.segments[1] = seg
		s.activeID = 1
		s.active = seg
		return s, nil
	}

	for _, id := range ids {
		var seg *log.Segment
		if id == ids[len(ids)-1] {
			seg, err = log.NewSegment(dir, id)
		} else {
			seg, err = log.OpenReadOnly(dir, id)
		}
		if err != nil {
			s.closeSegments()
			return nil, err
		}
		s.segments[id] = seg
	}
	s.activeID = ids[len(ids)-1]
	s.active = s.segments[s.activeID]

	// Prefer the hint file for a fast reload; otherwise scan the log.
	if hintIdx, herr := readHintFile(dir); herr != nil {
		s.closeSegments()
		return nil, fmt.Errorf("store: load hint: %w", herr)
	} else if hintIdx != nil {
		s.index = hintIdx
	} else if err := s.scanIndex(ids); err != nil {
		s.closeSegments()
		return nil, err
	}
	return s, nil
}

// scanIndex rebuilds the index by walking every segment in fileID order.
func (s *Store) scanIndex(ids []int64) error {
	for _, id := range ids {
		seg := s.segments[id]
		r := log.NewReader(seg.File())
		for {
			e, err := r.Next()
			if err != nil {
				if isEOF(err) {
					break
				}
				return fmt.Errorf("store: scan segment %d: %w", id, err)
			}
			s.index.Put(string(e.Key), index.Location{
				FileID:    id,
				Offset:    e.Offset,
				Size:      e.Size,
				Tombstone: e.Tombstone,
			})
		}
	}
	return nil
}

// Put stores value under key, overwriting any previous value. Empty keys are
// rejected.
func (s *Store) Put(key, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if len(key) == 0 {
		return ErrEmptyKey
	}
	off, size, err := s.active.Append(key, value, false)
	if err != nil {
		return err
	}
	s.index.Put(string(key), index.Location{
		FileID: s.activeID,
		Offset: off,
		Size:   size,
	})
	if s.opts.SyncOnWrite {
		if err := s.active.Sync(); err != nil {
			return err
		}
	}
	if s.opts.MaxFileSize > 0 && s.active.Size() > s.opts.MaxFileSize {
		if err := s.rotate(); err != nil {
			return err
		}
	}
	return nil
}

// Get returns the value stored under key. The second result is false when the
// key is absent or has been deleted.
func (s *Store) Get(key []byte) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, false, ErrClosed
	}
	loc, ok := s.index.Get(string(key))
	if !ok || loc.Tombstone {
		return nil, false, nil
	}
	seg := s.segments[loc.FileID]
	if seg == nil {
		return nil, false, fmt.Errorf("store: segment %d not open", loc.FileID)
	}
	e, err := seg.ReadAt(loc.Offset)
	if err != nil {
		return nil, false, err
	}
	return append([]byte(nil), e.Value...), true, nil
}

// Delete removes key by appending a tombstone record. Deleting a missing key is
// a no-op that still records a tombstone (idempotent).
func (s *Store) Delete(key []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if len(key) == 0 {
		return ErrEmptyKey
	}
	off, size, err := s.active.Append(key, nil, true)
	if err != nil {
		return err
	}
	s.index.Put(string(key), index.Location{
		FileID:    s.activeID,
		Offset:    off,
		Size:      size,
		Tombstone: true,
	})
	if s.opts.SyncOnWrite {
		return s.active.Sync()
	}
	return nil
}

// Count returns the number of keys currently present (excluding tombstoned).
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.index.LiveEntries())
}

// FileIDs returns the file IDs of all open segments in ascending order.
func (s *Store) FileIDs() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]int64, 0, len(s.segments))
	for id := range s.segments {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// Dir returns the database directory.
func (s *Store) Dir() string { return s.dir }

// Close flushes and closes every open segment.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.closeSegmentsLocked()
}

func (s *Store) closeSegments() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeSegmentsLocked()
}

func (s *Store) closeSegmentsLocked() error {
	var firstErr error
	for id, seg := range s.segments {
		if err := seg.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(s.segments, id)
	}
	s.active = nil
	s.activeID = 0
	return firstErr
}

func (s *Store) rotate() error {
	newID := s.maxFileID() + 1
	seg, err := log.NewSegment(s.dir, newID)
	if err != nil {
		return err
	}
	s.segments[newID] = seg
	s.activeID = newID
	s.active = seg
	return nil
}

func (s *Store) maxFileID() int64 {
	var max int64
	for id := range s.segments {
		if id > max {
			max = id
		}
	}
	return max
}

// listSegments returns the segment file IDs present in dir, ascending.
func listSegments(dir string) ([]int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("store: read dir %s: %w", dir, err)
	}
	var ids []int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if id, ok := log.ParseSegmentID(e.Name()); ok {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

func readHintFile(dir string) (*index.Index, error) {
	path := filepath.Join(dir, hintFile)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	idx := index.New()
	if err := idx.LoadHint(f); err != nil {
		return nil, err
	}
	return idx, nil
}

func writeHintFile(dir string, idx *index.Index) error {
	path := filepath.Join(dir, hintFile)
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("store: create hint %s: %w", path, err)
	}
	if err := idx.WriteHint(f); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// isEOF reports whether err is a clean end-of-log.
func isEOF(err error) bool {
	return errors.Is(err, io.EOF)
}
