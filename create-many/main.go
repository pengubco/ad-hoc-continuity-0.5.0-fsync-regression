//go:build linux

// create-many creates a directory with N regular (non-sparse) files, each of size M bytes.
//
// Usage: create-many [-n 10000] [-size 5120] <output-dir>
package main

import (
	"crypto/rand"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	n := flag.Int("n", 10000, "number of files to create")
	size := flag.Int("size", 5120, "size of each file in bytes")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "Usage: create-many [flags] <output-dir>\n")
		os.Exit(1)
	}
	dir := flag.Arg(0)

	if err := os.MkdirAll(dir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir: %v\n", err)
		os.Exit(1)
	}

	buf := make([]byte, *size)

	for i := 0; i < *n; i++ {
		// Fill with random data so there are no zero runs that could be confused for holes
		if _, err := rand.Read(buf); err != nil {
			fmt.Fprintf(os.Stderr, "rand: %v\n", err)
			os.Exit(1)
		}

		path := filepath.Join(dir, fmt.Sprintf("file_%06d.bin", i))
		if err := os.WriteFile(path, buf, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "write file %d: %v\n", i, err)
			os.Exit(1)
		}
	}

	fmt.Printf("Created %d regular files in %s\n", *n, dir)
	fmt.Printf("  Each file: %d bytes (fully allocated, no holes)\n", *size)
}
