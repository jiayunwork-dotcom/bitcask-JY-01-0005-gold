// Command bitcask is a small CLI front-end for the bitcask key/value library.
// It only parses flags and wires calls into the store package; all real logic
// lives in internal/store, internal/index and internal/log.
//
// Usage:
//
//	bitcask --db <dir> put <key> <value>
//	bitcask --db <dir> get <key>
//	bitcask --db <dir> del <key>
//	bitcask --db <dir> merge
//
// Flags may appear before or after the subcommand; they are normalised to the
// front of the argument list before parsing so the standard flag package (which
// stops at the first non-flag) accepts both orderings.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"bitcask/internal/store"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// reorder moves every flag token (and its attached/separate value) to the front
// of the argument list, preserving relative order of flags and of positionals.
// This lets `bitcask put k v --db dir` parse identically to the flag-first form.
func reorder(args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			pos = append(pos, a)
			continue
		}
		flags = append(flags, a)
		// A flag written as `-db value` (no '=') consumes the next positional
		// as its value only when that token is not itself a flag.
		if !strings.Contains(a, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			flags = append(flags, args[i+1])
			i++
		}
	}
	return append(flags, pos...)
}

func run(args []string) int {
	fs := flag.NewFlagSet("bitcask", flag.ContinueOnError)
	db := fs.String("db", "", "database directory (required)")
	if err := fs.Parse(reorder(args)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	rest := fs.Args()
	if *db == "" {
		fmt.Fprintln(os.Stderr, "error: --db <dir> is required")
		usage()
		return 2
	}
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "error: missing subcommand")
		usage()
		return 2
	}

	st, err := store.Open(*db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		return 1
	}
	defer st.Close()

	cmd := rest[0]
	switch cmd {
	case "put":
		if len(rest) < 3 {
			fmt.Fprintln(os.Stderr, "error: put requires <key> <value>")
			return 2
		}
		if err := st.Put([]byte(rest[1]), []byte(rest[2])); err != nil {
			fmt.Fprintln(os.Stderr, "put:", err)
			return 1
		}
		fmt.Println("OK")

	case "get":
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "error: get requires <key>")
			return 2
		}
		v, ok, err := st.Get([]byte(rest[1]))
		if err != nil {
			fmt.Fprintln(os.Stderr, "get:", err)
			return 1
		}
		if !ok {
			fmt.Println("NOT FOUND")
			return 3
		}
		fmt.Printf("%s\n", v)

	case "del":
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "error: del requires <key>")
			return 2
		}
		if err := st.Delete([]byte(rest[1])); err != nil {
			fmt.Fprintln(os.Stderr, "del:", err)
			return 1
		}
		fmt.Println("OK")

	case "merge":
		if err := st.Merge(); err != nil {
			fmt.Fprintln(os.Stderr, "merge:", err)
			return 1
		}
		fmt.Println("OK")

	default:
		fmt.Fprintln(os.Stderr, "error: unknown subcommand", cmd)
		usage()
		return 2
	}
	return 0
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: bitcask --db <dir> <put|get|del|merge> [key] [value]")
}
