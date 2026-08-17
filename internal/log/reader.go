package log

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
)

// maxRecordPayload guards against allocating an absurd amount of memory when a
// corrupted header reports an impossibly large key/value length. It is a safety
// bound, not a protocol limit; legitimate records are far smaller.
const maxRecordPayload = 1 << 30

// Reader iterates framed records sequentially from an io.Reader (typically a
// read-only segment file). It verifies the crc32 of every record and never
// panics on malformed input: a clean end-of-log yields io.EOF, a bit-flip in a
// record yields ErrChecksum, and a truncated record yields ErrCorrupt.
type Reader struct {
	r   io.Reader
	pos int64
}

// NewReader returns a Reader that starts at the beginning of r.
func NewReader(r io.Reader) *Reader {
	return &Reader{r: r}
}

// Position returns the number of bytes consumed so far, i.e. the offset just
// past the last successfully decoded record.
func (rd *Reader) Position() int64 { return rd.pos }

// Next decodes and returns the next record. It returns io.EOF at a clean end of
// the log (no partial record), ErrCorrupt if a record is truncated, and
// ErrChecksum if a record's crc32 does not match its payload.
func (rd *Reader) Next() (*Entry, error) {
	offset := rd.pos
	head := make([]byte, 4+headerSize)
	if _, err := io.ReadFull(rd.r, head); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, ErrCorrupt
		}
		return nil, err
	}
	crc := binary.BigEndian.Uint32(head[0:4])
	h := decodeHeader(head[4 : 4+headerSize])

	payloadLen := int(h.keyLen) + int(h.valueLen)
	if payloadLen < 0 || payloadLen > maxRecordPayload {
		return nil, ErrCorrupt
	}
	body := make([]byte, payloadLen)
	if _, err := io.ReadFull(rd.r, body); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, ErrCorrupt
		}
		return nil, err
	}

	// Recompute the crc32 exactly as Encode did: header bytes + payload.
	crcBody := make([]byte, 0, len(head[4:4+headerSize])+len(body))
	crcBody = append(crcBody, head[4:4+headerSize]...)
	crcBody = append(crcBody, body...)
	if crc32.ChecksumIEEE(crcBody) != crc {
		return nil, ErrChecksum
	}

	key := body[:h.keyLen]
	value := body[h.keyLen:]
	size := int64(4 + headerSize + payloadLen)

	rd.pos = offset + size
	return &Entry{
		Key:       key,
		Value:     value,
		Tombstone: h.tombstone,
		Offset:    offset,
		Size:      size,
	}, nil
}
