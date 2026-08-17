# bitcask

bitcask is a small, dependency-free key/value store written in Go. It persists
data as an append-only, log-structured journal: every write (including updates
and deletions) is appended as a self-describing record to a segment file on
disk, and an in-memory hash index maps each key to the byte offset of its most
recent record so reads are O(1). Records are framed with a CRC-32 checksum, so
a corrupted journal is detected on read rather than silently returning bad
data. A `Merge` operation compacts the journal by rewriting only the live keys
into a fresh segment and dropping stale and tombstoned records, and it writes a
hint file so the next open can rebuild the index without rescanning the whole
log. The package is well suited to write-heavy workloads with a working set
that fits in memory.

## Build

```
go build ./...
```

## Test

```
go test ./...
```

## CLI

Build the binary and use it as a tiny administrative shell:

```
go run . --db ./mydb put user:1 alice
go run . --db ./mydb get user:1
go run . --db ./mydb del user:1
go run . --db ./mydb merge
```

Flags may appear before or after the subcommand (for example
`go run . put user:2 bob --db ./mydb`).

## Library

The engine is split into three internal packages so the on-disk format, the
index, and the public facade are independently testable:

- `internal/log` — the append-only segment file: record framing, CRC
  verification, and a streaming `Reader`.
- `internal/index` — the in-memory key → (fileID, offset, size, tombstone)
  hash index and the on-disk hint file used for fast reload.
- `internal/store` — the facade that wires the log and index together and
  exposes `Open`, `Put`, `Get`, `Delete`, `Merge` and `Close`.

A minimal library use looks like:

```go
st, _ := store.Open("/tmp/mydb")
defer st.Close()
_ = st.Put([]byte("k"), []byte("v"))
v, ok, _ := st.Get([]byte("k"))
_ = st.Merge()
```
