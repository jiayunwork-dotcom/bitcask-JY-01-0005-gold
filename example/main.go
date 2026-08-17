// Command example demonstrates the bitcask library end-to-end against a
// throwaway temporary directory. It does not depend on the network or any
// external service; run it with `go run ./example`.
package main

import (
	"fmt"
	"os"

	"bitcask/internal/store"
)

func main() {
	dir, err := os.MkdirTemp("", "bitcask-example")
	if err != nil {
		fmt.Fprintln(os.Stderr, "mkdir temp:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	st, err := store.Open(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer st.Close()

	sample := map[string]string{
		"user:1:name": "alice",
		"user:1:role": "admin",
		"user:2:name": "bob",
		"user:2:role": "reader",
	}
	for k, v := range sample {
		if err := st.Put([]byte(k), []byte(v)); err != nil {
			fmt.Fprintln(os.Stderr, "put:", err)
			os.Exit(1)
		}
	}

	// Overwrite and delete to show the log absorbs edits.
	if err := st.Put([]byte("user:1:role"), []byte("superuser")); err != nil {
		fmt.Fprintln(os.Stderr, "put overwrite:", err)
		os.Exit(1)
	}
	if err := st.Delete([]byte("user:2:role")); err != nil {
		fmt.Fprintln(os.Stderr, "delete:", err)
		os.Exit(1)
	}

	fmt.Printf("keys before merge: %d\n", st.Count())
	for _, k := range []string{"user:1:name", "user:1:role", "user:2:name", "user:2:role"} {
		v, ok, err := st.Get([]byte(k))
		if err != nil {
			fmt.Fprintln(os.Stderr, "get:", err)
			os.Exit(1)
		}
		fmt.Printf("  %-14s -> %q (present=%v)\n", k, v, ok)
	}

	// Compact the log and confirm the observable state is unchanged.
	if err := st.Merge(); err != nil {
		fmt.Fprintln(os.Stderr, "merge:", err)
		os.Exit(1)
	}
	fmt.Printf("segments after merge: %d\n", len(st.FileIDs()))

	// Persist, reopen, and verify the state survives a restart.
	if err := st.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "close:", err)
		os.Exit(1)
	}
	st2, err := store.Open(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reopen:", err)
		os.Exit(1)
	}
	defer st2.Close()

	if v, ok, _ := st2.Get([]byte("user:1:role")); !ok || string(v) != "superuser" {
		fmt.Fprintln(os.Stderr, "reopened value mismatch:", string(v), ok)
		os.Exit(1)
	}
	fmt.Println("reopened store agrees with the original state")
}
