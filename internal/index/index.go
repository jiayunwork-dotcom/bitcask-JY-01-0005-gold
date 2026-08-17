// Package index maintains the in-memory key -> location hash index that makes
// bitcask reads O(1). A Location points at the most recent record for a key
// inside a specific log segment. The index is rebuilt by scanning the data log
// on Open, and a compact "hint" file can be written during Merge so that a
// subsequent Open can reload the index without re-reading every record.
package index

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
)

// Location identifies where the latest record for a key lives on disk.
type Location struct {
	// FileID is the segment the record was written to.
	FileID int64
	// Offset is the byte offset of the record's first byte in that segment.
	Offset int64
	// Size is the on-disk size of the framed record in bytes.
	Size int64
	// Tombstone is true when the latest record for the key is a deletion.
	Tombstone bool
}

// ErrHint is returned when a hint file cannot be parsed.
var ErrHint = errors.New("index: malformed hint file")

// Index is a thread-safe in-memory map from key to its latest Location.
type Index struct {
	mu sync.RWMutex
	m  map[string]Location
}

// New returns an empty index.
func New() *Index {
	return &Index{m: make(map[string]Location)}
}

// Get returns the location for key and whether it is present in the index.
// A present but tombstoned location means the key currently does not exist.
func (i *Index) Get(key string) (Location, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	loc, ok := i.m[key]
	return loc, ok
}

// Put records the latest location for key, replacing any previous entry.
func (i *Index) Put(key string, loc Location) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.m[key] = loc
}

// PutLive is a convenience for Put with Tombstone=false.
func (i *Index) PutLive(key string, fileID, offset, size int64) {
	i.Put(key, Location{FileID: fileID, Offset: offset, Size: size})
}

// MarkDelete flips the tombstone flag for an existing key in place, keeping its
// location. It returns false if the key was not present.
func (i *Index) MarkDelete(key string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	loc, ok := i.m[key]
	if !ok {
		return false
	}
	loc.Tombstone = true
	i.m[key] = loc
	return true
}

// Delete removes a key from the index entirely. It is used by Merge to discard
// keys whose latest record is a tombstone.
func (i *Index) Delete(key string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.m, key)
}

// Len returns the number of keys currently tracked (including tombstoned ones).
func (i *Index) Len() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return len(i.m)
}

// Keys returns all tracked keys in sorted order. Tombstoned keys are included.
func (i *Index) Keys() []string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	out := make([]string, 0, len(i.m))
	for k := range i.m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Range calls fn for every key/location pair in sorted key order. Iteration
// stops early when fn returns false.
func (i *Index) Range(fn func(key string, loc Location) bool) {
	for _, k := range i.Keys() {
		i.mu.RLock()
		loc := i.m[k]
		i.mu.RUnlock()
		if !fn(k, loc) {
			return
		}
	}
}

// LiveEntries returns every key whose latest record is not a tombstone, in
// sorted key order. These are the records that survive a Merge.
func (i *Index) LiveEntries() []KeyLocation {
	var out []KeyLocation
	i.Range(func(key string, loc Location) bool {
		if !loc.Tombstone {
			out = append(out, KeyLocation{Key: key, Loc: loc})
		}
		return true
	})
	return out
}

// KeyLocation pairs a key with its current Location.
type KeyLocation struct {
	Key string
	Loc Location
}

// Has reports whether key is tracked by the index (live or tombstoned).
func (i *Index) Has(key string) bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	_, ok := i.m[key]
	return ok
}

// WriteHint serialises the index to w. Every tracked key (live or tombstoned)
// is written so a reload reconstructs the exact current state.
func (i *Index) WriteHint(w io.Writer) error {
	// Encode the count first so LoadHint can pre-size its map.
	entries := i.LiveEntries()
	// Also include tombstoned keys so reload reflects deletions correctly.
	var all []KeyLocation
	i.Range(func(key string, loc Location) bool {
		all = append(all, KeyLocation{Key: key, Loc: loc})
		return true
	})
	_ = entries // live entries kept available for callers via LiveEntries

	if err := writeUint64(w, uint64(len(all))); err != nil {
		return fmt.Errorf("%w: write count: %v", ErrHint, err)
	}
	for _, kl := range all {
		if err := writeHintEntry(w, kl); err != nil {
			return err
		}
	}
	return nil
}

// LoadHint reads an index previously written by WriteHint, replacing the
// current contents.
func (i *Index) LoadHint(r io.Reader) error {
	count, err := readUint64(r)
	if err != nil {
		return fmt.Errorf("%w: read count: %v", ErrHint, err)
	}
	if count > 1<<28 {
		return fmt.Errorf("%w: absurd entry count %d", ErrHint, count)
	}
	next := make(map[string]Location, count)
	for n := uint64(0); n < count; n++ {
		kl, err := readHintEntry(r)
		if err != nil {
			return err
		}
		next[kl.Key] = kl.Loc
	}
	i.mu.Lock()
	i.m = next
	i.mu.Unlock()
	return nil
}

// --- hint serialisation helpers ---

// hintEntrySize is keylen(4) + fileid(8) + offset(8) + size(8) + tomb(1).
const hintEntrySize = 4 + 8 + 8 + 8 + 1

func writeHintEntry(w io.Writer, kl KeyLocation) error {
	var b [hintEntrySize]byte
	binary.BigEndian.PutUint32(b[0:4], uint32(len(kl.Key)))
	binary.BigEndian.PutUint64(b[4:12], uint64(kl.Loc.FileID))
	binary.BigEndian.PutUint64(b[12:20], uint64(kl.Loc.Offset))
	binary.BigEndian.PutUint64(b[20:28], uint64(kl.Loc.Size))
	if kl.Loc.Tombstone {
		b[28] = 1
	}
	if _, err := w.Write(b[:]); err != nil {
		return fmt.Errorf("%w: write header: %v", ErrHint, err)
	}
	if _, err := w.Write([]byte(kl.Key)); err != nil {
		return fmt.Errorf("%w: write key: %v", ErrHint, err)
	}
	return nil
}

func readHintEntry(r io.Reader) (KeyLocation, error) {
	var b [hintEntrySize]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return KeyLocation{}, fmt.Errorf("%w: read header: %v", ErrHint, err)
	}
	keyLen := binary.BigEndian.Uint32(b[0:4])
	kl := KeyLocation{
		Loc: Location{
			FileID:    int64(binary.BigEndian.Uint64(b[4:12])),
			Offset:    int64(binary.BigEndian.Uint64(b[12:20])),
			Size:      int64(binary.BigEndian.Uint64(b[20:28])),
			Tombstone: b[28] == 1,
		},
	}
	if keyLen > 1<<24 {
		return KeyLocation{}, fmt.Errorf("%w: absurd key length %d", ErrHint, keyLen)
	}
	key := make([]byte, keyLen)
	if _, err := io.ReadFull(r, key); err != nil {
		return KeyLocation{}, fmt.Errorf("%w: read key: %v", ErrHint, err)
	}
	kl.Key = string(key)
	return kl, nil
}

func writeUint64(w io.Writer, v uint64) error {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	if _, err := w.Write(b[:]); err != nil {
		return err
	}
	return nil
}

func readUint64(r io.Reader) (uint64, error) {
	var b [8]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(b[:]), nil
}
