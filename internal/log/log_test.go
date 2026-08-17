package log

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"
)

// scanSegment reads every record in a read-only segment and returns them in
// order together with the first non-EOF error encountered (nil on a clean
// end-of-log). Tests decide whether an error is expected.
func scanSegment(t *testing.T, dir string, fileID int64) ([]*Entry, error) {
	t.Helper()
	seg, err := OpenReadOnly(dir, fileID)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer seg.Close()
	r := NewReader(seg.File())
	var out []*Entry
	for {
		e, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, e)
	}
}

func TestLogAppendRead(t *testing.T) {
	dir := t.TempDir()
	seg, err := NewSegment(dir, 1)
	if err != nil {
		t.Fatalf("NewSegment: %v", err)
	}
	defer seg.Close()

	type rec struct {
		key   string
		value string
		tomb  bool
	}
	want := []rec{
		{"alpha", "one", false},
		{"beta", "two", false},
		{"gamma", "", false},
		{"delta", "four", true},
		{"emoji", "héllo🌍", false},
	}
	for _, r := range want {
		off, size, err := seg.Append([]byte(r.key), []byte(r.value), r.tomb)
		if err != nil {
			t.Fatalf("Append %q: %v", r.key, err)
		}
		if off < 0 || size <= 0 {
			t.Fatalf("Append %q: bad offset/size %d/%d", r.key, off, size)
		}
	}

	// Reopen read-only and verify every record round-trips intact.
	got, err := scanSegment(t, dir, 1)
	if err != nil {
		t.Fatalf("scanSegment: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d records, want %d", len(got), len(want))
	}
	for i, w := range want {
		g := got[i]
		if string(g.Key) != w.key {
			t.Errorf("record %d key = %q, want %q", i, g.Key, w.key)
		}
		if string(g.Value) != w.value {
			t.Errorf("record %d value = %q, want %q", i, g.Value, w.value)
		}
		if g.Tombstone != w.tomb {
			t.Errorf("record %d tombstone = %v, want %v", i, g.Tombstone, w.tomb)
		}
		if g.Size != frameSize(len(w.key), len(w.value)) {
			t.Errorf("record %d size = %d, want %d", i, g.Size, frameSize(len(w.key), len(w.value)))
		}
	}

	// ReadAt must locate the exact record a second time.
	off, _, _ := seg.Append([]byte("zeta"), []byte("last"), false)
	e, err := seg.ReadAt(off)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if string(e.Key) != "zeta" || string(e.Value) != "last" {
		t.Fatalf("ReadAt returned %q=%q", e.Key, e.Value)
	}
}

func TestLogChecksumCorruption(t *testing.T) {
	dir := t.TempDir()
	seg, err := NewSegment(dir, 1)
	if err != nil {
		t.Fatalf("NewSegment: %v", err)
	}
	if _, _, err := seg.Append([]byte("secret"), []byte("value"), false); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := seg.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := seg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Flip a byte inside the value region of the only record.
	path := SegmentPath(dir, 1)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// The value starts after crc(4) + header(13) + key(6).
	idx := 4 + headerSize + 6
	if idx >= len(raw) {
		t.Fatalf("corruption index %d out of range %d", idx, len(raw))
	}
	raw[idx] ^= 0xff
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := scanSegment(t, dir, 1)
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("expected ErrChecksum, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected checksum error to stop scan, got %d records", len(got))
	}
}

func TestLogTruncatedRecord(t *testing.T) {
	dir := t.TempDir()
	seg, err := NewSegment(dir, 1)
	if err != nil {
		t.Fatalf("NewSegment: %v", err)
	}
	if _, _, err := seg.Append([]byte("k"), []byte("vvvv"), false); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := seg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Keep only the first half of the file: the trailing record is truncated.
	path := SegmentPath(dir, 1)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if err := os.WriteFile(path, raw[:len(raw)/2], 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := scanSegment(t, dir, 1)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("expected ErrCorrupt, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected truncation error, got %d records", len(got))
	}
}

func TestLogEncodeDecodeRoundTrip(t *testing.T) {
	cases := []struct {
		key   string
		value string
		tomb  bool
	}{
		{"", "", false},
		{"k", "", false},
		{"", "v", false},
		{"key", "value", true},
		{"x", "y", false},
	}
	for _, c := range cases {
		buf := Encode([]byte(c.key), []byte(c.value), c.tomb)
		e, err := Decode(buf)
		if err != nil {
			t.Fatalf("Decode %+v: %v", c, err)
		}
		if string(e.Key) != c.key || string(e.Value) != c.value || e.Tombstone != c.tomb {
			t.Errorf("round trip %+v -> %q/%q/%v", c, e.Key, e.Value, e.Tombstone)
		}
		// A flipped crc byte must be detected.
		bad := make([]byte, len(buf))
		copy(bad, buf)
		bad[0] ^= 0xff
		if _, err := Decode(bad); err != ErrChecksum {
			t.Errorf("expected ErrChecksum, got %v", err)
		}
	}
}

func TestLogSegmentNameRoundTrip(t *testing.T) {
	for _, id := range []int64{0, 1, 9, 42, 1234567890, 9999999999} {
		name := SegmentName(id)
		got, ok := ParseSegmentID(name)
		if !ok {
			t.Fatalf("ParseSegmentID(%q) not ok", name)
		}
		if got != id {
			t.Errorf("ParseSegmentID(%q) = %d, want %d", name, got, id)
		}
	}
	if _, ok := ParseSegmentID("notafile.txt"); ok {
		t.Errorf("ParseSegmentID accepted invalid name")
	}
	if _, ok := ParseSegmentID("000000000.logx"); ok {
		t.Errorf("ParseSegmentID accepted name with wrong extension")
	}
}

func TestLogReaderDetectsCleanEOF(t *testing.T) {
	// An empty reader yields a clean EOF on the first call.
	r := NewReader(bytes.NewReader(nil))
	if _, err := r.Next(); err == nil {
		t.Fatalf("expected io.EOF on empty reader")
	} else if !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF, got %v", err)
	}
}
