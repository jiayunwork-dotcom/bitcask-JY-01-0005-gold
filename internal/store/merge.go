package store

import (
	"fmt"
	"os"
	"path/filepath"

	"bitcask/internal/index"
	"bitcask/internal/log"
)

// Merge compacts the database: it rewrites every live (non-tombstoned) key into
// a single new active segment, drops all stale and tombstoned records, removes
// the old segment files, and writes a fresh hint file for fast reload.
//
// After Merge the store has exactly one segment file whose file ID is greater
// than every previous file ID.
func (s *Store) Merge() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if err := s.active.Sync(); err != nil {
		return err
	}

	live := s.index.LiveEntries()
	if len(live) == 0 {
		return s.resetToEmpty()
	}

	newID := s.maxFileID() + 1
	newSeg, err := log.NewSegment(s.dir, newID)
	if err != nil {
		return err
	}

	// Cache a read-only handle per source file so we can read live values out
	// of old segments (including the current active one) without disturbing the
	// main handles.
	readers := make(map[int64]*os.File)
	defer func() {
		for _, f := range readers {
			_ = f.Close()
		}
	}()
	getReader := func(id int64) (*os.File, error) {
		if f, ok := readers[id]; ok {
			return f, nil
		}
		f, err := os.Open(log.SegmentPath(s.dir, id))
		if err != nil {
			return nil, err
		}
		readers[id] = f
		return f, nil
	}

	newIndex := index.New()
	for _, kl := range live {
		f, err := getReader(kl.Loc.FileID)
		if err != nil {
			_ = newSeg.Close()
			return fmt.Errorf("store: open source segment %d: %w", kl.Loc.FileID, err)
		}
		e, err := log.ReadRecordAt(f, kl.Loc.FileID, kl.Loc.Offset)
		if err != nil {
			_ = newSeg.Close()
			return fmt.Errorf("store: read live key %q: %w", kl.Key, err)
		}
		off, size, err := newSeg.Append(e.Key, e.Value, false)
		if err != nil {
			_ = newSeg.Close()
			return err
		}
		newIndex.Put(kl.Key, index.Location{FileID: newID, Offset: off, Size: size})
	}

	if err := newSeg.Sync(); err != nil {
		_ = newSeg.Close()
		return err
	}

	if err := writeHintFile(s.dir, newIndex); err != nil {
		_ = newSeg.Close()
		return err
	}

	// Close and remove all previous segments, then promote the merged one.
	oldIDs := make([]int64, 0, len(s.segments))
	for id := range s.segments {
		oldIDs = append(oldIDs, id)
	}
	for _, id := range oldIDs {
		if seg := s.segments[id]; seg != nil {
			_ = seg.Close()
		}
		delete(s.segments, id)
		if id != newID {
			if err := os.Remove(log.SegmentPath(s.dir, id)); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("store: remove old segment %d: %w", id, err)
			}
		}
	}

	s.segments[newID] = newSeg
	s.activeID = newID
	s.active = newSeg
	s.index = newIndex
	return nil
}

// resetToEmpty drops every segment and starts over with a single empty segment
// file. It is used when a Merge finds no live keys.
func (s *Store) resetToEmpty() error {
	oldIDs := make([]int64, 0, len(s.segments))
	for id := range s.segments {
		oldIDs = append(oldIDs, id)
	}
	for _, id := range oldIDs {
		if seg := s.segments[id]; seg != nil {
			_ = seg.Close()
		}
		delete(s.segments, id)
		if err := os.Remove(log.SegmentPath(s.dir, id)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("store: remove segment %d: %w", id, err)
		}
	}
	if err := os.Remove(filepath.Join(s.dir, hintFile)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("store: remove hint: %w", err)
	}
	seg, err := log.NewSegment(s.dir, 1)
	if err != nil {
		return err
	}
	s.segments[1] = seg
	s.activeID = 1
	s.active = seg
	s.index = index.New()
	return nil
}
