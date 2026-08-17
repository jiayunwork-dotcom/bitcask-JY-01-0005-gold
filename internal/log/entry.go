// Package log implements an append-only, log-structured data store used by the
// bitcask key/value engine. Each record is framed as:
//
//	[crc32:4][keylen:4][vallen:4][tombstone:1][key:keylen][value:vallen]
//
// All multi-byte integers are encoded big-endian. The crc32 (IEEE polynomial)
// covers the header bytes (keylen, vallen, tombstone) plus the raw key and
// value payloads, so any bit flip in a stored record is detected on read.
package log

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
)

// ErrChecksum is returned by a Reader when the crc32 of a record does not match
// the value stored alongside it.
var ErrChecksum = errors.New("log: record checksum mismatch")

// ErrCorrupt is returned when a record is truncated or otherwise undecodable.
var ErrCorrupt = errors.New("log: corrupt or truncated record")

// headerSize is the fixed number of bytes preceding the key/value payload:
// crc32 (4) + keylen (4) + vallen (4) + tombstone (1).
const headerSize = 13

// Entry is a single decoded record from the data log.
type Entry struct {
	// Key is the record key. It is an alias into the underlying buffer returned
	// by the Reader; callers that need to retain it must copy it.
	Key []byte
	// Value is the record value (nil for tombstones and empty values).
	Value []byte
	// Tombstone marks a deletion record. A tombstone supersedes any prior
	// value for the same key.
	Tombstone bool

	// FileID is the segment file identifier the record lives in.
	FileID int64
	// Offset is the byte offset of the record's first byte within its segment.
	Offset int64
	// Size is the total on-disk size of the framed record in bytes.
	Size int64
}

// header is the fixed-size prefix shared by every record.
type header struct {
	keyLen    uint32
	valueLen  uint32
	tombstone bool
}

// encodeHeader serialises the fixed prefix (without the crc32) into a 13-byte
// slice. The final byte is reserved and currently unused.
func encodeHeader(h header) [headerSize]byte {
	var b [headerSize]byte
	binary.BigEndian.PutUint32(b[0:4], h.keyLen)
	binary.BigEndian.PutUint32(b[4:8], h.valueLen)
	if h.tombstone {
		b[8] = 1
	}
	return b
}

// decodeHeader parses the fixed prefix (crc excluded) and returns its fields.
func decodeHeader(b []byte) header {
	h := header{
		keyLen:   binary.BigEndian.Uint32(b[0:4]),
		valueLen: binary.BigEndian.Uint32(b[4:8]),
	}
	h.tombstone = b[8] == 1
	return h
}

// checksumPayload builds the byte slice over which the crc32 is computed:
// the full fixed header followed by the key and value payloads.
func checksumPayload(h header, key, value []byte) []byte {
	hb := encodeHeader(h)
	payload := make([]byte, 0, len(hb)+len(key)+len(value))
	payload = append(payload, hb[:]...)
	payload = append(payload, key...)
	payload = append(payload, value...)
	return payload
}

// ComputeCRC returns the crc32 (IEEE) of a record described by h, key and value.
func ComputeCRC(key, value []byte, tombstone bool) uint32 {
	h := header{keyLen: uint32(len(key)), valueLen: uint32(len(value)), tombstone: tombstone}
	return crc32.ChecksumIEEE(checksumPayload(h, key, value))
}

// Encode serialises a record into its on-disk frame. The returned slice is
// safe to write directly to a segment file.
func Encode(key, value []byte, tombstone bool) []byte {
	h := header{keyLen: uint32(len(key)), valueLen: uint32(len(value)), tombstone: tombstone}
	body := checksumPayload(h, key, value)
	crc := crc32.ChecksumIEEE(body)

	out := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(out[0:4], crc)
	copy(out[4:], body)
	return out
}

// Decode parses a single frame from the start of data and verifies its crc32.
// It returns the decoded Entry (without FileID/Offset/Size populated) or one of
// ErrChecksum / ErrCorrupt. A short read at the very first byte yields io.EOF so
// callers can treat it as a clean end-of-log.
func Decode(data []byte) (*Entry, error) {
	if len(data) < headerSize+4 {
		return nil, ErrCorrupt
	}
	crc := binary.BigEndian.Uint32(data[0:4])
	h := decodeHeader(data[4 : 4+headerSize])
	total := headerSize + int(h.keyLen) + int(h.valueLen)
	if total < headerSize || 4+total > len(data) {
		return nil, ErrCorrupt
	}
	key := data[4+headerSize : 4+headerSize+int(h.keyLen)]
	value := data[4+headerSize+int(h.keyLen) : 4+total]

	body := checksumPayload(h, key, value)
	if crc32.ChecksumIEEE(body) != crc {
		return nil, ErrChecksum
	}
	return &Entry{
		Key:       key,
		Value:     value,
		Tombstone: h.tombstone,
		Size:      int64(4 + total),
	}, nil
}

// frameSize returns the on-disk size of the frame for a record with the given
// key/value lengths without allocating the full buffer.
func frameSize(keyLen, valueLen int) int64 {
	return int64(4 + headerSize + keyLen + valueLen)
}
