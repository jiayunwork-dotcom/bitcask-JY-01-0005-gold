package index

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"bitcask/internal/log"
)

// rebuildFromLog scans every segment in dir (lowest fileID first) and returns
// an index containing the latest record for each key, mimicking what the store
// does on Open.
func rebuildFromLog(t *testing.T, dir string, fileIDs []int64) *Index {
	t.Helper()
	idx := New()
	for _, id := range fileIDs {
		seg, err := log.OpenReadOnly(dir, id)
		if err != nil {
			t.Fatalf("OpenReadOnly %d: %v", id, err)
		}
		r := log.NewReader(seg.File())
		for {
			e, err := r.Next()
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				t.Fatalf("scan %d: %v", id, err)
			}
			idx.Put(string(e.Key), Location{
				FileID:    id,
				Offset:    e.Offset,
				Size:      e.Size,
				Tombstone: e.Tombstone,
			})
		}
		_ = seg.Close()
	}
	return idx
}

func TestIndexRebuild(t *testing.T) {
	dir := t.TempDir()
	// Two segments; the second overwrites some keys and tombstones another.
	seg1, err := log.NewSegment(dir, 1)
	if err != nil {
		t.Fatalf("NewSegment: %v", err)
	}
	seg2, err := log.NewSegment(dir, 2)
	if err != nil {
		t.Fatalf("NewSegment: %v", err)
	}

	mustAppend := func(s *log.Segment, k, v string, tomb bool) {
		if _, _, err := s.Append([]byte(k), []byte(v), tomb); err != nil {
			t.Fatalf("Append %q: %v", k, err)
		}
	}
	mustAppend(seg1, "a", "v1", false)
	mustAppend(seg1, "b", "v2", false)
	mustAppend(seg1, "c", "v3", false)
	mustAppend(seg2, "a", "v1-new", false) // overwrite a
	mustAppend(seg2, "b", "", true)        // tombstone b
	mustAppend(seg2, "d", "v4", false)

	if err := seg1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := seg2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	idx := rebuildFromLog(t, dir, []int64{1, 2})

	// a was overwritten in segment 2.
	loc, ok := idx.Get("a")
	if !ok || loc.FileID != 2 || loc.Tombstone {
		t.Fatalf("key a: ok=%v loc=%+v", ok, loc)
	}
	// b is tombstoned in segment 2.
	loc, ok = idx.Get("b")
	if !ok || !loc.Tombstone {
		t.Fatalf("key b should be tombstoned: ok=%v loc=%+v", ok, loc)
	}
	// c only exists in segment 1.
	loc, ok = idx.Get("c")
	if !ok || loc.FileID != 1 || loc.Tombstone {
		t.Fatalf("key c: ok=%v loc=%+v", ok, loc)
	}
	// d is brand new.
	if !idx.Has("d") {
		t.Fatalf("key d missing")
	}
	if idx.Has("ghost") {
		t.Fatalf("key ghost should be absent")
	}
	if idx.Len() != 4 {
		t.Fatalf("Len = %d, want 4", idx.Len())
	}
}

func TestIndexLiveEntries(t *testing.T) {
	idx := New()
	idx.PutLive("a", 1, 0, 10)
	idx.PutLive("b", 1, 10, 10)
	idx.PutLive("c", 1, 20, 10)
	idx.MarkDelete("b")

	live := idx.LiveEntries()
	if len(live) != 2 {
		t.Fatalf("LiveEntries = %d, want 2", len(live))
	}
	for _, kl := range live {
		if kl.Key == "b" {
			t.Fatalf("tombstoned key b leaked into LiveEntries")
		}
	}
}

func TestIndexHintRoundTrip(t *testing.T) {
	idx := New()
	idx.PutLive("alpha", 3, 100, 12)
	idx.PutLive("beta", 3, 112, 9)
	idx.Put("gamma", Location{FileID: 3, Offset: 121, Size: 8, Tombstone: true})

	var buf bytes.Buffer
	if err := idx.WriteHint(&buf); err != nil {
		t.Fatalf("WriteHint: %v", err)
	}

	loaded := New()
	if err := loaded.LoadHint(&buf); err != nil {
		t.Fatalf("LoadHint: %v", err)
	}
	if loaded.Len() != 3 {
		t.Fatalf("loaded Len = %d, want 3", loaded.Len())
	}
	loc, ok := loaded.Get("gamma")
	if !ok || !loc.Tombstone {
		t.Fatalf("gamma should reload as tombstone: ok=%v loc=%+v", ok, loc)
	}
	loc, ok = loaded.Get("alpha")
	if !ok || loc.FileID != 3 || loc.Offset != 100 || loc.Size != 12 {
		t.Fatalf("alpha reload mismatch: ok=%v loc=%+v", ok, loc)
	}
}

func TestIndexHintRejectsTruncated(t *testing.T) {
	idx := New()
	idx.PutLive("k", 1, 0, 4)
	var buf bytes.Buffer
	if err := idx.WriteHint(&buf); err != nil {
		t.Fatalf("WriteHint: %v", err)
	}
	// Truncate the hint so the key payload is missing.
	broken := buf.Bytes()[:buf.Len()-3]
	loaded := New()
	if err := loaded.LoadHint(bytes.NewReader(broken)); err == nil {
		t.Fatalf("expected error loading truncated hint")
	}
}

func TestIndexRangeAndKeys(t *testing.T) {
	idx := New()
	for _, k := range []string{"c", "a", "b"} {
		idx.PutLive(k, 1, 0, 1)
	}
	keys := idx.Keys()
	want := []string{"a", "b", "c"}
	for i, w := range want {
		if keys[i] != w {
			t.Fatalf("Keys[%d] = %q, want %q", i, keys[i], w)
		}
	}
	count := 0
	idx.Range(func(key string, loc Location) bool {
		count++
		return true
	})
	if count != 3 {
		t.Fatalf("Range visited %d keys, want 3", count)
	}
	// Early stop.
	seen := 0
	idx.Range(func(key string, loc Location) bool {
		seen++
		return false
	})
	if seen != 1 {
		t.Fatalf("Range should stop after first, saw %d", seen)
	}
}
