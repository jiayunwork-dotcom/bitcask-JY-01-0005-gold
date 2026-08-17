package store

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Logf("Close: %v", err)
		}
	})
	return s
}

func TestStorePutGet(t *testing.T) {
	s := openTemp(t)

	if err := s.Put([]byte("a"), []byte("1")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Put([]byte("b"), []byte("two")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	v, ok, err := s.Get([]byte("a"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok || string(v) != "1" {
		t.Fatalf("Get a = %q/%v, want 1/true", v, ok)
	}
	v, ok, err = s.Get([]byte("b"))
	if err != nil || !ok || string(v) != "two" {
		t.Fatalf("Get b = %q/%v/%v, want two/true/nil", v, ok, err)
	}

	// Missing key.
	if _, ok, _ = s.Get([]byte("nope")); ok {
		t.Fatalf("Get missing key should be false")
	}

	// Overwrite and confirm latest wins.
	if err := s.Put([]byte("a"), []byte("updated")); err != nil {
		t.Fatalf("Put overwrite: %v", err)
	}
	v, ok, err = s.Get([]byte("a"))
	if err != nil || !ok || string(v) != "updated" {
		t.Fatalf("Get a after overwrite = %q/%v/%v, want updated/true/nil", v, ok, err)
	}
	if s.Count() != 2 {
		t.Fatalf("Count = %d, want 2", s.Count())
	}
}

func TestStoreDelete(t *testing.T) {
	s := openTemp(t)
	if err := s.Put([]byte("x"), []byte("value")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	v, ok, err := s.Get([]byte("x"))
	if err != nil || !ok || string(v) != "value" {
		t.Fatalf("Get x = %q/%v/%v", v, ok, err)
	}
	if err := s.Delete([]byte("x")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if v, ok, err = s.Get([]byte("x")); err != nil || ok {
		t.Fatalf("Get x after delete = %q/%v/%v, want false", v, ok, err)
	}
	// Deleting again is idempotent.
	if err := s.Delete([]byte("x")); err != nil {
		t.Fatalf("Delete again: %v", err)
	}
	if _, ok, _ = s.Get([]byte("x")); ok {
		t.Fatalf("Get x after second delete should be false")
	}

	// Empty key is rejected.
	if err := s.Delete([]byte("")); err == nil {
		t.Fatalf("Delete empty key should error")
	}
	if err := s.Put([]byte(""), []byte("v")); err == nil {
		t.Fatalf("Put empty key should error")
	}
}

func TestStoreMerge(t *testing.T) {
	s := openTemp(t)

	// Seed keys 0..49; overwrite 0..9; delete 40..49.
	for i := 0; i < 50; i++ {
		k := []byte(string(rune('a' + i%26)) + itoa(i))
		if err := s.Put(k, []byte("v"+itoa(i))); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	for i := 0; i < 10; i++ {
		k := []byte(string(rune('a' + i%26)) + itoa(i))
		if err := s.Put(k, []byte("new"+itoa(i))); err != nil {
			t.Fatalf("overwrite %d: %v", i, err)
		}
	}
	for i := 40; i < 50; i++ {
		k := []byte(string(rune('a' + i%26)) + itoa(i))
		if err := s.Delete(k); err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
	}

	beforeFiles := len(s.FileIDs())
	if err := s.Merge(); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	// After merge there must be exactly one segment.
	if ids := s.FileIDs(); len(ids) != 1 {
		t.Fatalf("after merge file count = %d, want 1", len(ids))
	}
	if beforeFiles < 1 {
		t.Fatalf("unexpected: no segments before merge")
	}

	// Live keys reflect the latest writes; deleted keys are gone.
	for i := 0; i < 10; i++ {
		k := []byte(string(rune('a' + i%26)) + itoa(i))
		v, ok, err := s.Get(k)
		if err != nil || !ok || string(v) != "new"+itoa(i) {
			t.Fatalf("Get %q after merge = %q/%v/%v, want new%d/true/nil", k, v, ok, err, i)
		}
	}
	for i := 10; i < 40; i++ {
		k := []byte(string(rune('a' + i%26)) + itoa(i))
		v, ok, err := s.Get(k)
		if err != nil || !ok || string(v) != "v"+itoa(i) {
			t.Fatalf("Get %q after merge = %q/%v/%v, want v%d/true/nil", k, v, ok, err, i)
		}
	}
	for i := 40; i < 50; i++ {
		k := []byte(string(rune('a' + i%26)) + itoa(i))
		if _, ok, _ := s.Get(k); ok {
			t.Fatalf("deleted key %q should be absent after merge", k)
		}
	}
}

func TestStorePersistReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	pairs := map[string]string{
		"apple":  "red",
		"banana": "yellow",
		"cherry": "dark",
	}
	for k, v := range pairs {
		if err := s.Put([]byte(k), []byte(v)); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
	}
	// Update and delete before close so persistence of those ops is checked.
	if err := s.Put([]byte("apple"), []byte("green")); err != nil {
		t.Fatalf("Put update: %v", err)
	}
	if err := s.Delete([]byte("banana")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen and verify persistence.
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	v, ok, err := s2.Get([]byte("apple"))
	if err != nil || !ok || string(v) != "green" {
		t.Fatalf("Get apple after reopen = %q/%v/%v, want green/true/nil", v, ok, err)
	}
	if _, ok, _ = s2.Get([]byte("banana")); ok {
		t.Fatalf("banana should stay deleted after reopen")
	}
	v, ok, err = s2.Get([]byte("cherry"))
	if err != nil || !ok || string(v) != "dark" {
		t.Fatalf("Get cherry after reopen = %q/%v/%v, want dark/true/nil", v, ok, err)
	}

	// Writes after reopen persist again.
	if err := s2.Put([]byte("date"), []byte("brown")); err != nil {
		t.Fatalf("Put after reopen: %v", err)
	}
	if err := s2.Close(); err != nil {
		t.Fatalf("Close 2: %v", err)
	}
	s3, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen 2: %v", err)
	}
	defer s3.Close()
	if v, ok, _ = s3.Get([]byte("date")); !ok || string(v) != "brown" {
		t.Fatalf("date after second reopen = %q/%v, want brown/true", v, ok)
	}
}

func TestStoreHintLoad(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < 20; i++ {
		k := []byte("k" + itoa(i))
		if err := s.Put(k, []byte("val"+itoa(i))); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	if err := s.Merge(); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	// A hint file must now exist.
	if _, err := os.Stat(filepath.Join(dir, hintFile)); err != nil {
		t.Fatalf("hint file missing after merge: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen; this exercises the hint-load fast path instead of a full scan.
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	for i := 0; i < 20; i++ {
		k := []byte("k" + itoa(i))
		v, ok, err := s2.Get(k)
		if err != nil || !ok || string(v) != "val"+itoa(i) {
			t.Fatalf("Get %q after hint load = %q/%v/%v, want val%d/true/nil", k, v, ok, err, i)
		}
	}
	if s2.Count() != 20 {
		t.Fatalf("Count after hint load = %d, want 20", s2.Count())
	}
}

func TestStoreRotateAndMerge(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenWithOptions(dir, Options{MaxFileSize: 64})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// Each value is ~30 bytes, so writes should rotate across several segments.
	for i := 0; i < 30; i++ {
		k := []byte("key" + itoa(i))
		v := []byte("payload-value-number-" + itoa(i))
		if err := s.Put(k, v); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	if len(s.FileIDs()) < 2 {
		t.Fatalf("expected rotation to create multiple segments, got %d", len(s.FileIDs()))
	}
	if err := s.Merge(); err != nil {
		t.Fatalf("Merge after rotation: %v", err)
	}
	if ids := s.FileIDs(); len(ids) != 1 {
		t.Fatalf("after merge file count = %d, want 1", len(ids))
	}
	for i := 0; i < 30; i++ {
		k := []byte("key" + itoa(i))
		v, ok, err := s.Get(k)
		if err != nil || !ok || string(v) != "payload-value-number-"+itoa(i) {
			t.Fatalf("Get %q after merge = %q/%v/%v", k, v, ok, err)
		}
	}
}

func TestStoreMergeAllDeleted(t *testing.T) {
	s := openTemp(t)
	for i := 0; i < 10; i++ {
		if err := s.Put([]byte("k"+itoa(i)), []byte("v")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	for i := 0; i < 10; i++ {
		if err := s.Delete([]byte("k" + itoa(i))); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	}
	if err := s.Merge(); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if s.Count() != 0 {
		t.Fatalf("Count after merging all-deleted = %d, want 0", s.Count())
	}
	if ids := s.FileIDs(); len(ids) != 1 {
		t.Fatalf("segment count = %d, want 1", len(ids))
	}
}

func TestStoreClosedErrors(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Put([]byte("a"), []byte("b")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Put on closed store = %v, want ErrClosed", err)
	}
	if _, _, err := s.Get([]byte("a")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get on closed store = %v, want ErrClosed", err)
	}
	if err := s.Delete([]byte("a")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Delete on closed store = %v, want ErrClosed", err)
	}
	if err := s.Merge(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Merge on closed store = %v, want ErrClosed", err)
	}
	// Close is idempotent.
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestStoreLargeValueRoundTrip(t *testing.T) {
	s := openTemp(t)
	big := make([]byte, 1<<16)
	for i := range big {
		big[i] = byte(i % 251)
	}
	if err := s.Put([]byte("big"), big); err != nil {
		t.Fatalf("Put big: %v", err)
	}
	v, ok, err := s.Get([]byte("big"))
	if err != nil || !ok {
		t.Fatalf("Get big ok=%v err=%v", ok, err)
	}
	if len(v) != len(big) {
		t.Fatalf("Get big len = %d, want %d", len(v), len(big))
	}
	for i := range big {
		if v[i] != big[i] {
			t.Fatalf("Get big differs at %d", i)
		}
	}
}

// itoa is a tiny int->string helper used throughout the tests.
func itoa(i int) string { return strconv.Itoa(i) }
