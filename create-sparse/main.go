//go:build linux

// create-sparse creates a sparse file at the given path.
//
// Usage: create-sparse [-size 1073741824] [-data-size 4096] <output-path>
//
// The file will have logical size = size, with data-size bytes written at offset 0
// and data-size bytes written at the end. Everything in between is a hole.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"syscall"
)

func main() {
	size := flag.Int64("size", 1<<30, "logical file size in bytes (default 1GiB)")
	dataSize := flag.Int("data-size", 4096, "bytes of actual data written at start and end")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "Usage: create-sparse [flags] <output-path>\n")
		os.Exit(1)
	}
	path := flag.Arg(0)

	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	// Write data at offset 0
	data := make([]byte, *dataSize)
	for i := range data {
		data[i] = 0xAA
	}
	if _, err := f.Write(data); err != nil {
		fmt.Fprintf(os.Stderr, "write start: %v\n", err)
		os.Exit(1)
	}

	// Seek to near the end, creating a hole
	endOffset := *size - int64(*dataSize)
	if _, err := f.Seek(endOffset, io.SeekStart); err != nil {
		fmt.Fprintf(os.Stderr, "seek: %v\n", err)
		os.Exit(1)
	}

	// Write data at the end
	for i := range data {
		data[i] = 0xBB
	}
	if _, err := f.Write(data); err != nil {
		fmt.Fprintf(os.Stderr, "write end: %v\n", err)
		os.Exit(1)
	}

	f.Close()
	printStats(path)
}

func printStats(path string) {
	fi, err := os.Stat(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stat: %v\n", err)
		return
	}
	fmt.Printf("Created: %s\n", path)
	fmt.Printf("  Logical size: %s (%d bytes)\n", humanSize(fi.Size()), fi.Size())

	// Use Sys() to get block count (512-byte blocks)
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		disk := st.Blocks * 512
		fmt.Printf("  Disk usage:   %s (%d bytes)\n", humanSize(disk), disk)
		fmt.Printf("  Sparseness:   %.4f%% of logical size on disk\n", float64(disk)/float64(fi.Size())*100)
	}
}

func humanSize(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.2f MiB", float64(b)/float64(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.2f KiB", float64(b)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
