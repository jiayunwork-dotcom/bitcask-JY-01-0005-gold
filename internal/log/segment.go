package log

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Segment is a single append-only data file identified by a monotonically
// increasing fileID. A store keeps one active segment for new writes and may
// keep several read-only segments around until a merge reclaims them.
type Segment struct {
	mu     sync.Mutex
	fileID int64
	path   string
	f      *os.File
	size   int64
}

// SegmentName returns the canonical file name for a segment, e.g. 0000000007.log.
func SegmentName(fileID int64) string {
	return fmt.Sprintf("%010d.log", fileID)
}

// SegmentPath joins dir with the segment file name for fileID.
func SegmentPath(dir string, fileID int64) string {
	return filepath.Join(dir, SegmentName(fileID))
}

// ParseSegmentID extracts the fileID from a segment file name. It returns
// false if the name does not match the segment naming scheme.
func ParseSegmentID(name string) (int64, bool) {
	if len(name) != len("0000000000.log") || filepath.Ext(name) != ".log" {
		return 0, false
	}
	var id int64
	var ok bool
	for i := 0; i < 10; i++ {
		c := name[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		id = id*10 + int64(c-'0')
		ok = true
	}
	if !ok {
		return 0, false
	}
	return id, true
}

// NewSegment opens (creating if necessary) the segment file for fileID in dir
// and positions it for appending. The current file size is recorded so callers
// can compute record offsets.
func NewSegment(dir string, fileID int64) (*Segment, error) {
	path := SegmentPath(dir, fileID)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("log: open segment %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("log: stat segment %s: %w", path, err)
	}
	return &Segment{
		fileID: fileID,
		path:   path,
		f:      f,
		size:   info.Size(),
	}, nil
}

// OpenReadOnly opens an existing segment read-only without appending. It is
// used for scanning and for reading a specific record by offset.
func OpenReadOnly(dir string, fileID int64) (*Segment, error) {
	path := SegmentPath(dir, fileID)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("log: open segment %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("log: stat segment %s: %w", path, err)
	}
	return &Segment{
		fileID: fileID,
		path:   path,
		f:      f,
		size:   info.Size(),
	}, nil
}

// FileID returns the segment identifier.
func (s *Segment) FileID() int64 { return s.fileID }

// Path returns the on-disk path of the segment.
func (s *Segment) Path() string { return s.path }

// Size returns the current end-of-file offset in bytes.
func (s *Segment) Size() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.size
}

// File returns the underlying open file handle. It is intended for read-only
// streaming via log.NewReader while the segment is open and must not be closed
// by the caller. It returns nil once the segment has been closed.
func (s *Segment) File() *os.File {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f
}

// Append writes a framed record to the end of the segment and returns the byte
// offset at which it was written and its on-disk size. The offset is the value
// the index should store to later locate the record.
func (s *Segment) Append(key, value []byte, tombstone bool) (offset int64, size int64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	buf := Encode(key, value, tombstone)
	off := s.size
	n, err := s.f.Write(buf)
	if err != nil {
		return 0, 0, fmt.Errorf("log: append to segment %d: %w", s.fileID, err)
	}
	if n != len(buf) {
		return 0, 0, fmt.Errorf("log: short write on segment %d: wrote %d of %d", s.fileID, n, len(buf))
	}
	s.size += int64(n)
	return off, int64(n), nil
}

// Sync flushes the segment data to stable storage.
func (s *Segment) Sync() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.f.Sync(); err != nil {
		return fmt.Errorf("log: sync segment %d: %w", s.fileID, err)
	}
	return nil
}

// Close closes the underlying file. It is safe to call multiple times.
func (s *Segment) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// ReadAt reads and decodes the framed record starting at offset. It is used by
// the store to fetch a specific value during Get and during Merge compaction.
func (s *Segment) ReadAt(offset int64) (*Entry, error) {
	s.mu.Lock()
	f := s.f
	s.mu.Unlock()
	if f == nil {
		return nil, fmt.Errorf("log: segment %d is closed", s.fileID)
	}
	return readRecordAt(f, s.fileID, offset)
}

// ReadRecordAt reads and decodes the framed record starting at offset from an
// arbitrary open file handle. It is used by the store during Merge to read a
// live value out of an older segment without disturbing the segment's main
// handle.
func ReadRecordAt(f *os.File, fileID, offset int64) (*Entry, error) {
	return readRecordAt(f, fileID, offset)
}

func readRecordAt(f *os.File, fileID, offset int64) (*Entry, error) {
	head := make([]byte, 4+headerSize)
	if _, err := f.ReadAt(head, offset); err != nil {
		return nil, fmt.Errorf("log: read header at %d: %w", offset, err)
	}
	h := decodeHeader(head[4 : 4+headerSize])
	total := 4 + headerSize + int(h.keyLen) + int(h.valueLen)
	buf := make([]byte, total)
	if _, err := f.ReadAt(buf, offset); err != nil {
		return nil, fmt.Errorf("log: read record at %d: %w", offset, err)
	}
	e, err := Decode(buf)
	if err != nil {
		return nil, err
	}
	e.FileID = fileID
	e.Offset = offset
	return e, nil
}
